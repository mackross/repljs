# M005 Discussion Notes

## Agreed Scope

### 1. Non-determinism interception

Math.random and Date.now are non-deterministic. During restore, replaying cells that call these will produce different values — breaking deterministic replay.

**Approach:** Intercept Math.random and Date.now at the VMDelegate level. Seed them per-cell (e.g. from the cell ID) so each cell's calls are deterministic on replay. The delegate installs these overrides as part of ConfigureRuntime.

Open question: Should the override be always-on (in session/runtime.go) or opt-in (delegate installs it)? Leaning toward always-on since there's no case where non-deterministic Math.random is desirable in a replayable session. To be decided at planning time.

### 2. Replay policy simplification

Current model has: `ReplayReadonly`, `ReplayIdempotent`, `ReplayNonReplayable`

**Decision:** Remove `ReplayIdempotent`. Collapse to two values:
- `ReplayReplayable` (covers old Readonly + Idempotent) — recorded result reused on restore, no re-invocation
- `ReplayNonReplayable` — restore fails closed before writing any branch history

The idempotency-key mechanism was never wired through the runtime anyway. Embedders who want at-most-once semantics should use NonReplayable and handle the restore failure explicitly.

## Explicitly Out of Scope for M005

- `OnCellFailed` delegate hook — if the delegate wants to know about cell failures, use the store directly. Deferred to M006+ when there's a concrete Toolbox use case.
- Mid-session host reconfiguration driven by failure inspection — same, deferred.
- FailureReplay branch type — not a concept in the repl package.

## Background / Context

### What was built in M004

- **S01:** VMDelegate+HostFuncBuilder seam. Journaled host invocations: EffectStarted, EffectCompleted, EffectFailed facts. Per-cell LinkedEffects + CreatedPromises in CellEvaluated. Race-clean concurrent async settlement.
- **S02:** Restore replay contract. ReplayReadonly → reuse recorded result. ReplayNonReplayable → fail closed before BranchCreated is written. Three contract tests prove the invariants.

### Failure inspection (future, not M005)

When a cell fails after some tool calls, the effect records (EffectStarted/EffectCompleted for completed calls) are durable in the store. A future delegate could query the store for CellFailed records and reconstruct what happened. The repl package fires no callback — the delegate uses the store API directly.

If there were no tool calls (plain throw, syntax error), there's nothing to inspect and the session continues normally.

## M002 — What Still Needs Building (Parked)

M002 is "Manifest + Type Service" — the TypeScript pipeline layer on top of the session engine. It was parked when M003 (runtime) was prioritized. M005 and M006 do not depend on it, but M002 will need to ship before the package is useful to an LLM that writes TypeScript.

### What M002 delivers

- **Declaration assembly:** Given a session-start manifest (host functions) plus embedder-owned top-level inserts (declarations, scripts), produce the TypeScript-visible host surface.
- **Branch-aware incremental typechecking:** Using `store.LoadStaticEnv` to reconstruct prior committed sources on the current branch (including fork ancestry), typecheck a new cell against the accumulated static environment via the typescript-go fork.
- **Structured diagnostics:** On failure, return structured diagnostics useful for LLM correction. Exact field set depends on what typescript-go exposes.
- **JS emit:** On success, emit JavaScript via esbuild. Persisted as part of the committed cell.
- **Integration into session.Submit():** All of the above wired into the real submit flow — not a sidecar helper.

### Open questions carried forward

- What exact diagnostic fields does the typescript-go fork expose reliably?
- Where does durable emitted JS live in the fact model / store? The current store has `CellCommitted` with `Source` (original TS) but no emitted JS field. M002 needs to add it.
- Embedder-owned top-level inserts: blobs? typed shapes? Current direction: separate generic session-start input, embedder owns content/hashing.
- Additive mid-session host-function changes: explicitly deferred beyond M002.

### What's already in place for M002

- `model.Manifest` / `HostFunctionSpec` — machine-readable host surface exists.
- `store.LoadStaticEnv` returns `StaticEnvSnapshot` with manifest + ordered committed sources — the reconstruction seam M002 uses.
- `session/session.go` `StartSession` + `Submit` — M002 integrates here.
- `store/mem` and `store/sqlite` both implement the branch-aware ancestor traversal for static env.

### Why we dropped AtMostOnce / Idempotent

The model tried to capture: "this has a real side effect but is safe to replay with the same key." In practice the idempotency key was never threaded through, and the distinction from Readonly in the runtime was zero. Two policies is sufficient and honest.
