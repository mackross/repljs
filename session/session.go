// Package session provides the concrete implementation of the engine.Engine
// and engine.Session interfaces backed by any store.Store. The lifecycle facts
// appended by this package follow the exact order assumed by the S02 store
// algorithms so that replay and static-environment reconstruction remain
// correct without external coordination.
//
// Engine is safe for concurrent use. Each Session serialises its own
// evaluation sequence with a per-session mutex; concurrent Submit or Restore
// calls on the same Session are not supported by the callers, but the mutex
// defends against accidental concurrent access.
package session

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/solidarity-ai/repl/engine"
	"github.com/solidarity-ai/repl/model"
	"github.com/solidarity-ai/repl/store"
)

// Engine is the concrete implementation of engine.Engine. It is stateless
// beyond its dependencies and is safe for concurrent use.
type Engine struct{}

const defaultCellSettleTimeout = 5 * time.Second

func runtimeModeOrDefault(mode engine.RuntimeMode) engine.RuntimeMode {
	if mode == "" {
		return engine.RuntimeModePersistent
	}
	return mode
}

// New returns a ready Engine. The engine itself holds no per-session state;
// all session-level state lives in the returned Session values.
func New() *Engine {
	return &Engine{}
}

// StartSession creates a new session configured by cfg. It appends
// SessionStarted and ManifestAttached facts to the store before returning,
// so the session is durably recorded before the caller's first Submit.
//
// On any store error the method returns the error immediately; no partial
// state is visible to the caller.
func (e *Engine) StartSession(ctx context.Context, cfg model.SessionConfig, deps engine.SessionDeps) (engine.Session, error) {
	sessionID := model.SessionID(uuid.NewString())
	rootBranchID := model.BranchID(uuid.NewString())
	now := time.Now().UTC()

	mode := runtimeModeOrDefault(deps.RuntimeMode)

	if err := deps.Store.AppendFact(ctx, model.SessionStarted{
		Session:    sessionID,
		RootBranch: rootBranchID,
		At:         now,
	}); err != nil {
		return nil, fmt.Errorf("session: StartSession: append SessionStarted: %w", err)
	}

	rt, runtimeConfig, runtimeHash, err := newBranchRuntime(ctx, engine.SessionRuntimeContext{
		SessionID:   sessionID,
		BranchID:    rootBranchID,
		RuntimeHash: runtimeConfigHash(deps.RuntimeConfig),
	}, deps.Store, deps.VMDelegate, deps.RuntimeConfig, nil)
	if err != nil {
		return nil, fmt.Errorf("session: StartSession: init runtime: %w", err)
	}
	if err := deps.Store.PutRuntimeConfig(ctx, runtimeHash, runtimeConfig); err != nil {
		rt.close()
		return nil, fmt.Errorf("session: StartSession: store runtime config: %w", err)
	}

	if err := deps.Store.AppendFact(ctx, model.ManifestAttached{
		Session:    sessionID,
		ManifestID: cfg.Manifest.ID,
		Manifest:   cfg.Manifest,
		At:         now,
	}); err != nil {
		rt.close()
		return nil, fmt.Errorf("session: StartSession: append ManifestAttached: %w", err)
	}

	if err := deps.Store.AppendFact(ctx, model.RuntimeAttached{
		Session:     sessionID,
		RuntimeHash: runtimeHash,
		At:          now,
	}); err != nil {
		rt.close()
		return nil, fmt.Errorf("session: StartSession: append RuntimeAttached: %w", err)
	}

	return &session{
		id:            sessionID,
		store:         deps.Store,
		branch:        rootBranchID,
		head:          "", // no cells yet
		runtimeHash:   runtimeHash,
		runtimeConfig: append([]byte(nil), runtimeConfig...),
		delegate:      deps.VMDelegate,
		runtimeMode:   mode,
		runtime:       rt,
		values:        make(map[model.ValueID]engine.ValueView),
	}, nil
}

// RestoreSession replays history up to targetCell and returns a Session
// positioned at that cell. It loads the current head after restoring so the
// returned session reflects durable state.
//
// RestoreSession calls Restore internally to record the fork facts, then
// fetches the resulting head from the store to ground the returned session's
// in-memory state.
func (e *Engine) RestoreSession(ctx context.Context, sessionID model.SessionID, targetCell model.CellID, deps engine.SessionDeps) (engine.Session, error) {
	// Bootstrap a minimal session so restoreLocked can do its work. The branch
	// and head fields here are placeholders; restoreLocked replaces them
	// atomically after validating targetCell, replaying committed history into a
	// fresh VM, and durably appending the fork facts.
	//
	// The placeholder branch is never committed against — BranchCreated records
	// the fork from targetCell, not from this placeholder — so store invariants
	// are not violated.
	mode := runtimeModeOrDefault(deps.RuntimeMode)
	s := &session{
		id:            sessionID,
		store:         deps.Store,
		branch:        model.BranchID("__restore_bootstrap__"),
		head:          targetCell,
		runtimeHash:   runtimeConfigHash(deps.RuntimeConfig),
		runtimeConfig: append([]byte(nil), deps.RuntimeConfig...),
		delegate:      deps.VMDelegate,
		runtimeMode:   mode,
	}

	if err := s.Restore(ctx, targetCell); err != nil {
		return nil, fmt.Errorf("session: RestoreSession: %w", err)
	}

	// Confirm the new branch is durably recorded. The fork branch has no
	// HeadMoved facts yet so LoadHead returns an empty Head; we keep s.head =
	// targetCell (set by restoreLocked) as the parent for the first Submit.
	if _, err := deps.Store.LoadHead(ctx, sessionID, s.branch); err != nil {
		return nil, fmt.Errorf("session: RestoreSession: LoadHead after restore: %w", err)
	}

	return s, nil
}

// session is the concrete implementation of engine.Session. It is bound to
// a single session ID and active branch. All mutable fields are protected by
// mu to guard against accidental concurrent access.
type session struct {
	mu            sync.Mutex
	id            model.SessionID
	store         store.Store
	branch        model.BranchID                       // active branch
	head          model.CellID                         // most recently committed cell on the active branch
	runtimeHash   string                               // stable identity derived from runtimeConfig
	runtimeConfig []byte                               // serialised runtime descriptor for fresh VM creation
	delegate      engine.VMDelegate                    // optional VM configuration hook
	runtimeMode   engine.RuntimeMode                   // persistent vs replay-per-submit
	runtime       *branchRuntime                       // branch-local goja VM; replaced on each Restore
	values        map[model.ValueID]engine.ValueView // inspectable value handles for the current branch
}

// ID returns the stable session identifier.
func (s *session) ID() model.SessionID {
	return s.id
}

func withCellSettleTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, defaultCellSettleTimeout)
}

func (s *session) prepareSubmitRuntime(ctx context.Context) (*branchRuntime, []byte, string, error) {
	if s.runtimeMode != engine.RuntimeModeReplayPerSubmit {
		return s.runtime, append([]byte(nil), s.runtimeConfig...), s.runtimeHash, nil
	}
	if s.head == "" {
		rt, runtimeConfig, runtimeHash, err := newBranchRuntime(ctx, engine.SessionRuntimeContext{
			SessionID:   s.id,
			BranchID:    s.branch,
			RuntimeHash: s.runtimeHash,
		}, s.store, s.delegate, s.runtimeConfig, nil)
		if err != nil {
			return nil, nil, "", err
		}
		return rt, runtimeConfig, runtimeHash, nil
	}
	plan, err := s.store.LoadReplayPlan(ctx, s.id, s.head)
	if err != nil {
		return nil, nil, "", fmt.Errorf("load replay plan for submit: %w", err)
	}
	rt, runtimeConfig, runtimeHash, err := replayPlanIntoRuntime(ctx, plan, engine.SessionRuntimeContext{
		SessionID:   s.id,
		BranchID:    s.branch,
		RuntimeHash: plan.RuntimeHash,
	}, s.store, s.delegate)
	if err != nil {
		return nil, nil, "", fmt.Errorf("rebuild runtime for submit: %w", err)
	}
	return rt, runtimeConfig, runtimeHash, nil
}

// Submit evaluates src in the branch-local goja runtime, then commits the
// resulting cell to durable history. Evaluation happens before any fact is
// appended so that a parse or runtime failure leaves both the VM state and
// committed history unchanged.
//
// Fact sequence appended on success:
//  1. CellChecked   (records source and empty diagnostics)
//  2. CellEvaluated (records completion value from the runtime)
//  3. CellCommitted (marks the cell as part of durable history)
//  4. HeadMoved     (advances the branch head)
//
// s.head is only updated after all four facts succeed durably. On any error
// before or during fact-appending, the in-memory head and runtime state are
// left unchanged.
func (s *session) Submit(ctx context.Context, src string) (engine.SubmitResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// --- Step 1: evaluate in the live goja VM before touching durable state ---
	evalRuntime, evalRuntimeConfig, evalRuntimeHash, err := s.prepareSubmitRuntime(ctx)
	if err != nil {
		return engine.SubmitResult{}, fmt.Errorf("session: Submit: prepare runtime: %w", err)
	}
	freshRuntime := evalRuntime != s.runtime
	cellID := model.CellID(uuid.NewString())
	acc := &effectAccumulator{}
	evalRuntime.beginCell(cellID, acc)
	defer evalRuntime.endCell()

	evalCtx, cancel := withCellSettleTimeout(ctx)
	defer cancel()
	eval, err := evalRuntime.runContext(evalCtx, src)
	if err != nil {
		if freshRuntime {
			evalRuntime.close()
		}
		return engine.SubmitResult{}, fmt.Errorf("session: Submit: eval: %w", err)
	}

	effects, promises := acc.drain()
	now := time.Now().UTC()
	previousHead := s.head

	if err := s.store.AppendFact(ctx, model.CellChecked{
		Session:     s.id,
		Branch:      s.branch,
		Cell:        cellID,
		Parent:      previousHead,
		Source:      src,
		Diagnostics: nil,
		HasErrors:   false,
		At:          now,
	}); err != nil {
		if freshRuntime {
			evalRuntime.close()
		}
		return engine.SubmitResult{}, fmt.Errorf("session: Submit: append CellChecked: %w", err)
	}

	if err := s.store.AppendFact(ctx, model.CellEvaluated{
		Session:         s.id,
		Branch:          s.branch,
		Cell:            cellID,
		CreatedPromises: promises,
		LinkedEffects:   effects,
		CompletionValue: eval.completionValue,
		At:              now,
	}); err != nil {
		if freshRuntime {
			evalRuntime.close()
		}
		return engine.SubmitResult{}, fmt.Errorf("session: Submit: append CellEvaluated: %w", err)
	}

	if err := s.store.AppendFact(ctx, model.CellCommitted{
		Session: s.id,
		Branch:  s.branch,
		Cell:    cellID,
		At:      now,
	}); err != nil {
		if freshRuntime {
			evalRuntime.close()
		}
		return engine.SubmitResult{}, fmt.Errorf("session: Submit: append CellCommitted: %w", err)
	}

	if err := s.store.AppendFact(ctx, model.HeadMoved{
		Session:  s.id,
		Branch:   s.branch,
		Previous: previousHead,
		Next:     cellID,
		At:       now,
	}); err != nil {
		if freshRuntime {
			evalRuntime.close()
		}
		return engine.SubmitResult{}, fmt.Errorf("session: Submit: append HeadMoved: %w", err)
	}

	// Only advance in-memory head after all durable facts succeed.
	s.head = cellID
	if freshRuntime {
		oldRuntime := s.runtime
		s.runtime = evalRuntime
		s.runtimeConfig = append([]byte(nil), evalRuntimeConfig...)
		s.runtimeHash = evalRuntimeHash
		if oldRuntime != nil {
			oldRuntime.close()
		}
	}

	// Register an inspectable value handle when the eval produced a completion value.
	// This must happen after all durable facts succeed so rejected/pending cells never
	// get handles — the eval error above returns early before reaching this point.
	if eval.completionValue != nil {
		s.values[eval.completionValue.ID] = engine.ValueView{
			Handle:     eval.completionValue.ID,
			Preview:    eval.completionValue.Preview,
			TypeHint:   eval.completionValue.TypeHint,
			Structured: eval.structured,
		}
	}

	return engine.SubmitResult{
		Cell:            cellID,
		CompletionValue: eval.completionValue,
		CreatedPromises: promises,
	}, nil
}

// Inspect returns a view of the runtime value identified by handle. It looks up
// the handle in the branch-local value registry. Unknown or stale handles return
// an explicit error; callers must not treat a missing handle as an empty view.
func (s *session) Inspect(_ context.Context, handle model.ValueID) (engine.ValueView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	view, ok := s.values[handle]
	if !ok {
		return engine.ValueView{}, fmt.Errorf("session: Inspect: unknown handle %q", handle)
	}
	return view, nil
}

// Restore positions this session at targetCell by forking a new branch. It
// appends BranchCreated and RestoreCompleted facts, then updates the session's
// active branch and head to the new branch.
//
// Fact sequence appended:
//  1. BranchCreated  (records the fork point)
//  2. RestoreCompleted (records the restore event)
//
// s.branch and s.head are only updated after both facts succeed durably.
func (s *session) Restore(ctx context.Context, targetCell model.CellID) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.restoreLocked(ctx, targetCell)
}

// restoreLocked implements Restore without acquiring the mutex. It must be
// called with s.mu held. This allows RestoreSession (which also calls Restore
// via the public method) to share the implementation without double-locking.
//
// The method loads the replay plan for targetCell first: if targetCell is
// unknown the store returns an error here and the session is left unchanged.
// It then replays committed history into a fresh VM before appending any
// durable facts, so an evaluation failure during replay also aborts cleanly
// without creating a dangling branch.
func (s *session) restoreLocked(ctx context.Context, targetCell model.CellID) error {
	// Step 1: validate targetCell and load the ordered replay plan.
	// An unknown cell causes the store to return an error here, aborting
	// restore before any branch is created.
	plan, err := s.store.LoadReplayPlan(ctx, s.id, targetCell)
	if err != nil {
		return fmt.Errorf("session: Restore: load replay plan: %w", err)
	}
	newBranchID := model.BranchID(uuid.NewString())

	// Step 2: replay committed history into a fresh VM. We do this before
	// appending any durable facts so that a replay failure leaves both
	// in-memory state and the store unchanged.
	rt, runtimeConfig, runtimeHash, err := replayPlanIntoRuntime(ctx, plan, engine.SessionRuntimeContext{
		SessionID:   s.id,
		BranchID:    newBranchID,
		RuntimeHash: plan.RuntimeHash,
	}, s.store, s.delegate)
	if err != nil {
		return fmt.Errorf("session: Restore: replay history: %w", err)
	}

	// Step 3: append the fork facts now that the runtime is ready.
	now := time.Now().UTC()

	if err := s.store.AppendFact(ctx, model.BranchCreated{
		Session:    s.id,
		Branch:     newBranchID,
		ParentCell: targetCell,
		At:         now,
	}); err != nil {
		rt.close()
		return fmt.Errorf("session: Restore: append BranchCreated: %w", err)
	}

	if err := s.store.AppendFact(ctx, model.RestoreCompleted{
		Session:    s.id,
		TargetCell: targetCell,
		NewBranch:  newBranchID,
		At:         now,
	}); err != nil {
		rt.close()
		return fmt.Errorf("session: Restore: append RestoreCompleted: %w", err)
	}

	// Step 4: commit in-memory state only after all durable facts succeed.
	// The replayed runtime contains exactly the bindings visible at targetCell
	// on the ancestor branch — no post-fork bindings bleed in.
	// The value registry is reset so pre-fork handles are not accessible on
	// the new branch; callers that need a handle must re-submit the cell.
	oldRuntime := s.runtime
	s.branch = newBranchID
	s.head = targetCell
	s.runtimeHash = runtimeHash
	s.runtimeConfig = append([]byte(nil), runtimeConfig...)
	s.runtime = rt
	s.values = make(map[model.ValueID]engine.ValueView)
	if oldRuntime != nil {
		oldRuntime.close()
	}

	return nil
}

// Close releases all resources associated with the session.
func (s *session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runtime != nil {
		s.runtime.close()
		s.runtime = nil
	}
	return nil
}
