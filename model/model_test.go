package model_test

import (
	"encoding/json"
	"testing"

	// Verify mus-go is wired into the module (smoke-test import).
	_ "github.com/mus-format/mus-go"

	. "github.com/solidarity-ai/repl/model"
)

// TestIDTypes verifies that all opaque ID types are defined and can be
// converted from plain strings without truncation.
func TestIDTypes(t *testing.T) {
	tests := []struct {
		name string
		fn   func() bool
	}{
		{"SessionID", func() bool { return SessionID("s1") == "s1" }},
		{"BranchID", func() bool { return BranchID("b1") == "b1" }},
		{"CellID", func() bool { return CellID("c1") == "c1" }},
		{"EffectID", func() bool { return EffectID("e1") == "e1" }},
		{"PromiseID", func() bool { return PromiseID("p1") == "p1" }},
		{"ValueID", func() bool { return ValueID("v1") == "v1" }},
		{"CheckpointID", func() bool { return CheckpointID("k1") == "k1" }},
		{"FailureID", func() bool { return FailureID("f1") == "f1" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.fn() {
				t.Fatalf("%s round-trip failed", tc.name)
			}
		})
	}
}

// TestReplayPolicyConstants verifies the string values match the package plan.
func TestReplayPolicyConstants(t *testing.T) {
	cases := []struct {
		policy ReplayPolicy
		want   string
	}{
		{ReplayReadonly, "readonly"},
		{ReplayIdempotent, "idempotent"},
		{ReplayAtMostOnce, "at_most_once"},
		{ReplayNonReplayable, "non_replayable"},
	}
	for _, c := range cases {
		if string(c.policy) != c.want {
			t.Errorf("ReplayPolicy %q: got %q", c.want, c.policy)
		}
	}
}

// TestPromiseStateConstants verifies the string values match the package plan.
func TestPromiseStateConstants(t *testing.T) {
	cases := []struct {
		state PromiseState
		want  string
	}{
		{PromisePending, "pending"},
		{PromiseFulfilled, "fulfilled"},
		{PromiseRejected, "rejected"},
	}
	for _, c := range cases {
		if string(c.state) != c.want {
			t.Errorf("PromiseState %q: got %q", c.want, c.state)
		}
	}
}

// TestFactInterface verifies that all fact structs satisfy the Fact interface
// and return the correct FactType string.
func TestFactInterface(t *testing.T) {
	facts := []struct {
		fact     Fact
		wantType string
	}{
		{SessionStarted{}, FactTypeSessionStarted},
		{ManifestAttached{}, FactTypeManifestAttached},
		{RuntimeAttached{}, FactTypeRuntimeAttached},
		{CellChecked{}, FactTypeCellChecked},
		{CellEvaluated{}, FactTypeCellEvaluated},
		{CellCommitted{}, FactTypeCellCommitted},
		{CellFailed{}, FactTypeCellFailed},
		{EffectStarted{}, FactTypeEffectStarted},
		{EffectCompleted{}, FactTypeEffectCompleted},
		{EffectFailed{}, FactTypeEffectFailed},
		{PromiseSettled{}, FactTypePromiseSettled},
		{HeadMoved{}, FactTypeHeadMoved},
		{BranchCreated{}, FactTypeBranchCreated},
		{RestoreCompleted{}, FactTypeRestoreCompleted},
		{CheckpointSaved{}, FactTypeCheckpointSaved},
	}
	for _, tc := range facts {
		if got := tc.fact.FactType(); got != tc.wantType {
			t.Errorf("%T.FactType() = %q, want %q", tc.fact, got, tc.wantType)
		}
	}
}

// TestManifestJSONRoundTrip verifies that Manifest and HostFunctionSpec are
// JSON-safe by doing a marshal/unmarshal cycle.
func TestManifestJSONRoundTrip(t *testing.T) {
	m := Manifest{
		ID: "manifest-1",
		Functions: []HostFunctionSpec{
			{
				Name:         "tools.calc.add",
				ParamsSchema: json.RawMessage(`{"type":"object"}`),
				ResultSchema: json.RawMessage(`{"type":"number"}`),
				Replay:       ReplayReadonly,
			},
		},
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m2 Manifest
	if err := json.Unmarshal(data, &m2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m2.ID != m.ID {
		t.Errorf("ID round-trip: got %q, want %q", m2.ID, m.ID)
	}
	if len(m2.Functions) != 1 {
		t.Fatalf("functions count: got %d, want 1", len(m2.Functions))
	}
	if m2.Functions[0].Name != "tools.calc.add" {
		t.Errorf("function name: got %q, want %q", m2.Functions[0].Name, "tools.calc.add")
	}
	if m2.Functions[0].Replay != ReplayReadonly {
		t.Errorf("replay policy: got %q, want %q", m2.Functions[0].Replay, ReplayReadonly)
	}
}

// TestValueRefAndPromiseRef verifies JSON round-trips for reference types.
func TestValueRefAndPromiseRef(t *testing.T) {
	vr := ValueRef{ID: "v1", Preview: "42", TypeHint: "number"}
	data, err := json.Marshal(vr)
	if err != nil {
		t.Fatalf("ValueRef marshal: %v", err)
	}
	var vr2 ValueRef
	if err := json.Unmarshal(data, &vr2); err != nil {
		t.Fatalf("ValueRef unmarshal: %v", err)
	}
	if vr2.ID != vr.ID || vr2.Preview != vr.Preview {
		t.Errorf("ValueRef round-trip mismatch: %+v", vr2)
	}

	pr := PromiseRef{ID: "p1", State: PromisePending}
	data2, err := json.Marshal(pr)
	if err != nil {
		t.Fatalf("PromiseRef marshal: %v", err)
	}
	var pr2 PromiseRef
	if err := json.Unmarshal(data2, &pr2); err != nil {
		t.Fatalf("PromiseRef unmarshal: %v", err)
	}
	if pr2.ID != pr.ID || pr2.State != pr.State {
		t.Errorf("PromiseRef round-trip mismatch: %+v", pr2)
	}
}
