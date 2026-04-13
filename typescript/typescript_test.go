package typescript_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mackross/repljs/typescript"
)

func TestSession_CheckEmitCell_UsesCommittedHistory(t *testing.T) {
	ctx := context.Background()
	factory := typescript.NewFactory()
	sess, err := factory.NewSession(ctx)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Close()

	sess.SetCommittedSources([]string{"const x = 1"})
	result, err := sess.CheckEmitCell(ctx, typescript.Env{}, "const y: number = x + 1; y")
	if err != nil {
		t.Fatalf("CheckEmitCell: %v", err)
	}
	if result.HasErrors {
		t.Fatalf("HasErrors = true, diagnostics = %#v", result.Diagnostics)
	}
	if !strings.Contains(result.EmittedJS, "const y = x + 1;") {
		t.Fatalf("EmittedJS = %q, want stripped type annotation", result.EmittedJS)
	}
}

func TestSession_CheckEmitCell_MapsDiagnosticsToCurrentCell(t *testing.T) {
	ctx := context.Background()
	factory := typescript.NewFactory()
	sess, err := factory.NewSession(ctx)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Close()

	sess.SetCommittedSources([]string{"const earlier = 1"})
	result, err := sess.CheckEmitCell(ctx, typescript.Env{}, "const later: string = earlier")
	if err != nil {
		t.Fatalf("CheckEmitCell: %v", err)
	}
	if !result.HasErrors {
		t.Fatal("expected type error")
	}
	if len(result.Diagnostics) == 0 {
		t.Fatal("expected diagnostics")
	}
	if result.Diagnostics[0].Line != 1 {
		t.Fatalf("diagnostic line = %d, want 1", result.Diagnostics[0].Line)
	}
	if result.Diagnostics[0].Column < 1 {
		t.Fatalf("diagnostic column = %d, want >= 1", result.Diagnostics[0].Column)
	}
}

func TestSession_CheckEmitCell_SeparatesCommittedExpressionCells(t *testing.T) {
	ctx := context.Background()
	factory := typescript.NewFactory()
	sess, err := factory.NewSession(ctx)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Close()

	sess.SetCommittedSources([]string{"({ value: 1 })"})
	result, err := sess.CheckEmitCell(ctx, typescript.Env{}, "([1, 2] as any[]).map(String).join(',')")
	if err != nil {
		t.Fatalf("CheckEmitCell: %v", err)
	}
	if result.HasErrors {
		t.Fatalf("committed expression cell should not merge with current cell, diagnostics = %#v", result.Diagnostics)
	}
	if !strings.Contains(result.EmittedJS, "[1, 2].map(String).join(\",\")") {
		t.Fatalf("EmittedJS = %q, want array map expression preserved", result.EmittedJS)
	}
}

func TestSession_CheckEmitCell_AllowsTopLevelAwait(t *testing.T) {
	ctx := context.Background()
	factory := typescript.NewFactory()
	sess, err := factory.NewSession(ctx)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Close()

	result, err := sess.CheckEmitCell(ctx, typescript.Env{}, "await $last")
	if err != nil {
		t.Fatalf("CheckEmitCell: %v", err)
	}
	if result.HasErrors {
		t.Fatalf("HasErrors = true, diagnostics = %#v", result.Diagnostics)
	}
	if strings.Contains(result.EmittedJS, "export {}") {
		t.Fatalf("EmittedJS = %q, want checker-only module marker", result.EmittedJS)
	}
	if !strings.Contains(result.EmittedJS, "await $last;") {
		t.Fatalf("EmittedJS = %q, want top-level await preserved", result.EmittedJS)
	}
}

func TestSession_CheckEmitCell_RejectsNamespaceDeclarations(t *testing.T) {
	ctx := context.Background()
	factory := typescript.NewFactory()
	sess, err := factory.NewSession(ctx)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Close()

	result, err := sess.CheckEmitCell(ctx, typescript.Env{}, "const ok = 1\nnamespace Counter { export let value = ok }\n")
	if err != nil {
		t.Fatalf("CheckEmitCell: %v", err)
	}
	if !result.HasErrors {
		t.Fatalf("expected namespace declaration to be rejected, diagnostics = %#v", result.Diagnostics)
	}
	if len(result.Diagnostics) != 1 {
		t.Fatalf("diagnostics len = %d, want 1", len(result.Diagnostics))
	}
	if result.Diagnostics[0].Line != 2 || result.Diagnostics[0].Column != 1 {
		t.Fatalf("diagnostic location = %d:%d, want 2:1", result.Diagnostics[0].Line, result.Diagnostics[0].Column)
	}
	if !strings.Contains(result.Diagnostics[0].Message, "namespace/module declarations are not supported") {
		t.Fatalf("diagnostic message = %q", result.Diagnostics[0].Message)
	}
}

func TestSession_CheckEmitCell_RejectsInternalModuleDeclarations(t *testing.T) {
	ctx := context.Background()
	factory := typescript.NewFactory()
	sess, err := factory.NewSession(ctx)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Close()

	result, err := sess.CheckEmitCell(ctx, typescript.Env{}, "module Counter { export const value = 1 }\n")
	if err != nil {
		t.Fatalf("CheckEmitCell: %v", err)
	}
	if !result.HasErrors {
		t.Fatalf("expected module declaration to be rejected, diagnostics = %#v", result.Diagnostics)
	}
	if len(result.Diagnostics) != 1 {
		t.Fatalf("diagnostics len = %d, want 1", len(result.Diagnostics))
	}
	if result.Diagnostics[0].Line != 1 || result.Diagnostics[0].Column != 1 {
		t.Fatalf("diagnostic location = %d:%d, want 1:1", result.Diagnostics[0].Line, result.Diagnostics[0].Column)
	}
}

func TestSession_CheckEmitCell_DoesNotFalsePositiveOnNamespaceTextInStringsOrComments(t *testing.T) {
	ctx := context.Background()
	factory := typescript.NewFactory()
	sess, err := factory.NewSession(ctx)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Close()

	src := strings.Join([]string{
		`const a = "namespace Counter { }"`,
		`const b = 'module Counter { }'`,
		"const c = `namespace ${1 + 1}`",
		"// namespace Hidden { }",
		"/* module Hidden { } */",
		"const value: number = 2",
		"value",
	}, "\n")
	result, err := sess.CheckEmitCell(ctx, typescript.Env{}, src)
	if err != nil {
		t.Fatalf("CheckEmitCell: %v", err)
	}
	if result.HasErrors {
		t.Fatalf("unexpected diagnostics = %#v", result.Diagnostics)
	}
	if !strings.Contains(result.EmittedJS, "const value = 2;") {
		t.Fatalf("EmittedJS = %q, want ordinary TS emit to continue working", result.EmittedJS)
	}
}

func TestSession_CheckEmitCell_AllowsAmbientDeclareNamespace(t *testing.T) {
	ctx := context.Background()
	factory := typescript.NewFactory()
	sess, err := factory.NewSession(ctx)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Close()

	src := strings.Join([]string{
		"declare namespace Counter {",
		"  const value: number",
		"}",
		"const value: number = 2",
		"value",
	}, "\n")
	result, err := sess.CheckEmitCell(ctx, typescript.Env{}, src)
	if err != nil {
		t.Fatalf("CheckEmitCell: %v", err)
	}
	if result.HasErrors {
		t.Fatalf("unexpected diagnostics = %#v", result.Diagnostics)
	}
	if strings.Contains(result.EmittedJS, "namespace") || strings.Contains(result.EmittedJS, "Counter") {
		t.Fatalf("EmittedJS = %q, want ambient declaration to stay type-only", result.EmittedJS)
	}
	if !strings.Contains(result.EmittedJS, "const value = 2;") {
		t.Fatalf("EmittedJS = %q, want ordinary runtime code to remain", result.EmittedJS)
	}
}
