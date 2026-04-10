# repljs

`repljs` is a durable, branchable JavaScript/TypeScript REPL built around:

- durable session history
- restore and fork from committed cells
- replay-aware host effects
- structured value transport with `jswire`

## REPL Details And Quirks

This section is the "non-obvious behavior" list: semantics that are easy to
miss, choices that are intentional but unusual, and notation invented by this
project.

### Sessions, restore, and fork

- `OpenSession(...)` reopens an existing durable session at its current active cursor.
- `Restore(...)` does not fork. It moves the active session cursor to an existing committed cell on the same branch and rebuilds runtime state there.
- `Fork(...)` is the explicit branch-creating operation. If you want a new branch rooted at an earlier cell, use `Fork`, not `Restore`.
- Value handles are branch/runtime-local. A handle that was valid before `Restore` or `Fork` may become stale afterwards.

### Submit semantics

- `Submit(...)` blocks until the cell result and tracked async work created by that cell have settled.
- Failed submit returns a typed `*engine.SubmitFailure` with linked effect summaries for the effects started by that cell.
- Successful submit returns a `CompletionValue` handle, and callers can use `Inspect(...)` to obtain richer renderings.

### Non-determinism

- `Date` / `Date.now()` / `Date()` are replay-aware through the Goja time source hook.
- `Math.random()` is replay-aware through the Goja random source hook.
- Timer globals are intentionally unavailable:
  - `setTimeout`
  - `setInterval`
  - `clearTimeout`
  - `clearInterval`
- The event loop still exists for promises and async host callbacks. Only the timer globals are disabled.

### Top-level REPL input

- One-line REPL input treats a top-level object literal like an expression. For example, entering `{ a: "hello" }` is rewritten so it evaluates as an object literal instead of a block statement with a labeled expression.
- Top-level `await` is also rewritten for REPL convenience.
- Top-level-await support is not full JavaScript module semantics. The runtime uses a wrapper transform with some restrictions. See tests around top-level await for the current edge cases.

### Inspection output

- `Preview` is still the raw Goja string coercion of the completion value.
  - Example: a plain object preview is usually `[object Object]`.
- `Summary` is the low-token, shape-first rendering intended for embedders and LLMs.
- `Full` is a richer but still bounded rendering.
- `Summary` and `Full` are rendered from the durable `jswire` payload, not from a live VM walk.

### Inspection notation invented here

- Shared references and cycles use:
  - `&N` for the first definition of a shared value
  - `*N` for a later reference back to that same value
- Example:

```txt
&1 {self: *1}
```

This is project-specific notation inspired by YAML/Lisp-style shared-structure markers; it is not standard JavaScript syntax.

### Inspector truncation rules

- Long strings are truncated in `Summary` and shown as `string(N) "prefix…"` where `N` is the original rune length.
- Arrays, objects, maps, sets, typed arrays, and buffers show a bounded sample plus an omission count like `…+3`.
- `Full` uses larger budgets than `Summary`, but it is still intentionally bounded.

### Property order

- Structured inspection currently renders object properties in deterministic sorted order.
- This means inspection output may not preserve original insertion order for plain object properties.
- The main reason is stable, comparable output for tests and embedders.

### Host effects

- Host calls cross the effect layer and are journaled durably.
- Failed submit exposes linked effects directly on the returned error so embedders can decide what to show the LLM.
- Effect params/results are also encoded through `jswire`, so built-in JS types can survive host crossings better than plain JSON.

### `jswire`

- `jswire` is the project-specific wire format for moving JS values between runtimes and storing inspectable structured values.
- It preserves more than JSON, including built-in JS structures such as:
  - `Date`
  - `RegExp`
  - `Map`
  - `Set`
  - `ArrayBuffer`
  - typed arrays
  - shared refs / cycles
- Functions, promises, symbols, and other unsupported runtime values do not round-trip through `jswire`.

### Testing note

- Some exact inspector-output tests are intentionally "reviewed shape" tests. They exist to pin human-reviewed formatting, not just semantic equivalence.
