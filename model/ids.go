// Package model defines the core domain types for the repl engine.
// All IDs are opaque string aliases; callers should treat them as black-box
// handles and must not parse their internal format.
package model

// SessionID identifies a persistent interactive session.
type SessionID string

// BranchID identifies a named or opaque branch within a session.
type BranchID string

// CellID identifies one immutable code submission within a session.
type CellID string

// EffectID identifies a single durable host-mediated side-effect record.
type EffectID string

// PromiseID identifies a tracked async value in the runtime.
type PromiseID string

// ValueID identifies a live or inspectable runtime value.
type ValueID string

// CheckpointID identifies a persisted runtime snapshot.
type CheckpointID string
