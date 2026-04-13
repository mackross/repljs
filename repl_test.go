package repl_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/dop251/goja"
	repl "github.com/mackross/repljs"
	"github.com/mackross/repljs/store/mem"
)

type transitionDelegate struct{}

func (transitionDelegate) ConfigureRuntime(repl.SessionRuntimeContext, *goja.Runtime, repl.HostFuncBuilder, json.RawMessage) (json.RawMessage, error) {
	return nil, nil
}

func (transitionDelegate) TransitionRuntime(repl.RuntimeTransitionContext, *goja.Runtime, repl.HostFuncBuilder, json.RawMessage, json.RawMessage) error {
	return nil
}

func TestTopLevelPackageSurface(t *testing.T) {
	var eng repl.Engine = repl.New()
	if eng == nil {
		t.Fatal("New returned nil engine")
	}

	var tsFactory repl.TypeScriptFactory = repl.NewTypeScriptFactory()
	if tsFactory == nil {
		t.Fatal("NewTypeScriptFactory returned nil factory")
	}

	var st repl.Store = mem.New()
	if st == nil {
		t.Fatal("mem.New returned nil store")
	}

	deps := repl.SessionDeps{
		Store:             st,
		RuntimeMode:       repl.RuntimeModePersistent,
		TypeScriptFactory: tsFactory,
		TypeScriptEnvProvider: func(context.Context, repl.TypeScriptEnvContext) (repl.TypeScriptEnv, error) {
			return repl.TypeScriptEnv{}, nil
		},
	}
	if deps.Store == nil {
		t.Fatal("SessionDeps store should be set")
	}

	var delegate repl.VMTransitionDelegate = transitionDelegate{}
	if delegate == nil {
		t.Fatal("transition delegate should compile through top-level aliases")
	}
	_ = repl.RuntimeTransitionContext{}

	_ = repl.SubmitInput{Source: "1 + 1", Language: repl.CellLanguageJavaScript}
	_ = repl.SessionConfig{
		Manifest: repl.Manifest{
			ID: "manifest-1",
			Functions: []repl.HostFunctionSpec{
				{Name: "tools.echo", Replay: repl.ReplayReadonly},
			},
		},
	}
}
