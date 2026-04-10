package mem_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/solidarity-ai/repl/model"
	memstore "github.com/solidarity-ai/repl/store/mem"
)

// newTestStore creates a fresh in-memory store for test isolation.
func newTestStore(t *testing.T) *memstore.Store {
	t.Helper()
	return memstore.New()
}

// ---------------------------------------------------------------------------
// AppendFact round-trip
// ---------------------------------------------------------------------------

// TestMemStore_AppendFactRoundTrip mirrors the primary SQLite round-trip test.
// It appends a representative session fixture and exercises every query method
// to confirm that in-memory encode/decode produces correct results.
func TestMemStore_AppendFactRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	const (
		sessionID = model.SessionID("sess-001")
		branchID  = model.BranchID("branch-main")
		cellID1   = model.CellID("cell-001")
		cellID2   = model.CellID("cell-002")
		effectID  = model.EffectID("effect-001")
	)

	manifest := model.Manifest{
		ID: "manifest-v1",
		Functions: []model.HostFunctionSpec{
			{
				Name:         "tools.fs.read",
				ParamsSchema: json.RawMessage(`{"type":"object"}`),
				ResultSchema: json.RawMessage(`{"type":"string"}`),
				Replay:       model.ReplayReadonly,
			},
		},
	}

	facts := []model.Fact{
		model.SessionStarted{Session: sessionID, RootBranch: branchID, At: time.Now().UTC()},
		model.ManifestAttached{Session: sessionID, ManifestID: manifest.ID, Manifest: manifest, At: time.Now().UTC()},
		model.CellChecked{Session: sessionID, Branch: branchID, Cell: cellID1, Source: "const x = 1;", EmittedJS: "const x = 1;", At: time.Now().UTC()},
		model.CellEvaluated{Session: sessionID, Branch: branchID, Cell: cellID1, LinkedEffects: []model.EffectID{effectID}, At: time.Now().UTC()},
		model.EffectStarted{Session: sessionID, Effect: effectID, Cell: cellID1, FunctionName: "tools.fs.read", Params: json.RawMessage(`{"path":"/etc/hosts"}`), ReplayPolicy: model.ReplayReadonly, At: time.Now().UTC()},
		model.EffectCompleted{Session: sessionID, Effect: effectID, Result: json.RawMessage(`"127.0.0.1 localhost"`), At: time.Now().UTC()},
		model.CellCommitted{Session: sessionID, Branch: branchID, Cell: cellID1, At: time.Now().UTC()},
		model.HeadMoved{Session: sessionID, Branch: branchID, Next: cellID1, At: time.Now().UTC()},
		// Second cell with no effects.
		model.CellChecked{Session: sessionID, Branch: branchID, Cell: cellID2, Source: "const y = x + 1;", EmittedJS: "const y = x + 1;", At: time.Now().UTC()},
		model.CellEvaluated{Session: sessionID, Branch: branchID, Cell: cellID2, At: time.Now().UTC()},
		model.CellCommitted{Session: sessionID, Branch: branchID, Cell: cellID2, At: time.Now().UTC()},
		model.HeadMoved{Session: sessionID, Branch: branchID, Previous: cellID1, Next: cellID2, At: time.Now().UTC()},
	}

	for _, f := range facts {
		if err := s.AppendFact(ctx, f); err != nil {
			t.Fatalf("AppendFact(%q): %v", f.FactType(), err)
		}
	}

	t.Run("LoadHead", func(t *testing.T) {
		head, err := s.LoadHead(ctx, sessionID, branchID)
		if err != nil {
			t.Fatalf("LoadHead: %v", err)
		}
		if head.Session != sessionID {
			t.Errorf("Session: got %q, want %q", head.Session, sessionID)
		}
		if head.Branch != branchID {
			t.Errorf("Branch: got %q, want %q", head.Branch, branchID)
		}
		if head.Head != cellID2 {
			t.Errorf("Head: got %q, want %q", head.Head, cellID2)
		}
	})

	t.Run("LoadStaticEnv", func(t *testing.T) {
		env, err := s.LoadStaticEnv(ctx, sessionID, branchID, cellID2)
		if err != nil {
			t.Fatalf("LoadStaticEnv: %v", err)
		}
		if env.Manifest.ID != manifest.ID {
			t.Errorf("Manifest.ID: got %q, want %q", env.Manifest.ID, manifest.ID)
		}
		if len(env.Manifest.Functions) != 1 {
			t.Errorf("Manifest.Functions len: got %d, want 1", len(env.Manifest.Functions))
		}
		if env.Head != cellID2 {
			t.Errorf("Head: got %q, want %q", env.Head, cellID2)
		}
		wantSources := []string{"const x = 1;", "const y = x + 1;"}
		if len(env.CommittedSources) != len(wantSources) {
			t.Fatalf("CommittedSources len: got %d, want %d", len(env.CommittedSources), len(wantSources))
		}
		for i, want := range wantSources {
			if env.CommittedSources[i] != want {
				t.Errorf("CommittedSources[%d]: got %q, want %q", i, env.CommittedSources[i], want)
			}
		}
	})

	t.Run("LoadReplayPlan", func(t *testing.T) {
		plan, err := s.LoadReplayPlan(ctx, sessionID, cellID2)
		if err != nil {
			t.Fatalf("LoadReplayPlan: %v", err)
		}
		if plan.Session != sessionID {
			t.Errorf("Session: got %q, want %q", plan.Session, sessionID)
		}
		if plan.TargetCell != cellID2 {
			t.Errorf("TargetCell: got %q, want %q", plan.TargetCell, cellID2)
		}
		if len(plan.Steps) != 2 {
			t.Fatalf("Steps len: got %d, want 2", len(plan.Steps))
		}
		if plan.Steps[0].Cell != cellID1 {
			t.Errorf("Steps[0].Cell: got %q, want %q", plan.Steps[0].Cell, cellID1)
		}
		if plan.Steps[0].Source != "const x = 1;" {
			t.Errorf("Steps[0].Source: got %q, want %q", plan.Steps[0].Source, "const x = 1;")
		}
		if plan.Steps[1].Cell != cellID2 {
			t.Errorf("Steps[1].Cell: got %q, want %q", plan.Steps[1].Cell, cellID2)
		}
		dec, ok := plan.Decisions[effectID]
		if !ok {
			t.Fatalf("Decisions missing effect %q", effectID)
		}
		if dec.Policy != model.ReplayReadonly {
			t.Errorf("Decision.Policy: got %q, want %q", dec.Policy, model.ReplayReadonly)
		}
		if string(dec.RecordedResult) != `"127.0.0.1 localhost"` {
			t.Errorf("Decision.RecordedResult: got %q, want %q", dec.RecordedResult, `"127.0.0.1 localhost"`)
		}
	})
}

// ---------------------------------------------------------------------------
// LoadHead focused tests
// ---------------------------------------------------------------------------

// TestMemStore_LoadHead mirrors the SQLite LoadHead focused test suite.
func TestMemStore_LoadHead(t *testing.T) {
	ctx := context.Background()

	const (
		session = model.SessionID("sess-head")
		root    = model.BranchID("root")
		fork    = model.BranchID("fork")
		cell1   = model.CellID("c1")
		cell2   = model.CellID("c2")
		cell3   = model.CellID("c3")
	)

	t.Run("missing session/branch returns error", func(t *testing.T) {
		s := newTestStore(t)
		_, err := s.LoadHead(ctx, session, root)
		if err == nil {
			t.Fatal("expected error for unknown session/branch, got nil")
		}
	})

	t.Run("branch exists but no commits yet returns empty head", func(t *testing.T) {
		s := newTestStore(t)
		if err := s.AppendFact(ctx, model.SessionStarted{
			Session:    session,
			RootBranch: root,
			At:         time.Now().UTC(),
		}); err != nil {
			t.Fatalf("AppendFact SessionStarted: %v", err)
		}
		head, err := s.LoadHead(ctx, session, root)
		if err != nil {
			t.Fatalf("LoadHead: %v", err)
		}
		if head.Session != session {
			t.Errorf("Session: got %q, want %q", head.Session, session)
		}
		if head.Branch != root {
			t.Errorf("Branch: got %q, want %q", head.Branch, root)
		}
		if head.Head != "" {
			t.Errorf("Head: got %q, want empty (no commits)", head.Head)
		}
	})

	t.Run("root branch head advances correctly", func(t *testing.T) {
		s := newTestStore(t)
		for _, f := range []model.Fact{
			model.SessionStarted{Session: session, RootBranch: root, At: time.Now().UTC()},
			model.ManifestAttached{Session: session, ManifestID: "m", Manifest: model.Manifest{ID: "m"}, At: time.Now().UTC()},
			model.CellChecked{Session: session, Branch: root, Cell: cell1, Source: "a", At: time.Now().UTC()},
			model.CellCommitted{Session: session, Branch: root, Cell: cell1, At: time.Now().UTC()},
			model.HeadMoved{Session: session, Branch: root, Next: cell1, At: time.Now().UTC()},
			model.CellChecked{Session: session, Branch: root, Cell: cell2, Source: "b", At: time.Now().UTC()},
			model.CellCommitted{Session: session, Branch: root, Cell: cell2, At: time.Now().UTC()},
			model.HeadMoved{Session: session, Branch: root, Previous: cell1, Next: cell2, At: time.Now().UTC()},
		} {
			if err := s.AppendFact(ctx, f); err != nil {
				t.Fatalf("AppendFact(%q): %v", f.FactType(), err)
			}
		}
		head, err := s.LoadHead(ctx, session, root)
		if err != nil {
			t.Fatalf("LoadHead: %v", err)
		}
		if head.Head != cell2 {
			t.Errorf("Head: got %q, want %q", head.Head, cell2)
		}
	})

	t.Run("forked branch returns its own head", func(t *testing.T) {
		s := newTestStore(t)
		for _, f := range []model.Fact{
			model.SessionStarted{Session: session, RootBranch: root, At: time.Now().UTC()},
			model.ManifestAttached{Session: session, ManifestID: "m", Manifest: model.Manifest{ID: "m"}, At: time.Now().UTC()},
			model.CellChecked{Session: session, Branch: root, Cell: cell1, Source: "a", At: time.Now().UTC()},
			model.CellCommitted{Session: session, Branch: root, Cell: cell1, At: time.Now().UTC()},
			model.HeadMoved{Session: session, Branch: root, Next: cell1, At: time.Now().UTC()},
			model.CellChecked{Session: session, Branch: root, Cell: cell2, Source: "b", At: time.Now().UTC()},
			model.CellCommitted{Session: session, Branch: root, Cell: cell2, At: time.Now().UTC()},
			model.HeadMoved{Session: session, Branch: root, Previous: cell1, Next: cell2, At: time.Now().UTC()},
			model.BranchCreated{Session: session, Branch: fork, ParentCell: cell1, At: time.Now().UTC()},
			model.CellChecked{Session: session, Branch: fork, Cell: cell3, Source: "c", At: time.Now().UTC()},
			model.CellCommitted{Session: session, Branch: fork, Cell: cell3, At: time.Now().UTC()},
			model.HeadMoved{Session: session, Branch: fork, Next: cell3, At: time.Now().UTC()},
		} {
			if err := s.AppendFact(ctx, f); err != nil {
				t.Fatalf("AppendFact(%q): %v", f.FactType(), err)
			}
		}

		rootHead, err := s.LoadHead(ctx, session, root)
		if err != nil {
			t.Fatalf("LoadHead root: %v", err)
		}
		if rootHead.Head != cell2 {
			t.Errorf("root Head: got %q, want %q", rootHead.Head, cell2)
		}

		forkHead, err := s.LoadHead(ctx, session, fork)
		if err != nil {
			t.Fatalf("LoadHead fork: %v", err)
		}
		if forkHead.Head != cell3 {
			t.Errorf("fork Head: got %q, want %q", forkHead.Head, cell3)
		}
	})

	t.Run("forked branch with no commits returns empty head", func(t *testing.T) {
		s := newTestStore(t)
		for _, f := range []model.Fact{
			model.SessionStarted{Session: session, RootBranch: root, At: time.Now().UTC()},
			model.CellChecked{Session: session, Branch: root, Cell: cell1, Source: "a", At: time.Now().UTC()},
			model.CellCommitted{Session: session, Branch: root, Cell: cell1, At: time.Now().UTC()},
			model.HeadMoved{Session: session, Branch: root, Next: cell1, At: time.Now().UTC()},
			model.BranchCreated{Session: session, Branch: fork, ParentCell: cell1, At: time.Now().UTC()},
		} {
			if err := s.AppendFact(ctx, f); err != nil {
				t.Fatalf("AppendFact(%q): %v", f.FactType(), err)
			}
		}
		head, err := s.LoadHead(ctx, session, fork)
		if err != nil {
			t.Fatalf("LoadHead fork with no commits: %v", err)
		}
		if head.Head != "" {
			t.Errorf("Head: got %q, want empty", head.Head)
		}
	})
}

// ---------------------------------------------------------------------------
// LoadStaticEnv focused tests
// ---------------------------------------------------------------------------

// TestMemStore_LoadStaticEnv mirrors the SQLite LoadStaticEnv focused tests.
func TestMemStore_LoadStaticEnv(t *testing.T) {
	ctx := context.Background()

	const (
		session = model.SessionID("sess-env")
		root    = model.BranchID("root")
		fork    = model.BranchID("fork")
		cell1   = model.CellID("c1")
		cell2   = model.CellID("c2")
		cell3   = model.CellID("c3")
	)

	manifest := model.Manifest{
		ID: "manifest-env",
		Functions: []model.HostFunctionSpec{
			{
				Name:         "tools.env.get",
				ParamsSchema: json.RawMessage(`{"type":"object"}`),
				ResultSchema: json.RawMessage(`{"type":"string"}`),
				Replay:       model.ReplayReadonly,
			},
		},
	}

	setupRoot := func(t *testing.T) *memstore.Store {
		t.Helper()
		s := newTestStore(t)
		for _, f := range []model.Fact{
			model.SessionStarted{Session: session, RootBranch: root, At: time.Now().UTC()},
			model.ManifestAttached{Session: session, ManifestID: manifest.ID, Manifest: manifest, At: time.Now().UTC()},
			model.CellChecked{Session: session, Branch: root, Cell: cell1, Source: "const a = 1;", At: time.Now().UTC()},
			model.CellCommitted{Session: session, Branch: root, Cell: cell1, At: time.Now().UTC()},
			model.HeadMoved{Session: session, Branch: root, Next: cell1, At: time.Now().UTC()},
			model.CellChecked{Session: session, Branch: root, Cell: cell2, Source: "const b = 2;", At: time.Now().UTC()},
			model.CellCommitted{Session: session, Branch: root, Cell: cell2, At: time.Now().UTC()},
			model.HeadMoved{Session: session, Branch: root, Previous: cell1, Next: cell2, At: time.Now().UTC()},
		} {
			if err := s.AppendFact(ctx, f); err != nil {
				t.Fatalf("AppendFact(%q): %v", f.FactType(), err)
			}
		}
		return s
	}

	t.Run("manifest comes from durable facts", func(t *testing.T) {
		s := setupRoot(t)
		env, err := s.LoadStaticEnv(ctx, session, root, cell2)
		if err != nil {
			t.Fatalf("LoadStaticEnv: %v", err)
		}
		if env.Manifest.ID != manifest.ID {
			t.Errorf("Manifest.ID: got %q, want %q", env.Manifest.ID, manifest.ID)
		}
		if len(env.Manifest.Functions) != 1 {
			t.Errorf("Manifest.Functions len: got %d, want 1", len(env.Manifest.Functions))
		}
		if env.Manifest.Functions[0].Name != "tools.env.get" {
			t.Errorf("Manifest.Functions[0].Name: got %q", env.Manifest.Functions[0].Name)
		}
	})

	t.Run("root branch full history", func(t *testing.T) {
		s := setupRoot(t)
		env, err := s.LoadStaticEnv(ctx, session, root, cell2)
		if err != nil {
			t.Fatalf("LoadStaticEnv: %v", err)
		}
		want := []string{"const a = 1;", "const b = 2;"}
		if len(env.CommittedSources) != len(want) {
			t.Fatalf("CommittedSources len: got %d, want %d", len(env.CommittedSources), len(want))
		}
		for i, w := range want {
			if env.CommittedSources[i] != w {
				t.Errorf("CommittedSources[%d]: got %q, want %q", i, env.CommittedSources[i], w)
			}
		}
	})

	t.Run("head truncation stops at requested cell", func(t *testing.T) {
		s := setupRoot(t)
		env, err := s.LoadStaticEnv(ctx, session, root, cell1)
		if err != nil {
			t.Fatalf("LoadStaticEnv at cell1: %v", err)
		}
		if env.Head != cell1 {
			t.Errorf("Head: got %q, want %q", env.Head, cell1)
		}
		if len(env.CommittedSources) != 1 {
			t.Fatalf("CommittedSources len: got %d, want 1", len(env.CommittedSources))
		}
		if env.CommittedSources[0] != "const a = 1;" {
			t.Errorf("CommittedSources[0]: got %q", env.CommittedSources[0])
		}
	})

	t.Run("forked branch includes ancestor cells up to fork point", func(t *testing.T) {
		s := setupRoot(t)
		for _, f := range []model.Fact{
			model.BranchCreated{Session: session, Branch: fork, ParentCell: cell1, At: time.Now().UTC()},
			model.CellChecked{Session: session, Branch: fork, Cell: cell3, Source: "const c = 3;", At: time.Now().UTC()},
			model.CellCommitted{Session: session, Branch: fork, Cell: cell3, At: time.Now().UTC()},
			model.HeadMoved{Session: session, Branch: fork, Next: cell3, At: time.Now().UTC()},
		} {
			if err := s.AppendFact(ctx, f); err != nil {
				t.Fatalf("AppendFact(%q): %v", f.FactType(), err)
			}
		}
		env, err := s.LoadStaticEnv(ctx, session, fork, cell3)
		if err != nil {
			t.Fatalf("LoadStaticEnv fork: %v", err)
		}
		// Expect cell1 (root ancestor) then cell3 (fork); cell2 must NOT appear.
		want := []string{"const a = 1;", "const c = 3;"}
		if len(env.CommittedSources) != len(want) {
			t.Fatalf("CommittedSources len: got %d, want %d (got %v)", len(env.CommittedSources), len(want), env.CommittedSources)
		}
		for i, w := range want {
			if env.CommittedSources[i] != w {
				t.Errorf("CommittedSources[%d]: got %q, want %q", i, env.CommittedSources[i], w)
			}
		}
	})

	t.Run("missing session returns error", func(t *testing.T) {
		s := newTestStore(t)
		_, err := s.LoadStaticEnv(ctx, "no-such-session", root, cell1)
		if err == nil {
			t.Fatal("expected error for unknown session, got nil")
		}
	})

	t.Run("branch exists with no committed cells returns empty sources", func(t *testing.T) {
		s := newTestStore(t)
		for _, f := range []model.Fact{
			model.SessionStarted{Session: session, RootBranch: root, At: time.Now().UTC()},
			model.ManifestAttached{Session: session, ManifestID: manifest.ID, Manifest: manifest, At: time.Now().UTC()},
		} {
			if err := s.AppendFact(ctx, f); err != nil {
				t.Fatalf("AppendFact: %v", err)
			}
		}
		env, err := s.LoadStaticEnv(ctx, session, root, "")
		if err != nil {
			t.Fatalf("LoadStaticEnv empty head: %v", err)
		}
		if len(env.CommittedSources) != 0 {
			t.Errorf("CommittedSources: got %v, want empty", env.CommittedSources)
		}
	})
}

// ---------------------------------------------------------------------------
// LoadReplayPlan focused tests — branched fixture
// ---------------------------------------------------------------------------

// TestMemStore_LoadReplayPlan mirrors the SQLite LoadReplayPlan branched test.
//
// Fixture layout (same as SQLite parity test):
//
//	root branch:  cell1 → cell2
//	fork branch:  (forked at cell1)  → cell3
//
// cell1 has one effect (fx1, ReplayReadonly, completed).
// cell2 has one effect (fx2, ReplayNonReplayable, NOT completed).
// cell3 has one effect (fx3, ReplayIdempotent, completed).
func TestMemStore_LoadReplayPlan(t *testing.T) {
	ctx := context.Background()

	const (
		session = model.SessionID("sess-replay")
		root    = model.BranchID("root-rp")
		fork    = model.BranchID("fork-rp")
		cell1   = model.CellID("rp-c1")
		cell2   = model.CellID("rp-c2")
		cell3   = model.CellID("rp-c3")
		fx1     = model.EffectID("rp-fx1")
		fx2     = model.EffectID("rp-fx2")
		fx3     = model.EffectID("rp-fx3")
	)

	buildFixture := func(t *testing.T, s *memstore.Store) {
		t.Helper()
		for _, f := range []model.Fact{
			model.SessionStarted{Session: session, RootBranch: root, At: time.Now().UTC()},
			model.ManifestAttached{
				Session:    session,
				ManifestID: "m-rp",
				Manifest: model.Manifest{
					ID: "m-rp",
					Functions: []model.HostFunctionSpec{
						{Name: "fn.read", ParamsSchema: json.RawMessage(`{}`), ResultSchema: json.RawMessage(`{}`), Replay: model.ReplayReadonly},
						{Name: "fn.send", ParamsSchema: json.RawMessage(`{}`), ResultSchema: json.RawMessage(`{}`), Replay: model.ReplayNonReplayable},
						{Name: "fn.idempotent", ParamsSchema: json.RawMessage(`{}`), ResultSchema: json.RawMessage(`{}`), Replay: model.ReplayIdempotent},
					},
				},
				At: time.Now().UTC(),
			},
			// -- root: cell1 --
			model.CellChecked{Session: session, Branch: root, Cell: cell1, Source: "src1", EmittedJS: "js1", At: time.Now().UTC()},
			model.CellEvaluated{Session: session, Branch: root, Cell: cell1, LinkedEffects: []model.EffectID{fx1}, At: time.Now().UTC()},
			model.EffectStarted{Session: session, Effect: fx1, Cell: cell1, FunctionName: "fn.read", ReplayPolicy: model.ReplayReadonly, At: time.Now().UTC()},
			model.EffectCompleted{Session: session, Effect: fx1, Result: json.RawMessage(`"result1"`), At: time.Now().UTC()},
			model.CellCommitted{Session: session, Branch: root, Cell: cell1, At: time.Now().UTC()},
			model.HeadMoved{Session: session, Branch: root, Next: cell1, At: time.Now().UTC()},
			// -- root: cell2 --
			model.CellChecked{Session: session, Branch: root, Cell: cell2, Source: "src2", EmittedJS: "js2", At: time.Now().UTC()},
			model.CellEvaluated{Session: session, Branch: root, Cell: cell2, LinkedEffects: []model.EffectID{fx2}, At: time.Now().UTC()},
			model.EffectStarted{Session: session, Effect: fx2, Cell: cell2, FunctionName: "fn.send", ReplayPolicy: model.ReplayNonReplayable, At: time.Now().UTC()},
			// fx2 NOT completed.
			model.CellCommitted{Session: session, Branch: root, Cell: cell2, At: time.Now().UTC()},
			model.HeadMoved{Session: session, Branch: root, Previous: cell1, Next: cell2, At: time.Now().UTC()},
			// -- fork at cell1 → cell3 --
			model.BranchCreated{Session: session, Branch: fork, ParentCell: cell1, At: time.Now().UTC()},
			model.CellChecked{Session: session, Branch: fork, Cell: cell3, Source: "src3", EmittedJS: "js3", At: time.Now().UTC()},
			model.CellEvaluated{Session: session, Branch: fork, Cell: cell3, LinkedEffects: []model.EffectID{fx3}, At: time.Now().UTC()},
			model.EffectStarted{Session: session, Effect: fx3, Cell: cell3, FunctionName: "fn.idempotent", ReplayPolicy: model.ReplayIdempotent, At: time.Now().UTC()},
			model.EffectCompleted{Session: session, Effect: fx3, Result: json.RawMessage(`"result3"`), At: time.Now().UTC()},
			model.CellCommitted{Session: session, Branch: fork, Cell: cell3, At: time.Now().UTC()},
			model.HeadMoved{Session: session, Branch: fork, Next: cell3, At: time.Now().UTC()},
		} {
			if err := s.AppendFact(ctx, f); err != nil {
				t.Fatalf("AppendFact(%q): %v", f.FactType(), err)
			}
		}
	}

	t.Run("root-only replay up to cell2", func(t *testing.T) {
		s := newTestStore(t)
		buildFixture(t, s)

		plan, err := s.LoadReplayPlan(ctx, session, cell2)
		if err != nil {
			t.Fatalf("LoadReplayPlan: %v", err)
		}
		if plan.Session != session {
			t.Errorf("Session: got %q, want %q", plan.Session, session)
		}
		if plan.TargetCell != cell2 {
			t.Errorf("TargetCell: got %q, want %q", plan.TargetCell, cell2)
		}
		if len(plan.Steps) != 2 {
			t.Fatalf("Steps len: got %d, want 2", len(plan.Steps))
		}

		step0 := plan.Steps[0]
		if step0.Cell != cell1 {
			t.Errorf("Steps[0].Cell: got %q, want %q", step0.Cell, cell1)
		}
		if step0.Source != "src1" {
			t.Errorf("Steps[0].Source: got %q, want src1", step0.Source)
		}
		if step0.EmittedJS != "js1" {
			t.Errorf("Steps[0].EmittedJS: got %q, want js1", step0.EmittedJS)
		}
		if len(step0.Effects) != 1 || step0.Effects[0] != fx1 {
			t.Errorf("Steps[0].Effects: got %v, want [%q]", step0.Effects, fx1)
		}

		step1 := plan.Steps[1]
		if step1.Cell != cell2 {
			t.Errorf("Steps[1].Cell: got %q, want %q", step1.Cell, cell2)
		}

		if len(plan.Decisions) != 2 {
			t.Fatalf("Decisions len: got %d, want 2", len(plan.Decisions))
		}
		dec1, ok := plan.Decisions[fx1]
		if !ok {
			t.Fatalf("missing decision for %q", fx1)
		}
		if dec1.Policy != model.ReplayReadonly {
			t.Errorf("fx1 Policy: got %q, want ReadOnly", dec1.Policy)
		}
		if string(dec1.RecordedResult) != `"result1"` {
			t.Errorf("fx1 RecordedResult: got %q, want %q", dec1.RecordedResult, `"result1"`)
		}

		dec2, ok := plan.Decisions[fx2]
		if !ok {
			t.Fatalf("missing decision for %q", fx2)
		}
		if dec2.Policy != model.ReplayNonReplayable {
			t.Errorf("fx2 Policy: got %q, want NonReplayable", dec2.Policy)
		}
		if dec2.RecordedResult != nil {
			t.Errorf("fx2 RecordedResult: got %q, want nil (not completed)", dec2.RecordedResult)
		}
	})

	t.Run("forked branch replay includes ancestor cells before fork cells", func(t *testing.T) {
		s := newTestStore(t)
		buildFixture(t, s)

		plan, err := s.LoadReplayPlan(ctx, session, cell3)
		if err != nil {
			t.Fatalf("LoadReplayPlan fork: %v", err)
		}
		if len(plan.Steps) != 2 {
			t.Fatalf("Steps len: got %d, want 2 (cells: %v)", len(plan.Steps), func() []model.CellID {
				ids := make([]model.CellID, len(plan.Steps))
				for i, st := range plan.Steps {
					ids[i] = st.Cell
				}
				return ids
			}())
		}
		if plan.Steps[0].Cell != cell1 {
			t.Errorf("Steps[0].Cell: got %q, want %q (ancestor first)", plan.Steps[0].Cell, cell1)
		}
		if plan.Steps[1].Cell != cell3 {
			t.Errorf("Steps[1].Cell: got %q, want %q (fork cell last)", plan.Steps[1].Cell, cell3)
		}

		if _, ok := plan.Decisions[fx1]; !ok {
			t.Errorf("missing decision for ancestor effect %q", fx1)
		}
		if _, ok := plan.Decisions[fx3]; !ok {
			t.Errorf("missing decision for fork effect %q", fx3)
		}
		if _, ok := plan.Decisions[fx2]; ok {
			t.Errorf("unexpected decision for out-of-ancestry effect %q", fx2)
		}

		dec3 := plan.Decisions[fx3]
		if dec3.Policy != model.ReplayIdempotent {
			t.Errorf("fx3 Policy: got %q, want Idempotent", dec3.Policy)
		}
		if string(dec3.RecordedResult) != `"result3"` {
			t.Errorf("fx3 RecordedResult: got %q, want %q", dec3.RecordedResult, `"result3"`)
		}
	})

	t.Run("root-only replay stopping at first cell", func(t *testing.T) {
		s := newTestStore(t)
		buildFixture(t, s)

		plan, err := s.LoadReplayPlan(ctx, session, cell1)
		if err != nil {
			t.Fatalf("LoadReplayPlan at cell1: %v", err)
		}
		if len(plan.Steps) != 1 {
			t.Fatalf("Steps len: got %d, want 1", len(plan.Steps))
		}
		if plan.Steps[0].Cell != cell1 {
			t.Errorf("Steps[0].Cell: got %q, want %q", plan.Steps[0].Cell, cell1)
		}
	})

	t.Run("unknown target cell returns error", func(t *testing.T) {
		s := newTestStore(t)
		buildFixture(t, s)

		_, err := s.LoadReplayPlan(ctx, session, "no-such-cell")
		if err == nil {
			t.Fatal("expected error for unknown target cell, got nil")
		}
	})

	t.Run("unknown session returns error", func(t *testing.T) {
		s := newTestStore(t)
		_, err := s.LoadReplayPlan(ctx, "no-such-session", cell1)
		if err == nil {
			t.Fatal("expected error for unknown session, got nil")
		}
	})

	t.Run("effect without completion row yields nil RecordedResult", func(t *testing.T) {
		s := newTestStore(t)
		const (
			sess2  = model.SessionID("sess-rp-nocompl")
			br2    = model.BranchID("br2")
			c2     = model.CellID("c2-nocompl")
			fxMiss = model.EffectID("fx-missing-compl")
		)
		for _, f := range []model.Fact{
			model.SessionStarted{Session: sess2, RootBranch: br2, At: time.Now().UTC()},
			model.ManifestAttached{Session: sess2, ManifestID: "m2", Manifest: model.Manifest{ID: "m2"}, At: time.Now().UTC()},
			model.CellChecked{Session: sess2, Branch: br2, Cell: c2, Source: "s", EmittedJS: "j", At: time.Now().UTC()},
			model.CellEvaluated{Session: sess2, Branch: br2, Cell: c2, LinkedEffects: []model.EffectID{fxMiss}, At: time.Now().UTC()},
			model.EffectStarted{Session: sess2, Effect: fxMiss, Cell: c2, FunctionName: "fn.read", ReplayPolicy: model.ReplayReadonly, At: time.Now().UTC()},
			// No EffectCompleted appended — simulates crash.
			model.CellCommitted{Session: sess2, Branch: br2, Cell: c2, At: time.Now().UTC()},
		} {
			if err := s.AppendFact(ctx, f); err != nil {
				t.Fatalf("AppendFact(%q): %v", f.FactType(), err)
			}
		}

		plan, err := s.LoadReplayPlan(ctx, sess2, c2)
		if err != nil {
			t.Fatalf("LoadReplayPlan with incomplete effect: %v", err)
		}
		dec, ok := plan.Decisions[fxMiss]
		if !ok {
			t.Fatalf("missing decision for effect %q", fxMiss)
		}
		if dec.RecordedResult != nil {
			t.Errorf("RecordedResult: got %v, want nil for incomplete effect", dec.RecordedResult)
		}
	})
}

// ---------------------------------------------------------------------------
// Concurrent append / head-race test
// ---------------------------------------------------------------------------

// TestMemStore_ConcurrentAppendHeadRace proves that concurrent AppendFact calls
// do not cause data races on the in-memory store. Run with go test -race.
func TestMemStore_ConcurrentAppendHeadRace(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	const (
		session    = model.SessionID("sess-race")
		branch     = model.BranchID("br-race")
		numWriters = 16
	)

	if err := s.AppendFact(ctx, model.SessionStarted{
		Session:    session,
		RootBranch: branch,
		At:         time.Now().UTC(),
	}); err != nil {
		t.Fatalf("AppendFact SessionStarted: %v", err)
	}

	cells := make([]model.CellID, numWriters)
	for i := range cells {
		cells[i] = model.CellID(fmt.Sprintf("race-cell-%02d", i))
	}

	var wg sync.WaitGroup
	errs := make(chan error, numWriters)

	for i, cell := range cells {
		wg.Add(1)
		go func(idx int, c model.CellID) {
			defer wg.Done()
			for _, f := range []model.Fact{
				model.CellChecked{Session: session, Branch: branch, Cell: c, Source: fmt.Sprintf("src-%d", idx), At: time.Now().UTC()},
				model.CellCommitted{Session: session, Branch: branch, Cell: c, At: time.Now().UTC()},
				model.HeadMoved{Session: session, Branch: branch, Next: c, At: time.Now().UTC()},
			} {
				if err := s.AppendFact(ctx, f); err != nil {
					errs <- fmt.Errorf("writer %d AppendFact(%q): %w", idx, f.FactType(), err)
					return
				}
			}
		}(i, cell)
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent write error: %v", err)
	}

	head, err := s.LoadHead(ctx, session, branch)
	if err != nil {
		t.Fatalf("LoadHead after race: %v", err)
	}

	found := false
	for _, c := range cells {
		if head.Head == c {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("LoadHead returned unexpected head %q (not in written cells)", head.Head)
	}

	// Concurrent readers alongside one final writer.
	var readWg sync.WaitGroup
	readErrs := make(chan error, numWriters)
	for i := 0; i < numWriters; i++ {
		readWg.Add(1)
		go func() {
			defer readWg.Done()
			if _, err := s.LoadHead(ctx, session, branch); err != nil {
				readErrs <- fmt.Errorf("concurrent read LoadHead: %w", err)
			}
		}()
	}
	readWg.Wait()
	close(readErrs)
	for err := range readErrs {
		t.Errorf("concurrent read error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Verify store.Store interface satisfaction at compile time
// ---------------------------------------------------------------------------

// TestMemStore_SatisfiesStoreInterface is a compile-time assertion that
// *memstore.Store implements store.Store.
func TestMemStore_SatisfiesStoreInterface(t *testing.T) {
	// This test only exercises the compile-time check. The assignment below
	// would fail to compile if *Store did not satisfy store.Store.
	var _ interface {
		AppendFact(ctx context.Context, fact model.Fact) error
	} = (*memstore.Store)(nil)
}
