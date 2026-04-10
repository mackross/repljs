package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/solidarity-ai/repl/engine"
	"github.com/solidarity-ai/repl/model"
	"github.com/solidarity-ai/repl/session"
	memstore "github.com/solidarity-ai/repl/store/mem"
)

func TestFetchDelegate_ResponseMethodsRemainAvailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("9\n"))
	}))
	defer srv.Close()

	ctx := context.Background()
	eng := session.New()
	sess, err := eng.StartSession(ctx, model.SessionConfig{Manifest: defaultManifest()}, engine.SessionDeps{
		Store:      memstore.New(),
		VMDelegate: fetchDelegate{},
	})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	defer sess.Close()

	res, err := sess.Submit(ctx, `(async () => {
		const r = await fetch("`+srv.URL+`")
		return [r.status, r.ok, await r.text()]
	})()`)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if res.CompletionValue == nil {
		t.Fatal("expected completion value")
	}
	if res.CompletionValue.Preview != "200,true,9\n" {
		t.Fatalf("preview = %q, want %q", res.CompletionValue.Preview, "200,true,9\n")
	}
}
