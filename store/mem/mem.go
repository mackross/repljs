// Package mem implements the store.Store interface as an in-memory, append-only
// fact log protected by a sync.RWMutex. It is intended for use in tests and
// lightweight scenarios where a SQLite database file is not needed.
//
// The store provides identical branch/head/static-env/replay semantics to the
// SQLite implementation; all query helpers perform linear scans over the
// in-memory log rather than SQL queries. Correctness and contract parity with
// the SQLite store are verified by the parity test suite in mem_test.go.
package mem

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/solidarity-ai/repl/model"
	"github.com/solidarity-ai/repl/store"
)

// entry is one append-only record in the in-memory fact log.
// The extracted index fields mirror the SQLite schema's indexed columns so
// that linear scans can filter without a full type-assertion on every row.
type entry struct {
	session  model.SessionID
	branch   model.BranchID
	cell     model.CellID
	factType string
	fact     model.Fact
}

// Store is the in-memory implementation of store.Store. It is safe for
// concurrent use by multiple goroutines.
type Store struct {
	mu             sync.RWMutex
	facts          []entry
	runtimeConfigs map[string]json.RawMessage
}

// New returns a ready, empty in-memory Store. No cleanup is required; the
// store is eligible for garbage collection once all references are dropped.
func New() *Store {
	return &Store{runtimeConfigs: make(map[string]json.RawMessage)}
}

// AppendFact records one append-only domain fact to the in-memory log.
// The fact argument must implement model.Fact. Concurrent calls are safe.
func (s *Store) AppendFact(_ context.Context, fact model.Fact) error {
	session, branch, cell := extractIndexColumns(fact)
	e := entry{
		session:  session,
		branch:   branch,
		cell:     cell,
		factType: fact.FactType(),
		fact:     fact,
	}
	s.mu.Lock()
	s.facts = append(s.facts, e)
	s.mu.Unlock()
	return nil
}

// PutRuntimeConfig stores a normalised runtime descriptor keyed by hash.
func (s *Store) PutRuntimeConfig(_ context.Context, hash string, config json.RawMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.runtimeConfigs[hash]; ok {
		if string(existing) == string(config) {
			return nil
		}
		return fmt.Errorf("mem: PutRuntimeConfig: hash collision for %q", hash)
	}
	s.runtimeConfigs[hash] = append(json.RawMessage(nil), config...)
	return nil
}

// LoadRuntimeConfig returns the runtime descriptor stored under hash.
func (s *Store) LoadRuntimeConfig(_ context.Context, hash string) (json.RawMessage, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if hash == "" {
		return nil, nil
	}
	config, ok := s.runtimeConfigs[hash]
	if !ok {
		return nil, fmt.Errorf("mem: LoadRuntimeConfig: runtime hash %q not found", hash)
	}
	return append(json.RawMessage(nil), config...), nil
}

// LoadHead returns the current head record for the given session and branch.
// If the branch exists but has no HeadMoved facts yet the returned HeadRecord
// has an empty Head (zero CellID). If the branch does not exist an error is
// returned.
func (s *Store) LoadHead(_ context.Context, session model.SessionID, branch model.BranchID) (store.HeadRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if !s.branchExists(session, branch) {
		return store.HeadRecord{}, fmt.Errorf("mem: LoadHead: session %q branch %q not found", session, branch)
	}

	// Walk the log in reverse to find the latest HeadMoved for this branch.
	for i := len(s.facts) - 1; i >= 0; i-- {
		e := s.facts[i]
		if e.session != session || e.branch != branch || e.factType != model.FactTypeHeadMoved {
			continue
		}
		f, ok := e.fact.(model.HeadMoved)
		if !ok {
			continue
		}
		return store.HeadRecord{
			Session: f.Session,
			Branch:  f.Branch,
			Head:    f.Next,
		}, nil
	}

	// Branch exists but no HeadMoved facts yet.
	return store.HeadRecord{Session: session, Branch: branch}, nil
}

// LoadStaticEnv reconstructs the static-environment snapshot needed by the
// type service for the given session, branch, and head cell.
func (s *Store) LoadStaticEnv(_ context.Context, session model.SessionID, branch model.BranchID, head model.CellID) (store.StaticEnvSnapshot, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	attached, err := s.loadManifestAttached(session)
	if err != nil {
		return store.StaticEnvSnapshot{}, err
	}
	manifest := attached.Manifest

	sources, err := s.loadCommittedSources(session, branch, head)
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
	s.mu.RLock()
	defer s.mu.RUnlock()

	branch, err := s.branchForCell(session, targetCell)
	if err != nil {
		return store.ReplayPlan{}, err
	}

	steps, err := s.buildReplaySteps(session, branch, targetCell)
	if err != nil {
		return store.ReplayPlan{}, err
	}

	decisions, err := s.buildReplayDecisions(session, steps)
	if err != nil {
		return store.ReplayPlan{}, err
	}

	runtime, err := s.loadRuntimeAttached(session)
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

// ---------------------------------------------------------------------------
// Internal helpers (called with s.mu read-lock already held)
// ---------------------------------------------------------------------------

// extractIndexColumns mirrors the SQLite implementation to produce the
// session, branch, and cell index values for any known fact type.
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

// branchExists returns true if any fact row exists for the given session+branch
// combination, using the same logic as the SQLite branchExists helper.
// Must be called with s.mu held.
func (s *Store) branchExists(session model.SessionID, branch model.BranchID) bool {
	for _, e := range s.facts {
		if e.session == session && e.branch == branch {
			return true
		}
	}
	return false
}

// loadManifestAttached returns the first ManifestAttached fact for the given
// session. Must be called with s.mu held.
func (s *Store) loadManifestAttached(session model.SessionID) (model.ManifestAttached, error) {
	for _, e := range s.facts {
		if e.session != session || e.factType != model.FactTypeManifestAttached {
			continue
		}
		f, ok := e.fact.(model.ManifestAttached)
		if !ok {
			continue
		}
		return f, nil
	}
	return model.ManifestAttached{}, fmt.Errorf("mem: loadManifestAttached: no manifest for session %q", session)
}

// loadRuntimeAttached returns the first RuntimeAttached fact for the given
// session. Must be called with s.mu held.
func (s *Store) loadRuntimeAttached(session model.SessionID) (model.RuntimeAttached, error) {
	for _, e := range s.facts {
		if e.session != session || e.factType != model.FactTypeRuntimeAttached {
			continue
		}
		f, ok := e.fact.(model.RuntimeAttached)
		if !ok {
			continue
		}
		return f, nil
	}
	return model.RuntimeAttached{}, nil
}

// loadManifest returns the manifest from the first ManifestAttached fact for
// the given session. Must be called with s.mu held.
func (s *Store) loadManifest(session model.SessionID) (model.Manifest, error) {
	attached, err := s.loadManifestAttached(session)
	if err != nil {
		return model.Manifest{}, err
	}
	return attached.Manifest, nil
}

// loadCommittedSources collects TypeScript sources for all CellCommitted facts
// reachable from branch (including ancestor branches) in insertion order, up
// to and including stopCell. An empty stopCell returns an empty slice.
// Must be called with s.mu held.
func (s *Store) loadCommittedSources(session model.SessionID, branch model.BranchID, stopCell model.CellID) ([]string, error) {
	if stopCell == "" {
		return nil, nil
	}

	chainCells, chainBranches, err := s.collectAncestorChain(session, branch, stopCell)
	if err != nil {
		return nil, err
	}

	sources := make([]string, 0, len(chainCells))
	for i, cellID := range chainCells {
		src, err := s.loadCellSource(session, chainBranches[i], cellID)
		if err != nil {
			return nil, err
		}
		sources = append(sources, src)
	}
	return sources, nil
}

// collectAncestorChain resolves the full ordered list of committed cell IDs
// and their owning branch IDs from the root branch through to the target
// branch, stopping at stopCell (inclusive). The algorithm is identical to the
// SQLite implementation's collectAncestorChain.
// Must be called with s.mu held.
func (s *Store) collectAncestorChain(session model.SessionID, branch model.BranchID, stopCell model.CellID) (cells []model.CellID, branches []model.BranchID, err error) {
	type segment struct {
		branch   model.BranchID
		stopAt   model.CellID
		forkCell model.CellID
	}

	var stack []segment
	cur := branch
	curStop := stopCell
	for {
		parent, forkCell, found, lerr := s.branchParent(session, cur)
		if lerr != nil {
			return nil, nil, lerr
		}
		if !found {
			stack = append(stack, segment{branch: cur, stopAt: curStop})
			break
		}
		stack = append(stack, segment{branch: cur, stopAt: curStop, forkCell: forkCell})
		curStop = forkCell
		cur = parent
	}

	// Walk root → leaf.
	for i := len(stack) - 1; i >= 0; i-- {
		seg := stack[i]
		segCells, serr := s.committedCellsUpTo(session, seg.branch, seg.stopAt)
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

// branchParent returns the parent branch and fork-point cell for a non-root
// branch by finding its BranchCreated fact. For the root branch (no
// BranchCreated) it returns found=false.
// Must be called with s.mu held.
func (s *Store) branchParent(session model.SessionID, branch model.BranchID) (parent model.BranchID, forkCell model.CellID, found bool, err error) {
	for _, e := range s.facts {
		if e.session != session || e.branch != branch || e.factType != model.FactTypeBranchCreated {
			continue
		}
		f, ok := e.fact.(model.BranchCreated)
		if !ok {
			continue
		}
		parentBranch, lerr := s.branchForCell(session, f.ParentCell)
		if lerr != nil {
			return "", "", false, lerr
		}
		return parentBranch, f.ParentCell, true, nil
	}
	return "", "", false, nil
}

// committedCellsUpTo returns CellCommitted cell IDs on the given branch in
// insertion order, stopping at (and including) stopCell.
// Must be called with s.mu held.
func (s *Store) committedCellsUpTo(session model.SessionID, branch model.BranchID, stopCell model.CellID) ([]model.CellID, error) {
	var out []model.CellID
	for _, e := range s.facts {
		if e.session != session || e.branch != branch || e.factType != model.FactTypeCellCommitted {
			continue
		}
		f, ok := e.fact.(model.CellCommitted)
		if !ok {
			continue
		}
		out = append(out, f.Cell)
		if f.Cell == stopCell {
			break
		}
	}
	return out, nil
}

// loadCellSource returns the TypeScript source from the most recent
// CellChecked fact for the given cell on the given branch.
// Must be called with s.mu held.
func (s *Store) loadCellSource(session model.SessionID, branch model.BranchID, cell model.CellID) (string, error) {
	src := ""
	found := false
	for _, e := range s.facts {
		if e.session != session || e.branch != branch || e.cell != cell || e.factType != model.FactTypeCellChecked {
			continue
		}
		f, ok := e.fact.(model.CellChecked)
		if !ok {
			continue
		}
		src = f.Source
		found = true
		// Keep scanning to get the latest (last) CellChecked for this cell.
	}
	if !found {
		return "", fmt.Errorf("mem: loadCellSource: no CellChecked for cell %q", cell)
	}
	return src, nil
}

// branchForCell returns the branch that owns the CellCommitted fact for the
// given cell in the given session (first such fact wins).
// Must be called with s.mu held.
func (s *Store) branchForCell(session model.SessionID, cell model.CellID) (model.BranchID, error) {
	for _, e := range s.facts {
		if e.session != session || e.cell != cell || e.factType != model.FactTypeCellCommitted {
			continue
		}
		f, ok := e.fact.(model.CellCommitted)
		if !ok {
			continue
		}
		return f.Branch, nil
	}
	return "", fmt.Errorf("mem: branchForCell: cell %q not committed in session %q", cell, session)
}

// buildReplaySteps collects ordered ReplayStep entries for all committed cells
// reachable from branch up to and including stopCell via ancestor traversal.
// Must be called with s.mu held.
func (s *Store) buildReplaySteps(session model.SessionID, branch model.BranchID, stopCell model.CellID) ([]store.ReplayStep, error) {
	cellIDs, branchIDs, err := s.collectAncestorChain(session, branch, stopCell)
	if err != nil {
		return nil, fmt.Errorf("mem: buildReplaySteps: %w", err)
	}

	steps := make([]store.ReplayStep, 0, len(cellIDs))
	for i, cell := range cellIDs {
		src, emitted, effects, err := s.loadCellDetails(session, branchIDs[i], cell)
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

// loadCellDetails returns the source, emitted JS, and linked effects for one
// committed cell by scanning CellChecked and CellEvaluated facts.
// Must be called with s.mu held.
func (s *Store) loadCellDetails(session model.SessionID, branch model.BranchID, cell model.CellID) (src, emittedJS string, effects []model.EffectID, err error) {
	// Latest CellChecked for source + emittedJS.
	for _, e := range s.facts {
		if e.session != session || e.branch != branch || e.cell != cell || e.factType != model.FactTypeCellChecked {
			continue
		}
		f, ok := e.fact.(model.CellChecked)
		if !ok {
			continue
		}
		src = f.Source
		emittedJS = f.EmittedJS
	}

	// Latest CellEvaluated for linked effects.
	for _, e := range s.facts {
		if e.session != session || e.branch != branch || e.cell != cell || e.factType != model.FactTypeCellEvaluated {
			continue
		}
		f, ok := e.fact.(model.CellEvaluated)
		if !ok {
			continue
		}
		effects = f.LinkedEffects
	}

	return src, emittedJS, effects, nil
}

// buildReplayDecisions collects ReplayDecision entries for all effects
// referenced in the given replay steps.
// Must be called with s.mu held.
func (s *Store) buildReplayDecisions(session model.SessionID, steps []store.ReplayStep) (map[model.EffectID]store.ReplayDecision, error) {
	decisions := make(map[model.EffectID]store.ReplayDecision)
	for _, step := range steps {
		for _, effectID := range step.Effects {
			if _, seen := decisions[effectID]; seen {
				continue
			}
			dec, err := s.loadReplayDecision(session, effectID)
			if err != nil {
				return nil, err
			}
			decisions[effectID] = dec
		}
	}
	return decisions, nil
}

// loadReplayDecision builds a single ReplayDecision for the given effect by
// scanning EffectStarted and EffectCompleted facts.
// Must be called with s.mu held.
func (s *Store) loadReplayDecision(session model.SessionID, effectID model.EffectID) (store.ReplayDecision, error) {
	var policy model.ReplayPolicy
	found := false
	for _, e := range s.facts {
		if e.session != session || e.factType != model.FactTypeEffectStarted {
			continue
		}
		f, ok := e.fact.(model.EffectStarted)
		if !ok || f.Effect != effectID {
			continue
		}
		policy = f.ReplayPolicy
		found = true
		break
	}
	if !found {
		return store.ReplayDecision{}, fmt.Errorf("mem: loadReplayDecision: EffectStarted not found for effect %q", effectID)
	}

	var recordedResult []byte
	completionOrder := 0
	for i, e := range s.facts {
		if e.session != session || e.factType != model.FactTypeEffectCompleted {
			continue
		}
		f, ok := e.fact.(model.EffectCompleted)
		if !ok || f.Effect != effectID {
			continue
		}
		recordedResult = f.Result
		completionOrder = i + 1
		break
	}

	return store.ReplayDecision{
		Effect:          effectID,
		Policy:          policy,
		RecordedResult:  recordedResult,
		CompletionOrder: completionOrder,
	}, nil
}
