package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/dop251/goja"
	"github.com/dop251/goja_nodejs/eventloop"
	"github.com/google/uuid"
	"github.com/solidarity-ai/repl/engine"
	"github.com/solidarity-ai/repl/model"
	"github.com/solidarity-ai/repl/store"
)

const directRunTimeout = 100 * time.Millisecond

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
func newBranchRuntime(ctx engine.SessionRuntimeContext, delegate engine.VMDelegate, state json.RawMessage) (*branchRuntime, json.RawMessage, string, error) {
	loop := eventloop.NewEventLoop(eventloop.EnableConsole(false))
	loop.Start()

	type initResult struct {
		runtime       *branchRuntime
		configured    json.RawMessage
		runtimeHash   string
		err           error
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
func replayPlanIntoRuntime(ctx context.Context, plan store.ReplayPlan, rtCtx engine.SessionRuntimeContext, delegate engine.VMDelegate) (*branchRuntime, json.RawMessage, string, error) {
	rt, configuredState, runtimeHash, err := newBranchRuntime(rtCtx, delegate, plan.RuntimeConfig)
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
