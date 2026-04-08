package model

// ReplayPolicy describes how the effects layer should treat a host-function
// call during session restore.
type ReplayPolicy string

const (
	// ReplayReadonly marks calls that are safe to rerun because they have no
	// observable side effects. Replay may still prefer a recorded prior result
	// for performance.
	ReplayReadonly ReplayPolicy = "readonly"

	// ReplayIdempotent marks calls that are safe to rerun when the host can
	// supply an idempotency key.
	ReplayIdempotent ReplayPolicy = "idempotent"

	// ReplayAtMostOnce marks calls that must not be rerun automatically after
	// uncertainty. Replay may only reuse a previously recorded completed result.
	ReplayAtMostOnce ReplayPolicy = "at_most_once"

	// ReplayNonReplayable marks calls that cannot be safely replayed.
	// Restoring across such an effect may fail or enter a user-visible degraded
	// mode.
	ReplayNonReplayable ReplayPolicy = "non_replayable"
)

// PromiseState describes the settlement status of a tracked async value.
type PromiseState string

const (
	// PromisePending means the promise has not yet settled.
	PromisePending PromiseState = "pending"

	// PromiseFulfilled means the promise resolved successfully.
	PromiseFulfilled PromiseState = "fulfilled"

	// PromiseRejected means the promise was rejected with an error.
	PromiseRejected PromiseState = "rejected"
)
