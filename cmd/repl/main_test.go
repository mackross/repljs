package main

import (
	"bytes"
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
