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
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/mackross/repljs/engine"
	"github.com/mackross/repljs/jswire"
	"github.com/mackross/repljs/model"
	"github.com/mackross/repljs/store"
	"github.com/mackross/repljs/typescript"
)

// Engine is the concrete implementation of engine.Engine. It is stateless
// beyond its dependencies and is safe for concurrent use.
type Engine struct{}

const defaultCellSettleTimeout = 5 * time.Second

var (
	gojaParserLineColumnPattern = regexp.MustCompile(`Line (\d+):(\d+)`)
	gojaStackLineColumnPattern  = regexp.MustCompile(`cell\.js:(\d+):(\d+)\(`)
)

type translatedEvalError struct {
	cause   error
	message string
}

func (e *translatedEvalError) Error() string {
	if e == nil {
		return ""
	}
	return e.message
}

func (e *translatedEvalError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func runtimeModeOrDefault(mode engine.RuntimeMode) engine.RuntimeMode {
	if mode == "" {
		return engine.RuntimeModePersistent
	}
	return mode
}

func languageOrDefault(language model.CellLanguage) model.CellLanguage {
	if language == "" {
		return model.CellLanguageJavaScript
	}
	return language
}

func translateEvalError(err error) error {
	if err == nil {
		return nil
	}
	rendered := translateWrappedCellLocations(err.Error())
	if rendered == err.Error() {
		return err
	}
	return &translatedEvalError{cause: err, message: rendered}
}

func translateWrappedCellLocations(msg string) string {
	if msg == "" {
		return msg
	}
	msg = gojaParserLineColumnPattern.ReplaceAllStringFunc(msg, func(match string) string {
		parts := gojaParserLineColumnPattern.FindStringSubmatch(match)
		if len(parts) != 3 {
			return match
		}
		line, _ := strconv.Atoi(parts[1])
		column, _ := strconv.Atoi(parts[2])
		if line > 1 {
			line--
		} else {
			line = 1
		}
		return fmt.Sprintf("Line %d:%d", line, column)
	})
	msg = gojaStackLineColumnPattern.ReplaceAllStringFunc(msg, func(match string) string {
		parts := gojaStackLineColumnPattern.FindStringSubmatch(match)
		if len(parts) != 3 {
			return match
		}
		line, _ := strconv.Atoi(parts[1])
		column, _ := strconv.Atoi(parts[2])
		if line > 1 {
			line--
		} else {
			line = 1
		}
		return fmt.Sprintf("cell.js:%d:%d(", line, column)
	})
	return msg
}

// New returns a ready Engine. The engine itself holds no per-session state;
// all session-level state lives in the returned Session values.
func New() *Engine {
	return &Engine{}
}

// StartSession creates a new session configured by cfg. It fully initialises
// the runtime and persists the runtime descriptor before appending
// SessionStarted/ManifestAttached/RuntimeAttached, so failed startup does not
// leak a partially resumable session.
//
// On any store error the method returns the error immediately; no partial
// state is visible to the caller.
func (e *Engine) StartSession(ctx context.Context, cfg model.SessionConfig, deps engine.SessionDeps) (engine.Session, error) {
	sessionID := model.SessionID(uuid.NewString())
	rootBranchID := model.BranchID(uuid.NewString())
	now := time.Now().UTC()

	mode := runtimeModeOrDefault(deps.RuntimeMode)

	rt, runtimeConfig, runtimeHash, err := newBranchRuntime(ctx, engine.SessionRuntimeContext{
		SessionID: sessionID,
		BranchID:  rootBranchID,
	}, deps.Store, deps.VMDelegate, deps.RuntimeConfig, nil)
	if err != nil {
		return nil, fmt.Errorf("session: StartSession: init runtime: %w", err)
	}
	if err := deps.Store.PutRuntimeConfig(ctx, runtimeHash, runtimeConfig); err != nil {
		rt.close()
		return nil, fmt.Errorf("session: StartSession: store runtime config: %w", err)
	}

	if err := deps.Store.AppendFact(ctx, model.SessionStarted{
		Session:    sessionID,
		RootBranch: rootBranchID,
		At:         now,
	}); err != nil {
		rt.close()
		return nil, fmt.Errorf("session: StartSession: append SessionStarted: %w", err)
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
		currentIndex:  0,
		values:        make(map[model.ValueID]engine.ValueView),
		tsFactory:     deps.TypeScriptFactory,
		tsEnvProvider: deps.TypeScriptEnvProvider,
	}, nil
}

// OpenSession reopens an existing durable session at its current active
// branch/head cursor without appending any new facts.
func (e *Engine) OpenSession(ctx context.Context, sessionID model.SessionID, deps engine.SessionDeps) (engine.Session, error) {
	state, err := deps.Store.LoadSessionState(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("session: OpenSession: load state: %w", err)
	}
	return e.openSessionState(ctx, state, deps)
}

// RestoreSession replays history up to targetCell and returns a Session
// positioned at that existing committed cell on its owning branch.
func (e *Engine) RestoreSession(ctx context.Context, sessionID model.SessionID, targetCell model.CellID, deps engine.SessionDeps) (engine.Session, error) {
	s, err := e.bootstrapExistingSession(ctx, sessionID, deps)
	if err != nil {
		return nil, err
	}
	if err := s.Restore(ctx, targetCell); err != nil {
		return nil, fmt.Errorf("session: RestoreSession: %w", err)
	}
	return s, nil
}

// ForkSession creates a new branch rooted at targetCell and returns a Session
// positioned on that new branch.
func (e *Engine) ForkSession(ctx context.Context, sessionID model.SessionID, targetCell model.CellID, deps engine.SessionDeps) (engine.Session, error) {
	s, err := e.bootstrapExistingSession(ctx, sessionID, deps)
	if err != nil {
		return nil, err
	}
	if err := s.Fork(ctx, targetCell); err != nil {
		return nil, fmt.Errorf("session: ForkSession: %w", err)
	}
	return s, nil
}

func (e *Engine) bootstrapExistingSession(ctx context.Context, sessionID model.SessionID, deps engine.SessionDeps) (*session, error) {
	state, err := deps.Store.LoadSessionState(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("session: bootstrapExistingSession: load state: %w", err)
	}
	return &session{
		id:            sessionID,
		store:         deps.Store,
		branch:        state.Branch,
		head:          state.Head,
		runtimeHash:   state.RuntimeHash,
		runtimeConfig: cloneRawMessage(state.RuntimeConfig),
		delegate:      deps.VMDelegate,
		runtimeMode:   runtimeModeOrDefault(deps.RuntimeMode),
		currentIndex:  0,
		values:        make(map[model.ValueID]engine.ValueView),
		tsFactory:     deps.TypeScriptFactory,
		tsEnvProvider: deps.TypeScriptEnvProvider,
	}, nil
}

func (e *Engine) openSessionState(ctx context.Context, state store.SessionState, deps engine.SessionDeps) (engine.Session, error) {
	s := &session{
		id:            state.Session,
		store:         deps.Store,
		branch:        state.Branch,
		head:          state.Head,
		runtimeHash:   state.RuntimeHash,
		runtimeConfig: cloneRawMessage(state.RuntimeConfig),
		delegate:      deps.VMDelegate,
		runtimeMode:   runtimeModeOrDefault(deps.RuntimeMode),
		currentIndex:  0,
		values:        make(map[model.ValueID]engine.ValueView),
		tsFactory:     deps.TypeScriptFactory,
		tsEnvProvider: deps.TypeScriptEnvProvider,
	}

	var (
		rt            *branchRuntime
		runtimeConfig json.RawMessage
		runtimeHash   string
		err           error
	)
	if state.Head == "" {
		rt, runtimeConfig, runtimeHash, err = newBranchRuntime(ctx, engine.SessionRuntimeContext{
			SessionID:   state.Session,
			BranchID:    state.Branch,
			RuntimeHash: state.RuntimeHash,
		}, deps.Store, deps.VMDelegate, state.RuntimeConfig, nil)
		if err != nil {
			return nil, fmt.Errorf("session: OpenSession: init runtime: %w", err)
		}
	} else {
		plan, err := deps.Store.LoadReplayPlan(ctx, state.Session, state.Head)
		if err != nil {
			return nil, fmt.Errorf("session: OpenSession: load replay plan: %w", err)
		}
		rt, runtimeConfig, runtimeHash, err = replayPlanIntoRuntime(ctx, plan, engine.SessionRuntimeContext{
			SessionID:   state.Session,
			BranchID:    state.Branch,
			RuntimeHash: plan.RuntimeHash,
		}, deps.Store, deps.VMDelegate)
		if err != nil {
			return nil, fmt.Errorf("session: OpenSession: replay history: %w", err)
		}
		staticEnv, err := deps.Store.LoadStaticEnv(ctx, state.Session, state.Branch, state.Head)
		if err != nil {
			rt.close()
			return nil, fmt.Errorf("session: OpenSession: load static env: %w", err)
		}
		s.installLoadedBranchState(loadedBranchState{
			branch:        state.Branch,
			head:          state.Head,
			runtime:       rt,
			runtimeConfig: runtimeConfig,
			runtimeHash:   runtimeHash,
			currentIndex:  len(plan.Steps),
			staticEnv:     staticEnv,
		})
		return s, nil
	}

	s.installLoadedBranchState(loadedBranchState{
		branch:        state.Branch,
		head:          state.Head,
		runtime:       rt,
		runtimeConfig: runtimeConfig,
		runtimeHash:   runtimeHash,
	})
	return s, nil
}

func cloneRawMessage(in json.RawMessage) json.RawMessage {
	if len(in) == 0 {
		return nil
	}
	return append(json.RawMessage(nil), in...)
}

// session is the concrete implementation of engine.Session. It is bound to
// a single session ID and active branch. All mutable fields are protected by
// mu to guard against accidental concurrent access.
type session struct {
	mu               sync.Mutex
	id               model.SessionID
	store            store.Store
	branch           model.BranchID                     // active branch
	head             model.CellID                       // most recently committed cell on the active branch
	runtimeHash      string                             // stable identity derived from runtimeConfig
	runtimeConfig    []byte                             // serialised runtime descriptor for fresh VM creation
	delegate         engine.VMDelegate                  // optional VM configuration hook
	runtimeMode      engine.RuntimeMode                 // persistent vs replay-per-submit
	runtime          *branchRuntime                     // branch-local goja VM; replaced on each Restore
	runtimeDirty     bool                               // live runtime diverged from durable head after a failed submit; rebuild before reuse
	currentIndex     int                                // branch-local monotonic index of the current committed head
	values           map[model.ValueID]engine.ValueView // inspectable value handles for the current branch
	tsFactory        typescript.Factory
	tsEnvProvider    engine.TypeScriptEnvProvider
	tsSession        typescript.Session
	committedSources []string
	tsEnvHash        string
	tsEpochTS        string
}

type loadedBranchState struct {
	branch        model.BranchID
	head          model.CellID
	runtime       *branchRuntime
	runtimeConfig json.RawMessage
	runtimeHash   string
	currentIndex  int
	staticEnv     store.StaticEnvSnapshot
}

// ID returns the stable session identifier.
func (s *session) ID() model.SessionID {
	return s.id
}

func effectIDsFromSummaries(summaries []engine.EffectSummary) []model.EffectID {
	if len(summaries) == 0 {
		return nil
	}
	ids := make([]model.EffectID, 0, len(summaries))
	for _, summary := range summaries {
		ids = append(ids, summary.Effect)
	}
	return ids
}

func cloneEffectSummaries(in []engine.EffectSummary) []engine.EffectSummary {
	if len(in) == 0 {
		return nil
	}
	out := make([]engine.EffectSummary, 0, len(in))
	for _, summary := range in {
		summary.Params = cloneBytes(summary.Params)
		summary.Result = cloneBytes(summary.Result)
		out = append(out, summary)
	}
	return out
}

func (s *session) resetTypeScriptSession() {
	if s.tsSession != nil {
		_ = s.tsSession.Close()
		s.tsSession = nil
	}
}

func (s *session) ensureTypeScriptSession(ctx context.Context) (typescript.Session, error) {
	if s.tsFactory == nil {
		return nil, fmt.Errorf("typescript mode is not configured for this session")
	}
	if s.tsSession == nil {
		tsSession, err := s.tsFactory.NewSession(ctx)
		if err != nil {
			return nil, fmt.Errorf("create typescript session: %w", err)
		}
		tsSession.SetCommittedSources(s.committedSources)
		s.tsSession = tsSession
	}
	return s.tsSession, nil
}

func (s *session) setStaticEnvSnapshot(snapshot store.StaticEnvSnapshot) {
	s.committedSources = append([]string(nil), snapshot.CommittedSources...)
	s.tsEnvHash = snapshot.TypeScriptEnvHash
	s.tsEpochTS = snapshot.TypeScriptEpochTS
}

func (s *session) installLoadedBranchState(state loadedBranchState) {
	oldRuntime := s.runtime
	s.resetTypeScriptSession()
	s.branch = state.branch
	s.head = state.head
	s.currentIndex = state.currentIndex
	s.setStaticEnvSnapshot(state.staticEnv)
	s.runtimeHash = state.runtimeHash
	s.runtimeConfig = cloneRawMessage(state.runtimeConfig)
	s.runtime = state.runtime
	s.runtimeDirty = false
	s.values = make(map[model.ValueID]engine.ValueView)
	if oldRuntime != nil {
		oldRuntime.close()
	}
}

func (s *session) currentTypeScriptEnv(ctx context.Context) (typescript.Env, error) {
	if s.tsEnvProvider == nil {
		return typescript.Env{}, nil
	}
	return s.tsEnvProvider(ctx, engine.TypeScriptEnvContext{
		SessionID: s.id,
		BranchID:  s.branch,
		Head:      s.head,
	})
}

func (s *session) applyTypeScriptEnv(env typescript.Env) []engine.ReplWarning {
	if s.tsEnvHash != "" && env.Hash != s.tsEnvHash {
		s.resetTypeScriptSession()
		s.committedSources = nil
		s.tsEpochTS = ""
		s.tsEnvHash = env.Hash
		s.tsEpochTS = env.EpochTS
		return []engine.ReplWarning{{
			Code:    "typescript_env_reset",
			Message: "TypeScript static context was reset because the TypeScript env changed.",
		}}
	}
	s.tsEnvHash = env.Hash
	s.tsEpochTS = env.EpochTS
	return nil
}

func (s *session) syncTypeScriptEnv(ctx context.Context) (typescript.Env, []engine.ReplWarning, error) {
	if s.tsEnvProvider == nil {
		return typescript.Env{}, nil, nil
	}
	env, err := s.currentTypeScriptEnv(ctx)
	if err != nil {
		return typescript.Env{}, nil, err
	}
	return env, s.applyTypeScriptEnv(env), nil
}

func newSubmitFailure(failureID model.FailureID, parent model.CellID, phase string, cause error, linkedEffects []engine.EffectSummary, logs []string) *engine.SubmitFailure {
	if cause == nil {
		return nil
	}
	return &engine.SubmitFailure{
		Failure:       failureID,
		Parent:        parent,
		Phase:         phase,
		ErrorMessage:  cause.Error(),
		LinkedEffects: cloneEffectSummaries(linkedEffects),
		Log:           cloneStrings(logs),
		Cause:         cause,
	}
}

func combineSubmitErrors(primary, secondary error) error {
	switch {
	case primary == nil:
		return secondary
	case secondary == nil:
		return primary
	default:
		return fmt.Errorf("%v; awaiting cell settlement: %w", primary, secondary)
	}
}

func withCellSettleTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, defaultCellSettleTimeout)
}

func (s *session) abandonSubmitRuntime(freshRuntime bool, discardFreshRuntime *bool) {
	if freshRuntime {
		if discardFreshRuntime != nil {
			*discardFreshRuntime = true
		}
		return
	}
	s.runtimeDirty = true
}

func (s *session) submitFailureResult(freshRuntime bool, discardFreshRuntime *bool, failureID model.FailureID, source string, previousHead model.CellID, runtimeHash, phase string, effectIDs []model.EffectID, cause error, effects []engine.EffectSummary, logs []string) (engine.SubmitResult, error) {
	s.abandonSubmitRuntime(freshRuntime, discardFreshRuntime)
	if recordErr := s.recordFailure(failureID, source, previousHead, runtimeHash, phase, effectIDs, cause); recordErr != nil {
		return engine.SubmitResult{}, fmt.Errorf("session: Submit: %s: %w; record failure: %v", phase, cause, recordErr)
	}
	return engine.SubmitResult{}, newSubmitFailure(failureID, previousHead, phase, cause, effects, logs)
}

func (s *session) ensureCleanRuntime(ctx context.Context) error {
	if s.runtime != nil && !s.runtimeDirty {
		return nil
	}
	if err := s.rebuildRuntimeFromCommitted(ctx); err != nil {
		return fmt.Errorf("recover dirty runtime: %w", err)
	}
	return nil
}

func (s *session) prepareSubmitRuntime(ctx context.Context) (*branchRuntime, []byte, string, error) {
	if s.runtimeMode != engine.RuntimeModeReplayPerSubmit {
		if err := s.ensureCleanRuntime(ctx); err != nil {
			return nil, nil, "", err
		}
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

func (s *session) rebuildRuntimeFromCommitted(ctx context.Context) error {
	var (
		rt            *branchRuntime
		runtimeConfig []byte
		runtimeHash   string
		err           error
	)

	if s.head == "" {
		rt, runtimeConfig, runtimeHash, err = newBranchRuntime(ctx, engine.SessionRuntimeContext{
			SessionID:   s.id,
			BranchID:    s.branch,
			RuntimeHash: s.runtimeHash,
		}, s.store, s.delegate, s.runtimeConfig, nil)
		if err != nil {
			return fmt.Errorf("init runtime from current config: %w", err)
		}
	} else {
		plan, err := s.store.LoadReplayPlan(ctx, s.id, s.head)
		if err != nil {
			return fmt.Errorf("load replay plan for runtime rebuild: %w", err)
		}
		rt, runtimeConfig, runtimeHash, err = replayPlanIntoRuntime(ctx, plan, engine.SessionRuntimeContext{
			SessionID:   s.id,
			BranchID:    s.branch,
			RuntimeHash: plan.RuntimeHash,
		}, s.store, s.delegate)
		if err != nil {
			return fmt.Errorf("replay committed runtime state: %w", err)
		}
	}

	oldRuntime := s.runtime
	s.runtime = rt
	s.runtimeConfig = append([]byte(nil), runtimeConfig...)
	s.runtimeHash = runtimeHash
	s.runtimeDirty = false
	if oldRuntime != nil {
		oldRuntime.close()
	}
	return nil
}

func (s *session) recordFailure(failureID model.FailureID, source string, parent model.CellID, runtimeHash, phase string, effects []model.EffectID, evalErr error) error {
	if evalErr == nil {
		return nil
	}
	return s.store.AppendFact(context.Background(), model.CellFailed{
		Session:       s.id,
		Branch:        s.branch,
		Failure:       failureID,
		Parent:        parent,
		Source:        source,
		RuntimeHash:   runtimeHash,
		Phase:         phase,
		ErrorMessage:  evalErr.Error(),
		LinkedEffects: append([]model.EffectID(nil), effects...),
		At:            time.Now().UTC(),
	})
}

// Submit optionally type-checks src, evaluates it in the branch-local goja
// runtime, then commits the resulting cell to durable history.
//
// Fact sequence appended on success:
//  1. CellChecked   (records source, language, diagnostics, emitted JS)
//  2. CellEvaluated (records completion value from the runtime)
//  3. CellCommitted (marks the cell as part of durable history)
//  4. HeadMoved     (advances the branch head)
//
// s.head is only updated after all four facts succeed durably.
func (s *session) Submit(ctx context.Context, src string) (engine.SubmitResult, error) {
	return s.SubmitCell(ctx, engine.SubmitInput{
		Source:   src,
		Language: model.CellLanguageJavaScript,
	})
}

func (s *session) SubmitCell(ctx context.Context, input engine.SubmitInput) (engine.SubmitResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.ensureBranchHeadWritable(ctx, "submitting"); err != nil {
		return engine.SubmitResult{}, fmt.Errorf("session: Submit: %w", err)
	}

	source := input.Source
	language := languageOrDefault(input.Language)
	previousHead := s.head
	nextIndex := s.currentIndex + 1
	cellID := model.CellID(uuid.NewString())
	now := time.Now().UTC()

	var (
		diagnostics []model.Diagnostic
		emittedJS   string
		hasErrors   bool
		warnings    []engine.ReplWarning
	)
	env, envWarnings, err := s.syncTypeScriptEnv(ctx)
	if err != nil {
		return engine.SubmitResult{}, fmt.Errorf("session: Submit: load typescript env: %w", err)
	}
	warnings = append(warnings, envWarnings...)
	if language == model.CellLanguageTypeScript {
		tsSession, err := s.ensureTypeScriptSession(ctx)
		if err != nil {
			return engine.SubmitResult{}, fmt.Errorf("session: Submit: %w", err)
		}
		tsResult, err := tsSession.CheckEmitCell(ctx, env, source)
		if err != nil {
			return engine.SubmitResult{}, fmt.Errorf("session: Submit: typescript check: %w", err)
		}
		diagnostics = tsResult.Diagnostics
		emittedJS = tsResult.EmittedJS
		hasErrors = tsResult.HasErrors
	}

	if err := s.store.AppendFact(ctx, model.CellChecked{
		Session:           s.id,
		Branch:            s.branch,
		Cell:              cellID,
		Parent:            previousHead,
		Language:          language,
		Source:            source,
		Diagnostics:       diagnostics,
		EmittedJS:         emittedJS,
		TypeScriptEnvHash: s.tsEnvHash,
		TypeScriptEpochTS: s.tsEpochTS,
		HasErrors:         hasErrors,
		At:                now,
	}); err != nil {
		return engine.SubmitResult{}, fmt.Errorf("session: Submit: append CellChecked: %w", err)
	}
	if hasErrors {
		return engine.SubmitResult{
				Cell:        cellID,
				Language:    language,
				Diagnostics: append([]model.Diagnostic(nil), diagnostics...),
				HasErrors:   true,
				Warnings:    append([]engine.ReplWarning(nil), warnings...),
			}, &engine.SubmitCheckFailure{
				Cell:        cellID,
				Language:    language,
				Diagnostics: append([]model.Diagnostic(nil), diagnostics...),
			}
	}

	execSource := source
	if language == model.CellLanguageTypeScript {
		execSource = emittedJS
	} else if emittedJS != "" {
		execSource = emittedJS
	}

	// --- Step 1: evaluate in the live goja VM after CellChecked is durable ---
	evalRuntime, evalRuntimeConfig, evalRuntimeHash, err := s.prepareSubmitRuntime(ctx)
	if err != nil {
		return engine.SubmitResult{}, fmt.Errorf("session: Submit: prepare runtime: %w", err)
	}
	freshRuntime := evalRuntime != s.runtime
	failureID := model.FailureID(uuid.NewString())
	evalCtx, cancel := withCellSettleTimeout(ctx)
	defer cancel()
	discardFreshRuntime := false
	defer func() {
		if discardFreshRuntime && evalRuntime != nil {
			evalRuntime.close()
		}
	}()
	acc := &effectAccumulator{}
	evalRuntime.beginCell(evalCtx, cellID, acc)
	defer evalRuntime.endCell()

	eval, err := evalRuntime.runContext(evalCtx, execSource)
	settleErr := evalRuntime.awaitCellSettled(evalCtx, acc)
	terminalErr := combineSubmitErrors(err, settleErr)
	terminalErr = translateEvalError(terminalErr)
	if terminalErr != nil {
		effects := acc.drain()
		logs := acc.drainLogs()
		effectIDs := effectIDsFromSummaries(effects)
		if freshRuntime {
			evalRuntime.close()
		} else {
			s.runtimeDirty = true
		}
		phase := "eval"
		if err == nil && settleErr != nil {
			phase = "await_cell_settlement"
		} else if err != nil && settleErr != nil {
			phase = "eval_and_await_cell_settlement"
		}
		return s.submitFailureResult(freshRuntime, &discardFreshRuntime, failureID, source, previousHead, evalRuntimeHash, phase, effectIDs, terminalErr, effects, logs)
	}

	effects := acc.drain()
	logs := acc.drainLogs()
	effectIDs := effectIDsFromSummaries(effects)

	if err := s.store.AppendFact(ctx, model.CellEvaluated{
		Session:         s.id,
		Branch:          s.branch,
		Cell:            cellID,
		LinkedEffects:   effectIDs,
		CompletionValue: eval.completionValue,
		At:              now,
	}); err != nil {
		return s.submitFailureResult(freshRuntime, &discardFreshRuntime, failureID, source, previousHead, evalRuntimeHash, "append_cell_evaluated", effectIDs, err, effects, logs)
	}

	if err := s.store.AppendFact(ctx, model.CellCommitted{
		Session: s.id,
		Branch:  s.branch,
		Cell:    cellID,
		At:      now,
	}); err != nil {
		return s.submitFailureResult(freshRuntime, &discardFreshRuntime, failureID, source, previousHead, evalRuntimeHash, "append_cell_committed", effectIDs, err, effects, logs)
	}

	if err := s.store.AppendFact(ctx, model.HeadMoved{
		Session:  s.id,
		Branch:   s.branch,
		Previous: previousHead,
		Next:     cellID,
		At:       now,
	}); err != nil {
		return s.submitFailureResult(freshRuntime, &discardFreshRuntime, failureID, source, previousHead, evalRuntimeHash, "append_head_moved", effectIDs, err, effects, logs)
	}

	// Only advance in-memory head after all durable facts succeed.
	s.head = cellID
	s.runtimeDirty = false
	if freshRuntime {
		oldRuntime := s.runtime
		s.runtime = evalRuntime
		s.runtimeConfig = append([]byte(nil), evalRuntimeConfig...)
		s.runtimeHash = evalRuntimeHash
		if oldRuntime != nil {
			oldRuntime.close()
		}
	}
	if err := s.runtime.setIndexedResult(nextIndex, eval); err != nil {
		s.runtimeDirty = true
		return engine.SubmitResult{}, fmt.Errorf("session: Submit: set $last/$val(%d): %w", nextIndex, err)
	}
	s.currentIndex = nextIndex
	s.committedSources = append(s.committedSources, source)
	if s.tsSession != nil {
		s.tsSession.AppendCommittedSource(source)
	}

	// Register an inspectable value handle when the eval produced a completion value.
	// This must happen after all durable facts succeed so rejected/pending cells never
	// get handles — the eval error above returns early before reaching this point.
	if eval.completionValue != nil {
		summary, full := describeValue(eval.completionValue.Preview, eval.structured)
		s.values[eval.completionValue.ID] = engine.ValueView{
			Handle:     eval.completionValue.ID,
			Preview:    eval.completionValue.Preview,
			Summary:    summary,
			Full:       full,
			TypeHint:   eval.completionValue.TypeHint,
			Structured: eval.structured,
		}
	}

	return engine.SubmitResult{
		Cell:            cellID,
		Index:           nextIndex,
		Language:        language,
		Diagnostics:     append([]model.Diagnostic(nil), diagnostics...),
		HasErrors:       false,
		CompletionValue: eval.completionValue,
		Log:             cloneStrings(logs),
		Warnings:        append([]engine.ReplWarning(nil), warnings...),
	}, nil
}

func (s *session) ensureBranchHeadWritable(ctx context.Context, action string) error {
	head, err := s.store.LoadHead(ctx, s.id, s.branch)
	if err != nil {
		return fmt.Errorf("load branch head: %w", err)
	}
	if head.Head == "" || head.Head == s.head {
		return nil
	}
	return fmt.Errorf("active cursor is at cell %q while branch %q head is %q; fork before %s", s.head, s.branch, head.Head, action)
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

func (s *session) Logs(ctx context.Context, targetCell model.CellID) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	plan, err := s.store.LoadReplayPlan(ctx, s.id, targetCell)
	if err != nil {
		return nil, fmt.Errorf("session: Logs: load replay plan: %w", err)
	}
	if len(plan.Steps) == 0 {
		return nil, nil
	}
	target := plan.Steps[len(plan.Steps)-1]
	rt, _, _, err := replayPlanPrefixIntoRuntime(ctx, plan, len(plan.Steps)-1, engine.SessionRuntimeContext{
		SessionID:   s.id,
		BranchID:    plan.Branch,
		RuntimeHash: plan.RuntimeHash,
	}, s.store, s.delegate)
	if err != nil {
		return nil, fmt.Errorf("session: Logs: replay prefix: %w", err)
	}
	defer rt.close()

	acc := &effectAccumulator{}
	rt.beginCell(ctx, target.Cell, acc)
	defer rt.endCell()
	if err := rt.beginReplayStep(target.Effects, plan.Decisions); err != nil {
		return nil, fmt.Errorf("session: Logs: begin replay step: %w", err)
	}
	_, runErr := rt.runContext(ctx, replaySourceForStep(target))
	settleErr := rt.awaitCellSettled(ctx, acc)
	finishErr := rt.finishReplayStep()
	logs := acc.drainLogs()
	if terminalErr := combineSubmitErrors(runErr, settleErr); terminalErr != nil {
		return logs, fmt.Errorf("session: Logs: replay target cell: %w", terminalErr)
	}
	if finishErr != nil {
		return logs, fmt.Errorf("session: Logs: finish replay step: %w", finishErr)
	}
	return logs, nil
}

func describeValue(preview string, structured []byte) (summary, full string) {
	summary = preview
	full = preview
	if len(structured) == 0 {
		return summary, full
	}
	description, err := jswire.Describe(structured)
	if err != nil {
		return summary, full
	}
	if description.Summary != "" {
		summary = description.Summary
	}
	if description.Full != "" {
		full = description.Full
	}
	return summary, full
}

// Failures returns durable failed submit attempts for this session in append
// order. The last entry is therefore the most recent failure.
func (s *session) Failures(ctx context.Context) ([]engine.FailureView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	records, err := s.store.LoadFailures(ctx, s.id)
	if err != nil {
		return nil, fmt.Errorf("session: Failures: %w", err)
	}
	out := make([]engine.FailureView, 0, len(records))
	for _, record := range records {
		out = append(out, engine.FailureView{
			Failure:       record.Failure,
			Branch:        record.Branch,
			Parent:        record.Parent,
			Source:        record.Source,
			RuntimeHash:   record.RuntimeHash,
			Phase:         record.Phase,
			ErrorMessage:  record.ErrorMessage,
			LinkedEffects: append([]model.EffectID(nil), record.LinkedEffects...),
			At:            record.At,
		})
	}
	return out, nil
}

// Restore positions this session at targetCell on the existing branch that
// owns it. It appends RestoreCompleted, then updates the session's active
// branch and head to the restored point.
//
// Fact sequence appended:
//  1. RestoreCompleted (records the restore event)
//
// s.branch and s.head are only updated after the durable fact succeeds.
func (s *session) Restore(ctx context.Context, targetCell model.CellID) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.restoreLocked(ctx, targetCell)
}

// Fork creates a new branch rooted at targetCell and makes it active.
func (s *session) Fork(ctx context.Context, targetCell model.CellID) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.forkLocked(ctx, targetCell)
}

// restoreLocked implements Restore without acquiring the mutex. It must be
// called with s.mu held.
//
// The method loads the replay plan for targetCell first: if targetCell is
// unknown the store returns an error here and the session is left unchanged.
// It then replays committed history into a fresh VM before appending the
// durable RestoreCompleted fact, so an evaluation failure during replay also
// aborts cleanly without changing session state.
func (s *session) restoreLocked(ctx context.Context, targetCell model.CellID) error {
	plan, err := s.store.LoadReplayPlan(ctx, s.id, targetCell)
	if err != nil {
		return fmt.Errorf("session: Restore: load replay plan: %w", err)
	}

	rt, runtimeConfig, runtimeHash, err := replayPlanIntoRuntime(ctx, plan, engine.SessionRuntimeContext{
		SessionID:   s.id,
		BranchID:    plan.Branch,
		RuntimeHash: plan.RuntimeHash,
	}, s.store, s.delegate)
	if err != nil {
		return fmt.Errorf("session: Restore: replay history: %w", err)
	}
	staticEnv, err := s.store.LoadStaticEnv(ctx, s.id, plan.Branch, targetCell)
	if err != nil {
		rt.close()
		return fmt.Errorf("session: Restore: load static env: %w", err)
	}

	now := time.Now().UTC()
	if err := s.store.AppendFact(ctx, model.RestoreCompleted{
		Session:    s.id,
		TargetCell: targetCell,
		At:         now,
	}); err != nil {
		rt.close()
		return fmt.Errorf("session: Restore: append RestoreCompleted: %w", err)
	}

	s.installLoadedBranchState(loadedBranchState{
		branch:        plan.Branch,
		head:          targetCell,
		runtime:       rt,
		runtimeConfig: runtimeConfig,
		runtimeHash:   runtimeHash,
		currentIndex:  len(plan.Steps),
		staticEnv:     staticEnv,
	})

	return nil
}

// forkLocked implements Fork without acquiring the mutex. It must be called
// with s.mu held.
func (s *session) forkLocked(ctx context.Context, targetCell model.CellID) error {
	plan, err := s.store.LoadReplayPlan(ctx, s.id, targetCell)
	if err != nil {
		return fmt.Errorf("session: Fork: load replay plan: %w", err)
	}
	newBranchID := model.BranchID(uuid.NewString())

	rt, runtimeConfig, runtimeHash, err := replayPlanIntoRuntime(ctx, plan, engine.SessionRuntimeContext{
		SessionID:   s.id,
		BranchID:    newBranchID,
		RuntimeHash: plan.RuntimeHash,
	}, s.store, s.delegate)
	if err != nil {
		return fmt.Errorf("session: Fork: replay history: %w", err)
	}
	staticEnv, err := s.store.LoadStaticEnv(ctx, s.id, plan.Branch, targetCell)
	if err != nil {
		rt.close()
		return fmt.Errorf("session: Fork: load static env: %w", err)
	}

	now := time.Now().UTC()
	if err := s.store.AppendFact(ctx, model.BranchCreated{
		Session:    s.id,
		Branch:     newBranchID,
		ParentCell: targetCell,
		At:         now,
	}); err != nil {
		rt.close()
		return fmt.Errorf("session: Fork: append BranchCreated: %w", err)
	}
	if err := s.store.AppendFact(ctx, model.RestoreCompleted{
		Session:    s.id,
		TargetCell: targetCell,
		NewBranch:  newBranchID,
		At:         now,
	}); err != nil {
		rt.close()
		return fmt.Errorf("session: Fork: append RestoreCompleted: %w", err)
	}

	s.installLoadedBranchState(loadedBranchState{
		branch:        newBranchID,
		head:          targetCell,
		runtime:       rt,
		runtimeConfig: runtimeConfig,
		runtimeHash:   runtimeHash,
		currentIndex:  len(plan.Steps),
		staticEnv:     staticEnv,
	})
	return nil
}

func (s *session) TransitionToState(ctx context.Context, toState json.RawMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.ensureBranchHeadWritable(ctx, "transitioning runtime"); err != nil {
		return fmt.Errorf("session: TransitionToState: %w", err)
	}
	if err := s.ensureCleanRuntime(ctx); err != nil {
		return fmt.Errorf("session: TransitionToState: %w", err)
	}

	resolvedState, resolvedHash, err := resolveRuntimeDescriptor(ctx, engine.SessionRuntimeContext{
		SessionID:   s.id,
		BranchID:    s.branch,
		RuntimeHash: s.runtimeHash,
	}, s.store, s.delegate, toState, nil)
	if err != nil {
		return fmt.Errorf("session: TransitionToState: resolve runtime descriptor: %w", err)
	}
	if resolvedHash == s.runtimeHash {
		s.runtimeConfig = cloneRawMessage(resolvedState)
		return nil
	}

	if err := s.store.PutRuntimeConfig(ctx, resolvedHash, resolvedState); err != nil {
		return fmt.Errorf("session: TransitionToState: store runtime config: %w", err)
	}
	if s.delegate != nil {
		if _, ok := s.delegate.(engine.VMTransitionDelegate); !ok {
			s.runtimeDirty = true
			return fmt.Errorf("session: TransitionToState: delegate does not implement engine.VMTransitionDelegate")
		}
	}
	if err := applyRuntimeTransitionAndInvalidateIndexedResults(ctx, s.runtime, engine.RuntimeTransitionContext{
		SessionID:       s.id,
		BranchID:        s.branch,
		FromRuntimeHash: s.runtimeHash,
		ToRuntimeHash:   resolvedHash,
	}, s.delegate, s.runtimeConfig, resolvedState, s.currentIndex); err != nil {
		s.runtimeDirty = true
		return fmt.Errorf("session: TransitionToState: %w", err)
	}

	if err := s.store.AppendFact(ctx, model.RuntimeTransitioned{
		Session:     s.id,
		Branch:      s.branch,
		AfterCell:   s.head,
		RuntimeHash: resolvedHash,
		At:          time.Now().UTC(),
	}); err != nil {
		s.runtimeDirty = true
		return fmt.Errorf("session: TransitionToState: append RuntimeTransitioned: %w", err)
	}

	s.runtimeHash = resolvedHash
	s.runtimeConfig = cloneRawMessage(resolvedState)
	s.runtimeDirty = false
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
	s.resetTypeScriptSession()
	return nil
}
