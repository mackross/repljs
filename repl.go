// Package repl exposes the embedder-facing surface for the durable
// JavaScript/TypeScript REPL.
//
// Downstream code should prefer this package over importing engine/model/store
// subpackages directly unless it needs implementation-specific details such as
// a concrete store backend.
package repl

import (
	"github.com/dop251/goja"
	"github.com/mackross/repljs/engine"
	"github.com/mackross/repljs/model"
	"github.com/mackross/repljs/session"
	"github.com/mackross/repljs/store"
	"github.com/mackross/repljs/typescript"
)

type Engine = engine.Engine
type Session = engine.Session

type SubmitInput = engine.SubmitInput
type SubmitResult = engine.SubmitResult
type ReplWarning = engine.ReplWarning
type ValueView = engine.ValueView
type FailureView = engine.FailureView

type EffectStatus = engine.EffectStatus

const (
	EffectStatusPending   = engine.EffectStatusPending
	EffectStatusCompleted = engine.EffectStatusCompleted
	EffectStatusFailed    = engine.EffectStatusFailed
)

type EffectSummary = engine.EffectSummary
type SubmitFailure = engine.SubmitFailure
type SubmitCheckFailure = engine.SubmitCheckFailure

type RuntimeMode = engine.RuntimeMode

const (
	RuntimeModePersistent      = engine.RuntimeModePersistent
	RuntimeModeReplayPerSubmit = engine.RuntimeModeReplayPerSubmit
)

type SessionRuntimeContext = engine.SessionRuntimeContext
type RuntimeTransitionContext = engine.RuntimeTransitionContext
type TypeScriptEnvContext = engine.TypeScriptEnvContext
type TypeScriptEnvProvider = engine.TypeScriptEnvProvider
type HostFuncInvoke = engine.HostFuncInvoke
type HostFuncBuilder = engine.HostFuncBuilder
type VMDelegate = engine.VMDelegate
type VMTransitionDelegate = engine.VMTransitionDelegate
type SessionDeps = engine.SessionDeps

type Store = store.Store
type ReplayStep = store.ReplayStep
type ReplayDecision = store.ReplayDecision
type ReplayPlan = store.ReplayPlan
type HeadRecord = store.HeadRecord
type SessionState = store.SessionState
type StaticEnvSnapshot = store.StaticEnvSnapshot

type SessionID = model.SessionID
type BranchID = model.BranchID
type CellID = model.CellID
type EffectID = model.EffectID
type PromiseID = model.PromiseID
type ValueID = model.ValueID
type CheckpointID = model.CheckpointID
type FailureID = model.FailureID

type ReplayPolicy = model.ReplayPolicy

const (
	ReplayReadonly      = model.ReplayReadonly
	ReplayIdempotent    = model.ReplayIdempotent
	ReplayNonReplayable = model.ReplayNonReplayable
)

type PromiseState = model.PromiseState

const (
	PromisePending   = model.PromisePending
	PromiseFulfilled = model.PromiseFulfilled
	PromiseRejected  = model.PromiseRejected
)

type CellLanguage = model.CellLanguage

const (
	CellLanguageJavaScript = model.CellLanguageJavaScript
	CellLanguageTypeScript = model.CellLanguageTypeScript
)

type Manifest = model.Manifest
type HostFunctionSpec = model.HostFunctionSpec
type SessionConfig = model.SessionConfig
type ValueRef = model.ValueRef
type Diagnostic = model.Diagnostic

type TypeScriptEnv = typescript.Env
type TypeScriptResult = typescript.Result
type TypeScriptSession = typescript.Session
type TypeScriptFactory = typescript.Factory

// New returns the default engine implementation.
func New() Engine {
	return session.New()
}

// NewTypeScriptFactory returns the default incremental TypeScript checker.
func NewTypeScriptFactory() TypeScriptFactory {
	return typescript.NewFactory()
}

// BindRuntimeLoop associates a runtime with a loop scheduler so VM delegates
// can safely resume host promises on the owning runtime loop.
func BindRuntimeLoop(rt *goja.Runtime, runOnLoop func(func(*goja.Runtime))) {
	engine.BindRuntimeLoop(rt, runOnLoop)
}

// UnbindRuntimeLoop removes any loop scheduler associated with the runtime.
func UnbindRuntimeLoop(rt *goja.Runtime) {
	engine.UnbindRuntimeLoop(rt)
}

// RunOnRuntimeLoop schedules fn on the runtime's owning loop.
func RunOnRuntimeLoop(rt *goja.Runtime, fn func(*goja.Runtime)) bool {
	return engine.RunOnRuntimeLoop(rt, fn)
}
