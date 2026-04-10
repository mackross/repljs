package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dop251/goja"
	"github.com/solidarity-ai/repl/engine"
	"github.com/solidarity-ai/repl/jswire"
	"github.com/solidarity-ai/repl/model"
	"github.com/solidarity-ai/repl/session"
	storemem "github.com/solidarity-ai/repl/store/mem"
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
		`effect[0].params={url: "https://example.com"}`,
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

func TestRun_PrintsCompletionSummary(t *testing.T) {
	var out, errOut bytes.Buffer
	in := strings.NewReader(`({name: "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx", items: [1, 2, 3, 4, 5, 6]})
:quit
`)
	if err := run(in, &out, &errOut, nil); err != nil {
		t.Fatalf("run: %v\nstderr:\n%s", err, errOut.String())
	}

	got := out.String()
	if !strings.Contains(got, `cell.index=1`) {
		t.Fatalf("run output missing cell index:\n%s", got)
	}
	if !strings.Contains(got, `completion.summary=`) ||
		!strings.Contains(got, `string(45) \"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx…\"`) ||
		!strings.Contains(got, `Array(6)[1, 2, 3, 4, …+2]`) {
		t.Fatalf("run output missing bounded completion summary:\n%s", got)
	}
}

func TestRun_TopLevelObjectLiteralIsTreatedAsExpression(t *testing.T) {
	var out, errOut bytes.Buffer
	in := strings.NewReader("{ a: \"hello\" }\n:quit\n")
	if err := run(in, &out, &errOut, nil); err != nil {
		t.Fatalf("run: %v\nstderr:\n%s", err, errOut.String())
	}

	got := out.String()
	if !strings.Contains(got, `completion.preview="[object Object]"`) {
		t.Fatalf("run output should preserve object completion preview:\n%s", got)
	}
	if !strings.Contains(got, `completion.summary="{a: \"hello\"}"`) {
		t.Fatalf("run output should summarize object literal, got:\n%s", got)
	}
}

func TestCmdInspect_PrintsSummaryAndFull(t *testing.T) {
	ctx := context.Background()
	eng := session.New()
	sess, err := eng.StartSession(ctx, model.SessionConfig{Manifest: defaultManifest()}, engine.SessionDeps{
		Store: storemem.New(),
	})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	defer sess.Close()

	res, err := sess.Submit(ctx, `({
		name: "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
		items: [1, 2, 3, 4, 5, 6]
	})`)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if res.CompletionValue == nil {
		t.Fatal("expected completion value")
	}

	var out bytes.Buffer
	if err := cmdInspect(ctx, &out, sess, res.CompletionValue.ID); err != nil {
		t.Fatalf("cmdInspect: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, `summary=`) ||
		!strings.Contains(got, `string(45) \"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx…\"`) ||
		!strings.Contains(got, `Array(6)[1, 2, 3, 4, …+2]`) {
		t.Fatalf("inspect output missing summary:\n%s", got)
	}
	if !strings.Contains(got, `full=`) ||
		!strings.Contains(got, `\"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx\"`) ||
		!strings.Contains(got, `Array(6)[1, 2, 3, 4, 5, 6]`) {
		t.Fatalf("inspect output missing full rendering:\n%s", got)
	}
}

func TestRun_PrintsConsoleLogOnSuccessAndFailure(t *testing.T) {
	var out, errOut bytes.Buffer
	in := strings.NewReader("console.log(\"ok\", { a: 1 }); 1\nconsole.log(\"bad\", { a: 2 }); missingName\n:quit\n")
	if err := run(in, &out, &errOut, nil); err != nil {
		t.Fatalf("run: %v\nstderr:\n%s", err, errOut.String())
	}

	got := out.String()
	if !strings.Contains(got, `log[0]="ok" {a: 1}`) {
		t.Fatalf("success log missing from output:\n%s", got)
	}
	if !strings.Contains(got, `log[0]="bad" {a: 2}`) {
		t.Fatalf("failure log missing from output:\n%s", got)
	}
}

func TestCmdLogs_ReplaysCellLogs(t *testing.T) {
	ctx := context.Background()
	eng := session.New()
	sess, err := eng.StartSession(ctx, model.SessionConfig{Manifest: defaultManifest()}, engine.SessionDeps{
		Store: storemem.New(),
	})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	defer sess.Close()

	res, err := sess.Submit(ctx, `console.log("hello", { a: 1 }); 1`)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	var out bytes.Buffer
	if err := cmdLogs(ctx, &out, sess, res.Cell); err != nil {
		t.Fatalf("cmdLogs: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, `log[0]="hello" {a: 1}`) {
		t.Fatalf(":logs output missing replayed log line:\n%s", got)
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
