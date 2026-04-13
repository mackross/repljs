package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dop251/goja"
	"github.com/mackross/repljs/engine"
	"github.com/mackross/repljs/jswire"
	"github.com/mackross/repljs/model"
	"github.com/mackross/repljs/session"
	storemem "github.com/mackross/repljs/store/mem"
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

func TestRun_MultilineBlankSubmitDoesNotCreateCell(t *testing.T) {
	var out, errOut bytes.Buffer
	in := strings.NewReader(":submit\n.end\n1\n:quit\n")
	if err := run(in, &out, &errOut, nil); err != nil {
		t.Fatalf("run: %v\nstderr:\n%s", err, errOut.String())
	}

	got := out.String()
	if strings.Count(got, "ok cell=") != 1 {
		t.Fatalf("blank multiline submit should not create a cell:\n%s", got)
	}
	if !strings.Contains(got, "cell.index=1") {
		t.Fatalf("first real submit should still be index 1:\n%s", got)
	}
}

func TestRun_ModeSwitchesBetweenJavaScriptAndTypeScript(t *testing.T) {
	var out, errOut bytes.Buffer
	in := strings.NewReader(":ts\nconst typed: number = 1\n:js\ntyped + 1\n:quit\n")
	if err := run(in, &out, &errOut, nil); err != nil {
		t.Fatalf("run: %v\nstderr:\n%s", err, errOut.String())
	}

	got := out.String()
	if !strings.Contains(got, "mode=ts") || !strings.Contains(got, "mode=js") {
		t.Fatalf("mode switch output missing:\n%s", got)
	}
	if !strings.Contains(got, "cell.language=ts") {
		t.Fatalf("TS submit output missing language marker:\n%s", got)
	}
	if !strings.Contains(got, `completion.preview="2"`) {
		t.Fatalf("JS cell after TS mode should still see prior binding:\n%s", got)
	}
}

func TestRun_TypeScriptModeSeesBuiltInRuntimeHelpers(t *testing.T) {
	var out, errOut bytes.Buffer
	in := strings.NewReader(":ts\n1\nconsole.log($val(1), $last); const next = ($last as number) + ($val(1) as number); next\n:quit\n")
	if err := run(in, &out, &errOut, nil); err != nil {
		t.Fatalf("run: %v\nstderr:\n%s", err, errOut.String())
	}

	got := out.String()
	if strings.Contains(got, "TS Err:") {
		t.Fatalf("TS built-ins should be declared, output:\n%s", got)
	}
	if !strings.Contains(got, `log[0]=1 1`) {
		t.Fatalf("console.log should accept TS-visible built-ins:\n%s", got)
	}
	if !strings.Contains(got, `completion.preview="2"`) {
		t.Fatalf("$last/$val should be usable in TS mode:\n%s", got)
	}
}

func TestRun_TypeScriptModeBuiltInsAreAny(t *testing.T) {
	var out, errOut bytes.Buffer
	in := strings.NewReader(":ts\n({ greet() { return 'hi' }, nested: { value: 2 } })\n$last.greet() + String($val(1).nested.value)\n:quit\n")
	if err := run(in, &out, &errOut, nil); err != nil {
		t.Fatalf("run: %v\nstderr:\n%s", err, errOut.String())
	}

	got := out.String()
	if strings.Contains(got, "TS Err:") {
		t.Fatalf("TS built-ins should allow direct property/method access:\n%s", got)
	}
	if !strings.Contains(got, `completion.preview="hi2"`) {
		t.Fatalf("expected raw string preview to work:\n%s", got)
	}
	if !strings.Contains(got, `completion.summary="\"hi2\""`) {
		t.Fatalf("expected direct any-style access to work:\n%s", got)
	}
}

func TestRun_TypeScriptModeAllowsImplicitAnyInExploratoryCells(t *testing.T) {
	var out, errOut bytes.Buffer
	in := strings.NewReader(":ts\nconst pick = (m) => m.subject\nconst msgs = [{ subject: 'a' }]\nmsgs.map(pick).join(',')\n:quit\n")
	if err := run(in, &out, &errOut, nil); err != nil {
		t.Fatalf("run: %v\nstderr:\n%s", err, errOut.String())
	}

	got := out.String()
	if strings.Contains(got, "TS Err:") {
		t.Fatalf("exploratory cells should allow implicit any parameters:\n%s", got)
	}
	if !strings.Contains(got, `completion.preview="a"`) {
		t.Fatalf("expected mapped preview to work:\n%s", got)
	}
}

func TestRun_TypeScriptModeLastArrayCastRemainsCallable(t *testing.T) {
	var out, errOut bytes.Buffer
	in := strings.NewReader(":ts\n({ messages: [{ subject: 'a' }, { subject: 'b' }] })\n($last.messages as any[]).map(m => m.subject).join(',')\n:quit\n")
	if err := run(in, &out, &errOut, nil); err != nil {
		t.Fatalf("run: %v\nstderr:\n%s", err, errOut.String())
	}

	got := out.String()
	if strings.Contains(got, "TS Err:") {
		t.Fatalf("$last any[] cast should preserve array methods:\n%s", got)
	}
	if !strings.Contains(got, `completion.preview="a,b"`) {
		t.Fatalf("expected mapped preview to work:\n%s", got)
	}
}

func TestRun_TypeScriptCheckFailurePrintsDiagnostics(t *testing.T) {
	var out, errOut bytes.Buffer
	in := strings.NewReader(":ts\nconst typed: string = 1\n:quit\n")
	if err := run(in, &out, &errOut, nil); err != nil {
		t.Fatalf("run: %v\nstderr:\n%s", err, errOut.String())
	}

	got := out.String()
	if !strings.Contains(got, "submit error: TS Err:") {
		t.Fatalf("missing TS submit error:\n%s", got)
	}
	if !strings.Contains(got, "cell.language=ts") {
		t.Fatalf("missing TS cell language on error:\n%s", got)
	}
	if !strings.Contains(got, "diagnostics=1") {
		t.Fatalf("missing diagnostic count:\n%s", got)
	}
	if !strings.Contains(got, `diagnostic[0].severity=error`) {
		t.Fatalf("missing diagnostic severity:\n%s", got)
	}
	if !strings.Contains(got, `diagnostic[0].location=1:`) {
		t.Fatalf("missing diagnostic location:\n%s", got)
	}
	if !strings.Contains(got, `Type 'number' is not assignable to type 'string'`) {
		t.Fatalf("missing diagnostic message:\n%s", got)
	}
	if strings.Contains(got, "cell.index=") {
		t.Fatalf("TS check failures should not claim a committed branch index:\n%s", got)
	}
}

func TestRun_TypeScriptModeAllowsTopLevelAwaitOnBuiltIns(t *testing.T) {
	var out, errOut bytes.Buffer
	in := strings.NewReader(":ts\nPromise.resolve(7)\nawait $last\n:quit\n")
	if err := run(in, &out, &errOut, nil); err != nil {
		t.Fatalf("run: %v\nstderr:\n%s", err, errOut.String())
	}

	got := out.String()
	if strings.Contains(got, "TS Err:") {
		t.Fatalf("top-level await should typecheck in TS mode:\n%s", got)
	}
	if !strings.Contains(got, `completion.preview="7"`) {
		t.Fatalf("await $last should resolve previous promise value:\n%s", got)
	}
}

func TestRun_TypeScriptModeTypeAliasThenTypedValueThenReadField(t *testing.T) {
	var out, errOut bytes.Buffer
	in := strings.NewReader(":ts\ntype User = { name: string, age: number }\nconst user: User = { name: \"Ada\", age: 36 }\nuser.age\n:quit\n")
	if err := run(in, &out, &errOut, nil); err != nil {
		t.Fatalf("run: %v\nstderr:\n%s", err, errOut.String())
	}

	got := out.String()
	if strings.Contains(got, "TS Err:") {
		t.Fatalf("type-only TS cell and subsequent typed cells should submit cleanly:\n%s", got)
	}
	if strings.Count(got, "ok cell=") != 3 {
		t.Fatalf("expected three successful cells:\n%s", got)
	}
	if !strings.Contains(got, "cell.index=1") || !strings.Contains(got, "cell.index=2") || !strings.Contains(got, "cell.index=3") {
		t.Fatalf("expected three committed indices:\n%s", got)
	}
	if !strings.Contains(got, `completion.preview="36"`) {
		t.Fatalf("third cell should evaluate user.age to 36:\n%s", got)
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
	if !strings.Contains(got, `log[0]=ok [object Object]`) {
		t.Fatalf("success log missing from output:\n%s", got)
	}
	if !strings.Contains(got, `log[0]=bad [object Object]`) {
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
	if !strings.Contains(got, `log[0]=hello [object Object]`) {
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
