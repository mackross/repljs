package model

import "encoding/json"

// Manifest is an immutable snapshot of the host functions visible to a session.
// It is journaled at session start and consumed by both the type layer (to
// generate TS declarations) and the runtime layer (to inject host objects).
// Existing sessions must not be replayed against a different manifest.
type Manifest struct {
	// ID is a stable identifier or content hash for this manifest.
	ID string `json:"id"`

	// Functions lists the host capabilities available to the session.
	Functions []HostFunctionSpec `json:"functions"`
}

// HostFunctionSpec describes a single host capability at the manifest level.
// It carries only serialisable metadata; the live Call implementation is held
// separately in the engine layer.
type HostFunctionSpec struct {
	// Name is the stable dotted identifier of the host function,
	// e.g. "tools.github.issues.list".
	Name string `json:"name"`

	// ParamsSchema is a JSON Schema (or compatible descriptor) for the
	// function's input parameters.
	ParamsSchema json.RawMessage `json:"params_schema,omitempty"`

	// ResultSchema is a JSON Schema (or compatible descriptor) for the
	// function's return value.
	ResultSchema json.RawMessage `json:"result_schema,omitempty"`

	// Replay controls how the effects layer treats this call during restore.
	Replay ReplayPolicy `json:"replay"`
}

// SessionConfig carries the caller-supplied options for starting a new session.
type SessionConfig struct {
	// Manifest describes the host capabilities available to this session.
	Manifest Manifest `json:"manifest"`
}

// ValueRef is a JSON-safe handle to a live or previously inspected runtime
// value. It carries enough metadata to support preview rendering without
// requiring the live runtime.
type ValueRef struct {
	// ID is the opaque runtime value identifier.
	ID ValueID `json:"id"`

	// Preview is a short human-readable representation (e.g. "42", "[Array]").
	Preview string `json:"preview,omitempty"`

	// TypeHint carries an optional TypeScript type annotation for display.
	TypeHint string `json:"type_hint,omitempty"`
}
