// Package engine exposes the top-level contracts for starting and restoring
// repl sessions. No implementation lives in this package; it defines only the
// compile-time boundaries that downstream packages will satisfy.
//
// The Engine interface is the long-lived entry point. The Session interface
// represents one persistent interactive context.
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/dop251/goja"
	"github.com/solidarity-ai/repl/model"
	"github.com/solidarity-ai/repl/store"
)

var runtimeLoopRegistry sync.Map

// BindRuntimeLoop associates a runtime with a loop scheduler so VM delegates
// can resume host promises on the owning loop.
func BindRuntimeLoop(rt *goja.Runtime, runOnLoop func(func(*goja.Runtime))) {
	if rt == nil || runOnLoop == nil {
		return
	}
	runtimeLoopRegistry.Store(rt, runOnLoop)
}

// UnbindRuntimeLoop removes any loop scheduler associated with the runtime.
func UnbindRuntimeLoop(rt *goja.Runtime) {
	if rt == nil {
		return
	}
	runtimeLoopRegistry.Delete(rt)
}

// RunOnRuntimeLoop schedules fn on the runtime's owning loop. It returns false
// if no loop scheduler is registered for the runtime.
func RunOnRuntimeLoop(rt *goja.Runtime, fn func(*goja.Runtime)) bool {
	if rt == nil || fn == nil {
		return false
	}
	v, ok := runtimeLoopRegistry.Load(rt)
	if !ok {
		return false
	}
	v.(func(func(*goja.Runtime)))(fn)
	return true
}

// SubmitResult is returned by Session.Submit after a cell has been checked,
// evaluated, and committed.
type SubmitResult struct {
	// Cell is the ID assigned to the newly committed cell.
	Cell model.CellID

	// Diagnostics lists any TypeScript diagnostics produced during checking.
	Diagnostics []model.Diagnostic

	// HasErrors is true when at least one diagnostic has severity "error".
	HasErrors bool

	// CompletionValue is the runtime completion value, if any.
	CompletionValue *model.ValueRef
}

// ValueView is the result of inspecting a runtime value.
type ValueView struct {
	// Handle is the value being described.
	Handle model.ValueID

	// Preview is a short human-readable representation.
	Preview string

	// TypeHint is an optional TypeScript type annotation.
	TypeHint string

	// Structured carries the versioned bridge encoding of the value when
	// available. Nil for values that can only be previewed as a string.
	Structured []byte
}

// FailureView describes one durable failed submit attempt. Unlike SubmitResult,
// a FailureView does not imply that the cell became part of committed session
// history; it is a debug/inspection artifact only.
type FailureView struct {
	Failure       model.FailureID
	Branch        model.BranchID
	Parent        model.CellID
	Source        string
	RuntimeHash   string
	Phase         string
	ErrorMessage  string
	LinkedEffects []model.EffectID
	At            time.Time
}

type EffectStatus string

const (
	EffectStatusPending   EffectStatus = "pending"
	EffectStatusCompleted EffectStatus = "completed"
	EffectStatusFailed    EffectStatus = "failed"
)

// EffectSummary is a compact view of one host/tool effect started by a cell.
// Embedders can filter or present these however they want.
type EffectSummary struct {
	Effect       model.EffectID
	FunctionName string
	Params       []byte
	Result       []byte
	ErrorMessage string
	ReplayPolicy model.ReplayPolicy
	Status       EffectStatus
}

// SubmitFailure is returned as the error value from Session.Submit when a cell
// fails after evaluation has begun. It carries the durable failure id together
// with the effects started by that cell so embedders can surface them.
type SubmitFailure struct {
	Failure       model.FailureID
	Parent        model.CellID
	Phase         string
	ErrorMessage  string
	LinkedEffects []EffectSummary
	Cause         error
}

func (e *SubmitFailure) Error() string {
	if e == nil {
		return ""
	}
	if e.Phase != "" {
		return fmt.Sprintf("session: Submit: %s: %s", e.Phase, e.ErrorMessage)
	}
	return fmt.Sprintf("session: Submit: %s", e.ErrorMessage)
}

func (e *SubmitFailure) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// SessionDeps bundles the external collaborators needed to start or restore a
// session. Keeping them in a struct avoids a fragile positional argument list
// and makes future additions non-breaking.
// SessionRuntimeContext is passed to the VM delegate each time a fresh VM is
// created. The delegate must treat it as read-only.
type RuntimeMode string

const (
	RuntimeModePersistent      RuntimeMode = "persistent"
	RuntimeModeReplayPerSubmit RuntimeMode = "replay_per_submit"
)

type SessionRuntimeContext struct {
	SessionID   model.SessionID
	BranchID    model.BranchID
	RuntimeHash string
}

// HostFuncInvoke is the Go implementation behind one journaled host call.
// params is the bridge-encoded first argument passed from JS. The returned
// bytes must use the same bridge encoding.
type HostFuncInvoke func(ctx context.Context, params []byte) ([]byte, error)

// HostFuncBuilder wraps Go implementations with the runtime's effect-journaling
// and replay machinery. The delegate chooses where the returned callables are
// installed in the goja object graph.
type HostFuncBuilder interface {
	// WrapSync returns a callable goja binding that executes synchronously from
	// JS while still passing through effect journaling and replay control.
	WrapSync(name string, replay model.ReplayPolicy, invoke HostFuncInvoke) func(goja.FunctionCall) goja.Value

	// WrapAsync returns a callable goja binding that resolves via a JS Promise
	// while still passing through effect journaling and replay control.
	WrapAsync(name string, replay model.ReplayPolicy, invoke HostFuncInvoke) func(goja.FunctionCall) goja.Value
}

type VMDelegate interface {
	// ConfigureRuntime installs any host bindings onto a fresh VM. rt exposes
	// the raw goja surface for arbitrary setup; host wraps Go callbacks so the
	// delegate can place replay-aware bindings anywhere it wants in the JS
	// object graph (top-level globals, nested objects like Math.random, etc.).
	// The input state is the last serialised runtime descriptor persisted for
	// this session. The returned state is what should be persisted for future
	// VM recreation.
	ConfigureRuntime(ctx SessionRuntimeContext, rt *goja.Runtime, host HostFuncBuilder, state json.RawMessage) (json.RawMessage, error)
}

type SessionDeps struct {
	// Store is the persistence boundary for the session.
	Store store.Store
	// VMDelegate configures each freshly created runtime before any replay/eval.
	// Optional; nil keeps legacy behavior.
	VMDelegate VMDelegate
	// RuntimeConfig is optional serialisable delegate state passed to the
	// delegate when the first VM is created for the session.
	RuntimeConfig json.RawMessage
	// RuntimeMode controls whether the session keeps one long-lived runtime or
	// rebuilds from durable history before each submit. Zero value defaults to
	// RuntimeModePersistent.
	RuntimeMode RuntimeMode
}

// Engine is the long-lived entry point for the repl package. Implementations
// manage session lifecycle and coordinate the type service, runtime, effects
// layer, and store.
//
// Engine implementations must be safe for concurrent use by multiple
// goroutines. Each session is independently sequenced at the Session level.
type Engine interface {
	// StartSession creates a new session configured by cfg. The returned
	// Session is ready for its first Submit call.
	StartSession(ctx context.Context, cfg model.SessionConfig, deps SessionDeps) (Session, error)

	// RestoreSession replays history up to targetCell and returns a Session
	// positioned at that cell. Implementations create a new branch when
	// callers intend to continue execution from the restored point.
	RestoreSession(ctx context.Context, sessionID model.SessionID, targetCell model.CellID, deps SessionDeps) (Session, error)
}

// Session represents one persistent interactive context within a repl engine.
//
// Each Session has:
//   - one active branch head
//   - one optional host capability manifest
//   - one live runtime instance
//   - durable history in the backing store
//
// Session implementations serialise evaluation per session; concurrent
// Submit calls on the same Session are not supported.
type Session interface {
	// ID returns the stable session identifier.
	ID() model.SessionID

	// Submit type-checks src, evaluates it in the live runtime, and commits
	// the resulting cell to durable history. The call blocks until the cell
	// and any tracked async work it started have settled.
	Submit(ctx context.Context, src string) (SubmitResult, error)

	// Inspect returns a view of the runtime value identified by handle.
	// The handle must have been obtained from a prior SubmitResult or
	// async settlement.
	Inspect(ctx context.Context, handle model.ValueID) (ValueView, error)

	// Failures returns the durable failed submit attempts for this session in
	// append order. The last element is therefore the most recent failure.
	Failures(ctx context.Context) ([]FailureView, error)

	// Restore positions this session at targetCell by replaying history up to
	// that point. If the session has already committed cells beyond
	// targetCell, Restore forks a new branch. The method blocks until replay
	// is complete.
	Restore(ctx context.Context, targetCell model.CellID) error

	// Close releases all resources associated with the session. Callers must
	// not use the Session after Close returns.
	Close() error
}
