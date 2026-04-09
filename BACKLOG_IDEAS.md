# Backlog Ideas

## ReplayFailure()

### Why this exists

We now treat failed submits as **durable debug artifacts** rather than normal committed cells:

- failed cells do **not** advance branch history
- failed cells do **not** leave runtime state visible to later normal cells
- failed cells **are** recorded durably via `CellFailed`
- callers can inspect them through `Session.Failures()`

That solves normal session correctness, but it leaves one important toolbox/debugging gap:

> after a failed cell, the agent should be able to re-run that exact failed attempt in isolation, potentially with a **different set of registered host functions / tool bindings**, without contaminating the normal session runtime.

This matters especially for toolbox-style usage:

- a tool call may fail because the registered tool set was wrong, incomplete, or too permissive
- the agent may want to inspect the failure, then replay the failed cell with a safer or different host registry
- we do **not** want the failed cell to remain ambiently live in the main session, because that breaks replay/persistent parity and normal history semantics

So the desired model is:

1. **Committed cells** shape normal session state
2. **Failed attempts** are durable debug records
3. **ReplayFailure()** runs one failed attempt in a fresh transient sandbox

### What ReplayFailure() should do

Add a session-level API that:

- takes a previously recorded failed attempt
- rebuilds a fresh runtime from the failed attempt's `Parent` committed head
- replays the failed attempt's original `Source`
- allows an alternate runtime/tool configuration for the replay
- returns debug output from that replay
- does **not** mutate the real session head or branch
- does **not** make replayed debug state visible to later normal submits unless there is an explicit future “promote” flow

### Core invariants

ReplayFailure() must preserve these rules:

- normal `Submit()` semantics stay unchanged
- failed cells are still **not** part of committed execution history
- calling ReplayFailure() must **not** alter:
  - `s.head`
  - `s.branch`
  - committed facts/history
  - the normal live runtime used for future submits
- ReplayFailure() is a **debug sandbox**, not a history mutation

### Expected API shape

Exact signature is still open, but roughly:

```go
type FailureReplayOptions struct {
    // Optional alternate runtime/tool registration.
    VMDelegate engine.VMDelegate

    // Optional alternate serialized runtime config.
    RuntimeConfig json.RawMessage
}

type FailureReplayResult struct {
    Failure       model.FailureID
    Completion    *engine.ValueView
    ErrorMessage  string
    LinkedEffects []model.EffectID
    // Possibly more debug fields later.
}

ReplayFailure(ctx context.Context, failureID model.FailureID, opts FailureReplayOptions) (FailureReplayResult, error)
```

Could live on `engine.Session` once stable.

### Minimum implementation plan

1. **Lookup failure record**
   - resolve `failureID` from durable `CellFailed` facts
   - retrieve:
     - `Source`
     - `Parent`
     - `RuntimeHash`
     - `Phase`
     - `LinkedEffects`

2. **Build transient runtime**
   - if `Parent == ""`, start from a fresh configured runtime
   - otherwise load replay plan for `Parent` and replay committed history into a fresh runtime
   - use alternate `VMDelegate` / `RuntimeConfig` when provided, otherwise default to the session’s current ones

3. **Run failed source in isolation**
   - evaluate `Source` in that transient runtime
   - capture either completion or error
   - do not append new normal history facts to the real session branch

4. **Return debug result**
   - include error/completion from the sandbox replay
   - optionally include any effects triggered during the replay sandbox

5. **Tear down sandbox**
   - close the transient runtime after the call returns

### Open design questions

#### 1. Should replayed failures write durable facts?

Probably **not** to the normal append-only session history.

If we need auditability later, add a separate debug artifact/fact class like:

- `FailureReplayStarted`
- `FailureReplayCompleted`
- `FailureReplayFailed`

But do **not** mix those into committed cell history.

#### 2. What host policies should be supported?

We likely want replay modes like:

- deny all tools
- use the session’s normal delegate
- use a caller-supplied alternate delegate
- maybe stub tools / readonly-only tools

This is the main reason ReplayFailure() exists rather than just “treat errors like committed cells”.

#### 3. Should replayed effects be allowed to fire?

Default should probably be conservative.

At minimum, the API should make it possible to replay with a different delegate that:

- blocks effectful tools
- stubs missing tools
- replaces dangerous tools with readonly/debug variants

#### 4. Should we expose thrown values structurally?

Current failure records store `ErrorMessage` only.

ReplayFailure() may be the right place to later return richer error information:

- preview of thrown JS value
- structured JSON when serializable
- host error classification

### Why not just treat failed cells as committed cells?

Because that would blur two incompatible concepts:

- “this attempt is worth remembering”
- “this attempt is part of future runtime state”

We want the first, not the second.

Treating failures like normal committed cells would create ambiguity around:

- replay semantics
- branch history
- top-level bindings from failed code
- partially triggered host effects
- parity between persistent and replay-per-submit modes

The current direction is intentionally cleaner:

- failed attempts are **remembered**
- failed attempts are **inspectable**
- failed attempts are **replayable in isolation**
- failed attempts do **not** shape normal future execution

### Acceptance checks for future implementation

When this is implemented, verify at least:

1. replaying a failed syntax/runtime/tool cell does not change session head
2. replaying a failed cell with an alternate delegate can succeed even if the original failed
3. replaying a failed cell does not leak bindings into later normal submits
4. replaying a failed cell can be done repeatedly from the same failure record
5. `Failures()` still returns the original durable failure history unchanged after replay
6. persistent mode and replay-per-submit mode behave the same after normal failures, regardless of any later ReplayFailure() calls
