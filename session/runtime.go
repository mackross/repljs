package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/dop251/goja"
	"github.com/dop251/goja_nodejs/eventloop"
	"github.com/google/uuid"
	"github.com/solidarity-ai/repl/engine"
	"github.com/solidarity-ai/repl/model"
	"github.com/solidarity-ai/repl/store"
)

const directRunTimeout = 100 * time.Millisecond

// effectAccumulator collects the EffectIDs and PromiseRefs launched during a
// single cell evaluation. It is goroutine-safe; both the JS event-loop
// goroutine (via add*) and the caller (via drain) may touch it concurrently.
type effectAccumulator struct {
	mu       sync.Mutex
	effects  []model.EffectID
	promises []model.PromiseRef
}

func (a *effectAccumulator) addEffect(effectID model.EffectID) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.effects = append(a.effects, effectID)
}

func (a *effectAccumulator) addAsync(effectID model.EffectID, promiseRef model.PromiseRef) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.effects = append(a.effects, effectID)
	a.promises = append(a.promises, promiseRef)
}

func (a *effectAccumulator) drain() ([]model.EffectID, []model.PromiseRef) {
	a.mu.Lock()
	defer a.mu.Unlock()
	effects := a.effects
	promises := a.promises
	a.effects = nil
	a.promises = nil
	return effects, promises
}

type hostRuntimeWiring struct {
	store      store.Store
	sessionID  model.SessionID
	replayPlan *store.ReplayPlan
}

type hostFunctionMode uint8

const (
	hostFunctionModeLive hostFunctionMode = iota
	hostFunctionModeReplay
)

type replayInvocation struct {
	effectID model.EffectID
	decision store.ReplayDecision
}

type replayStepState struct {
	mu             sync.Mutex
	effects        []replayInvocation
	nextStart      int
	pendingAsync   map[int][]func(*goja.Runtime)
	flushScheduled bool
}

func newReplayStepState(effectIDs []model.EffectID, decisions map[model.EffectID]store.ReplayDecision) (*replayStepState, error) {
	state := &replayStepState{}
	if len(effectIDs) == 0 {
		return state, nil
	}
	state.effects = make([]replayInvocation, 0, len(effectIDs))
	for _, effectID := range effectIDs {
		decision, ok := decisions[effectID]
		if !ok {
			return nil, fmt.Errorf("missing replay decision for effect %q", effectID)
		}
		state.effects = append(state.effects, replayInvocation{effectID: effectID, decision: decision})
	}
	return state, nil
}

func (s *replayStepState) nextEffect() (replayInvocation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.nextStart >= len(s.effects) {
		return replayInvocation{}, fmt.Errorf("replay effect stream exhausted")
	}
	inv := s.effects[s.nextStart]
	s.nextStart++
	return inv, nil
}

func (s *replayStepState) queueAsync(order int, settle func(*goja.Runtime)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pendingAsync == nil {
		s.pendingAsync = make(map[int][]func(*goja.Runtime))
	}
	s.pendingAsync[order] = append(s.pendingAsync[order], settle)
	if s.flushScheduled {
		return false
	}
	s.flushScheduled = true
	return true
}

func (s *replayStepState) takePendingAsync() []func(*goja.Runtime) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pendingAsync) == 0 {
		s.flushScheduled = false
		return nil
	}
	orders := make([]int, 0, len(s.pendingAsync))
	for order := range s.pendingAsync {
		orders = append(orders, order)
	}
	sort.Ints(orders)
	settlers := make([]func(*goja.Runtime), 0, len(orders))
	for _, order := range orders {
		settlers = append(settlers, s.pendingAsync[order]...)
	}
	s.pendingAsync = nil
	s.flushScheduled = false
	return settlers
}

func (s *replayStepState) ensureConsumed() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.nextStart != len(s.effects) {
		return fmt.Errorf("replay consumed %d of %d expected effects", s.nextStart, len(s.effects))
	}
	return nil
}

type liveCallState struct {
	ctx    context.Context
	cellID model.CellID
	acc    *effectAccumulator
}

type hostFunctionRouter struct {
	vm        *goja.Runtime
	store     store.Store
	sessionID model.SessionID

	modeMu sync.RWMutex
	mode   hostFunctionMode

	liveMu sync.RWMutex
	live   *liveCallState

	replayMu   sync.Mutex
	replayStep *replayStepState
}

func newHostFunctionRouter(vm *goja.Runtime, wiring *hostRuntimeWiring) *hostFunctionRouter {
	router := &hostFunctionRouter{vm: vm}
	if wiring != nil {
		router.store = wiring.store
		router.sessionID = wiring.sessionID
		if wiring.replayPlan != nil {
			router.mode = hostFunctionModeReplay
		} else {
			router.mode = hostFunctionModeLive
		}
	}
	return router
}

func (r *hostFunctionRouter) beginCell(ctx context.Context, cellID model.CellID, acc *effectAccumulator) {
	if r == nil {
		return
	}
	r.liveMu.Lock()
	defer r.liveMu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	r.live = &liveCallState{ctx: ctx, cellID: cellID, acc: acc}
}

func (r *hostFunctionRouter) endCell() {
	if r == nil {
		return
	}
	r.liveMu.Lock()
	defer r.liveMu.Unlock()
	r.live = nil
}

func (r *hostFunctionRouter) beginReplayStep(effectIDs []model.EffectID, decisions map[model.EffectID]store.ReplayDecision) error {
	if r == nil {
		return nil
	}
	state, err := newReplayStepState(effectIDs, decisions)
	if err != nil {
		return err
	}
	r.replayMu.Lock()
	defer r.replayMu.Unlock()
	r.replayStep = state
	return nil
}

func (r *hostFunctionRouter) finishReplayStep() error {
	if r == nil {
		return nil
	}
	r.replayMu.Lock()
	state := r.replayStep
	r.replayStep = nil
	r.replayMu.Unlock()
	if state == nil {
		return nil
	}
	return state.ensureConsumed()
}

func (r *hostFunctionRouter) activateLive() {
	if r == nil {
		return
	}
	r.modeMu.Lock()
	defer r.modeMu.Unlock()
	r.mode = hostFunctionModeLive
}

func (r *hostFunctionRouter) currentMode() hostFunctionMode {
	if r == nil {
		return hostFunctionModeLive
	}
	r.modeMu.RLock()
	defer r.modeMu.RUnlock()
	return r.mode
}

func (r *hostFunctionRouter) currentLiveState() (*liveCallState, error) {
	r.liveMu.RLock()
	defer r.liveMu.RUnlock()
	if r.live == nil {
		return nil, fmt.Errorf("host function invoked with no active cell evaluation")
	}
	return r.live, nil
}

func (r *hostFunctionRouter) nextReplayInvocation() (replayInvocation, error) {
	r.replayMu.Lock()
	state := r.replayStep
	r.replayMu.Unlock()
	if state == nil {
		return replayInvocation{}, fmt.Errorf("replay host function invoked outside replay step")
	}
	return state.nextEffect()
}

func (r *hostFunctionRouter) queueReplayAsync(order int, settle func(*goja.Runtime)) {
	r.replayMu.Lock()
	state := r.replayStep
	r.replayMu.Unlock()
	if state == nil {
		return
	}
	if !state.queueAsync(order, settle) {
		return
	}
	engine.RunOnRuntimeLoop(r.vm, func(vm *goja.Runtime) {
		settlers := state.takePendingAsync()
		for _, fn := range settlers {
			fn(vm)
		}
	})
}

type hostFuncBuilder struct {
	router *hostFunctionRouter
}

func (b hostFuncBuilder) WrapSync(name string, replay model.ReplayPolicy, invoke engine.HostFuncInvoke) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		if b.router == nil || b.router.vm == nil {
			panic(goja.Undefined())
		}
		return b.router.invokeSync(name, replay, invoke, call)
	}
}

func (b hostFuncBuilder) WrapAsync(name string, replay model.ReplayPolicy, invoke engine.HostFuncInvoke) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		if b.router == nil || b.router.vm == nil {
			return goja.Undefined()
		}
		return b.router.invokeAsync(name, replay, invoke, call)
	}
}

// branchRuntime holds a single goja VM that persists across successful submits
// on one branch. Each new branch (including after Restore) gets its own VM so
// bindings from sibling branches are never visible to one another.
type branchRuntime struct {
	vm     *goja.Runtime
	loop   *eventloop.EventLoop
	router *hostFunctionRouter
}

func normalizeRuntimeConfig(state json.RawMessage) (json.RawMessage, error) {
	if len(state) == 0 {
		return nil, nil
	}
	var decoded any
	if err := json.Unmarshal(state, &decoded); err != nil {
		return nil, fmt.Errorf("normalize runtime config: %w", err)
	}
	normalized, err := json.Marshal(decoded)
	if err != nil {
		return nil, fmt.Errorf("normalize runtime config: %w", err)
	}
	return normalized, nil
}

func runtimeConfigHash(state json.RawMessage) string {
	sum := sha256.Sum256(state)
	return hex.EncodeToString(sum[:])
}

// newBranchRuntime allocates a fresh goja VM with no pre-loaded bindings,
// starts an event loop for it, and applies the optional delegate before
// returning. It returns the configured VM plus the serialised runtime
// descriptor that should be persisted for future recreation.
func newBranchRuntime(bgCtx context.Context, ctx engine.SessionRuntimeContext, st store.Store, delegate engine.VMDelegate, state json.RawMessage, replayPlan *store.ReplayPlan) (*branchRuntime, json.RawMessage, string, error) {
	loop := eventloop.NewEventLoop(eventloop.EnableConsole(false))
	loop.Start()

	type initResult struct {
		runtime     *branchRuntime
		configured  json.RawMessage
		runtimeHash string
		err         error
	}
	initCh := make(chan initResult, 1)
	loop.RunOnLoop(func(vm *goja.Runtime) {
		engine.BindRuntimeLoop(vm, loop.RunOnLoop)
		router := newHostFunctionRouter(vm, &hostRuntimeWiring{store: st, sessionID: ctx.SessionID, replayPlan: replayPlan})

		normalizedState, err := normalizeRuntimeConfig(state)
		if err != nil {
			initCh <- initResult{err: err}
			return
		}
		configuredState := normalizedState
		if delegate != nil {
			configuredState, err = delegate.ConfigureRuntime(ctx, vm, hostFuncBuilder{router: router}, normalizedState)
			if err != nil {
				initCh <- initResult{err: fmt.Errorf("configure runtime: %w", err)}
				return
			}
		}
		normalizedConfiguredState, err := normalizeRuntimeConfig(configuredState)
		if err != nil {
			initCh <- initResult{err: err}
			return
		}
		if normalizedConfiguredState != nil {
			normalizedConfiguredState = append(json.RawMessage(nil), normalizedConfiguredState...)
		}

		_ = bgCtx
		initCh <- initResult{
			runtime:     &branchRuntime{vm: vm, loop: loop, router: router},
			configured:  normalizedConfiguredState,
			runtimeHash: runtimeConfigHash(normalizedConfiguredState),
		}
	})

	init := <-initCh
	if init.err != nil {
		loop.RunOnLoop(func(vm *goja.Runtime) {
			engine.UnbindRuntimeLoop(vm)
		})
		loop.Stop()
		return nil, nil, "", init.err
	}
	return init.runtime, init.configured, init.runtimeHash, nil
}

func (r *branchRuntime) beginCell(ctx context.Context, cellID model.CellID, acc *effectAccumulator) {
	if r == nil || r.router == nil {
		return
	}
	r.router.beginCell(ctx, cellID, acc)
}

func (r *branchRuntime) endCell() {
	if r == nil || r.router == nil {
		return
	}
	r.router.endCell()
}

func (r *branchRuntime) beginReplayStep(effectIDs []model.EffectID, decisions map[model.EffectID]store.ReplayDecision) error {
	if r == nil || r.router == nil {
		return nil
	}
	return r.router.beginReplayStep(effectIDs, decisions)
}

func (r *branchRuntime) finishReplayStep() error {
	if r == nil || r.router == nil {
		return nil
	}
	return r.router.finishReplayStep()
}

func (r *branchRuntime) activateLiveBindings() {
	if r == nil || r.router == nil {
		return
	}
	r.router.activateLive()
}

func (r *branchRuntime) close() {
	if r == nil || r.loop == nil {
		return
	}
	if r.vm != nil {
		engine.UnbindRuntimeLoop(r.vm)
	}
	r.loop.Stop()
}

func (r *hostFunctionRouter) invokeSync(name string, replay model.ReplayPolicy, invoke engine.HostFuncInvoke, call goja.FunctionCall) goja.Value {
	vm := r.vm
	params, err := marshalFirstArgument(call)
	if err != nil {
		panic(vm.ToValue(err.Error()))
	}
	if r.currentMode() == hostFunctionModeReplay {
		inv, replayErr := r.nextReplayInvocation()
		if replayErr != nil {
			panic(vm.ToValue(fmt.Sprintf("host function %q replay: %v", name, replayErr)))
		}
		if len(inv.decision.RecordedResult) == 0 {
			panic(vm.ToValue(fmt.Sprintf("host function %q replay: missing recorded result for effect %q", name, inv.effectID)))
		}
		return jsonResultToValue(vm, inv.decision.RecordedResult)
	}

	live, err := r.currentLiveState()
	if err != nil {
		panic(vm.ToValue(fmt.Sprintf("host function %q: %v", name, err)))
	}
	effectID := model.EffectID(uuid.NewString())
	if err := r.store.AppendFact(context.Background(), model.EffectStarted{
		Session:      r.sessionID,
		Effect:       effectID,
		Cell:         live.cellID,
		FunctionName: name,
		Params:       params,
		ReplayPolicy: replay,
		At:           time.Now().UTC(),
	}); err != nil {
		panic(vm.ToValue(fmt.Sprintf("host function %q: journal EffectStarted: %v", name, err)))
	}
	live.acc.addEffect(effectID)

	result, err := invoke(live.ctx, params)
	if err != nil {
		_ = r.store.AppendFact(context.Background(), model.EffectFailed{
			Session:      r.sessionID,
			Effect:       effectID,
			ErrorMessage: err.Error(),
			At:           time.Now().UTC(),
		})
		panic(vm.ToValue(err.Error()))
	}
	if err := r.store.AppendFact(context.Background(), model.EffectCompleted{
		Session: r.sessionID,
		Effect:  effectID,
		Result:  result,
		At:      time.Now().UTC(),
	}); err != nil {
		panic(vm.ToValue(fmt.Sprintf("host function %q: journal EffectCompleted: %v", name, err)))
	}
	return jsonResultToValue(vm, result)
}

func (r *hostFunctionRouter) invokeAsync(name string, replay model.ReplayPolicy, invoke engine.HostFuncInvoke, call goja.FunctionCall) goja.Value {
	vm := r.vm
	promise, resolve, reject := vm.NewPromise()

	params, err := marshalFirstArgument(call)
	if err != nil {
		_ = reject(vm.ToValue(err.Error()))
		return vm.ToValue(promise)
	}

	if r.currentMode() == hostFunctionModeReplay {
		inv, replayErr := r.nextReplayInvocation()
		if replayErr != nil {
			_ = reject(vm.ToValue(fmt.Sprintf("host function %q replay: %v", name, replayErr)))
			return vm.ToValue(promise)
		}
		if len(inv.decision.RecordedResult) == 0 {
			_ = reject(vm.ToValue(fmt.Sprintf("host function %q replay: missing recorded result for effect %q", name, inv.effectID)))
			return vm.ToValue(promise)
		}
		order := inv.decision.CompletionOrder
		if order <= 0 {
			order = int(time.Now().UnixNano())
		}
		r.queueReplayAsync(order, func(vm *goja.Runtime) {
			_ = resolve(jsonResultToValue(vm, inv.decision.RecordedResult))
		})
		return vm.ToValue(promise)
	}

	live, stateErr := r.currentLiveState()
	if stateErr != nil {
		_ = reject(vm.ToValue(fmt.Sprintf("host function %q: %v", name, stateErr)))
		return vm.ToValue(promise)
	}
	effectID := model.EffectID(uuid.NewString())
	promiseID := model.PromiseID(uuid.NewString())
	if err := r.store.AppendFact(context.Background(), model.EffectStarted{
		Session:      r.sessionID,
		Effect:       effectID,
		Cell:         live.cellID,
		FunctionName: name,
		Params:       params,
		ReplayPolicy: replay,
		At:           time.Now().UTC(),
	}); err != nil {
		_ = reject(vm.ToValue(fmt.Sprintf("host function %q: journal EffectStarted: %v", name, err)))
		return vm.ToValue(promise)
	}
	live.acc.addAsync(effectID, model.PromiseRef{ID: promiseID, State: model.PromisePending})

	go func(callCtx context.Context) {
		result, invokeErr := invoke(callCtx, params)
		engine.RunOnRuntimeLoop(vm, func(vm *goja.Runtime) {
			now := time.Now().UTC()
			if invokeErr != nil {
				_ = r.store.AppendFact(context.Background(), model.EffectFailed{
					Session:      r.sessionID,
					Effect:       effectID,
					ErrorMessage: invokeErr.Error(),
					At:           now,
				})
				_ = reject(vm.ToValue(invokeErr.Error()))
				return
			}
			_ = r.store.AppendFact(context.Background(), model.EffectCompleted{
				Session: r.sessionID,
				Effect:  effectID,
				Result:  result,
				At:      now,
			})
			_ = resolve(jsonResultToValue(vm, result))
		})
	}(live.ctx)

	return vm.ToValue(promise)
}

func marshalFirstArgument(call goja.FunctionCall) (json.RawMessage, error) {
	if len(call.Arguments) == 0 || goja.IsUndefined(call.Arguments[0]) || goja.IsNull(call.Arguments[0]) {
		return json.RawMessage("null"), nil
	}
	b, err := json.Marshal(call.Arguments[0].Export())
	if err != nil {
		return nil, fmt.Errorf("marshal params: %w", err)
	}
	return b, nil
}

func jsonResultToValue(vm *goja.Runtime, raw json.RawMessage) goja.Value {
	if len(raw) == 0 {
		return vm.ToValue(nil)
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return vm.ToValue(string(raw))
	}
	return vm.ToValue(decoded)
}

// evalResult carries the output of a single evaluation.
type evalResult struct {
	// completionValue is non-nil when the program produced a defined export
	// value (i.e. the last expression evaluated to something other than
	// undefined).
	completionValue *model.ValueRef

	// structured carries the JSON-encoded export of the completion value.
	// It is nil when the value cannot be JSON-marshaled (cyclic objects,
	// functions, etc.) or when completionValue is nil.
	structured []byte
}

// run evaluates src in the VM using a short default timeout suitable for
// direct tests. Session submit/restore paths should call runContext instead.
func (r *branchRuntime) run(src string) (evalResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), directRunTimeout)
	defer cancel()
	return r.runContext(ctx, src)
}

// runContext evaluates src on the runtime's event loop and blocks until the
// result settles or ctx is done.
func (r *branchRuntime) runContext(ctx context.Context, src string) (evalResult, error) {
	resultCh := make(chan outcome, 1)
	deliver := func(out outcome) {
		select {
		case resultCh <- out:
		default:
		}
	}

	r.loop.RunOnLoop(func(vm *goja.Runtime) {
		raw, err := vm.RunString(src)
		if err != nil {
			deliver(outcome{err: fmt.Errorf("goja: %w", err)})
			return
		}
		r.settleValueAsync(raw, deliver)
	})

	select {
	case out := <-resultCh:
		return out.res, out.err
	case <-ctx.Done():
		return evalResult{}, fmt.Errorf("promise still pending after evaluation: %w", ctx.Err())
	}
}

func (r *branchRuntime) settleValueAsync(v goja.Value, deliver func(outcome)) {
	if v == nil {
		deliver(outcome{res: evalResult{}})
		return
	}
	p, ok := v.Export().(*goja.Promise)
	if !ok {
		res, err := valueToEvalResult(v)
		deliver(outcome{res: res, err: err})
		return
	}
	switch p.State() {
	case goja.PromiseStateFulfilled:
		res, err := valueToEvalResult(p.Result())
		deliver(outcome{res: res, err: err})
	case goja.PromiseStateRejected:
		reason := p.Result()
		var preview string
		if reason != nil {
			preview = reason.String()
		}
		deliver(outcome{err: fmt.Errorf("promise rejected: %s", preview)})
	default:
		obj := r.vm.ToValue(p).ToObject(r.vm)
		then, ok := goja.AssertFunction(obj.Get("then"))
		if !ok {
			deliver(outcome{err: fmt.Errorf("promise still pending after evaluation")})
			return
		}
		_, err := then(obj,
			r.vm.ToValue(func(call goja.FunctionCall) goja.Value {
				res, err := valueToEvalResult(call.Argument(0))
				deliver(outcome{res: res, err: err})
				return goja.Undefined()
			}),
			r.vm.ToValue(func(call goja.FunctionCall) goja.Value {
				reason := call.Argument(0)
				var preview string
				if reason != nil {
					preview = reason.String()
				}
				deliver(outcome{err: fmt.Errorf("promise rejected: %s", preview)})
				return goja.Undefined()
			}),
		)
		if err != nil {
			deliver(outcome{err: fmt.Errorf("promise then: %w", err)})
		}
	}
}

func valueToEvalResult(val goja.Value) (evalResult, error) {
	var ref *model.ValueRef
	var structured []byte
	if val != nil && !goja.IsUndefined(val) && !goja.IsNull(val) {
		ref = &model.ValueRef{
			ID:       model.ValueID(uuid.NewString()),
			Preview:  val.String(),
			TypeHint: gojaTypeHint(val),
		}
		if exported := val.Export(); exported != nil {
			if b, merr := json.Marshal(exported); merr == nil {
				structured = b
			}
		}
	}
	return evalResult{completionValue: ref, structured: structured}, nil
}

// replayPlanIntoRuntime creates a fresh branchRuntime and replays every step in
// plan into it, in order, using the same evaluation path as Submit. This
// ensures a restored branch starts with exactly the bindings visible at the
// restore target, and no bindings created after the fork point.
func replayPlanIntoRuntime(ctx context.Context, plan store.ReplayPlan, rtCtx engine.SessionRuntimeContext, st store.Store, delegate engine.VMDelegate) (*branchRuntime, json.RawMessage, string, error) {
	rt, configuredState, runtimeHash, err := newBranchRuntime(ctx, rtCtx, st, delegate, plan.RuntimeConfig, &plan)
	if err != nil {
		return nil, nil, "", err
	}
	for _, step := range plan.Steps {
		if err := rt.beginReplayStep(step.Effects, plan.Decisions); err != nil {
			rt.close()
			return nil, nil, "", fmt.Errorf("prepare replay step %q: %w", step.Cell, err)
		}
		if _, err := rt.runContext(ctx, step.Source); err != nil {
			_ = rt.finishReplayStep()
			rt.close()
			return nil, nil, "", fmt.Errorf("replay cell %q: %w", step.Cell, err)
		}
		if err := rt.finishReplayStep(); err != nil {
			rt.close()
			return nil, nil, "", fmt.Errorf("replay cell %q effects: %w", step.Cell, err)
		}
	}
	rt.activateLiveBindings()
	return rt, configuredState, runtimeHash, nil
}

// gojaTypeHint returns a simple TypeScript-style type name for a goja value.
func gojaTypeHint(v goja.Value) string {
	switch v.ExportType() {
	case nil:
		return "undefined"
	default:
		switch v.Export().(type) {
		case bool:
			return "boolean"
		case int64, float64:
			return "number"
		case string:
			return "string"
		default:
			return "object"
		}
	}
}

type outcome struct {
	res evalResult
	err error
}
