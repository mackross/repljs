package engine

import (
	"strings"
	"testing"

	"github.com/solidarity-ai/repl/model"
)

func TestSubmitCheckFailure_ErrorIncludesDiagnostics(t *testing.T) {
	err := (&SubmitCheckFailure{
		Language: model.CellLanguageTypeScript,
		Diagnostics: []model.Diagnostic{
			{
				Message:  "Type 'number' is not assignable to type 'string'.",
				Severity: "error",
				Line:     1,
				Column:   14,
			},
			{
				Message:  "Unused local.",
				Severity: "warning",
				Line:     2,
				Column:   1,
			},
		},
	}).Error()

	wantContains := []string{
		"TS Err:\n",
		"1:14: error: Type 'number' is not assignable to type 'string'.",
		"2:1: warning: Unused local.",
		"\n2:1: warning: Unused local.",
	}
	for _, want := range wantContains {
		if !strings.Contains(err, want) {
			t.Fatalf("Error() missing %q\nfull error:\n%s", want, err)
		}
	}
}
