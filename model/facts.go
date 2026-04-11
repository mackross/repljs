package model

import "time"

// Fact is implemented by every append-only fact struct.
// The store uses FactType() to route and index facts without falling back to
// raw any type switches.
type Fact interface {
	FactType() string
}

// --- Fact type constants ---

const (
	FactTypeSessionStarted   = "SessionStarted"
	FactTypeManifestAttached = "ManifestAttached"
	FactTypeRuntimeAttached  = "RuntimeAttached"
	FactTypeCellChecked      = "CellChecked"
	FactTypeCellEvaluated    = "CellEvaluated"
	FactTypeCellCommitted    = "CellCommitted"
	FactTypeCellFailed       = "CellFailed"
	FactTypeEffectStarted    = "EffectStarted"
	FactTypeEffectCompleted  = "EffectCompleted"
	FactTypeEffectFailed     = "EffectFailed"
	FactTypePromiseSettled   = "PromiseSettled"
	FactTypeHeadMoved        = "HeadMoved"
	FactTypeBranchCreated    = "BranchCreated"
	FactTypeRestoreCompleted = "RestoreCompleted"
	FactTypeCheckpointSaved  = "CheckpointSaved"
)

// --- Session lifecycle facts ---

// SessionStarted is appended when a new session is created.
type SessionStarted struct {
	Session    SessionID `json:"session"`
	RootBranch BranchID  `json:"root_branch"`
	At         time.Time `json:"at"`
}

func (SessionStarted) FactType() string { return FactTypeSessionStarted }

// ManifestAttached is appended when a manifest is bound to a session.
// Manifest carries the full snapshot so that LoadStaticEnv can reconstruct
// type-service state from facts alone, without any external manifest registry.
type ManifestAttached struct {
	Session    SessionID `json:"session"`
	ManifestID string    `json:"manifest_id"`
	// Manifest is the full manifest snapshot captured at attach time.
	// Storing the snapshot here keeps the fact log self-contained and ensures
	// replay can reconstruct the exact host-function environment without
	// external lookup.
	Manifest Manifest  `json:"manifest"`
	At       time.Time `json:"at"`
}

func (ManifestAttached) FactType() string { return FactTypeManifestAttached }

// RuntimeAttached is appended when the embedder-provided runtime surface is
// bound to a session. RuntimeConfig is the canonical replay payload; the hash
// is derived from it for identity and diagnostics.
type RuntimeAttached struct {
	Session     SessionID `json:"session"`
	RuntimeHash string    `json:"runtime_hash"`
	At          time.Time `json:"at"`
}

func (RuntimeAttached) FactType() string { return FactTypeRuntimeAttached }

// --- Cell facts ---

// Diagnostic carries a single TypeScript diagnostic for a cell.
type Diagnostic struct {
	Message  string `json:"message"`
	Severity string `json:"severity"` // "error" | "warning" | "info"
	Line     int    `json:"line,omitempty"`
	Column   int    `json:"column,omitempty"`
}

// CellChecked is appended when the type service has finished checking a cell.
type CellChecked struct {
	Session     SessionID    `json:"session"`
	Branch      BranchID     `json:"branch"`
	Cell        CellID       `json:"cell"`
	Parent      CellID       `json:"parent,omitempty"`
	Language    CellLanguage `json:"language,omitempty"`
	Source      string       `json:"source"`
	Diagnostics []Diagnostic `json:"diagnostics"`
	EmittedJS   string       `json:"emitted_js,omitempty"`
	HasErrors   bool         `json:"has_errors"`
	At          time.Time    `json:"at"`
}

func (CellChecked) FactType() string { return FactTypeCellChecked }

// CellEvaluated is appended when the runtime has finished evaluating a cell.
type CellEvaluated struct {
	Session         SessionID  `json:"session"`
	Branch          BranchID   `json:"branch"`
	Cell            CellID     `json:"cell"`
	CreatedBindings []string   `json:"created_bindings,omitempty"`
	LinkedEffects   []EffectID `json:"linked_effects,omitempty"`
	CompletionValue *ValueRef  `json:"completion_value,omitempty"`
	ErrorMessage    string     `json:"error_message,omitempty"`
	At              time.Time  `json:"at"`
}

func (CellEvaluated) FactType() string { return FactTypeCellEvaluated }

// CellCommitted is appended when a cell has been durably accepted as part of
// the session history.
type CellCommitted struct {
	Session SessionID `json:"session"`
	Branch  BranchID  `json:"branch"`
	Cell    CellID    `json:"cell"`
	At      time.Time `json:"at"`
}

func (CellCommitted) FactType() string { return FactTypeCellCommitted }

// CellFailed is appended when a submit attempt fails and therefore does not
// become part of committed session history. The source and parent head are
// captured so callers can inspect and later replay the failed attempt in an
// isolated debug flow.
type CellFailed struct {
	Session       SessionID  `json:"session"`
	Branch        BranchID   `json:"branch"`
	Failure       FailureID  `json:"failure"`
	Parent        CellID     `json:"parent,omitempty"`
	Source        string     `json:"source"`
	RuntimeHash   string     `json:"runtime_hash,omitempty"`
	Phase         string     `json:"phase,omitempty"`
	ErrorMessage  string     `json:"error_message"`
	LinkedEffects []EffectID `json:"linked_effects,omitempty"`
	At            time.Time  `json:"at"`
}

func (CellFailed) FactType() string { return FactTypeCellFailed }

// --- Effect facts ---

// EffectStarted is appended when a host function invocation begins.
// ReplayPolicy is captured here at call time so that LoadReplayPlan can
// populate ReplayDecision.Policy directly from facts without re-querying the
// manifest.
type EffectStarted struct {
	Session        SessionID `json:"session"`
	Effect         EffectID  `json:"effect"`
	Cell           CellID    `json:"cell"`
	FunctionName   string    `json:"function_name"`
	Params         []byte    `json:"params,omitempty"`
	IdempotencyKey string    `json:"idempotency_key,omitempty"`
	// ReplayPolicy is the policy in effect for this function at the time the
	// effect was started, copied from HostFunctionSpec.Replay. Recording it
	// here means the replay layer does not need to re-derive it from the
	// manifest during restore.
	ReplayPolicy ReplayPolicy `json:"replay_policy,omitempty"`
	At           time.Time    `json:"at"`
}

func (EffectStarted) FactType() string { return FactTypeEffectStarted }

// EffectCompleted is appended when a host function invocation succeeds.
type EffectCompleted struct {
	Session SessionID `json:"session"`
	Effect  EffectID  `json:"effect"`
	Result  []byte    `json:"result,omitempty"`
	At      time.Time `json:"at"`
}

func (EffectCompleted) FactType() string { return FactTypeEffectCompleted }

// EffectFailed is appended when a host function invocation fails.
type EffectFailed struct {
	Session      SessionID `json:"session"`
	Effect       EffectID  `json:"effect"`
	ErrorMessage string    `json:"error_message"`
	At           time.Time `json:"at"`
}

func (EffectFailed) FactType() string { return FactTypeEffectFailed }

// --- Promise facts ---

// PromiseSettled is appended when an async value reaches a terminal state.
type PromiseSettled struct {
	Session SessionID    `json:"session"`
	Promise PromiseID    `json:"promise"`
	Effect  EffectID     `json:"effect,omitempty"`
	State   PromiseState `json:"state"`
	Value   []byte       `json:"value,omitempty"`
	Error   string       `json:"error,omitempty"`
	At      time.Time    `json:"at"`
}

func (PromiseSettled) FactType() string { return FactTypePromiseSettled }

// --- Branch and head facts ---

// HeadMoved is appended when a branch head advances to a new cell.
type HeadMoved struct {
	Session  SessionID `json:"session"`
	Branch   BranchID  `json:"branch"`
	Previous CellID    `json:"previous,omitempty"`
	Next     CellID    `json:"next"`
	At       time.Time `json:"at"`
}

func (HeadMoved) FactType() string { return FactTypeHeadMoved }

// BranchCreated is appended when a new branch is forked from an existing cell.
type BranchCreated struct {
	Session    SessionID `json:"session"`
	Branch     BranchID  `json:"branch"`
	ParentCell CellID    `json:"parent_cell"`
	At         time.Time `json:"at"`
}

func (BranchCreated) FactType() string { return FactTypeBranchCreated }

// --- Restore and checkpoint facts ---

// RestoreCompleted is appended when a session has been successfully replayed to
// a target cell.
type RestoreCompleted struct {
	Session    SessionID `json:"session"`
	TargetCell CellID    `json:"target_cell"`
	NewBranch  BranchID  `json:"new_branch,omitempty"`
	At         time.Time `json:"at"`
}

func (RestoreCompleted) FactType() string { return FactTypeRestoreCompleted }

// CheckpointSaved is appended when a runtime snapshot has been persisted.
type CheckpointSaved struct {
	Session    SessionID    `json:"session"`
	Checkpoint CheckpointID `json:"checkpoint"`
	AtCell     CellID       `json:"at_cell"`
	At         time.Time    `json:"at"`
}

func (CheckpointSaved) FactType() string { return FactTypeCheckpointSaved }
