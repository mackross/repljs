// Package store defines the persistence boundary for the repl engine.
// It exposes append-oriented interfaces and query result types that allow
// downstream implementations (e.g. SQLite) to back the engine without leaking
// implementation details into the core domain.
//
// All methods accept a context.Context so that callers can propagate
// cancellation and deadlines across the persistence boundary.
package store

import (
	"context"
	"encoding/json"

	"github.com/solidarity-ai/repl/model"
)

// ReplayStep describes one committed cell that must be replayed in order when
// reconstructing a session from history.
type ReplayStep struct {
	// Cell is the identifier of the committed cell.
	Cell model.CellID

	// Source is the original TypeScript source for the cell.
	Source string

	// EmittedJS is the JavaScript that was emitted by the type checker for
	// this cell, if available. The restore path may re-emit if absent.
	EmittedJS string

	// Effects lists the effect IDs that were started during this cell's
	// evaluation, in the order they were appended.
	Effects []model.EffectID
}

// ReplayDecision describes how the effects layer should treat one historical
// host-function call during restore.
type ReplayDecision struct {
	// Effect is the effect being decided upon.
	Effect model.EffectID

	// Policy is the replay policy recorded at call time.
	Policy model.ReplayPolicy

	// RecordedResult holds the serialised completed result, if any. A nil
	// value means the effect did not complete before the prior process ended.
	RecordedResult []byte

	// CompletionOrder is the append-order index of the terminal success row for
	// this effect. Replay uses it to preserve settlement order for concurrent
	// async effects within a replayed cell.
	CompletionOrder int

	// Rerun, when true, instructs the effects layer to re-invoke the host
	// function rather than reusing a recorded result.
	Rerun bool
}

// ReplayPlan is the ordered sequence of steps needed to rebuild a session up
// to a target cell, together with per-effect replay decisions.
type ReplayPlan struct {
	// Session is the session being restored.
	Session model.SessionID

	// Branch is the branch that owns TargetCell.
	Branch model.BranchID

	// TargetCell is the cell at which replay should stop (inclusive).
	TargetCell model.CellID

	// RuntimeHash is the stable identity derived from RuntimeConfig.
	RuntimeHash string

	// RuntimeConfig is the serialised runtime descriptor recorded for the
	// session when it was started.
	RuntimeConfig json.RawMessage

	// Steps is the ordered list of committed cells to replay.
	Steps []ReplayStep

	// Decisions maps each effect ID encountered during the plan to its replay
	// decision. Keyed by EffectID for O(1) lookup during replay.
	Decisions map[model.EffectID]ReplayDecision
}

// HeadRecord describes the current head of a branch.
type HeadRecord struct {
	// Session is the owning session.
	Session model.SessionID

	// Branch is the branch whose head is described.
	Branch model.BranchID

	// Head is the most-recently committed cell on this branch.
	// An empty CellID indicates the branch has no committed cells yet.
	Head model.CellID
}

// SessionState describes the active branch/head cursor for a session.
type SessionState struct {
	// Session is the owning session.
	Session model.SessionID

	// Branch is the currently active branch.
	Branch model.BranchID

	// Head is the currently active committed cell. An empty value means the
	// session has not committed any cells yet.
	Head model.CellID

	// RuntimeHash is the runtime descriptor attached to the session.
	RuntimeHash string

	// RuntimeConfig is the canonical runtime descriptor payload.
	RuntimeConfig json.RawMessage
}

// StaticEnvSnapshot carries the information needed by the type service to
// reconstruct the incremental TypeScript program state for a session head.
// It captures only serialisable facts; live runtime values are not included.
type StaticEnvSnapshot struct {
	// Session identifies the owning session.
	Session model.SessionID

	// Branch identifies the branch being reconstructed.
	Branch model.BranchID

	// Head is the cell the snapshot was built up to.
	Head model.CellID

	// Manifest is the host capability manifest in effect for this session.
	Manifest model.Manifest

	// CommittedSources is the ordered list of TypeScript sources for all
	// committed cells up to and including Head. The type service feeds these
	// into its incremental program to recreate binding state.
	CommittedSources []string
}

// Store is the persistence boundary for the repl engine.
//
// Implementations must be safe for concurrent use by multiple goroutines.
// Callers coordinate serialised head advancement above this layer; the store
// itself does not enforce single-writer semantics.
type Store interface {
	// AppendFact durably records one append-only domain fact. The fact
	// argument must implement model.Fact so that implementations can route
	// and index it by type without resorting to untyped switches.
	AppendFact(ctx context.Context, fact model.Fact) error

	// PutRuntimeConfig stores a normalised runtime descriptor keyed by its hash.
	// Implementations should deduplicate by hash.
	PutRuntimeConfig(ctx context.Context, hash string, config json.RawMessage) error

	// LoadRuntimeConfig returns the runtime descriptor previously stored under
	// the given hash.
	LoadRuntimeConfig(ctx context.Context, hash string) (json.RawMessage, error)

	// LoadHead returns the current head record for the given session and
	// branch. If the branch exists but has no committed cells, Head is the
	// zero CellID. If the session or branch does not exist, an error is
	// returned.
	LoadHead(ctx context.Context, session model.SessionID, branch model.BranchID) (HeadRecord, error)

	// LoadStaticEnv reconstructs the static-environment snapshot needed by the
	// type service for the given session, branch, and head cell.
	LoadStaticEnv(ctx context.Context, session model.SessionID, branch model.BranchID, head model.CellID) (StaticEnvSnapshot, error)

	// LoadReplayPlan builds the ordered replay plan for restoring a session to
	// the given target cell. All cells on the branch ancestry up to and
	// including targetCell are included in the plan.
	LoadReplayPlan(ctx context.Context, session model.SessionID, targetCell model.CellID) (ReplayPlan, error)

	// LoadFailures returns the durable failed submit attempts for the given
	// session in append order.
	LoadFailures(ctx context.Context, session model.SessionID) ([]model.CellFailed, error)

	// LoadSessionState returns the active branch/head cursor for the given
	// session together with its attached runtime descriptor.
	LoadSessionState(ctx context.Context, session model.SessionID) (SessionState, error)
}
