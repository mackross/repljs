package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dop251/goja"
	"github.com/solidarity-ai/repl/engine"
	"github.com/solidarity-ai/repl/jswire"
	"github.com/solidarity-ai/repl/model"
)

func TestPrintSubmitError_IncludesLinkedEffects(t *testing.T) {
	var out bytes.Buffer

	printSubmitError(&out, &engine.SubmitFailure{
		Failure:      model.FailureID("failure-1"),
		Parent:       model.CellID("cell-1"),
		Phase:        "eval",
		ErrorMessage: "boom",
		LinkedEffects: []engine.EffectSummary{
			{
				Effect:       model.EffectID("effect-1"),
				FunctionName: "tools.fetch",
				Params: func() []byte {
					b, err := jswire.EncodeGoja(goja.New().ToValue(map[string]any{"url": "https://example.com"}))
					if err != nil {
						t.Fatalf("EncodeGoja params: %v", err)
					}
					return b
				}(),
				ErrorMessage: "network down",
				ReplayPolicy: model.ReplayReadonly,
				Status:       engine.EffectStatusFailed,
			},
		},
	})

	got := out.String()
	wantContains := []string{
		"submit error: session: Submit: eval: boom",
		"failure.id=failure-1",
		"failure.parent=cell-1",
		"failure.phase=eval",
		"linked_effects=1",
		"effect[0].id=effect-1",
		"effect[0].function=tools.fetch",
		"effect[0].status=failed",
		`effect[0].params={"url":"https://example.com"}`,
		`effect[0].error="network down"`,
	}
	for _, want := range wantContains {
		if !bytes.Contains([]byte(got), []byte(want)) {
			t.Fatalf("output missing %q\nfull output:\n%s", want, got)
		}
	}
}

func TestRun_SQLiteResumesLatestSession(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "repl.sqlite")

	var firstOut, firstErr bytes.Buffer
	firstIn := strings.NewReader("const a = 1\n:quit\n")
	if err := run(firstIn, &firstOut, &firstErr, []string{"-backend=sqlite", "-sqlite-path=" + dbPath}); err != nil {
		t.Fatalf("first run: %v\nstderr:\n%s", err, firstErr.String())
	}

	firstOutput := firstOut.String()
	if !strings.Contains(firstOutput, "resumed=false") {
		t.Fatalf("first run should start a fresh session, output:\n%s", firstOutput)
	}
	firstSession := extractSessionID(t, firstOutput)

	var secondOut, secondErr bytes.Buffer
	secondIn := strings.NewReader("a\n:quit\n")
	if err := run(secondIn, &secondOut, &secondErr, []string{"-backend=sqlite", "-sqlite-path=" + dbPath}); err != nil {
		t.Fatalf("second run: %v\nstderr:\n%s", err, secondErr.String())
	}

	secondOutput := secondOut.String()
	if !strings.Contains(secondOutput, "resumed=true") {
		t.Fatalf("second run should resume prior session, output:\n%s", secondOutput)
	}
	if extractSessionID(t, secondOutput) != firstSession {
		t.Fatalf("second run resumed a different session\nfirst:\n%s\nsecond:\n%s", firstOutput, secondOutput)
	}
	if strings.Contains(secondOutput, "ReferenceError") {
		t.Fatalf("second run should preserve bindings, output:\n%s", secondOutput)
	}
	if !strings.Contains(secondOutput, `completion.preview="1"`) {
		t.Fatalf("second run should read persisted binding, output:\n%s", secondOutput)
	}
}

func extractSessionID(t *testing.T, output string) string {
	t.Helper()
	for _, line := range strings.Split(output, "\n") {
		if !strings.HasPrefix(line, "session=") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			break
		}
		return strings.TrimPrefix(fields[0], "session=")
	}
	t.Fatalf("missing session line in output:\n%s", output)
	return ""
}
