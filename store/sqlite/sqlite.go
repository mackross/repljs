// Package sqlite implements the store.Store interface backed by a SQLite
// database via modernc.org/sqlite (a pure-Go CGo-free driver).
//
// The store is append-only: facts are never updated or deleted. The schema
// keeps every fact as a JSON payload with indexed session/branch/cell columns
// so that query methods can filter efficiently without scanning all rows.
//
// WAL journal mode and a busy timeout are configured at open time to allow
// safe concurrent readers while a single writer appends facts.
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // register "sqlite" driver with database/sql

	"github.com/solidarity-ai/repl/model"
	"github.com/solidarity-ai/repl/store"
)

const (
	// busyTimeoutMS is the SQLite busy-timeout in milliseconds. If the
	// database is locked by another writer, the driver will retry for this
	// duration before returning SQLITE_BUSY.
	busyTimeoutMS = 5000

	// schema is the DDL applied at open time.  The facts table is append-only;
	// no UPDATE or DELETE is ever issued against it.
	schema = `
CREATE TABLE IF NOT EXISTS facts (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    session     TEXT    NOT NULL,
    branch      TEXT    NOT NULL DEFAULT '',
    cell        TEXT    NOT NULL DEFAULT '',
    fact_type   TEXT    NOT NULL,
    payload     TEXT    NOT NULL,
    appended_at TEXT    NOT NULL
);

CREATE TABLE IF NOT EXISTS runtime_configs (
    runtime_hash   TEXT PRIMARY KEY,
    runtime_config TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_facts_session       ON facts (session);
CREATE INDEX IF NOT EXISTS idx_facts_session_branch ON facts (session, branch);
CREATE INDEX IF NOT EXISTS idx_facts_session_cell   ON facts (session, cell);
`
)

// Store is the SQLite-backed implementation of store.Store.
// It is safe for concurrent use; the underlying *sql.DB manages the connection
// pool, and WAL mode allows concurrent readers alongside a single writer.
type Store struct {
	db *sql.DB
}

// Open opens (or creates) a SQLite database at the given DSN, configures WAL
// mode and busy timeout, applies the schema migrations, and returns a ready
// Store. The DSN may be a file path or the special ":memory:" string for an
// in-memory database.
//
// The caller is responsible for calling Close when the Store is no longer
// needed.
func Open(ctx context.Context, dsn string) (*Store, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite: open %q: %w", dsn, err)
	}

	// Limit to a single connection so that PRAGMA settings and WAL apply
	// consistently. modernc.org/sqlite is safe for single-connection concurrent
	// access because the driver serialises writes internally.
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0) // connections are long-lived

	s := &Store{db: db}
	if err := s.configure(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the underlying database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// configure sets SQLite pragmas required for correctness and performance.
func (s *Store) configure(ctx context.Context) error {
	pragmas := []string{
		fmt.Sprintf("PRAGMA busy_timeout = %d;", busyTimeoutMS),
		"PRAGMA journal_mode = WAL;",
		"PRAGMA synchronous = NORMAL;",
		"PRAGMA foreign_keys = ON;",
	}
	for _, p := range pragmas {
		if _, err := s.db.ExecContext(ctx, p); err != nil {
			return fmt.Errorf("sqlite: configure pragma %q: %w", p, err)
		}
	}
	return nil
}

// migrate applies the DDL schema to an empty or existing database.
// It is idempotent thanks to CREATE TABLE/INDEX IF NOT EXISTS.
func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("sqlite: migrate schema: %w", err)
	}
	return nil
}

// AppendFact durably records one append-only domain fact.
// The fact is encoded as JSON and stored alongside indexed metadata columns
// (session, branch, cell, fact_type) so that query helpers can filter without
// a full table scan.
//
// AppendFact does not import Toolbox-specific packages; it depends only on the
// model package and standard library JSON encoding.
func (s *Store) AppendFact(ctx context.Context, fact model.Fact) error {
	payload, err := json.Marshal(fact)
	if err != nil {
		return fmt.Errorf("sqlite: marshal fact %q: %w", fact.FactType(), err)
	}

	session, branch, cell := extractIndexColumns(fact)

	const q = `
INSERT INTO facts (session, branch, cell, fact_type, payload, appended_at)
VALUES (?, ?, ?, ?, ?, ?)`

	_, err = s.db.ExecContext(ctx, q,
		string(session),
		string(branch),
		string(cell),
		fact.FactType(),
		string(payload),
		time.Now().UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("sqlite: append fact %q: %w", fact.FactType(), err)
	}
	return nil
}

// PutRuntimeConfig stores a normalised runtime descriptor keyed by hash.
func (s *Store) PutRuntimeConfig(ctx context.Context, hash string, config json.RawMessage) error {
	const q = `
INSERT INTO runtime_configs (runtime_hash, runtime_config)
VALUES (?, ?)
ON CONFLICT(runtime_hash) DO NOTHING`
	if _, err := s.db.ExecContext(ctx, q, hash, string(config)); err != nil {
		return fmt.Errorf("sqlite: PutRuntimeConfig: %w", err)
	}
	stored, err := s.LoadRuntimeConfig(ctx, hash)
	if err != nil {
		return err
	}
	if string(stored) != string(config) {
		return fmt.Errorf("sqlite: PutRuntimeConfig: hash collision for %q", hash)
	}
	return nil
}

// LoadRuntimeConfig returns the runtime descriptor stored under hash.
func (s *Store) LoadRuntimeConfig(ctx context.Context, hash string) (json.RawMessage, error) {
	if hash == "" {
		return nil, nil
	}
	const q = `
SELECT runtime_config FROM runtime_configs
WHERE runtime_hash = ?
LIMIT 1`
	row := s.db.QueryRowContext(ctx, q, hash)
	var payload string
	if err := row.Scan(&payload); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("sqlite: LoadRuntimeConfig: runtime hash %q not found", hash)
		}
		return nil, fmt.Errorf("sqlite: LoadRuntimeConfig scan: %w", err)
	}
	return json.RawMessage(payload), nil
}

// LoadHead returns the current head record for the given session and branch.
// If the branch exists but has no HeadMoved facts yet, it returns a
// HeadRecord with an empty Head (zero CellID). If the branch is entirely
// unknown an explicit error is returned.
func (s *Store) LoadHead(ctx context.Context, session model.SessionID, branch model.BranchID) (store.HeadRecord, error) {
	// Verify the branch exists before querying HeadMoved so we can
	// distinguish "no commits yet" from "branch not found".
	exists, err := s.branchExists(ctx, session, branch)
	if err != nil {
		return store.HeadRecord{}, err
	}
	if !exists {
		return store.HeadRecord{}, fmt.Errorf("sqlite: LoadHead: session %q branch %q not found", session, branch)
	}

	const q = `
SELECT payload FROM facts
WHERE session = ? AND branch = ? AND fact_type = ?
ORDER BY id DESC
LIMIT 1`

	row := s.db.QueryRowContext(ctx, q, string(session), string(branch), model.FactTypeHeadMoved)
	var payload string
	if err := row.Scan(&payload); err != nil {
		if err == sql.ErrNoRows {
			// Branch exists but has no committed cells yet.
			return store.HeadRecord{Session: session, Branch: branch}, nil
		}
		return store.HeadRecord{}, fmt.Errorf("sqlite: LoadHead scan: %w", err)
	}

	var f model.HeadMoved
	if err := json.Unmarshal([]byte(payload), &f); err != nil {
		return store.HeadRecord{}, fmt.Errorf("sqlite: LoadHead decode: %w", err)
	}
	return store.HeadRecord{
		Session: f.Session,
		Branch:  f.Branch,
		Head:    f.Next,
	}, nil
}

// LoadStaticEnv reconstructs the static-environment snapshot needed by the
// type service for the given session, branch, and head cell.
func (s *Store) LoadStaticEnv(ctx context.Context, session model.SessionID, branch model.BranchID, head model.CellID) (store.StaticEnvSnapshot, error) {
	attached, err := s.loadManifestAttached(ctx, session)
	if err != nil {
		return store.StaticEnvSnapshot{}, err
	}
	manifest := attached.Manifest

	// Collect committed cell sources in insertion order up to and including
	// the head cell.
	sources, err := s.loadCommittedSources(ctx, session, branch, head)
	if err != nil {
		return store.StaticEnvSnapshot{}, err
	}

	return store.StaticEnvSnapshot{
		Session:          session,
		Branch:           branch,
		Head:             head,
		Manifest:         manifest,
		CommittedSources: sources,
	}, nil
}

// LoadReplayPlan builds the ordered replay plan for restoring a session to the
// given target cell.
func (s *Store) LoadReplayPlan(ctx context.Context, session model.SessionID, targetCell model.CellID) (store.ReplayPlan, error) {
	// Determine which branch owns targetCell.
	branch, err := s.branchForCell(ctx, session, targetCell)
	if err != nil {
		return store.ReplayPlan{}, err
	}

	// Collect ordered committed cells up to and including targetCell.
	steps, err := s.buildReplaySteps(ctx, session, branch, targetCell)
	if err != nil {
		return store.ReplayPlan{}, err
	}

	// Collect replay decisions for all effects touched in the plan.
	decisions, err := s.buildReplayDecisions(ctx, session, steps)
	if err != nil {
		return store.ReplayPlan{}, err
	}

	runtime, err := s.loadRuntimeAttached(ctx, session)
	if err != nil {
		return store.ReplayPlan{}, err
	}
	config, err := s.LoadRuntimeConfig(ctx, runtime.RuntimeHash)
	if err != nil {
		return store.ReplayPlan{}, err
	}

	return store.ReplayPlan{
		Session:       session,
		TargetCell:    targetCell,
		RuntimeHash:   runtime.RuntimeHash,
		RuntimeConfig: config,
		Steps:         steps,
		Decisions:     decisions,
	}, nil
}

// LoadFailures returns durable failed submit attempts for the given session in
// append order.
func (s *Store) LoadFailures(ctx context.Context, session model.SessionID) ([]model.CellFailed, error) {
	const q = `
SELECT payload FROM facts
WHERE session = ? AND fact_type = ?
ORDER BY id ASC`

	rows, err := s.db.QueryContext(ctx, q, string(session), model.FactTypeCellFailed)
	if err != nil {
		return nil, fmt.Errorf("sqlite: LoadFailures query: %w", err)
	}

	var failures []model.CellFailed
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("sqlite: LoadFailures scan: %w", err)
		}
		var f model.CellFailed
		if err := json.Unmarshal([]byte(payload), &f); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("sqlite: LoadFailures decode: %w", err)
		}
		failures = append(failures, f)
	}
	rowsErr := rows.Err()
	_ = rows.Close()
	if rowsErr != nil {
		return nil, fmt.Errorf("sqlite: LoadFailures rows: %w", rowsErr)
	}
	return failures, nil
}

// --- internal helpers ---

// extractIndexColumns extracts the session, branch, and cell fields from any
// known fact type for indexed storage. Unknown types yield empty strings for
// branch and cell so that the nullable index columns remain consistent.
func extractIndexColumns(fact model.Fact) (session model.SessionID, branch model.BranchID, cell model.CellID) {
	switch f := fact.(type) {
	case model.SessionStarted:
		return f.Session, model.BranchID(f.RootBranch), ""
	case model.ManifestAttached:
		return f.Session, "", ""
	case model.RuntimeAttached:
		return f.Session, "", ""
	case model.CellChecked:
		return f.Session, f.Branch, f.Cell
	case model.CellEvaluated:
		return f.Session, f.Branch, f.Cell
	case model.CellCommitted:
		return f.Session, f.Branch, f.Cell
	case model.CellFailed:
		return f.Session, f.Branch, ""
	case model.EffectStarted:
		return f.Session, "", f.Cell
	case model.EffectCompleted:
		return f.Session, "", ""
	case model.EffectFailed:
		return f.Session, "", ""
	case model.PromiseSettled:
		return f.Session, "", ""
	case model.HeadMoved:
		return f.Session, f.Branch, f.Next
	case model.BranchCreated:
		return f.Session, f.Branch, f.ParentCell
	case model.RestoreCompleted:
		return f.Session, "", f.TargetCell
	case model.CheckpointSaved:
		return f.Session, "", f.AtCell
	default:
		return "", "", ""
	}
}

// branchExists returns true if the given session/branch combination has at
// least one fact recorded (either the root branch mentioned in SessionStarted
// or a branch created via BranchCreated).
func (s *Store) branchExists(ctx context.Context, session model.SessionID, branch model.BranchID) (bool, error) {
	// The root branch is recorded in SessionStarted (branch column set to
	// RootBranch). Any other branch appears in BranchCreated.
	// A quick way to check: does any row exist for this session+branch?
	const q = `
SELECT 1 FROM facts
WHERE session = ? AND branch = ?
LIMIT 1`
	row := s.db.QueryRowContext(ctx, q, string(session), string(branch))
	var dummy int
	if err := row.Scan(&dummy); err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, fmt.Errorf("sqlite: branchExists scan: %w", err)
	}
	return true, nil
}

// loadManifestAttached returns the first ManifestAttached fact for the given
// session.
func (s *Store) loadManifestAttached(ctx context.Context, session model.SessionID) (model.ManifestAttached, error) {
	const q = `
SELECT payload FROM facts
WHERE session = ? AND fact_type = ?
ORDER BY id ASC
LIMIT 1`

	row := s.db.QueryRowContext(ctx, q, string(session), model.FactTypeManifestAttached)
	var payload string
	if err := row.Scan(&payload); err != nil {
		if err == sql.ErrNoRows {
			return model.ManifestAttached{}, fmt.Errorf("sqlite: loadManifestAttached: no manifest for session %q", session)
		}
		return model.ManifestAttached{}, fmt.Errorf("sqlite: loadManifestAttached scan: %w", err)
	}
	var f model.ManifestAttached
	if err := json.Unmarshal([]byte(payload), &f); err != nil {
		return model.ManifestAttached{}, fmt.Errorf("sqlite: loadManifestAttached decode: %w", err)
	}
	return f, nil
}

// loadRuntimeAttached returns the first RuntimeAttached fact for the given
// session.
func (s *Store) loadRuntimeAttached(ctx context.Context, session model.SessionID) (model.RuntimeAttached, error) {
	const q = `
SELECT payload FROM facts
WHERE session = ? AND fact_type = ?
ORDER BY id ASC
LIMIT 1`

	row := s.db.QueryRowContext(ctx, q, string(session), model.FactTypeRuntimeAttached)
	var payload string
	if err := row.Scan(&payload); err != nil {
		if err == sql.ErrNoRows {
			return model.RuntimeAttached{}, nil
		}
		return model.RuntimeAttached{}, fmt.Errorf("sqlite: loadRuntimeAttached scan: %w", err)
	}
	var f model.RuntimeAttached
	if err := json.Unmarshal([]byte(payload), &f); err != nil {
		return model.RuntimeAttached{}, fmt.Errorf("sqlite: loadRuntimeAttached decode: %w", err)
	}
	return f, nil
}

// loadManifest returns the manifest from the first ManifestAttached fact for
// the given session.
func (s *Store) loadManifest(ctx context.Context, session model.SessionID) (model.Manifest, error) {
	f, err := s.loadManifestAttached(ctx, session)
	if err != nil {
		return model.Manifest{}, err
	}
	return f.Manifest, nil
}

// loadCommittedSources returns the TypeScript sources for all CellCommitted
// facts reachable from the given branch, in insertion order, up to and
// including stopCell.
//
// For a forked branch the committed cells from its parent branch up to (and
// including) the fork-point cell are prepended automatically, so the returned
// list always represents the full linear history visible to the type service.
//
// The rows cursor is explicitly closed before any nested queries so that the
// single pooled connection is not held across recursive db calls (which would
// deadlock with MaxOpenConns=1).
func (s *Store) loadCommittedSources(ctx context.Context, session model.SessionID, branch model.BranchID, stopCell model.CellID) ([]string, error) {
	// Collect the branch ancestry chain from root → branch. This resolves
	// fork points so we can prepend ancestor cells in the correct order.
	chainCells, chainBranches, err := s.collectAncestorChain(ctx, session, branch, stopCell)
	if err != nil {
		return nil, err
	}

	sources := make([]string, 0, len(chainCells))
	for i, cellID := range chainCells {
		src, err := s.loadCellSource(ctx, session, chainBranches[i], cellID)
		if err != nil {
			return nil, err
		}
		sources = append(sources, src)
	}
	return sources, nil
}

// collectAncestorChain resolves the full ordered list of committed cell IDs
// and their owning branch IDs from the root branch through to the requested
// branch, stopping at stopCell (inclusive).
//
// The algorithm:
//  1. Walk the BranchCreated chain upward to build a stack of (branch,
//     forkCell) pairs from the target branch back to the root.
//  2. Walk the stack downward (root first), collecting CellCommitted facts on
//     each branch up to the fork point of the next branch (or stopCell for
//     the leaf branch).
func (s *Store) collectAncestorChain(ctx context.Context, session model.SessionID, branch model.BranchID, stopCell model.CellID) (cells []model.CellID, branches []model.BranchID, err error) {
	// Build ancestor stack: [(leafBranch, stopCell), (parentBranch, forkCell), …]
	type segment struct {
		branch   model.BranchID
		stopAt   model.CellID // inclusive upper bound for this segment
		forkCell model.CellID // the cell that was forked FROM (parent branch), empty for root
	}

	var stack []segment
	cur := branch
	curStop := stopCell
	for {
		parent, forkCell, found, lerr := s.branchParent(ctx, session, cur)
		if lerr != nil {
			return nil, nil, lerr
		}
		if !found {
			// cur is the root branch.
			stack = append(stack, segment{branch: cur, stopAt: curStop})
			break
		}
		stack = append(stack, segment{branch: cur, stopAt: curStop, forkCell: forkCell})
		// Walk parent's committed cells only up to (but not including) the
		// fork cell; the fork cell itself is already included in the parent
		// segment.
		curStop = forkCell
		cur = parent
	}

	// Walk the stack in reverse (root → leaf) and collect cell IDs.
	for i := len(stack) - 1; i >= 0; i-- {
		seg := stack[i]
		segCells, serr := s.committedCellsUpTo(ctx, session, seg.branch, seg.stopAt)
		if serr != nil {
			return nil, nil, serr
		}
		for _, c := range segCells {
			cells = append(cells, c)
			branches = append(branches, seg.branch)
		}
	}
	return cells, branches, nil
}

// branchParent returns the parent branch and the fork point cell for a
// non-root branch by reading its BranchCreated fact. For the root branch
// (no BranchCreated fact) it returns found=false.
func (s *Store) branchParent(ctx context.Context, session model.SessionID, branch model.BranchID) (parent model.BranchID, forkCell model.CellID, found bool, err error) {
	const q = `
SELECT payload FROM facts
WHERE session = ? AND branch = ? AND fact_type = ?
ORDER BY id ASC
LIMIT 1`

	row := s.db.QueryRowContext(ctx, q, string(session), string(branch), model.FactTypeBranchCreated)
	var payload string
	if err := row.Scan(&payload); err != nil {
		if err == sql.ErrNoRows {
			return "", "", false, nil
		}
		return "", "", false, fmt.Errorf("sqlite: branchParent scan: %w", err)
	}
	var f model.BranchCreated
	if err := json.Unmarshal([]byte(payload), &f); err != nil {
		return "", "", false, fmt.Errorf("sqlite: branchParent decode: %w", err)
	}
	// BranchCreated.Branch is the NEW branch; we need the parent branch.
	// The parent is the branch that owned ParentCell.
	parentBranch, err := s.branchForCell(ctx, session, f.ParentCell)
	if err != nil {
		return "", "", false, err
	}
	return parentBranch, f.ParentCell, true, nil
}

// committedCellsUpTo returns CellCommitted cell IDs on the given branch, in
// insertion order, up to and including stopCell.
func (s *Store) committedCellsUpTo(ctx context.Context, session model.SessionID, branch model.BranchID, stopCell model.CellID) ([]model.CellID, error) {
	const q = `
SELECT cell FROM facts
WHERE session = ? AND branch = ? AND fact_type = ?
ORDER BY id ASC`

	rows, err := s.db.QueryContext(ctx, q, string(session), string(branch), model.FactTypeCellCommitted)
	if err != nil {
		return nil, fmt.Errorf("sqlite: committedCellsUpTo query: %w", err)
	}

	var cellIDs []model.CellID
	for rows.Next() {
		var cell string
		if err := rows.Scan(&cell); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("sqlite: committedCellsUpTo scan: %w", err)
		}
		cellIDs = append(cellIDs, model.CellID(cell))
		if model.CellID(cell) == stopCell {
			break
		}
	}
	rowsErr := rows.Err()
	_ = rows.Close()
	if rowsErr != nil {
		return nil, fmt.Errorf("sqlite: committedCellsUpTo rows: %w", rowsErr)
	}
	return cellIDs, nil
}

// loadCellSource fetches the TypeScript source from the most recent
// CellChecked fact for the given cell.
func (s *Store) loadCellSource(ctx context.Context, session model.SessionID, branch model.BranchID, cell model.CellID) (string, error) {
	const q = `
SELECT payload FROM facts
WHERE session = ? AND branch = ? AND cell = ? AND fact_type = ?
ORDER BY id DESC
LIMIT 1`

	row := s.db.QueryRowContext(ctx, q, string(session), string(branch), string(cell), model.FactTypeCellChecked)
	var payload string
	if err := row.Scan(&payload); err != nil {
		if err == sql.ErrNoRows {
			return "", fmt.Errorf("sqlite: loadCellSource: no CellChecked for cell %q", cell)
		}
		return "", fmt.Errorf("sqlite: loadCellSource scan: %w", err)
	}
	var f model.CellChecked
	if err := json.Unmarshal([]byte(payload), &f); err != nil {
		return "", fmt.Errorf("sqlite: loadCellSource decode: %w", err)
	}
	return f.Source, nil
}

// branchForCell returns the branch ID for the CellCommitted fact matching the
// given session and cell.
func (s *Store) branchForCell(ctx context.Context, session model.SessionID, cell model.CellID) (model.BranchID, error) {
	const q = `
SELECT branch FROM facts
WHERE session = ? AND cell = ? AND fact_type = ?
ORDER BY id ASC
LIMIT 1`

	row := s.db.QueryRowContext(ctx, q, string(session), string(cell), model.FactTypeCellCommitted)
	var branch string
	if err := row.Scan(&branch); err != nil {
		if err == sql.ErrNoRows {
			return "", fmt.Errorf("sqlite: branchForCell: cell %q not committed in session %q", cell, session)
		}
		return "", fmt.Errorf("sqlite: branchForCell scan: %w", err)
	}
	return model.BranchID(branch), nil
}

// buildReplaySteps collects ordered ReplayStep entries for all committed cells
// reachable from the given branch up to and including stopCell. For forked
// branches the ancestor chain is traversed so replay always starts from the
// root.
func (s *Store) buildReplaySteps(ctx context.Context, session model.SessionID, branch model.BranchID, stopCell model.CellID) ([]store.ReplayStep, error) {
	cellIDs, branchIDs, err := s.collectAncestorChain(ctx, session, branch, stopCell)
	if err != nil {
		return nil, fmt.Errorf("sqlite: buildReplaySteps: %w", err)
	}

	var steps []store.ReplayStep
	for i, cell := range cellIDs {
		src, emitted, effects, err := s.loadCellDetails(ctx, session, branchIDs[i], cell)
		if err != nil {
			return nil, err
		}
		steps = append(steps, store.ReplayStep{
			Cell:      cell,
			Source:    src,
			EmittedJS: emitted,
			Effects:   effects,
		})
	}
	return steps, nil
}

// loadCellDetails fetches the source, emitted JS, and linked effects for one
// committed cell.
func (s *Store) loadCellDetails(ctx context.Context, session model.SessionID, branch model.BranchID, cell model.CellID) (src, emittedJS string, effects []model.EffectID, err error) {
	// Source and emittedJS from the most recent CellChecked fact.
	const checkedQ = `
SELECT payload FROM facts
WHERE session = ? AND branch = ? AND cell = ? AND fact_type = ?
ORDER BY id DESC LIMIT 1`

	row := s.db.QueryRowContext(ctx, checkedQ, string(session), string(branch), string(cell), model.FactTypeCellChecked)
	var payload string
	if err = row.Scan(&payload); err != nil && err != sql.ErrNoRows {
		return "", "", nil, fmt.Errorf("sqlite: loadCellDetails CellChecked scan: %w", err)
	}
	if err == nil {
		var f model.CellChecked
		if err = json.Unmarshal([]byte(payload), &f); err != nil {
			return "", "", nil, fmt.Errorf("sqlite: loadCellDetails CellChecked decode: %w", err)
		}
		src = f.Source
		emittedJS = f.EmittedJS
	}

	// LinkedEffects from the most recent CellEvaluated fact.
	const evalQ = `
SELECT payload FROM facts
WHERE session = ? AND branch = ? AND cell = ? AND fact_type = ?
ORDER BY id DESC LIMIT 1`

	row2 := s.db.QueryRowContext(ctx, evalQ, string(session), string(branch), string(cell), model.FactTypeCellEvaluated)
	var payload2 string
	if err = row2.Scan(&payload2); err != nil && err != sql.ErrNoRows {
		return "", "", nil, fmt.Errorf("sqlite: loadCellDetails CellEvaluated scan: %w", err)
	}
	if err == nil {
		var f model.CellEvaluated
		if err = json.Unmarshal([]byte(payload2), &f); err != nil {
			return "", "", nil, fmt.Errorf("sqlite: loadCellDetails CellEvaluated decode: %w", err)
		}
		effects = f.LinkedEffects
	}

	err = nil // reset ErrNoRows
	return src, emittedJS, effects, nil
}

// buildReplayDecisions collects ReplayDecision entries for all effects
// referenced in the given replay steps.
func (s *Store) buildReplayDecisions(ctx context.Context, session model.SessionID, steps []store.ReplayStep) (map[model.EffectID]store.ReplayDecision, error) {
	decisions := make(map[model.EffectID]store.ReplayDecision)

	for _, step := range steps {
		for _, effectID := range step.Effects {
			if _, seen := decisions[effectID]; seen {
				continue
			}
			dec, err := s.loadReplayDecision(ctx, session, effectID)
			if err != nil {
				return nil, err
			}
			decisions[effectID] = dec
		}
	}
	return decisions, nil
}

// loadReplayDecision builds a single ReplayDecision for the given effect.
// Both queries run sequentially with cursors fully drained and closed before
// the next query opens to avoid connection contention with MaxOpenConns=1.
func (s *Store) loadReplayDecision(ctx context.Context, session model.SessionID, effectID model.EffectID) (store.ReplayDecision, error) {
	// Load EffectStarted to get the replay policy.
	const startQ = `
SELECT payload FROM facts
WHERE session = ? AND fact_type = ?
ORDER BY id ASC`

	rows, err := s.db.QueryContext(ctx, startQ, string(session), model.FactTypeEffectStarted)
	if err != nil {
		return store.ReplayDecision{}, fmt.Errorf("sqlite: loadReplayDecision started query: %w", err)
	}

	var policy model.ReplayPolicy
	found := false
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			_ = rows.Close()
			return store.ReplayDecision{}, fmt.Errorf("sqlite: loadReplayDecision started scan: %w", err)
		}
		var f model.EffectStarted
		if err := json.Unmarshal([]byte(payload), &f); err != nil {
			_ = rows.Close()
			return store.ReplayDecision{}, fmt.Errorf("sqlite: loadReplayDecision started decode: %w", err)
		}
		if f.Effect == effectID {
			policy = f.ReplayPolicy
			found = true
			break
		}
	}
	rowsErr := rows.Err()
	_ = rows.Close() // release connection before next query
	if rowsErr != nil {
		return store.ReplayDecision{}, fmt.Errorf("sqlite: loadReplayDecision started rows: %w", rowsErr)
	}
	if !found {
		return store.ReplayDecision{}, fmt.Errorf("sqlite: loadReplayDecision: EffectStarted not found for effect %q", effectID)
	}

	// Attempt to load EffectCompleted result.
	const completedQ = `
SELECT payload FROM facts
WHERE session = ? AND fact_type = ?
ORDER BY id ASC`

	rows2, err := s.db.QueryContext(ctx, completedQ, string(session), model.FactTypeEffectCompleted)
	if err != nil {
		return store.ReplayDecision{}, fmt.Errorf("sqlite: loadReplayDecision completed query: %w", err)
	}

	var recordedResult []byte
	completionOrder := 0
	rowIndex := 0
	for rows2.Next() {
		rowIndex++
		var payload string
		if err := rows2.Scan(&payload); err != nil {
			_ = rows2.Close()
			return store.ReplayDecision{}, fmt.Errorf("sqlite: loadReplayDecision completed scan: %w", err)
		}
		var f model.EffectCompleted
		if err := json.Unmarshal([]byte(payload), &f); err != nil {
			_ = rows2.Close()
			return store.ReplayDecision{}, fmt.Errorf("sqlite: loadReplayDecision completed decode: %w", err)
		}
		if f.Effect == effectID {
			recordedResult = f.Result
			completionOrder = rowIndex
			break
		}
	}
	rows2Err := rows2.Err()
	_ = rows2.Close()
	if rows2Err != nil {
		return store.ReplayDecision{}, fmt.Errorf("sqlite: loadReplayDecision completed rows: %w", rows2Err)
	}

	return store.ReplayDecision{
		Effect:          effectID,
		Policy:          policy,
		RecordedResult:  recordedResult,
		CompletionOrder: completionOrder,
	}, nil
}
