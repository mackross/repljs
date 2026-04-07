# Persistent TypeScript REPL Package Plan

## Goal

Build an open-source Go package that provides a persistent TypeScript REPL/session engine with these properties:

- incremental TypeScript checking
- a live JavaScript runtime across submissions
- asynchronous host calls that can run in parallel
- durable history and replay
- restore to any committed REPL point
- a host-function model that is generic and not Toolbox-specific

The package should slot cleanly into Toolbox as a replacement for the current `codemode` shape, while leaving [`invoke`](./invoke/invoke.go) as the single-tool execution seam.

Toolbox integration should be an adapter layer, not part of the reusable package API.

## Non-Goals

- owning tool discovery, tool loading, or package assembly
- owning MCP transport details
- persisting arbitrary raw JavaScript heap state as the source of truth
- exact restart of arbitrary in-process timers, sockets, streams, or closures
- embedding Toolbox-specific types in the reusable package

## Design Principles

### 1. The UI can feel like a REPL, but the model is cell-based

Each user submission becomes an immutable cell. A plain mutable REPL process does not provide precise restore semantics, branchability, or replayability.

The user experience remains interactive. The storage and execution model is notebook-like.

### 2. Durable history is authoritative

The journal is the source of truth after restart.

The live runtime is a cache derived from durable history:

- cells
- effects
- promise settlements
- branches
- checkpoints
- manifests

### 3. All side effects must cross an explicit host boundary

If a piece of work matters for replay, restore, or deterministic session history, it must be mediated by a host function and logged as an effect.

### 4. Host capabilities are session-scoped and manifest-driven

The host function surface available to a session is described by an immutable manifest captured at session start.

Type generation and runtime injection both derive from the same manifest.

### 5. Async-first API surface

Host functions should appear in TypeScript as promise-returning functions even if the host implementation currently completes synchronously. That keeps the programming model stable and makes durable effect tracking possible.

## What This Package Replaces In Toolbox

Today, [`codemode`](./codemode/codemode.go) does useful but temporary things:

- synthesizes a TS SDK for the visible tools
- typechecks a one-shot program
- creates a fresh JS runtime
- injects a synchronous tool bridge
- evaluates once and discards everything

That shape is incompatible with:

- persistent runtime state
- parallel async work
- replay and restore
- branches
- durable effect tracking

The replacement package should preserve the good ideas:

- generated typed host surface
- typecheck before run
- delegation of actual tool execution through `invoke`

But it should do so through a session engine rather than a one-shot `Run(prepared, code)` API.

## Core Concepts

### Engine

The long-lived entry point that starts or restores sessions.

### Session

A persistent interactive context with:

- one active branch/head
- one host capability manifest
- one live runtime instance
- durable history in a store

### Cell

One immutable code submission.

A cell records:

- source
- parent cell id
- diagnostics
- emitted JS
- evaluation outcome
- created bindings
- created promises
- linked effects

### Manifest

An immutable snapshot of the host functions visible to the session.

It is used to:

- generate TS declarations
- inject runtime host objects
- preserve replay compatibility

### Effect

A durable record of one host-mediated side effect or async operation.

### Promise

A tracked async value in the runtime. Promise creation and settlement are observable facts because later cells may depend on them.

### Branch

A named or opaque timeline of cells. Restoring to an older point and continuing execution should create a new branch rather than mutating old history.

## Package Boundaries

The open-source package should be generic. A Toolbox adapter should sit on top.

Recommended high-level packages:

- `engine`
- `model`
- `store`
- `types`
- `runtime`
- `effects`
- `restore`
- `manifest`

These can later be grouped differently if package count feels too fine-grained, but the responsibilities should remain separate.

## Public API

The public API should stay small and generic.

```go
package repl

import (
 "context"
 "encoding/json"
)

type Engine interface {
 StartSession(ctx context.Context, cfg SessionConfig) (Session, error)
 RestoreSession(ctx context.Context, sessionID string, target CellID) (Session, error)
}

type Session interface {
 ID() string
 Submit(ctx context.Context, src string) (SubmitResult, error)
 Inspect(ctx context.Context, handle ValueHandle) (ValueView, error)
 Restore(ctx context.Context, target CellID) error
 Close() error
}

type SessionConfig struct {
 Manifest Manifest
 Store    Store
}

type HostFunction struct {
 Name          string
 ParamsSchema  json.RawMessage
 ResultSchema  json.RawMessage
 Replay        ReplayPolicy
 Call          func(context.Context, Invocation) (any, error)
}
```

Possible additions later:

- event subscriptions for async settlement notifications
- checkpoint configuration
- session metadata labels

These should not be required for the first version.

## Session Manifest

The manifest is central to keeping the type system and runtime consistent.

```go
type Manifest struct {
 ID        string
 Functions []HostFunctionSpec
}

type HostFunctionSpec struct {
 Name          string
 ParamsSchema  json.RawMessage
 ResultSchema  json.RawMessage
 Replay        ReplayPolicy
}
```

Important rules:

- the manifest is immutable for a session
- it is journaled at session start
- it has a stable ID or hash
- both the type layer and runtime layer consume the same manifest

If host functions are added or removed in the embedding application, that should produce a new manifest for new sessions. Existing sessions continue to use the old manifest.

This avoids subtle restore failures where a cell was checked against one host surface and replayed against another.

## Host Function Model

Host functions are the reusable abstraction that Toolbox tools will map onto.

They need:

- a stable name such as `tools.github.issues.list`
- JSON-schema-like argument description
- result shape description
- replay policy
- optional idempotency key support
- implementation callback

The reusable package must not know anything about Toolbox tools.

The Toolbox adapter will convert the current visible toolset into a manifest and register one host function per tool. The implementation callback for each such host function will call [`invoke.Run`](./invoke/invoke.go).

### Replay Policies

At minimum:

- `readonly`
- `idempotent`
- `at_most_once`
- `non_replayable`

Semantics:

- `readonly`: safe to rerun, though replay may still prefer recorded prior results
- `idempotent`: safe to rerun if the host implementation can provide a real idempotency key
- `at_most_once`: must not be rerun automatically after uncertainty; replay can only reuse a recorded completed result
- `non_replayable`: restoring across this effect may fail or require a user-visible degraded mode

## TypeScript Surface

The package should generate a virtual TypeScript module from the manifest. The host surface should be async-first.

Example generated declaration:

```ts
declare module "@session/host" {
  export const host: {
    tools: {
      calc: {
        add(args: { a: number; b: number }): Promise<string>
      }
    }
  }
}
```

The exact namespace is flexible. The important part is:

- generated from manifest, not handwritten
- imported or injected consistently
- promise-returning host functions
- stable across cells in one session

The type service should own:

- virtual files
- generated declaration module(s)
- incremental TS program/check state
- diagnostics
- JS emit

The type service should not:

- execute code
- call host functions
- see live runtime values

## Runtime Model

The first implementation should use `goja`.

The runtime owns:

- the live JS realm
- bindings and globals
- runtime value handles
- promise bridges
- microtask execution

The runtime must not own:

- persistence
- replay policy decisions
- direct external I/O
- branch logic

The runtime should expose host objects derived from the manifest. Internally, these host objects should call back into the effect layer, not directly into persistence or application-specific logic.

### Runtime Constraints

- evaluation is serialized per session head
- top-level `await` is supported
- cells that only start async work can commit before that work settles
- later promise settlement events can update the live runtime and journal

## Persistence Model

Persist facts, not commands.

Recommended fact types:

- `SessionStarted`
- `ManifestAttached`
- `CellChecked`
- `CellEvaluated`
- `CellCommitted`
- `EffectStarted`
- `EffectCompleted`
- `EffectFailed`
- `PromiseSettled`
- `HeadMoved`
- `BranchCreated`
- `RestoreCompleted`
- `CheckpointSaved`

The store should be append-oriented. SQLite is a strong default for a first implementation because it offers:

- durable local persistence
- simple indexing
- transactional head updates
- straightforward debugging

The reusable package should define a `Store` interface and ship one reference SQLite implementation if that keeps adoption easy.

## Core Domain Types

```go
type SessionID string
type BranchID string
type CellID string
type EffectID string
type PromiseID string
type ValueID string
type CheckpointID string

type ValueRef struct {
 ID       ValueID
 Preview  string
 TypeHint string
}

type PromiseRef struct {
 ID    PromiseID
 State PromiseState
}

type PromiseState string

const (
 PromisePending   PromiseState = "pending"
 PromiseFulfilled PromiseState = "fulfilled"
 PromiseRejected  PromiseState = "rejected"
)
```

These IDs should be opaque and stable. Fact payloads should be JSON-safe and not depend on runtime pointers.

## Message Model

The package does not need a heavyweight bus, but it should be message-shaped.

Use only:

- intents
- facts
- queries

### Intents

- `SubmitCell`
- `CheckCell`
- `EvalCell`
- `InvokeEffect`
- `RestoreToCell`

### Facts

- `CellChecked`
- `CellEvaluated`
- `EffectStarted`
- `EffectCompleted`
- `EffectFailed`
- `PromiseSettled`
- `CellCommitted`
- `HeadMoved`
- `RestoreCompleted`

### Queries

- `LoadHead`
- `LoadStaticEnv`
- `LoadReplayPlan`
- `InspectValue`

The store should persist facts only. Intents are orchestration-level constructs.

## Services and Responsibilities

### Coordinator

The coordinator owns sequencing:

- submit cell
- append facts
- advance head
- restore session

It is the only component allowed to decide the order of cross-component operations.

### Types

Owns:

- static environment reconstruction
- diagnostics
- incremental program state
- JS emission

### Runtime

Owns:

- evaluation
- live values
- promise bridge table
- value inspection

### Effects

Owns:

- invocation of host functions
- replay semantics
- mapping effect completion back to promise settlement

### Store

Owns:

- durable facts
- session metadata
- branch graph
- replay plan lookup
- checkpoints

### Restore

Owns:

- replay planning
- checkpoint selection
- effect replay decisions
- runtime rehydration

## Clean Interfaces

Suggested interfaces:

```go
type Types interface {
 Check(CheckCell) (CellChecked, error)
}

type Runtime interface {
 Eval(EvalCell) (CellEvaluated, error)
 SettlePromise(session SessionID, promise PromiseID, state PromiseState, value any, err ErrorValue) error
 Inspect(session SessionID, handle ValueID) (ValueView, error)
 Reset(session SessionID) error
}

type Effects interface {
 Start(InvokeEffect) (EffectStarted, error)
 DecideReplay(session SessionID, effect EffectID) (ReplayDecision, error)
}

type Store interface {
 AppendFact(ctx context.Context, fact any) error
 LoadHead(ctx context.Context, session SessionID, branch BranchID) (CellID, error)
 LoadStaticEnv(ctx context.Context, session SessionID, branch BranchID, head CellID) (StaticEnvSnapshot, error)
 LoadReplayPlan(ctx context.Context, session SessionID, target CellID) (ReplayPlan, error)
}
```

These interfaces should stay narrow. If they start exposing implementation details, the boundary is drifting.

## Submit Flow

One session submission should follow this shape:

1. coordinator allocates a new `CellID`
2. coordinator loads the current head and static environment
3. coordinator sends `CheckCell` to the type service
4. `CellChecked` is appended
5. if runnable, coordinator sends `EvalCell` to the runtime
6. `CellEvaluated` is appended
7. `CellCommitted` is appended
8. `HeadMoved` is appended

Important behavior:

- if the cell uses top-level `await`, the submission waits for that awaited expression
- if the cell starts async work without awaiting it, the cell can still commit
- any later effect completions are appended as their own facts

## Async Effect Flow

This is the second key path:

1. runtime code calls a host function
2. runtime delegates to effects via `InvokeEffect`
3. `EffectStarted` is appended
4. host implementation runs
5. on completion, append `EffectCompleted` or `EffectFailed`
6. convert that into `PromiseSettled`
7. push settlement back into the runtime promise bridge

This is how parallel async work becomes durable.

## Restore Flow

Restore should be a first-class workflow, not an afterthought.

1. request restore to a target cell
2. load the replay plan from the store
3. initialize or reset runtime
4. optionally load checkpoint
5. replay committed cells in order
6. for each effect encountered during replay, ask effects for a replay decision
7. reuse prior result, reattach, rerun, or fail according to policy
8. append `RestoreCompleted`
9. move the session head or create a new branch head depending on chosen semantics

### Recommended restore semantics

The simplest and safest model:

- restore positions the session at a prior committed cell
- new submissions from that point create a new branch
- old history remains immutable

## Checkpoints

Checkpoints should be optional in the initial implementation.

The first milestone should replay from durable history correctly.

Only after replay is reliable should checkpoints be added to reduce restore time.

Possible checkpoint contents:

- runtime snapshot metadata
- resolved value cache
- manifest reference
- last replayed cell id

Avoid pretending raw runtime memory is durable truth. Checkpoints are a performance optimization, not the core model.

## Toolbox Adapter

Toolbox should use a thin adapter package on top of the reusable engine.

Responsibilities of the adapter:

- read the visible tool surface from `PreparedToolset.AgentView()`
- generate one host function spec per visible tool
- create a manifest
- implement each host function by calling [`invoke.Run`](./invoke/invoke.go)
- expose MCP tools for session operations

The reusable package must not import:

- `toolset`
- `invoke`
- MCP libraries

The adapter owns those dependencies.

## How Toolbox Would Map Onto Host Functions

Each visible tool becomes a host function:

- name: tool name
- params schema: derived from the visible tool params
- result schema: start with opaque string if necessary
- replay policy: likely `at_most_once` or `readonly` depending on tool classification
- implementation: call `invoke.Run(prepared, toolName, args)`

This keeps Toolbox execution semantics in one place: `invoke`.

## Why Host Functions Should Be Async In JS

Even if `invoke.Run` returns synchronously today, exposing it as synchronous in the runtime would be a mistake.

Reasons:

- it bakes in the wrong mental model
- it prevents later durable async orchestration
- it makes parallel host work harder
- it forces a breaking API change later

The JS/TS surface should be:

```ts
const result = await host.tools.calc.add({ a: 1, b: 2 })
```

not:

```ts
const result = host.tools.calc.add({ a: 1, b: 2 })
```

## Proposed MCP Surface In Toolbox

The current `codemodemcp` stub should eventually be replaced with session-oriented tools such as:

- `repl_session_start`
- `repl_cell_submit`
- `repl_session_restore`
- `repl_value_inspect`
- `repl_session_close`

These should live in Toolbox, not in the reusable package.

The reusable package should expose a Go API only.

## Failure Modes To Design For

### 1. Effect finished remotely but process crashed before result was recorded

Need:

- idempotency keys where possible
- replay decisions that can mark uncertainty
- `at_most_once` semantics that refuse dangerous reruns

### 2. Host surface changes between session creation and restore

Need:

- immutable manifest per session
- restore against the original manifest

### 3. Promise was pending at crash time

Need:

- durable effect link where promise originated from a host call
- clear policy for reattach, rerun, reuse, or fail

### 4. Non-host async work in the JS runtime

Need:

- explicit guidance that durable behavior only applies to host-mediated async
- optional degraded behavior for in-memory-only async

### 5. Two submissions race the same session head

Need:

- serialized head advancement per session
- optimistic check using `ExpectedHead`

## Minimal Viable Version

The smallest useful first release should provide:

- session start
- submit cell
- persistent runtime per session
- generated TS host declarations
- async host calls
- append-only fact store
- restore by replay
- value inspection

It does not need initially:

- checkpointing
- multi-process coordination
- distributed runtime backends
- perfect replay of non-host async behavior

## Recommended Build Order

### Phase 1: Model and Store

Build:

- ids, refs, facts, replay policy enums
- append-only store
- session head and branch tracking

Exit criteria:

- facts can be persisted and replay plans can be queried

### Phase 2: Manifest and Type Service

Build:

- manifest format
- generated TypeScript declarations
- incremental typechecking and JS emit

Exit criteria:

- one session can typecheck successive cells against the same manifest

### Phase 3: Persistent Runtime

Build:

- `goja` runtime host
- value handles
- promise tracking
- top-level `await`

Exit criteria:

- successive cells share live bindings and promise references

### Phase 4: Effect Layer

Build:

- host function invocation path
- effect facts
- promise settlement feedback into runtime

Exit criteria:

- host calls become durable effects and can resolve promises later

### Phase 5: Restore

Build:

- replay planner
- runtime reset and replay
- replay decision handling

Exit criteria:

- a session can be restored to any committed cell by replay

### Phase 6: Toolbox Adapter

Build:

- adapter from `PreparedToolset` to manifest
- host function implementations using `invoke.Run`
- MCP session tools
- removal of the old `codemode`

Exit criteria:

- Toolbox uses the reusable package instead of `codemode`

## Open Questions

These should be resolved early because they influence the API:

- Should manifest changes within one long-lived logical session create a new branch or require a new session?
- Should the package ship only interfaces, or also a default SQLite store and `goja` runtime implementation?
- Should value inspection be best-effort stringification only at first, or structured graph exploration?
- Should restore always create a new branch, or optionally move the active branch head backwards?
- How should non-replayable effects be surfaced to the caller during restore?

## Recommended Defaults

If decisions are needed quickly, these defaults are pragmatic:

- immutable manifest per session
- one-process session manager initially
- `goja` runtime first
- SQLite store shipped in-package
- async-only host function surface
- replay by history first, checkpoints later
- restore creates a new branch when execution continues from an earlier cell

## Summary

What needs to be built is not a better one-shot code runner. It is a small durable programming environment with strict component boundaries:

- typechecking is static and incremental
- runtime state is live but disposable
- effects are explicit and journaled
- restore works from durable facts
- host capabilities are manifest-driven
- embedding applications adapt their own capabilities into host functions

In Toolbox, that means:

- `invoke` remains the single execution seam for real tool calls
- `codemode` is deleted
- a new session engine becomes the persistent code execution layer
- Toolbox contributes only an adapter and MCP surface, not the generic runtime core

Note: ~/dev/toolbox uses ~/dev/vendor/typescript-go/toolbox to do type checking. You can use that same forked version of typescript-go (see the go.mod import) to do typescript checking too.
