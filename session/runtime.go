package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	mrand "math/rand"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dop251/goja"
	"github.com/dop251/goja_nodejs/eventloop"
	"github.com/google/uuid"
	"github.com/mackross/repljs/engine"
	"github.com/mackross/repljs/jswire"
	"github.com/mackross/repljs/model"
	"github.com/mackross/repljs/store"
)

const directRunTimeout = 100 * time.Millisecond
const maxRuntimeHashResolutionPasses = 8

// effectAccumulator collects the EffectIDs launched during a
// single cell evaluation. It is goroutine-safe; both the JS event-loop
// goroutine (via add*) and the caller (via drain) may touch it concurrently.
type effectAccumulator struct {
	mu                 sync.Mutex
	order              []model.EffectID
	effects            map[model.EffectID]engine.EffectSummary
	asyncEffects       map[model.EffectID]struct{}
	pendingAsync       int
	asyncSettled       chan struct{}
	unhandledRejection map[*goja.Promise]string
	logs               []string
}

func cloneBytes(raw []byte) []byte {
	if len(raw) == 0 {
		return nil
	}
	return append([]byte(nil), raw...)
}

func (a *effectAccumulator) recordStarted(effectID model.EffectID, name string, params []byte, replay model.ReplayPolicy) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.effects == nil {
		a.effects = make(map[model.EffectID]engine.EffectSummary)
	}
	if _, exists := a.effects[effectID]; !exists {
		a.order = append(a.order, effectID)
	}
	a.effects[effectID] = engine.EffectSummary{
		Effect:       effectID,
		FunctionName: name,
		Params:       cloneBytes(params),
		ReplayPolicy: replay,
		Status:       engine.EffectStatusPending,
	}
}

func (a *effectAccumulator) addAsync(effectID model.EffectID, name string, params []byte, replay model.ReplayPolicy) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.effects == nil {
		a.effects = make(map[model.EffectID]engine.EffectSummary)
	}
	if _, exists := a.effects[effectID]; !exists {
		a.order = append(a.order, effectID)
	}
	a.effects[effectID] = engine.EffectSummary{
		Effect:       effectID,
		FunctionName: name,
		Params:       cloneBytes(params),
		ReplayPolicy: replay,
		Status:       engine.EffectStatusPending,
	}
	if a.asyncEffects == nil {
		a.asyncEffects = make(map[model.EffectID]struct{})
	}
	a.asyncEffects[effectID] = struct{}{}
	a.pendingAsync++
}

func (a *effectAccumulator) recordCompleted(effectID model.EffectID, result []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	summary, ok := a.effects[effectID]
	if !ok {
		return
	}
	summary.Status = engine.EffectStatusCompleted
	summary.Result = cloneBytes(result)
	summary.ErrorMessage = ""
	a.effects[effectID] = summary
	a.markAsyncSettledLocked(effectID)
}

func (a *effectAccumulator) recordFailed(effectID model.EffectID, errorMessage string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	summary, ok := a.effects[effectID]
	if !ok {
		return
	}
	summary.Status = engine.EffectStatusFailed
	summary.Result = nil
	summary.ErrorMessage = errorMessage
	a.effects[effectID] = summary
	a.markAsyncSettledLocked(effectID)
}

func (a *effectAccumulator) markAsyncSettledLocked(effectID model.EffectID) {
	if _, ok := a.asyncEffects[effectID]; !ok {
		return
	}
	delete(a.asyncEffects, effectID)
	if a.pendingAsync > 0 {
		a.pendingAsync--
	}
	if a.pendingAsync == 0 && a.asyncSettled != nil {
		close(a.asyncSettled)
		a.asyncSettled = nil
	}
}

func (a *effectAccumulator) waitPendingAsync(ctx context.Context) error {
	for {
		a.mu.Lock()
		if a.pendingAsync == 0 {
			a.mu.Unlock()
			return nil
		}
		ch := a.asyncSettled
		if ch == nil {
			ch = make(chan struct{})
			a.asyncSettled = ch
		}
		a.mu.Unlock()

		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (a *effectAccumulator) pendingAsyncCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.pendingAsync
}

func (a *effectAccumulator) recordPromiseRejection(p *goja.Promise, operation goja.PromiseRejectionOperation) {
	a.mu.Lock()
	defer a.mu.Unlock()

	switch operation {
	case goja.PromiseRejectionReject:
		if a.unhandledRejection == nil {
			a.unhandledRejection = make(map[*goja.Promise]string)
		}
		reason := formatPromiseRejectionReason(p.Result())
		a.unhandledRejection[p] = reason
	case goja.PromiseRejectionHandle:
		delete(a.unhandledRejection, p)
		if len(a.unhandledRejection) == 0 {
			a.unhandledRejection = nil
		}
	}
}

func formatPromiseRejectionReason(result goja.Value) string {
	if result == nil {
		return ""
	}
	if obj, ok := result.(*goja.Object); ok {
		stack := obj.Get("stack")
		if stack != nil && stack != goja.Undefined() && stack != goja.Null() {
			if text := stack.String(); text != "" {
				return text
			}
		}
	}
	return result.String()
}

func (a *effectAccumulator) unhandledPromiseError() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, reason := range a.unhandledRejection {
		return fmt.Errorf("promise rejected: %s", reason)
	}
	return nil
}

func (a *effectAccumulator) drain() []engine.EffectSummary {
	a.mu.Lock()
	defer a.mu.Unlock()
	effects := make([]engine.EffectSummary, 0, len(a.order))
	for _, effectID := range a.order {
		summary := a.effects[effectID]
		summary.Params = cloneBytes(summary.Params)
		summary.Result = cloneBytes(summary.Result)
		effects = append(effects, summary)
	}
	a.order = nil
	a.effects = nil
	a.asyncEffects = nil
	a.pendingAsync = 0
	a.asyncSettled = nil
	a.unhandledRejection = nil
	return effects
}

func cloneStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	return append([]string(nil), in...)
}

func (a *effectAccumulator) recordLog(line string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.logs = append(a.logs, line)
}

func (a *effectAccumulator) drainLogs() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	logs := cloneStrings(a.logs)
	a.logs = nil
	return logs
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

func jsErrorObject(vm *goja.Runtime, kind string, format string, args ...any) *goja.Object {
	message := fmt.Sprintf(format, args...)
	if vm == nil {
		return nil
	}
	switch kind {
	case "TypeError":
		return vm.NewTypeError("%s", message)
	default:
		ctor := vm.Get(kind)
		if ctor != nil && ctor != goja.Undefined() {
			if obj, err := vm.New(ctor, vm.ToValue(message)); err == nil {
				return obj
			}
		}
		if kind != "Error" {
			if obj := jsErrorObject(vm, "Error", "%s", message); obj != nil {
				return obj
			}
		}
		return vm.NewTypeError("%s", message)
	}
}

func panicJSError(vm *goja.Runtime, kind string, format string, args ...any) {
	if obj := jsErrorObject(vm, kind, format, args...); obj != nil {
		panic(obj)
	}
	panic(fmt.Sprintf(format, args...))
}

func rejectJSError(vm *goja.Runtime, reject func(interface{}) error, kind string, format string, args ...any) {
	if reject == nil {
		return
	}
	if obj := jsErrorObject(vm, kind, format, args...); obj != nil {
		_ = reject(obj)
		return
	}
	_ = reject(goja.Undefined())
}

func (b hostFuncBuilder) WrapSync(name string, replay model.ReplayPolicy, invoke engine.HostFuncInvoke) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		if b.router == nil || b.router.vm == nil {
			panic("internal runtime: missing host function router")
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

func installReplayAwareGojaNondeterminism(vm *goja.Runtime, host hostFuncBuilder) error {
	if vm == nil || host.router == nil {
		return nil
	}

	randomSource := host.WrapSync("Math.random", model.ReplayReadonly, func(_ context.Context, _ []byte) ([]byte, error) {
		return encodeBridgeLiteral(mrand.Float64())
	})
	timeSource := host.WrapSync("Date.now", model.ReplayReadonly, func(_ context.Context, _ []byte) ([]byte, error) {
		return encodeBridgeLiteral(time.Now().UTC().UnixMilli())
	})

	vm.SetRandSource(func() float64 {
		return randomSource(goja.FunctionCall{}).ToFloat()
	})
	vm.SetTimeSource(func() time.Time {
		ms := timeSource(goja.FunctionCall{}).ToInteger()
		return time.UnixMilli(ms).UTC()
	})
	return nil
}

func disableUnsupportedTimerGlobals(vm *goja.Runtime) error {
	if vm == nil {
		return nil
	}
	for _, name := range []string{"setTimeout", "setInterval", "clearTimeout", "clearInterval"} {
		timerName := name
		if err := vm.Set(timerName, func(goja.FunctionCall) goja.Value {
			panicJSError(vm, "Error", "%s is not available in this environment", timerName)
			return goja.Undefined()
		}); err != nil {
			return fmt.Errorf("set %s: %w", timerName, err)
		}
	}
	return nil
}

func installConsoleAndInspect(vm *goja.Runtime, router *hostFunctionRouter) error {
	if vm == nil {
		return nil
	}
	console := vm.NewObject()
	if err := console.Set("log", func(call goja.FunctionCall) goja.Value {
		if router == nil {
			return goja.Undefined()
		}
		return router.consoleLog(call)
	}); err != nil {
		return fmt.Errorf("set console.log: %w", err)
	}
	if err := vm.Set("console", console); err != nil {
		return fmt.Errorf("set console: %w", err)
	}
	if err := vm.Set("inspect", func(call goja.FunctionCall) goja.Value {
		if router == nil {
			return vm.ToValue("")
		}
		return router.inspectSummary(call)
	}); err != nil {
		return fmt.Errorf("set inspect: %w", err)
	}
	return nil
}

func encodeBridgeLiteral(value any) ([]byte, error) {
	return jswire.EncodeGoja(goja.New().ToValue(value))
}

// branchRuntime holds a single goja VM that persists across successful submits
// on one branch. Each new branch (including after Restore) gets its own VM so
// bindings from sibling branches are never visible to one another.
type branchRuntime struct {
	vm            *goja.Runtime
	loop          *eventloop.EventLoop
	router        *hostFunctionRouter
	topLevelAwait *topLevelAwaitState
	valueByIndex  map[int]indexedValue
	epoch         *runtimeEpochState
	closeOnce     sync.Once
}

type runtimeBootstrap struct {
	router        *hostFunctionRouter
	topLevelAwait *topLevelAwaitState
}

type indexedValue struct {
	value        goja.Value
	runtimeHash  string
	staleMessage string
}

type runtimeEpochState struct {
	mu          sync.RWMutex
	currentHash string
}

const (
	defaultStaleIndexedValueMessage = "indexed value from a previous runtime is no longer callable"
)

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

func bootstrapRuntimeVM(vm *goja.Runtime, loop *eventloop.EventLoop, st store.Store, ctx engine.SessionRuntimeContext, replayPlan *store.ReplayPlan) (*runtimeBootstrap, error) {
	engine.BindRuntimeLoop(vm, loop.RunOnLoop)
	router := newHostFunctionRouter(vm, &hostRuntimeWiring{store: st, sessionID: ctx.SessionID, replayPlan: replayPlan})
	vm.SetPromiseRejectionTracker(router.trackPromiseRejection)
	if err := installReplayAwareGojaNondeterminism(vm, hostFuncBuilder{router: router}); err != nil {
		return nil, fmt.Errorf("install replay-aware goja nondeterminism: %w", err)
	}
	if err := disableUnsupportedTimerGlobals(vm); err != nil {
		return nil, fmt.Errorf("disable timer globals: %w", err)
	}
	if err := installConsoleAndInspect(vm, router); err != nil {
		return nil, fmt.Errorf("install console/inspect: %w", err)
	}
	topLevelAwait := newTopLevelAwaitState()
	if err := topLevelAwait.install(vm); err != nil {
		return nil, fmt.Errorf("install top-level await hook: %w", err)
	}
	return &runtimeBootstrap{
		router:        router,
		topLevelAwait: topLevelAwait,
	}, nil
}

func (s *runtimeEpochState) setCurrent(runtimeHash string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.currentHash = runtimeHash
}

func (s *runtimeEpochState) isCurrentFunc(runtimeHash string) func() bool {
	return func() bool {
		if s == nil {
			return false
		}
		s.mu.RLock()
		defer s.mu.RUnlock()
		return s.currentHash == runtimeHash
	}
}

func (s *runtimeEpochState) current() string {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.currentHash
}

func bindSessionRuntimeContext(ctx engine.SessionRuntimeContext, epoch *runtimeEpochState, runtimeHash string) engine.SessionRuntimeContext {
	ctx.RuntimeHash = runtimeHash
	ctx.IsCurrentRuntime = epoch.isCurrentFunc(runtimeHash)
	return ctx
}

func bindRuntimeTransitionContext(ctx engine.RuntimeTransitionContext, epoch *runtimeEpochState) engine.RuntimeTransitionContext {
	ctx.IsCurrentRuntime = epoch.isCurrentFunc(ctx.ToRuntimeHash)
	return ctx
}

func installRuntimeValueLookups(vm *goja.Runtime, valueByIndex map[int]indexedValue) error {
	if err := vm.Set("$last", goja.Undefined()); err != nil {
		return fmt.Errorf("install $last: %w", err)
	}
	if err := vm.Set("$val", func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) == 0 {
			panicJSError(vm, "TypeError", "$val(index): missing index")
		}
		index := int(call.Argument(0).ToInteger())
		if index <= 0 {
			panicJSError(vm, "RangeError", "$val(%d): index must be positive", index)
		}
		entry, ok := valueByIndex[index]
		if !ok {
			panicJSError(vm, "Error", "$val(%d): no such visible cell value on this branch", index)
		}
		return entry.value
	}); err != nil {
		return fmt.Errorf("install $val: %w", err)
	}
	return nil
}

func previewConfiguredRuntimeState(bgCtx context.Context, ctx engine.SessionRuntimeContext, st store.Store, delegate engine.VMDelegate, state json.RawMessage, replayPlan *store.ReplayPlan) (json.RawMessage, error) {
	if delegate == nil {
		return state, nil
	}
	epoch := &runtimeEpochState{}
	epoch.setCurrent(ctx.RuntimeHash)
	ctx = bindSessionRuntimeContext(ctx, epoch, ctx.RuntimeHash)

	loop := eventloop.NewEventLoop(eventloop.EnableConsole(false))
	loop.Start()

	type previewResult struct {
		configured json.RawMessage
		err        error
	}
	resultCh := make(chan previewResult, 1)
	loop.RunOnLoop(func(vm *goja.Runtime) {
		bootstrap, err := bootstrapRuntimeVM(vm, loop, st, ctx, replayPlan)
		if err != nil {
			resultCh <- previewResult{err: err}
			return
		}
		configuredState, err := delegate.ConfigureRuntime(ctx, vm, hostFuncBuilder{router: bootstrap.router}, state)
		if err != nil {
			resultCh <- previewResult{err: fmt.Errorf("configure runtime: %w", err)}
			return
		}
		resultCh <- previewResult{configured: configuredState}
	})

	result := <-resultCh
	_ = bgCtx
	loop.RunOnLoop(func(vm *goja.Runtime) {
		engine.UnbindRuntimeLoop(vm)
	})
	loop.Stop()
	return result.configured, result.err
}

func resolveRuntimeDescriptor(bgCtx context.Context, ctx engine.SessionRuntimeContext, st store.Store, delegate engine.VMDelegate, state json.RawMessage, replayPlan *store.ReplayPlan) (json.RawMessage, string, error) {
	normalizedState, err := normalizeRuntimeConfig(state)
	if err != nil {
		return nil, "", err
	}
	if delegate == nil {
		if normalizedState != nil {
			normalizedState = append(json.RawMessage(nil), normalizedState...)
		}
		return normalizedState, runtimeConfigHash(normalizedState), nil
	}

	hash := runtimeConfigHash(normalizedState)
	for pass := 0; pass < maxRuntimeHashResolutionPasses; pass++ {
		configuredState, err := previewConfiguredRuntimeState(bgCtx, engine.SessionRuntimeContext{
			SessionID:   ctx.SessionID,
			BranchID:    ctx.BranchID,
			RuntimeHash: hash,
		}, st, delegate, normalizedState, replayPlan)
		if err != nil {
			return nil, "", err
		}
		normalizedConfiguredState, err := normalizeRuntimeConfig(configuredState)
		if err != nil {
			return nil, "", err
		}
		nextHash := runtimeConfigHash(normalizedConfiguredState)
		if nextHash == hash {
			if normalizedConfiguredState != nil {
				normalizedConfiguredState = append(json.RawMessage(nil), normalizedConfiguredState...)
			}
			return normalizedConfiguredState, nextHash, nil
		}
		hash = nextHash
	}
	return nil, "", fmt.Errorf("resolve runtime descriptor: runtime hash did not stabilize after %d passes", maxRuntimeHashResolutionPasses)
}

// newBranchRuntime allocates a fresh goja VM with no pre-loaded bindings,
// starts an event loop for it, and applies the optional delegate before
// returning. It returns the configured VM plus the serialised runtime
// descriptor that should be persisted for future recreation.
func newBranchRuntime(bgCtx context.Context, ctx engine.SessionRuntimeContext, st store.Store, delegate engine.VMDelegate, state json.RawMessage, replayPlan *store.ReplayPlan) (*branchRuntime, json.RawMessage, string, error) {
	_, resolvedHash, err := resolveRuntimeDescriptor(bgCtx, ctx, st, delegate, state, replayPlan)
	if err != nil {
		return nil, nil, "", err
	}
	epoch := &runtimeEpochState{}
	epoch.setCurrent(resolvedHash)

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
		bootstrap, err := bootstrapRuntimeVM(vm, loop, st, bindSessionRuntimeContext(engine.SessionRuntimeContext{
			SessionID: ctx.SessionID,
			BranchID:  ctx.BranchID,
		}, epoch, resolvedHash), replayPlan)
		if err != nil {
			initCh <- initResult{err: err}
			return
		}
		rt := &branchRuntime{vm: vm, loop: loop, router: bootstrap.router, topLevelAwait: bootstrap.topLevelAwait, valueByIndex: make(map[int]indexedValue), epoch: epoch}
		if err := installRuntimeValueLookups(vm, rt.valueByIndex); err != nil {
			initCh <- initResult{err: err}
			return
		}

		normalizedState, err := normalizeRuntimeConfig(state)
		if err != nil {
			initCh <- initResult{err: err}
			return
		}
		configuredState := normalizedState
		if delegate != nil {
			configuredState, err = delegate.ConfigureRuntime(bindSessionRuntimeContext(engine.SessionRuntimeContext{
				SessionID: ctx.SessionID,
				BranchID:  ctx.BranchID,
			}, epoch, resolvedHash), vm, hostFuncBuilder{router: bootstrap.router}, normalizedState)
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
		actualHash := runtimeConfigHash(normalizedConfiguredState)
		if actualHash != resolvedHash {
			initCh <- initResult{err: fmt.Errorf("configure runtime: resolved runtime hash %q but delegate produced %q", resolvedHash, actualHash)}
			return
		}

		_ = bgCtx
		initCh <- initResult{
			runtime:     rt,
			configured:  normalizedConfiguredState,
			runtimeHash: actualHash,
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

func (r *branchRuntime) flushLoop(ctx context.Context) error {
	if r == nil || r.loop == nil {
		return nil
	}
	done := make(chan struct{})
	r.loop.RunOnLoop(func(*goja.Runtime) {
		close(done)
	})
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *branchRuntime) awaitCellSettled(ctx context.Context, acc *effectAccumulator) error {
	if acc == nil {
		return r.flushLoop(ctx)
	}
	for {
		if err := acc.waitPendingAsync(ctx); err != nil {
			return err
		}
		if err := r.flushLoop(ctx); err != nil {
			return err
		}
		if acc.pendingAsyncCount() == 0 {
			break
		}
	}
	return acc.unhandledPromiseError()
}

func (r *branchRuntime) close() {
	if r == nil || r.loop == nil {
		return
	}
	r.closeOnce.Do(func() {
		if r.vm != nil {
			engine.UnbindRuntimeLoop(r.vm)
		}
		r.loop.Stop()
	})
}

func (r *hostFunctionRouter) invokeSync(name string, replay model.ReplayPolicy, invoke engine.HostFuncInvoke, call goja.FunctionCall) goja.Value {
	vm := r.vm
	params, err := marshalFirstArgument(call)
	if err != nil {
		panicJSError(vm, "TypeError", "%v", err)
	}
	if r.currentMode() == hostFunctionModeReplay {
		inv, replayErr := r.nextReplayInvocation()
		if replayErr != nil {
			panicJSError(vm, "Error", "host function %q replay: %v", name, replayErr)
		}
		if inv.decision.Policy == model.ReplayNonReplayable {
			panicJSError(vm, "Error", "non-replayable effect: function %q effectID %q cannot be replayed", name, inv.effectID)
		}
		if len(inv.decision.RecordedResult) == 0 {
			panicJSError(vm, "Error", "host function %q replay: missing recorded result for effect %q", name, inv.effectID)
		}
		return bridgeResultToValue(vm, inv.decision.RecordedResult)
	}

	live, err := r.currentLiveState()
	if err != nil {
		panicJSError(vm, "Error", "host function %q: %v", name, err)
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
		panicJSError(vm, "Error", "host function %q: journal EffectStarted: %v", name, err)
	}
	live.acc.recordStarted(effectID, name, params, replay)

	result, err := invoke(live.ctx, params)
	if err != nil {
		live.acc.recordFailed(effectID, err.Error())
		_ = r.store.AppendFact(context.Background(), model.EffectFailed{
			Session:      r.sessionID,
			Effect:       effectID,
			ErrorMessage: err.Error(),
			At:           time.Now().UTC(),
		})
		panicJSError(vm, "Error", "%v", err)
	}
	if err := r.store.AppendFact(context.Background(), model.EffectCompleted{
		Session: r.sessionID,
		Effect:  effectID,
		Result:  result,
		At:      time.Now().UTC(),
	}); err != nil {
		msg := fmt.Sprintf("host function %q: journal EffectCompleted: %v", name, err)
		live.acc.recordFailed(effectID, msg)
		_ = r.store.AppendFact(context.Background(), model.EffectFailed{
			Session:      r.sessionID,
			Effect:       effectID,
			ErrorMessage: msg,
			At:           time.Now().UTC(),
		})
		panicJSError(vm, "Error", "%s", msg)
	}
	live.acc.recordCompleted(effectID, result)
	return bridgeResultToValue(vm, result)
}

func (r *hostFunctionRouter) trackPromiseRejection(p *goja.Promise, operation goja.PromiseRejectionOperation) {
	if r == nil {
		return
	}
	r.liveMu.RLock()
	live := r.live
	r.liveMu.RUnlock()
	if live == nil || live.acc == nil {
		return
	}
	live.acc.recordPromiseRejection(p, operation)
}

func (r *hostFunctionRouter) consoleLog(call goja.FunctionCall) goja.Value {
	if r == nil || r.vm == nil {
		return goja.Undefined()
	}
	live, err := r.currentLiveState()
	if err != nil || live == nil || live.acc == nil {
		return goja.Undefined()
	}
	live.acc.recordLog(renderObservationalPreviewArgs(call.Arguments))
	return goja.Undefined()
}

func (r *hostFunctionRouter) inspectSummary(call goja.FunctionCall) goja.Value {
	if r == nil || r.vm == nil {
		return nil
	}
	return r.vm.ToValue(renderSummaryArgs(call.Arguments))
}

func renderSummaryArgs(args []goja.Value) string {
	if len(args) == 0 {
		return ""
	}
	parts := make([]string, 0, len(args))
	for _, arg := range args {
		parts = append(parts, renderSummaryValue(arg))
	}
	return strings.Join(parts, " ")
}

func renderPreviewArgs(args []goja.Value) string {
	if len(args) == 0 {
		return ""
	}
	parts := make([]string, 0, len(args))
	for _, arg := range args {
		parts = append(parts, gojaValuePreview(arg))
	}
	return strings.Join(parts, " ")
}

func renderObservationalPreviewArgs(args []goja.Value) string {
	if len(args) == 0 {
		return ""
	}
	parts := make([]string, 0, len(args))
	for _, arg := range args {
		parts = append(parts, gojaValueObservationalPreview(arg))
	}
	return strings.Join(parts, " ")
}

func renderSummaryValue(v goja.Value) string {
	if v == nil {
		return "undefined"
	}
	raw, err := jswire.EncodeGoja(v)
	if err != nil {
		return v.String()
	}
	inspection, err := jswire.Describe(raw)
	if err != nil || inspection.Summary == "" {
		return v.String()
	}
	return inspection.Summary
}

func (r *hostFunctionRouter) invokeAsync(name string, replay model.ReplayPolicy, invoke engine.HostFuncInvoke, call goja.FunctionCall) goja.Value {
	vm := r.vm
	promise, resolve, reject := vm.NewPromise()

	params, err := marshalFirstArgument(call)
	if err != nil {
		rejectJSError(vm, reject, "TypeError", "%v", err)
		return vm.ToValue(promise)
	}

	if r.currentMode() == hostFunctionModeReplay {
		inv, replayErr := r.nextReplayInvocation()
		if replayErr != nil {
			rejectJSError(vm, reject, "Error", "host function %q replay: %v", name, replayErr)
			return vm.ToValue(promise)
		}
		if inv.decision.Policy == model.ReplayNonReplayable {
			rejectJSError(vm, reject, "Error", "non-replayable effect: function %q effectID %q cannot be replayed", name, inv.effectID)
			return vm.ToValue(promise)
		}
		if len(inv.decision.RecordedResult) == 0 {
			rejectJSError(vm, reject, "Error", "host function %q replay: missing recorded result for effect %q", name, inv.effectID)
			return vm.ToValue(promise)
		}
		order := inv.decision.CompletionOrder
		if order <= 0 {
			order = int(time.Now().UnixNano())
		}
		r.queueReplayAsync(order, func(vm *goja.Runtime) {
			_ = resolve(bridgeResultToValue(vm, inv.decision.RecordedResult))
		})
		return vm.ToValue(promise)
	}

	live, stateErr := r.currentLiveState()
	if stateErr != nil {
		rejectJSError(vm, reject, "Error", "host function %q: %v", name, stateErr)
		return vm.ToValue(promise)
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
		rejectJSError(vm, reject, "Error", "host function %q: journal EffectStarted: %v", name, err)
		return vm.ToValue(promise)
	}
	live.acc.addAsync(effectID, name, params, replay)

	go func(callCtx context.Context) {
		result, invokeErr := invoke(callCtx, params)
		engine.RunOnRuntimeLoop(vm, func(vm *goja.Runtime) {
			now := time.Now().UTC()
			if invokeErr != nil {
				live.acc.recordFailed(effectID, invokeErr.Error())
				_ = r.store.AppendFact(context.Background(), model.EffectFailed{
					Session:      r.sessionID,
					Effect:       effectID,
					ErrorMessage: invokeErr.Error(),
					At:           now,
				})
				rejectJSError(vm, reject, "Error", "%v", invokeErr)
				return
			}
			if err := r.store.AppendFact(context.Background(), model.EffectCompleted{
				Session: r.sessionID,
				Effect:  effectID,
				Result:  result,
				At:      now,
			}); err != nil {
				msg := fmt.Sprintf("host function %q: journal EffectCompleted: %v", name, err)
				live.acc.recordFailed(effectID, msg)
				_ = r.store.AppendFact(context.Background(), model.EffectFailed{
					Session:      r.sessionID,
					Effect:       effectID,
					ErrorMessage: msg,
					At:           now,
				})
				rejectJSError(vm, reject, "Error", "%s", msg)
				return
			}
			live.acc.recordCompleted(effectID, result)
			_ = resolve(bridgeResultToValue(vm, result))
		})
	}(live.ctx)

	return vm.ToValue(promise)
}

func marshalFirstArgument(call goja.FunctionCall) ([]byte, error) {
	if len(call.Arguments) == 0 {
		return jswire.EncodeGoja(goja.Undefined())
	}
	b, err := jswire.EncodeGoja(call.Arguments[0])
	if err != nil {
		return nil, fmt.Errorf("marshal params: %w", err)
	}
	return b, nil
}

func bridgeResultToValue(vm *goja.Runtime, raw []byte) goja.Value {
	if len(raw) == 0 {
		return vm.ToValue(nil)
	}
	decoded, err := jswire.DecodeGoja(vm, raw)
	if err != nil {
		return vm.ToValue(fmt.Sprintf("bridge decode failed: %v", err))
	}
	return decoded
}

// evalResult carries the output of a single evaluation.
type evalResult struct {
	// completionValue is non-nil when the program produced a defined export
	// value (i.e. the last expression evaluated to something other than
	// undefined).
	completionValue *model.ValueRef

	// structured carries the versioned bridge encoding of the completion value.
	// It is nil when the value cannot be bridge-encoded or when completionValue
	// is nil.
	structured []byte

	// indexedValue is the actual JS value that should be rebound into REPL
	// conveniences like `$last` and `$val(n)`.
	indexedValue goja.Value

	// hasIndexedValue reports whether indexedValue should be applied.
	hasIndexedValue bool
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
	plan, err := buildTopLevelAwaitPlan(src, r.topLevelAwait)
	if err != nil {
		return evalResult{}, fmt.Errorf("goja: %w", err)
	}

	resultCh := make(chan outcome, 1)
	deliver := func(out outcome) {
		select {
		case resultCh <- out:
		default:
		}
	}

	r.topLevelAwait.beginCommit(plan.Declarations)
	defer r.topLevelAwait.endCommit()

	r.loop.RunOnLoop(func(vm *goja.Runtime) {
		raw, err := vm.RunProgram(plan.Program)
		if err != nil {
			deliver(outcome{err: fmt.Errorf("goja: %w", err)})
			return
		}
		r.settleValueAsync(raw, deliver)
	})

	select {
	case out := <-resultCh:
		if out.err == nil {
			r.topLevelAwait.commitDeclarations(plan.Declarations)
		}
		return out.res, out.err
	case <-ctx.Done():
		return evalResult{}, fmt.Errorf("promise still pending after evaluation: %w", ctx.Err())
	}
}

func (r *branchRuntime) setIndexedResult(index int, eval evalResult) error {
	if r == nil || r.loop == nil {
		return nil
	}
	runtimeHash := ""
	if r.epoch != nil {
		runtimeHash = r.epoch.current()
	}
	done := make(chan error, 1)
	r.loop.RunOnLoop(func(vm *goja.Runtime) {
		value := goja.Undefined()
		staleMessage := defaultStaleIndexedValueMessage
		if eval.hasIndexedValue {
			value = eval.indexedValue
			if meta, ok := engine.IndexedValueMetadataFor(value); ok && strings.TrimSpace(meta.StaleMessage) != "" {
				staleMessage = meta.StaleMessage
			}
		}
		if err := vm.Set("$last", value); err != nil {
			done <- err
			return
		}
		if index > 0 {
			if r.valueByIndex == nil {
				r.valueByIndex = make(map[int]indexedValue)
			}
			r.valueByIndex[index] = indexedValue{value: value, runtimeHash: runtimeHash, staleMessage: staleMessage}
			done <- nil
			return
		}
		done <- nil
	})
	return <-done
}

func (r *branchRuntime) replaceStaleIndexedResults(runtimeHash string, currentIndex int) error {
	if r == nil || r.loop == nil || runtimeHash == "" || currentIndex <= 0 {
		return nil
	}
	done := make(chan error, 1)
	r.loop.RunOnLoop(func(vm *goja.Runtime) {
		for index, entry := range r.valueByIndex {
			if index > currentIndex {
				continue
			}
			if _, ok := goja.AssertFunction(entry.value); !ok {
				continue
			}
			if entry.runtimeHash != "" && entry.runtimeHash != runtimeHash {
				replaced, err := newStaleIndexedCallable(vm, indexedValueStaleMessage(entry))
				if err != nil {
					done <- err
					return
				}
				entry.value = replaced
				entry.runtimeHash = ""
				r.valueByIndex[index] = entry
			}
		}
		last := goja.Undefined()
		for index := currentIndex; index >= 1; index-- {
			if entry, ok := r.valueByIndex[index]; ok {
				last = entry.value
				break
			}
		}
		if err := vm.Set("$last", last); err != nil {
			done <- err
			return
		}
		done <- nil
	})
	return <-done
}

func indexedValueStaleMessage(entry indexedValue) string {
	if strings.TrimSpace(entry.staleMessage) == "" {
		return defaultStaleIndexedValueMessage
	}
	return entry.staleMessage
}

func newStaleIndexedCallable(vm *goja.Runtime, message string) (goja.Value, error) {
	fn := vm.ToValue(func(goja.FunctionCall) goja.Value {
		panicJSError(vm, "Error", "%s", message)
		return goja.Undefined()
	})
	return fn, nil
}

func (r *branchRuntime) settleValueAsync(v goja.Value, deliver func(outcome)) {
	if v == nil {
		deliver(outcome{res: evalResult{}})
		return
	}
	p, ok := v.Export().(*goja.Promise)
	if !ok {
		r.finalizeCompletionValue(v, deliver)
		return
	}
	switch p.State() {
	case goja.PromiseStateFulfilled:
		r.finalizeCompletionValue(p.Result(), deliver)
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
				r.finalizeCompletionValue(call.Argument(0), deliver)
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

func (r *branchRuntime) finalizeCompletionValue(v goja.Value, deliver func(outcome)) {
	if committed, ok := unwrapInternalCompletionEnvelope(v); ok {
		r.finalizeCommittedValue(committed, deliver)
		return
	}
	res, err := valueToEvalResult(v, v)
	deliver(outcome{res: res, err: err})
}

func (r *branchRuntime) finalizeCommittedValue(v goja.Value, deliver func(outcome)) {
	if p, ok := exportedGojaPromise(v); ok {
		switch p.State() {
		case goja.PromiseStateFulfilled:
			res, err := valueToEvalResult(v, v)
			deliver(outcome{res: res, err: err})
		case goja.PromiseStateRejected:
			reason := p.Result()
			var preview string
			if reason != nil {
				preview = reason.String()
			}
			deliver(outcome{err: fmt.Errorf("promise rejected: %s", preview)})
		default:
			obj := v.ToObject(r.vm)
			then, ok := goja.AssertFunction(obj.Get("then"))
			if !ok {
				deliver(outcome{err: fmt.Errorf("promise still pending after evaluation")})
				return
			}
			_, err := then(obj,
				r.vm.ToValue(func(call goja.FunctionCall) goja.Value {
					res, err := valueToEvalResult(v, v)
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
		return
	}
	res, err := valueToEvalResult(v, v)
	deliver(outcome{res: res, err: err})
}

func valueToEvalResult(displayVal goja.Value, indexedVal goja.Value) (evalResult, error) {
	out := evalResult{
		indexedValue:    indexedVal,
		hasIndexedValue: true,
	}
	var ref *model.ValueRef
	var structured []byte
	if displayVal != nil && !goja.IsUndefined(displayVal) && !goja.IsNull(displayVal) {
		typeHint := gojaTypeHint(displayVal)
		preview := gojaValuePreview(displayVal)
		ref = &model.ValueRef{
			ID:       model.ValueID(uuid.NewString()),
			Preview:  preview,
			TypeHint: typeHint,
		}
		if b, merr := jswire.EncodeGoja(displayVal); merr == nil {
			structured = b
		}
	}
	out.completionValue = ref
	out.structured = structured
	return out, nil
}

func gojaValuePreview(v goja.Value) string {
	switch {
	case v == nil:
		return "undefined"
	case goja.IsUndefined(v):
		return "undefined"
	case goja.IsNull(v):
		return "null"
	default:
		return v.String()
	}
}

func gojaValueObservationalPreview(v goja.Value) string {
	switch {
	case v == nil:
		return "undefined"
	case goja.IsUndefined(v):
		return "undefined"
	case goja.IsNull(v):
		return "null"
	}
	if obj, ok := v.(*goja.Object); ok {
		return "[object " + obj.ClassName() + "]"
	}
	return v.String()
}

// replayPlanIntoRuntime creates a fresh branchRuntime and replays every step in
// plan into it, in order, using the same evaluation path as Submit. This
// ensures a restored branch starts with exactly the bindings visible at the
// restore target, and no bindings created after the fork point.
func replayPlanIntoRuntime(ctx context.Context, plan store.ReplayPlan, rtCtx engine.SessionRuntimeContext, st store.Store, delegate engine.VMDelegate) (*branchRuntime, json.RawMessage, string, error) {
	return replayPlanPrefixIntoRuntime(ctx, plan, len(plan.Steps), rtCtx, st, delegate)
}

func replaySourceForStep(step store.ReplayStep) string {
	if step.Language == model.CellLanguageTypeScript {
		return step.EmittedJS
	}
	if step.EmittedJS != "" {
		return step.EmittedJS
	}
	return step.Source
}

func applyRuntimeTransition(ctx context.Context, rt *branchRuntime, transitionCtx engine.RuntimeTransitionContext, delegate engine.VMDelegate, fromState, toState json.RawMessage) error {
	if rt == nil {
		return fmt.Errorf("apply runtime transition: nil runtime")
	}
	if transitionCtx.FromRuntimeHash == transitionCtx.ToRuntimeHash {
		return nil
	}
	if delegate == nil {
		return nil
	}
	transitionDelegate, ok := delegate.(engine.VMTransitionDelegate)
	if !ok {
		return fmt.Errorf("apply runtime transition: delegate does not implement engine.VMTransitionDelegate")
	}
	if rt.epoch != nil {
		rt.epoch.setCurrent(transitionCtx.ToRuntimeHash)
	}
	transitionCtx = bindRuntimeTransitionContext(transitionCtx, rt.epoch)

	resultCh := make(chan error, 1)
	rt.loop.RunOnLoop(func(vm *goja.Runtime) {
		resultCh <- transitionDelegate.TransitionRuntime(transitionCtx, vm, hostFuncBuilder{router: rt.router}, fromState, toState)
	})

	select {
	case err := <-resultCh:
		if err != nil {
			return fmt.Errorf("apply runtime transition: %w", err)
		}
	case <-ctx.Done():
		return ctx.Err()
	}
	return rt.flushLoop(ctx)
}

func applyRuntimeTransitionAndInvalidateIndexedResults(ctx context.Context, rt *branchRuntime, transitionCtx engine.RuntimeTransitionContext, delegate engine.VMDelegate, fromState, toState json.RawMessage, currentIndex int) error {
	if transitionCtx.FromRuntimeHash == transitionCtx.ToRuntimeHash {
		return nil
	}
	if err := applyRuntimeTransition(ctx, rt, transitionCtx, delegate, fromState, toState); err != nil {
		return err
	}
	if err := rt.replaceStaleIndexedResults(transitionCtx.ToRuntimeHash, currentIndex); err != nil {
		return fmt.Errorf("apply runtime transition: replace stale indexed results: %w", err)
	}
	return nil
}

func replayPlanPrefixIntoRuntime(ctx context.Context, plan store.ReplayPlan, stepCount int, rtCtx engine.SessionRuntimeContext, st store.Store, delegate engine.VMDelegate) (*branchRuntime, json.RawMessage, string, error) {
	rt, configuredState, runtimeHash, err := newBranchRuntime(ctx, rtCtx, st, delegate, plan.RuntimeConfig, &plan)
	if err != nil {
		return nil, nil, "", err
	}
	if stepCount < 0 {
		stepCount = 0
	}
	if stepCount > len(plan.Steps) {
		stepCount = len(plan.Steps)
	}
	transitionsByAfterCell := make(map[model.CellID][]store.RuntimeTransition)
	for _, transition := range plan.RuntimeTransitions {
		transitionsByAfterCell[transition.AfterCell] = append(transitionsByAfterCell[transition.AfterCell], transition)
	}
	applyTransitions := func(afterCell model.CellID, currentIndex int) error {
		for _, transition := range transitionsByAfterCell[afterCell] {
			if err := applyRuntimeTransitionAndInvalidateIndexedResults(ctx, rt, engine.RuntimeTransitionContext{
				SessionID:       rtCtx.SessionID,
				BranchID:        rtCtx.BranchID,
				FromRuntimeHash: runtimeHash,
				ToRuntimeHash:   transition.RuntimeHash,
			}, delegate, configuredState, transition.RuntimeConfig, currentIndex); err != nil {
				return err
			}
			configuredState = append(json.RawMessage(nil), transition.RuntimeConfig...)
			runtimeHash = transition.RuntimeHash
		}
		return nil
	}
	if err := applyTransitions("", 0); err != nil {
		rt.close()
		return nil, nil, "", err
	}
	for stepIndex, step := range plan.Steps[:stepCount] {
		if err := rt.beginReplayStep(step.Effects, plan.Decisions); err != nil {
			rt.close()
			return nil, nil, "", fmt.Errorf("prepare replay step %q: %w", step.Cell, err)
		}
		eval, err := rt.runContext(ctx, replaySourceForStep(step))
		if err != nil {
			_ = rt.finishReplayStep()
			rt.close()
			return nil, nil, "", fmt.Errorf("replay cell %q: %w", step.Cell, err)
		}
		if err := rt.finishReplayStep(); err != nil {
			rt.close()
			return nil, nil, "", fmt.Errorf("replay cell %q effects: %w", step.Cell, err)
		}
		if err := rt.setIndexedResult(stepIndex+1, eval); err != nil {
			rt.close()
			return nil, nil, "", fmt.Errorf("replay cell %q set $last/$val(%d): %w", step.Cell, stepIndex+1, err)
		}
		if err := applyTransitions(step.Cell, stepIndex+1); err != nil {
			rt.close()
			return nil, nil, "", fmt.Errorf("replay cell %q transition runtime: %w", step.Cell, err)
		}
	}
	if stepCount == len(plan.Steps) {
		rt.activateLiveBindings()
	}
	return rt, configuredState, runtimeHash, nil
}

// gojaTypeHint returns a simple TypeScript-style type name for a goja value.
func gojaTypeHint(v goja.Value) string {
	if promise, ok := exportedSettledGojaPromise(v); ok {
		inner := gojaTypeHint(promise.Result())
		if inner == "" {
			inner = "unknown"
		}
		return "Promise<" + inner + ">"
	}
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

func exportedGojaPromise(v goja.Value) (*goja.Promise, bool) {
	if v == nil {
		return nil, false
	}
	promise, ok := v.Export().(*goja.Promise)
	if !ok || promise == nil {
		return nil, false
	}
	return promise, true
}

func exportedSettledGojaPromise(v goja.Value) (*goja.Promise, bool) {
	promise, ok := exportedGojaPromise(v)
	if !ok {
		return nil, false
	}
	if promise.State() != goja.PromiseStateFulfilled {
		return nil, false
	}
	return promise, true
}

func isSettledGojaPromiseValue(v goja.Value) bool {
	_, ok := exportedSettledGojaPromise(v)
	return ok
}

func unwrapInternalCompletionEnvelope(v goja.Value) (goja.Value, bool) {
	obj, ok := v.(*goja.Object)
	if !ok || obj == nil {
		return nil, false
	}
	brand := obj.Get(replInternalCompletionBrand)
	if brand == nil || goja.IsUndefined(brand) || goja.IsNull(brand) || !brand.ToBoolean() {
		return nil, false
	}
	return obj.Get("value"), true
}

type outcome struct {
	res evalResult
	err error
}
