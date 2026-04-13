package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/google/uuid"
	"github.com/mackross/repljs/engine"
	"github.com/mackross/repljs/jswire"
	"github.com/mackross/repljs/model"
	"github.com/mackross/repljs/session"
	"github.com/mackross/repljs/store"
	storemem "github.com/mackross/repljs/store/mem"
	storesqlite "github.com/mackross/repljs/store/sqlite"
	"github.com/mackross/repljs/typescript"
)

func main() {
	if err := run(os.Stdin, os.Stdout, os.Stderr, os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(in io.Reader, out io.Writer, errOut io.Writer, args []string) error {
	fs := flag.NewFlagSet("repl", flag.ContinueOnError)
	fs.SetOutput(errOut)

	backend := fs.String("backend", "mem", "store backend: mem|sqlite")
	sqlitePath := fs.String("sqlite-path", ".repl-session.db", "sqlite database path (used when --backend=sqlite)")
	runtimeMode := fs.String("runtime-mode", string(engine.RuntimeModePersistent), "runtime mode: persistent|replay_per_submit")

	if err := fs.Parse(args); err != nil {
		return err
	}

	mode, err := parseRuntimeMode(*runtimeMode)
	if err != nil {
		return err
	}

	ctx := context.Background()
	st, err := buildStore(ctx, *backend, *sqlitePath)
	if err != nil {
		return err
	}

	eng := session.New()
	deps := engine.SessionDeps{
		Store:             st,
		VMDelegate:        fetchDelegate{},
		RuntimeMode:       mode,
		TypeScriptFactory: typescript.NewFactory(),
	}
	sess, resumed, err := openOrStartSession(ctx, eng, st, *backend, deps)
	if err != nil {
		return err
	}
	defer sess.Close()

	fmt.Fprintf(out, "repl started\n")
	fmt.Fprintf(out, "session=%s backend=%s runtime_mode=%s resumed=%t\n", sess.ID(), *backend, mode, resumed)
	printHelp(out)

	scanner := bufio.NewScanner(in)
	currentLanguage := model.CellLanguageJavaScript
	for {
		fmt.Fprintf(out, "repl[%s]> ", currentLanguage)
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return err
			}
			fmt.Fprintln(out)
			return nil
		}

		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		switch {
		case line == ":quit" || line == ":q" || line == "exit":
			fmt.Fprintln(out, "bye")
			return nil
		case line == ":help":
			printHelp(out)
		case line == ":ts":
			currentLanguage = model.CellLanguageTypeScript
			fmt.Fprintln(out, "mode=ts")
		case line == ":js":
			currentLanguage = model.CellLanguageJavaScript
			fmt.Fprintln(out, "mode=js")
		case line == ":head":
			fmt.Fprintln(out, "head is store-backed; use submit/restore flow to observe branch history")
		case strings.HasPrefix(line, ":inspect "):
			handle := strings.TrimSpace(strings.TrimPrefix(line, ":inspect "))
			if handle == "" {
				fmt.Fprintln(out, "usage: :inspect <value-id>")
				continue
			}
			if err := cmdInspect(ctx, out, sess, model.ValueID(handle)); err != nil {
				fmt.Fprintf(out, "inspect error: %v\n", err)
			}
		case strings.HasPrefix(line, ":logs "):
			cell := strings.TrimSpace(strings.TrimPrefix(line, ":logs "))
			if cell == "" {
				fmt.Fprintln(out, "usage: :logs <cell-id>")
				continue
			}
			if err := cmdLogs(ctx, out, sess, model.CellID(cell)); err != nil {
				fmt.Fprintf(out, "logs error: %v\n", err)
			}
		case strings.HasPrefix(line, ":restore "):
			cell := strings.TrimSpace(strings.TrimPrefix(line, ":restore "))
			if cell == "" {
				fmt.Fprintln(out, "usage: :restore <cell-id>")
				continue
			}
			if err := cmdRestore(ctx, out, sess, model.CellID(cell)); err != nil {
				fmt.Fprintf(out, "restore error: %v\n", err)
			}
		case strings.HasPrefix(line, ":fork "):
			cell := strings.TrimSpace(strings.TrimPrefix(line, ":fork "))
			if cell == "" {
				fmt.Fprintln(out, "usage: :fork <cell-id>")
				continue
			}
			if err := cmdFork(ctx, out, sess, model.CellID(cell)); err != nil {
				fmt.Fprintf(out, "fork error: %v\n", err)
			}
		case line == ":submit":
			src, ok := readMultiline(scanner, out)
			if !ok {
				fmt.Fprintln(out, "submit cancelled")
				continue
			}
			if err := cmdSubmit(ctx, out, sess, src, currentLanguage); err != nil {
				printSubmitError(out, err)
			}
		default:
			if err := cmdSubmit(ctx, out, sess, line, currentLanguage); err != nil {
				printSubmitError(out, err)
			}
		}
	}
}

func openOrStartSession(ctx context.Context, eng *session.Engine, st store.Store, backend string, deps engine.SessionDeps) (engine.Session, bool, error) {
	if backend == "sqlite" {
		if sqliteStore, ok := st.(*storesqlite.Store); ok {
			sessionID, err := sqliteStore.LatestSessionID(ctx)
			if err != nil {
				return nil, false, fmt.Errorf("load latest sqlite session: %w", err)
			}
			if sessionID != "" {
				sess, err := eng.OpenSession(ctx, sessionID, deps)
				if err != nil {
					return nil, false, fmt.Errorf("open session: %w", err)
				}
				return sess, true, nil
			}
		}
	}
	sess, err := eng.StartSession(ctx, model.SessionConfig{Manifest: defaultManifest()}, deps)
	if err != nil {
		return nil, false, fmt.Errorf("start session: %w", err)
	}
	return sess, false, nil
}

func parseRuntimeMode(raw string) (engine.RuntimeMode, error) {
	mode := engine.RuntimeMode(strings.TrimSpace(raw))
	switch mode {
	case engine.RuntimeModePersistent, engine.RuntimeModeReplayPerSubmit:
		return mode, nil
	default:
		return "", fmt.Errorf("unknown runtime mode %q (expected persistent|replay_per_submit)", raw)
	}
}

func buildStore(ctx context.Context, backend, sqlitePath string) (store.Store, error) {
	switch backend {
	case "mem":
		return storemem.New(), nil
	case "sqlite":
		st, err := storesqlite.Open(ctx, sqlitePath)
		if err != nil {
			return nil, fmt.Errorf("open sqlite store: %w", err)
		}
		return st, nil
	default:
		return nil, fmt.Errorf("unknown backend %q (expected mem|sqlite)", backend)
	}
}

func defaultManifest() model.Manifest {
	return model.Manifest{ID: "cli-manifest", Functions: nil}
}

func cmdSubmit(ctx context.Context, out io.Writer, sess engine.Session, src string, language model.CellLanguage) error {
	if strings.TrimSpace(src) == "" {
		return nil
	}
	res, err := sess.SubmitCell(ctx, engine.SubmitInput{Source: src, Language: language})
	if err != nil {
		return err
	}
	printLogLines(out, res.Log)
	fmt.Fprintf(out, "ok cell=%s\n", res.Cell)
	fmt.Fprintf(out, "cell.index=%d\n", res.Index)
	fmt.Fprintf(out, "cell.language=%s\n", res.Language)
	if res.CompletionValue == nil {
		fmt.Fprintln(out, "completion: <nil>")
		return nil
	}
	fmt.Fprintf(out, "completion.id=%s\n", res.CompletionValue.ID)
	preview := res.CompletionValue.Preview
	if view, err := sess.Inspect(ctx, res.CompletionValue.ID); err == nil {
		fmt.Fprintf(out, "completion.preview=%q\n", preview)
		fmt.Fprintf(out, "completion.type=%s\n", res.CompletionValue.TypeHint)
		fmt.Fprintf(out, "completion.summary=%q\n", view.Summary)
		return nil
	}
	fmt.Fprintf(out, "completion.preview=%q\n", preview)
	fmt.Fprintf(out, "completion.type=%s\n", res.CompletionValue.TypeHint)
	return nil
}

func printSubmitError(out io.Writer, err error) {
	var submitErr *engine.SubmitFailure
	if errors.As(err, &submitErr) {
		printLogLines(out, submitErr.Log)
	}
	fmt.Fprintf(out, "submit error: %v\n", err)

	var checkErr *engine.SubmitCheckFailure
	if errors.As(err, &checkErr) {
		if checkErr.Cell != "" {
			fmt.Fprintf(out, "cell=%s\n", checkErr.Cell)
		}
		if checkErr.Index != 0 {
			fmt.Fprintf(out, "cell.index=%d\n", checkErr.Index)
		}
		if checkErr.Language != "" {
			fmt.Fprintf(out, "cell.language=%s\n", checkErr.Language)
		}
		printDiagnostics(out, checkErr.Diagnostics)
		return
	}

	if !errors.As(err, &submitErr) {
		return
	}
	fmt.Fprintf(out, "failure.id=%s\n", submitErr.Failure)
	if submitErr.Parent != "" {
		fmt.Fprintf(out, "failure.parent=%s\n", submitErr.Parent)
	}
	if submitErr.Phase != "" {
		fmt.Fprintf(out, "failure.phase=%s\n", submitErr.Phase)
	}
	if len(submitErr.LinkedEffects) == 0 {
		fmt.Fprintln(out, "linked_effects=<none>")
		return
	}
	fmt.Fprintf(out, "linked_effects=%d\n", len(submitErr.LinkedEffects))
	for i, effect := range submitErr.LinkedEffects {
		fmt.Fprintf(out, "effect[%d].id=%s\n", i, effect.Effect)
		fmt.Fprintf(out, "effect[%d].function=%s\n", i, effect.FunctionName)
		fmt.Fprintf(out, "effect[%d].status=%s\n", i, effect.Status)
		fmt.Fprintf(out, "effect[%d].replay_policy=%s\n", i, effect.ReplayPolicy)
		if len(effect.Params) != 0 {
			fmt.Fprintf(out, "effect[%d].params=%s\n", i, formatStructuredInline(effect.Params))
		}
		if len(effect.Result) != 0 {
			fmt.Fprintf(out, "effect[%d].result=%s\n", i, formatStructuredInline(effect.Result))
		}
		if effect.ErrorMessage != "" {
			fmt.Fprintf(out, "effect[%d].error=%q\n", i, effect.ErrorMessage)
		}
	}
}

func printDiagnostics(out io.Writer, diagnostics []model.Diagnostic) {
	if len(diagnostics) == 0 {
		fmt.Fprintln(out, "diagnostics=<none>")
		return
	}
	fmt.Fprintf(out, "diagnostics=%d\n", len(diagnostics))
	for i, diagnostic := range diagnostics {
		fmt.Fprintf(out, "diagnostic[%d].severity=%s\n", i, diagnostic.Severity)
		if diagnostic.Line > 0 {
			if diagnostic.Column > 0 {
				fmt.Fprintf(out, "diagnostic[%d].location=%d:%d\n", i, diagnostic.Line, diagnostic.Column)
			} else {
				fmt.Fprintf(out, "diagnostic[%d].location=%d\n", i, diagnostic.Line)
			}
		}
		fmt.Fprintf(out, "diagnostic[%d].message=%q\n", i, diagnostic.Message)
	}
}

func formatStructuredInline(raw []byte) string {
	if len(raw) == 0 {
		return "<nil>"
	}
	inspection, err := jswire.Describe(raw)
	if err != nil {
		return fmt.Sprintf("<jswire-describe-error:%v>", err)
	}
	if inspection.Summary == "" {
		return "<nil>"
	}
	return inspection.Summary
}

func cmdInspect(ctx context.Context, out io.Writer, sess engine.Session, handle model.ValueID) error {
	view, err := sess.Inspect(ctx, handle)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "handle=%s\n", view.Handle)
	fmt.Fprintf(out, "preview=%q\n", view.Preview)
	fmt.Fprintf(out, "type=%s\n", view.TypeHint)
	fmt.Fprintf(out, "summary=%q\n", view.Summary)
	fmt.Fprintf(out, "full=%q\n", view.Full)
	return nil
}

func cmdLogs(ctx context.Context, out io.Writer, sess engine.Session, cell model.CellID) error {
	if _, err := uuid.Parse(string(cell)); err != nil {
		return fmt.Errorf("invalid cell id %q: %w", cell, err)
	}
	logs, err := sess.Logs(ctx, cell)
	if err != nil {
		return err
	}
	printLogLines(out, logs)
	if len(logs) == 0 {
		fmt.Fprintln(out, "logs=<none>")
	}
	return nil
}

func cmdRestore(ctx context.Context, out io.Writer, sess engine.Session, cell model.CellID) error {
	if _, err := uuid.Parse(string(cell)); err != nil {
		return fmt.Errorf("invalid cell id %q: %w", cell, err)
	}
	if err := sess.Restore(ctx, cell); err != nil {
		return err
	}
	fmt.Fprintf(out, "restored to cell=%s\n", cell)
	return nil
}

func cmdFork(ctx context.Context, out io.Writer, sess engine.Session, cell model.CellID) error {
	if _, err := uuid.Parse(string(cell)); err != nil {
		return fmt.Errorf("invalid cell id %q: %w", cell, err)
	}
	if err := sess.Fork(ctx, cell); err != nil {
		return err
	}
	fmt.Fprintf(out, "forked from cell=%s\n", cell)
	return nil
}

func readMultiline(scanner *bufio.Scanner, out io.Writer) (string, bool) {
	fmt.Fprintln(out, "enter JS, end with a line containing only .end")
	var lines []string
	for {
		fmt.Fprint(out, "... ")
		if !scanner.Scan() {
			return "", false
		}
		line := scanner.Text()
		if strings.TrimSpace(line) == ".end" {
			break
		}
		lines = append(lines, line)
	}
	src := strings.TrimSpace(strings.Join(lines, "\n"))
	if src == "" {
		return "", false
	}
	return src, true
}

func printHelp(out io.Writer) {
	fmt.Fprintln(out, "commands:")
	fmt.Fprintln(out, "  <code>               submit one-line cell in the current mode")
	fmt.Fprintln(out, "  :submit              submit multi-line cell in the current mode (end with .end)")
	fmt.Fprintln(out, "  :js                  switch default submit mode to JavaScript")
	fmt.Fprintln(out, "  :ts                  switch default submit mode to TypeScript")
	fmt.Fprintln(out, "  :inspect <value-id>  inspect completion handle")
	fmt.Fprintln(out, "  :logs <cell-id>      replay one committed cell and print its console.log lines")
	fmt.Fprintln(out, "  :restore <cell-id>   move active session to a committed cell")
	fmt.Fprintln(out, "  :fork <cell-id>      fork a new branch from a committed cell")
	fmt.Fprintln(out, "  :head                show note about head semantics")
	fmt.Fprintln(out, "  :help                show this help")
	fmt.Fprintln(out, "  :quit                exit")
}

func printLogLines(out io.Writer, logs []string) {
	for i, line := range logs {
		fmt.Fprintf(out, "log[%d]=%s\n", i, line)
	}
}
