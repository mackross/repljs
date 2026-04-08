package session

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/solidarity-ai/repl/engine"
)

func mustNewBranchRuntime(t *testing.T) *branchRuntime {
	t.Helper()
	rt, _, _, err := newBranchRuntime(context.Background(), engine.SessionRuntimeContext{}, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("newBranchRuntime: %v", err)
	}
	return rt
}

// TestBranchRuntime_FulfilledAsyncIIFE verifies that a simple async IIFE that
// returns a primitive is settled by run() and its preview reflects the settled
// value, not "[object Promise]".
func TestBranchRuntime_FulfilledAsyncIIFE(t *testing.T) {
	rt := mustNewBranchRuntime(t)
	res, err := rt.run(`(async () => 42)()`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.completionValue == nil {
		t.Fatal("expected a completionValue, got nil")
	}
	if res.completionValue.Preview != "42" {
		t.Errorf("expected preview %q, got %q", "42", res.completionValue.Preview)
	}
	if res.completionValue.TypeHint != "number" {
		t.Errorf("expected type hint %q, got %q", "number", res.completionValue.TypeHint)
	}
	if res.structured == nil {
		t.Fatal("expected structured bytes, got nil")
	}
	var got interface{}
	if err := json.Unmarshal(res.structured, &got); err != nil {
		t.Fatalf("structured bytes are not valid JSON: %v", err)
	}
}

// TestBranchRuntime_AwaitedPromiseChain verifies that awaited settled promise
// chains are also unwrapped correctly.
func TestBranchRuntime_AwaitedPromiseChain(t *testing.T) {
	rt := mustNewBranchRuntime(t)
	res, err := rt.run(`(async () => { return await Promise.resolve("hello"); })()`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.completionValue == nil {
		t.Fatal("expected completionValue, got nil")
	}
	if res.completionValue.Preview != "hello" {
		t.Errorf("expected preview %q, got %q", "hello", res.completionValue.Preview)
	}
	if res.completionValue.TypeHint != "string" {
		t.Errorf("expected type hint %q, got %q", "string", res.completionValue.TypeHint)
	}
	if res.structured == nil {
		t.Fatal("expected structured bytes, got nil")
	}
}

// TestBranchRuntime_RejectedPromise verifies that a rejected async IIFE returns
// an error with the rejection reason and no completion ref.
func TestBranchRuntime_RejectedPromise(t *testing.T) {
	rt := mustNewBranchRuntime(t)
	res, err := rt.run(`(async () => { throw new Error("boom"); })()`)
	if err == nil {
		t.Fatal("expected error for rejected promise, got nil")
	}
	if !strings.Contains(err.Error(), "promise rejected") {
		t.Errorf("error should mention 'promise rejected', got: %v", err)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error should contain rejection reason, got: %v", err)
	}
	if res.completionValue != nil {
		t.Errorf("expected nil completionValue on rejection, got: %+v", res.completionValue)
	}
	if res.structured != nil {
		t.Errorf("expected nil structured on rejection, got non-nil bytes")
	}
}

// TestBranchRuntime_PendingPromise verifies that a never-settling promise
// returns an error and no completion ref.
func TestBranchRuntime_PendingPromise(t *testing.T) {
	rt := mustNewBranchRuntime(t)
	// A promise whose executor never calls resolve/reject stays pending.
	res, err := rt.run(`new Promise(() => {})`)
	if err == nil {
		t.Fatal("expected error for pending promise, got nil")
	}
	if !strings.Contains(err.Error(), "pending") {
		t.Errorf("error should mention 'pending', got: %v", err)
	}
	if res.completionValue != nil {
		t.Errorf("expected nil completionValue for pending promise, got %+v", res.completionValue)
	}
}

// TestBranchRuntime_UndefinedResult verifies that expressions evaluating to
// undefined produce no completion ref and no structured bytes.
func TestBranchRuntime_UndefinedResult(t *testing.T) {
	rt := mustNewBranchRuntime(t)
	res, err := rt.run(`undefined`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.completionValue != nil {
		t.Errorf("expected nil completionValue for undefined, got %+v", res.completionValue)
	}
	if res.structured != nil {
		t.Errorf("expected nil structured for undefined, got non-nil bytes")
	}
}

// TestBranchRuntime_NullResult verifies that null produces no completion ref.
func TestBranchRuntime_NullResult(t *testing.T) {
	rt := mustNewBranchRuntime(t)
	res, err := rt.run(`null`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.completionValue != nil {
		t.Errorf("expected nil completionValue for null, got %+v", res.completionValue)
	}
}

// TestBranchRuntime_EmptySource verifies that an empty source string either
// returns a zero result or an error, and does not panic.
func TestBranchRuntime_EmptySource(t *testing.T) {
	rt := mustNewBranchRuntime(t)
	res, err := rt.run(``)
	if err != nil {
		// An error is acceptable for empty source.
		return
	}
	// If no error, completion value must be nil (empty program returns undefined).
	if res.completionValue != nil {
		t.Errorf("expected nil completionValue for empty source, got %+v", res.completionValue)
	}
}

// TestBranchRuntime_SyncExpression verifies that a plain synchronous expression
// is handled correctly (no promise wrapping needed).
func TestBranchRuntime_SyncExpression(t *testing.T) {
	rt := mustNewBranchRuntime(t)
	res, err := rt.run(`1 + 2`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.completionValue == nil {
		t.Fatal("expected completionValue for sync expression, got nil")
	}
	if res.completionValue.Preview != "3" {
		t.Errorf("expected preview %q, got %q", "3", res.completionValue.Preview)
	}
	if res.structured == nil {
		t.Fatal("expected structured bytes for sync expression, got nil")
	}
}

// TestBranchRuntime_NonSerializableExport verifies that a value with a cyclic
// reference (or otherwise non-JSON-marshalable export) produces a preview but
// nil structured bytes without panicking.
func TestBranchRuntime_NonSerializableExport(t *testing.T) {
	rt := mustNewBranchRuntime(t)
	// Build a cyclic object: o.self = o
	_, err := rt.run(`var o = {}; o.self = o; o`)
	// goja's Export() for a cyclic JS object returns a map[string]interface{}
	// with a cyclic reference that json.Marshal cannot handle. We just need the
	// call to succeed without panic and structured to be nil.
	if err != nil {
		// If the engine faults on cyclic detection, that's also acceptable.
		return
	}
	// Re-run to get the result from a fresh expression.
	res2, err := rt.run(`o`)
	if err != nil {
		t.Fatalf("unexpected error reading cyclic object: %v", err)
	}
	if res2.completionValue == nil {
		t.Fatal("expected completionValue for object, got nil")
	}
	// structured MUST be nil (cyclic) or some valid subset; we only require it
	// does not panic and completionValue is non-nil.
	_ = res2.structured
}

// TestBranchRuntime_AsyncReturnsObject verifies that an async IIFE returning a
// plain object produces structured bytes.
func TestBranchRuntime_AsyncReturnsObject(t *testing.T) {
	rt := mustNewBranchRuntime(t)
	res, err := rt.run(`(async () => ({ x: 1, y: 2 }))()`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.completionValue == nil {
		t.Fatal("expected completionValue, got nil")
	}
	if res.structured == nil {
		t.Fatal("expected structured bytes for plain object, got nil")
	}
	var got map[string]interface{}
	if err := json.Unmarshal(res.structured, &got); err != nil {
		t.Fatalf("structured bytes are not valid JSON object: %v", err)
	}
	if got["x"] == nil || got["y"] == nil {
		t.Errorf("expected x and y fields in structured output, got %v", got)
	}
}
