package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
// goroutine (via add) and the caller (via drain) may touch it concurrently.
type effectAccumulator struct {
	mu       sync.Mutex
	effects  []model.EffectID
	promises []model.PromiseRef
}

func (a *effectAccumulator) add(effectID model.EffectID, promiseRef model.PromiseRef) {
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

// effectsWiring bundles the dependencies needed to install journaling wrappers
// for host functions. newBranchRuntime accepts a *effectsWiring; nil disables
// the feature entirely.
type effectsWiring struct {
	delegate  engine.EffectsDelegate
	store     store.Store
	sessionID model.SessionID
	cellID    func() model.CellID   // returns the cell being evaluated (may be called during setup)
	acc       *effectAccumulator
}

// branchRuntime holds a single goja VM that persists across successful submits
// on one branch. Each new branch (including after Restore) gets its own VM so
// bindings from sibling branches are never visible to one another.
type branchRuntime struct {
	vm   *goja.Runtime
	loop *eventloop.EventLoop
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
//
// When wiring is non-nil, newBranchRuntime also calls
// wiring.delegate.HostFunctions and installs one journaling JS global per
// HostFunctionDef. Pass nil to skip effect wiring (no delegate configured).
func newBranchRuntime(bgCtx context.Context, ctx engine.SessionRuntimeContext, delegate engine.VMDelegate, state json.RawMessage, wiring *effectsWiring) (*branchRuntime, json.RawMessage, string, error) {
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

		normalizedState, err := normalizeRuntimeConfig(state)
		if err != nil {
			initCh <- initResult{err: err}
			return
		}
		configuredState := normalizedState
		if delegate != nil {
			configuredState, err = delegate.ConfigureRuntime(ctx, vm, normalizedState)
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

		// Install journaling host-function bindings if an effects delegate is wired.
		if wiring != nil {
			defs, hErr := wiring.delegate.HostFunctions(bgCtx, wiring.sessionID)
			if hErr != nil {
				initCh <- initResult{err: fmt.Errorf("effects delegate: HostFunctions: %w", hErr)}
				return
			}
			for _, def := range defs {
				def := def // capture loop variable
				if hErr := installHostFunction(vm, wiring, def); hErr != nil {
					initCh <- initResult{err: fmt.Errorf("install host function %q: %w", def.Name, hErr)}
					return
				}
			}
		}

		initCh <- initResult{
			runtime:     &branchRuntime{vm: vm, loop: loop},
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

// installHostFunction sets a JS global on vm that journals EffectStarted /
// EffectCompleted / EffectFailed facts and calls def.Invoke in a goroutine.
// Must be called on the runtime event loop.
func installHostFunction(vm *goja.Runtime, wiring *effectsWiring, def engine.HostFunctionDef) error {
	return vm.Set(def.Name, func(call goja.FunctionCall) goja.Value {
		promise, resolve, reject := vm.NewPromise()

		// Serialize the first argument to JSON.
		var params json.RawMessage
		if len(call.Arguments) > 0 && !goja.IsUndefined(call.Arguments[0]) && !goja.IsNull(call.Arguments[0]) {
			exported := call.Arguments[0].Export()
			b, err := json.Marshal(exported)
			if err != nil {
				_ = reject(vm.ToValue(fmt.Sprintf("host function %q: marshal params: %v", def.Name, err)))
				return vm.ToValue(promise)
			}
			params = b
		} else {
			params = json.RawMessage("null")
		}

		effectID := model.EffectID(uuid.NewString())
		promiseID := model.PromiseID(uuid.NewString())

		// AppendFact(EffectStarted) synchronously on the event loop before the
		// goroutine is launched — no goroutine may run without a journal entry.
		cellID := wiring.cellID()
		if err := wiring.store.AppendFact(context.Background(), model.EffectStarted{
			Session:      wiring.sessionID,
			Effect:       effectID,
			Cell:         cellID,
			FunctionName: def.Name,
			Params:       params,
			ReplayPolicy: def.ReplayPolicy,
			At:           time.Now().UTC(),
		}); err != nil {
			_ = reject(vm.ToValue(fmt.Sprintf("host function %q: journal EffectStarted: %v", def.Name, err)))
			return vm.ToValue(promise)
		}

		// Record the effect+promise in the accumulator so the caller can populate
		// CellEvaluated.LinkedEffects and CellEvaluated.CreatedPromises.
		wiring.acc.add(effectID, model.PromiseRef{ID: promiseID, State: model.PromisePending})

		invoke := def.Invoke
		sessionID := wiring.sessionID
		st := wiring.store

		go func() {
			result, err := invoke(context.Background(), params)
			engine.RunOnRuntimeLoop(vm, func(vm *goja.Runtime) {
				now := time.Now().UTC()
				if err != nil {
					_ = st.AppendFact(context.Background(), model.EffectFailed{
						Session:      sessionID,
						Effect:       effectID,
						ErrorMessage: err.Error(),
						At:           now,
					})
					_ = reject(vm.ToValue(err.Error()))
					return
				}
				_ = st.AppendFact(context.Background(), model.EffectCompleted{
					Session: sessionID,
					Effect:  effectID,
					Result:  result,
					At:      now,
				})
				// Parse the JSON result back into a JS value so callers can
				// await the promise and receive a proper object.
				var resultVal any
				if len(result) > 0 {
					if jsonErr := json.Unmarshal(result, &resultVal); jsonErr != nil {
						// If we can't unmarshal, pass the raw string.
						resultVal = string(result)
					}
				}
				_ = resolve(vm.ToValue(resultVal))
			})
		}()

		return vm.ToValue(promise)
	})
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
//
// Returns an error (and a nil runtime) if any step fails to evaluate; in that
// case the caller should abort the restore without activating the new branch.
//
// wiring is forwarded to newBranchRuntime; pass nil when no effects delegate is configured.
func replayPlanIntoRuntime(ctx context.Context, plan store.ReplayPlan, rtCtx engine.SessionRuntimeContext, delegate engine.VMDelegate, wiring *effectsWiring) (*branchRuntime, json.RawMessage, string, error) {
	rt, configuredState, runtimeHash, err := newBranchRuntime(ctx, rtCtx, delegate, plan.RuntimeConfig, wiring)
	if err != nil {
		return nil, nil, "", err
	}
	for _, step := range plan.Steps {
		if _, err := rt.runContext(ctx, step.Source); err != nil {
			rt.close()
			return nil, nil, "", fmt.Errorf("replay cell %q: %w", step.Cell, err)
		}
	}
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
