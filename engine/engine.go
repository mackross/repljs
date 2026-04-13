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
	"strings"
	"sync"
	"time"

	"github.com/dop251/goja"
	"github.com/mackross/repljs/model"
	"github.com/mackross/repljs/store"
	"github.com/mackross/repljs/typescript"
)

var runtimeLoopRegistry sync.Map
var indexedValueMetadataRegistry sync.Map

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

type IndexedValueMetadata struct {
	StaleMessage string
}

func SetIndexedValueMetadata(value goja.Value, meta IndexedValueMetadata) {
	obj, ok := value.(*goja.Object)
	if !ok || obj == nil {
		return
	}
	if meta == (IndexedValueMetadata{}) {
		indexedValueMetadataRegistry.Delete(obj)
		return
	}
	indexedValueMetadataRegistry.Store(obj, meta)
}

func IndexedValueMetadataFor(value goja.Value) (IndexedValueMetadata, bool) {
	obj, ok := value.(*goja.Object)
	if !ok || obj == nil {
		return IndexedValueMetadata{}, false
	}
	meta, ok := indexedValueMetadataRegistry.Load(obj)
	if !ok {
		return IndexedValueMetadata{}, false
	}
	typed, ok := meta.(IndexedValueMetadata)
	if !ok {
		return IndexedValueMetadata{}, false
	}
	return typed, true
}

type SubmitInput struct {
	Source   string
	Language model.CellLanguage
}

type ReplWarning struct {
	Code    string
	Message string
}

// SubmitResult is returned by Session.Submit after a cell has been checked,
// evaluated, and committed.
type SubmitResult struct {
	// Cell is the ID assigned to the newly committed cell.
	Cell model.CellID

	// Index is the branch-local monotonic cell number for this committed cell.
	Index int

	// Language is the source language used for this cell.
	Language model.CellLanguage

	// Diagnostics lists any TypeScript diagnostics produced during checking.
	Diagnostics []model.Diagnostic

	// HasErrors is true when at least one diagnostic has severity "error".
	HasErrors bool

	// CompletionValue is the runtime completion value, if any.
	CompletionValue *model.ValueRef

	// Log captures console.log lines produced during this submit.
	Log []string

	// Warnings reports non-fatal session-level events that affected how the
	// submit was handled, such as a TypeScript static-context reset caused by an
	// env epoch change.
	Warnings []ReplWarning
}

// ValueView is the result of inspecting a runtime value.
type ValueView struct {
	// Handle is the value being described.
	Handle model.ValueID

	// Preview is a short human-readable representation.
	Preview string

	// Summary is a compact shape-first rendering intended for low-token
	// inspection by embedders and LLMs.
	Summary string

	// Full is a more complete rendering of the value with smarter truncation
	// than Preview while still remaining bounded.
	Full string

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
	Log           []string
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

// SubmitCheckFailure is returned as the error value from Submit/SubmitCell
// when a cell fails during pre-execution checking, such as TypeScript
// diagnostics with severity error.
type SubmitCheckFailure struct {
	Cell        model.CellID
	Index       int
	Language    model.CellLanguage
	Diagnostics []model.Diagnostic
}

func (e *SubmitCheckFailure) Error() string {
	if e == nil {
		return ""
	}
	if len(e.Diagnostics) == 0 {
		return "TS Err:"
	}
	parts := make([]string, 0, len(e.Diagnostics))
	for _, diagnostic := range e.Diagnostics {
		prefix := ""
		if diagnostic.Line > 0 {
			if diagnostic.Column > 0 {
				prefix = fmt.Sprintf("%d:%d: ", diagnostic.Line, diagnostic.Column)
			} else {
				prefix = fmt.Sprintf("%d: ", diagnostic.Line)
			}
		}
		if diagnostic.Severity != "" {
			prefix += diagnostic.Severity + ": "
		}
		parts = append(parts, prefix+diagnostic.Message)
	}
	return "TS Err:\n" + strings.Join(parts, "\n")
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
	SessionID model.SessionID
	BranchID  model.BranchID
	// RuntimeHash is the descriptor hash for the runtime being configured.
	RuntimeHash string
	// IsCurrentRuntime reports whether this lifecycle event's runtime is still
	// the live runtime for the VM. Wrappers must capture and call it later:
	//   isCurrent := ctx.IsCurrentRuntime
	//   wrapper := func(...) { if !isCurrent() { /* stale */ } }
	IsCurrentRuntime func() bool
}

type RuntimeTransitionContext struct {
	SessionID       model.SessionID
	BranchID        model.BranchID
	FromRuntimeHash string
	// ToRuntimeHash is the descriptor hash being applied by this transition.
	ToRuntimeHash string
	// IsCurrentRuntime reports whether this transition's target runtime is still
	// the live runtime for the VM. Wrappers must capture and call it later:
	//   isCurrent := ctx.IsCurrentRuntime
	//   wrapper := func(...) { if !isCurrent() { /* stale */ } }
	IsCurrentRuntime func() bool
}

type TypeScriptEnvContext struct {
	SessionID model.SessionID
	BranchID  model.BranchID
	Head      model.CellID
}

type TypeScriptEnvProvider func(ctx context.Context, tsCtx TypeScriptEnvContext) (typescript.Env, error)

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

type VMTransitionDelegate interface {
	// TransitionRuntime mutates an already-live VM from fromState to toState.
	// Unlike ConfigureRuntime, the runtime is not fresh; implementers must add,
	// update, and remove bindings as needed without assuming a clean VM.
	TransitionRuntime(ctx RuntimeTransitionContext, rt *goja.Runtime, host HostFuncBuilder, fromState, toState json.RawMessage) error
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
	// TypeScriptFactory lazily creates a TypeScript check+emit session for TS
	// cells. Optional; nil disables TS mode.
	TypeScriptFactory typescript.Factory
	// TypeScriptEnvProvider returns the declarations/prelude environment for
	// the next TS cell. Optional; nil means an empty environment.
	TypeScriptEnvProvider TypeScriptEnvProvider
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

	// OpenSession reopens an existing durable session at its current active
	// branch/head cursor.
	OpenSession(ctx context.Context, sessionID model.SessionID, deps SessionDeps) (Session, error)

	// RestoreSession replays history up to targetCell and returns a Session
	// positioned at that existing committed cell on its owning branch.
	RestoreSession(ctx context.Context, sessionID model.SessionID, targetCell model.CellID, deps SessionDeps) (Session, error)

	// ForkSession replays history up to targetCell and returns a Session on a
	// newly created branch rooted at that cell.
	ForkSession(ctx context.Context, sessionID model.SessionID, targetCell model.CellID, deps SessionDeps) (Session, error)
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

	// Submit evaluates a JavaScript cell in the live runtime and commits the
	// resulting cell to durable history. The call blocks until the cell and any
	// tracked async work it started have settled.
	Submit(ctx context.Context, src string) (SubmitResult, error)

	// SubmitCell is the structured submit API. It allows callers to choose the
	// per-cell source language instead of defaulting to JavaScript.
	SubmitCell(ctx context.Context, input SubmitInput) (SubmitResult, error)

	// Inspect returns a view of the runtime value identified by handle.
	// The handle must have been obtained from a prior SubmitResult or
	// async settlement.
	Inspect(ctx context.Context, handle model.ValueID) (ValueView, error)

	// Logs replays targetCell in a scratch runtime and returns the console.log
	// lines it produces. Logs are not stored durably.
	Logs(ctx context.Context, targetCell model.CellID) ([]string, error)

	// Failures returns the durable failed submit attempts for this session in
	// append order. The last element is therefore the most recent failure.
	Failures(ctx context.Context) ([]FailureView, error)

	// Restore positions this session at targetCell by replaying history up to
	// that existing committed cell on its owning branch. The method blocks
	// until replay is complete.
	Restore(ctx context.Context, targetCell model.CellID) error

	// Fork creates a new branch rooted at targetCell, replays history into a
	// fresh runtime, and makes the new branch active.
	Fork(ctx context.Context, targetCell model.CellID) error

	// TransitionToState mutates the current runtime to the requested
	// embedder-defined state and records the transition durably at the current
	// branch/head boundary.
	TransitionToState(ctx context.Context, toState json.RawMessage) error

	// Close releases all resources associated with the session. Callers must
	// not use the Session after Close returns.
	Close() error
}
