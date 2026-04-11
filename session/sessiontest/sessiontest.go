package sessiontest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/solidarity-ai/repl/engine"
	"github.com/solidarity-ai/repl/model"
	"github.com/solidarity-ai/repl/session"
	memstore "github.com/solidarity-ai/repl/store/mem"
)

type Harness struct {
	Persistent engine.Session
	Replay     engine.Session
}

type SubmitPair struct {
	PersistentResult engine.SubmitResult
	PersistentErr    error
	PersistentView   *engine.ValueView

	ReplayResult engine.SubmitResult
	ReplayErr    error
	ReplayView   *engine.ValueView
}

func StartComparableSessions(ctx context.Context, cfg model.SessionConfig, delegate engine.VMDelegate, runtimeConfig json.RawMessage) (*Harness, error) {
	return StartComparableSessionsWithDeps(ctx, cfg, engine.SessionDeps{
		VMDelegate:    delegate,
		RuntimeConfig: runtimeConfig,
	})
}

func StartComparableSessionsWithDeps(ctx context.Context, cfg model.SessionConfig, deps engine.SessionDeps) (*Harness, error) {
	eng := session.New()

	persistent, err := eng.StartSession(ctx, cfg, engine.SessionDeps{
		Store:                 memstore.New(),
		VMDelegate:            deps.VMDelegate,
		RuntimeConfig:         append(json.RawMessage(nil), deps.RuntimeConfig...),
		RuntimeMode:           engine.RuntimeModePersistent,
		TypeScriptFactory:     deps.TypeScriptFactory,
		TypeScriptEnvProvider: deps.TypeScriptEnvProvider,
	})
	if err != nil {
		return nil, fmt.Errorf("start persistent session: %w", err)
	}

	replay, err := eng.StartSession(ctx, cfg, engine.SessionDeps{
		Store:                 memstore.New(),
		VMDelegate:            deps.VMDelegate,
		RuntimeConfig:         append(json.RawMessage(nil), deps.RuntimeConfig...),
		RuntimeMode:           engine.RuntimeModeReplayPerSubmit,
		TypeScriptFactory:     deps.TypeScriptFactory,
		TypeScriptEnvProvider: deps.TypeScriptEnvProvider,
	})
	if err != nil {
		_ = persistent.Close()
		return nil, fmt.Errorf("start replay-per-submit session: %w", err)
	}

	return &Harness{Persistent: persistent, Replay: replay}, nil
}

func (h *Harness) Close() error {
	var firstErr error
	if h.Persistent != nil {
		if err := h.Persistent.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if h.Replay != nil {
		if err := h.Replay.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (h *Harness) SubmitBoth(ctx context.Context, src string) (SubmitPair, error) {
	return h.SubmitCellBoth(ctx, engine.SubmitInput{Source: src})
}

func (h *Harness) SubmitCellBoth(ctx context.Context, input engine.SubmitInput) (SubmitPair, error) {
	var pair SubmitPair

	pair.PersistentResult, pair.PersistentErr = h.Persistent.SubmitCell(ctx, input)
	if pair.PersistentErr == nil && pair.PersistentResult.CompletionValue != nil {
		view, err := h.Persistent.Inspect(ctx, pair.PersistentResult.CompletionValue.ID)
		if err != nil {
			return pair, fmt.Errorf("inspect persistent completion: %w", err)
		}
		pair.PersistentView = &view
	}

	pair.ReplayResult, pair.ReplayErr = h.Replay.SubmitCell(ctx, input)
	if pair.ReplayErr == nil && pair.ReplayResult.CompletionValue != nil {
		view, err := h.Replay.Inspect(ctx, pair.ReplayResult.CompletionValue.ID)
		if err != nil {
			return pair, fmt.Errorf("inspect replay completion: %w", err)
		}
		pair.ReplayView = &view
	}

	return pair, nil
}

func CompareSubmitPair(pair SubmitPair) error {
	if (pair.PersistentErr != nil) != (pair.ReplayErr != nil) {
		return fmt.Errorf("error mismatch: persistent=%v replay=%v", pair.PersistentErr, pair.ReplayErr)
	}
	if pair.PersistentErr != nil || pair.ReplayErr != nil {
		if fmt.Sprint(pair.PersistentErr) != fmt.Sprint(pair.ReplayErr) {
			return fmt.Errorf("error text mismatch: persistent=%v replay=%v", pair.PersistentErr, pair.ReplayErr)
		}
		return nil
	}

	if (pair.PersistentResult.CompletionValue != nil) != (pair.ReplayResult.CompletionValue != nil) {
		return fmt.Errorf("completion presence mismatch: persistent=%v replay=%v", pair.PersistentResult.CompletionValue != nil, pair.ReplayResult.CompletionValue != nil)
	}
	if pair.PersistentView == nil || pair.ReplayView == nil {
		return nil
	}
	if pair.PersistentView.Preview != pair.ReplayView.Preview {
		return fmt.Errorf("preview mismatch: persistent=%q replay=%q", pair.PersistentView.Preview, pair.ReplayView.Preview)
	}
	if pair.PersistentView.TypeHint != pair.ReplayView.TypeHint {
		return fmt.Errorf("type mismatch: persistent=%q replay=%q", pair.PersistentView.TypeHint, pair.ReplayView.TypeHint)
	}
	if !bytes.Equal(pair.PersistentView.Structured, pair.ReplayView.Structured) {
		return fmt.Errorf("structured mismatch: persistent=%s replay=%s", string(pair.PersistentView.Structured), string(pair.ReplayView.Structured))
	}
	return nil
}
