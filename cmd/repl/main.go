package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/google/uuid"
	"github.com/solidarity-ai/repl/engine"
	"github.com/solidarity-ai/repl/model"
	"github.com/solidarity-ai/repl/session"
	"github.com/solidarity-ai/repl/store"
	storemem "github.com/solidarity-ai/repl/store/mem"
	storesqlite "github.com/solidarity-ai/repl/store/sqlite"
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
	sess, err := eng.StartSession(ctx, model.SessionConfig{Manifest: defaultManifest()}, engine.SessionDeps{
		Store:       st,
		VMDelegate:  fetchDelegate{},
		RuntimeMode: mode,
	})
	if err != nil {
		return fmt.Errorf("start session: %w", err)
	}
	defer sess.Close()

	fmt.Fprintf(out, "repl started\n")
	fmt.Fprintf(out, "session=%s backend=%s runtime_mode=%s\n", sess.ID(), *backend, mode)
	printHelp(out)

	scanner := bufio.NewScanner(in)
	for {
		fmt.Fprint(out, "repl> ")
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
		case strings.HasPrefix(line, ":restore "):
			cell := strings.TrimSpace(strings.TrimPrefix(line, ":restore "))
			if cell == "" {
				fmt.Fprintln(out, "usage: :restore <cell-id>")
				continue
			}
			if err := cmdRestore(ctx, out, sess, model.CellID(cell)); err != nil {
				fmt.Fprintf(out, "restore error: %v\n", err)
			}
		case line == ":submit":
			src, ok := readMultiline(scanner, out)
			if !ok {
				fmt.Fprintln(out, "submit cancelled")
				continue
			}
			if err := cmdSubmit(ctx, out, sess, src); err != nil {
				fmt.Fprintf(out, "submit error: %v\n", err)
			}
		default:
			if err := cmdSubmit(ctx, out, sess, line); err != nil {
				fmt.Fprintf(out, "submit error: %v\n", err)
			}
		}
	}
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

func rewriteTopLevelAwait(src string) string {
	trimmed := strings.TrimSpace(src)
	if strings.HasPrefix(trimmed, "await ") || strings.HasPrefix(trimmed, "await(") {
		return "(async () => { return " + trimmed + "; })()"
	}
	return src
}

func cmdSubmit(ctx context.Context, out io.Writer, sess engine.Session, src string) error {
	submission := rewriteTopLevelAwait(src)
	res, err := sess.Submit(ctx, submission)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "ok cell=%s\n", res.Cell)
	if res.CompletionValue == nil {
		fmt.Fprintln(out, "completion: <nil>")
		return nil
	}
	fmt.Fprintf(out, "completion.id=%s\n", res.CompletionValue.ID)
	fmt.Fprintf(out, "completion.preview=%q\n", res.CompletionValue.Preview)
	fmt.Fprintf(out, "completion.type=%s\n", res.CompletionValue.TypeHint)
	return nil
}

func cmdInspect(ctx context.Context, out io.Writer, sess engine.Session, handle model.ValueID) error {
	view, err := sess.Inspect(ctx, handle)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "handle=%s\n", view.Handle)
	fmt.Fprintf(out, "preview=%q\n", view.Preview)
	fmt.Fprintf(out, "type=%s\n", view.TypeHint)
	if len(view.Structured) == 0 {
		fmt.Fprintln(out, "structured=<nil>")
		return nil
	}
	var pretty any
	if err := json.Unmarshal(view.Structured, &pretty); err != nil {
		fmt.Fprintf(out, "structured(raw)=%s\n", string(view.Structured))
		return nil
	}
	b, _ := json.MarshalIndent(pretty, "", "  ")
	fmt.Fprintf(out, "structured=%s\n", string(b))
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
	fmt.Fprintln(out, "  <js>                 submit one-line JS cell")
	fmt.Fprintln(out, "  :submit              submit multi-line JS cell (end with .end)")
	fmt.Fprintln(out, "  :inspect <value-id>  inspect completion handle")
	fmt.Fprintln(out, "  :restore <cell-id>   fork/restore to a committed cell")
	fmt.Fprintln(out, "  :head                show note about head semantics")
	fmt.Fprintln(out, "  :help                show this help")
	fmt.Fprintln(out, "  :quit                exit")
}
