package typescript

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"testing/fstest"

	"github.com/evanw/esbuild/pkg/api"
	tstoolbox "github.com/microsoft/typescript-go/toolbox"
	"github.com/solidarity-ai/repl/model"
)

const (
	declarationsFile = "__repl_env.d.ts"
	entryFile        = "__repl_cell.ts"
	moduleMarker     = "export {};\n"
)

const builtinDeclarations = `
declare const $last: any;
declare function $val(index: number): any;
declare function inspect(...args: any[]): string;
declare const console: {
  log(...args: any[]): void;
};
`

type Env struct {
	Hash            string
	DeclarationsDTS string
	PreludeTS       string
	CurrentDir      string
}

type Result struct {
	Diagnostics []model.Diagnostic
	HasErrors   bool
	EmittedJS   string
}

type Session interface {
	CheckEmitCell(ctx context.Context, env Env, src string) (Result, error)
	SetCommittedSources(srcs []string)
	AppendCommittedSource(src string)
	Close() error
}

type Factory interface {
	NewSession(ctx context.Context) (Session, error)
}

type defaultFactory struct{}

func NewFactory() Factory {
	return defaultFactory{}
}

func (defaultFactory) NewSession(_ context.Context) (Session, error) {
	return &session{}, nil
}

type session struct {
	checkSession *tstoolbox.CheckSession
	envHash      string
	history      []string
}

func (s *session) Close() error {
	if s.checkSession != nil {
		err := s.checkSession.Close()
		s.checkSession = nil
		return err
	}
	return nil
}

func (s *session) SetCommittedSources(srcs []string) {
	s.history = append([]string(nil), srcs...)
}

func (s *session) AppendCommittedSource(src string) {
	s.history = append(s.history, src)
}

func (s *session) CheckEmitCell(ctx context.Context, env Env, src string) (Result, error) {
	env = normalizeEnv(env)
	if env.CurrentDir == "" {
		env.CurrentDir = "/"
	}
	prefix, currentOffset := buildTypecheckEntry(env.PreludeTS, s.history, src)
	input := tstoolbox.CheckInput{
		Files:            filesForEnv(env, prefix),
		Entry:            entryFile,
		CurrentDirectory: env.CurrentDir,
	}

	var (
		checkResult tstoolbox.ReplCellCheckResult
		err         error
	)
	if s.checkSession == nil || s.envHash != env.Hash {
		if s.checkSession != nil {
			_ = s.checkSession.Close()
			s.checkSession = nil
		}
		checkResult, s.checkSession, err = tstoolbox.ReplCellCheck(ctx, input, nil)
		if err != nil {
			return Result{}, err
		}
		s.envHash = env.Hash
	} else {
		checkResult, s.checkSession, err = tstoolbox.ReplCellCheck(ctx, input, s.checkSession)
		if err != nil {
			return Result{}, err
		}
	}

	if diagnostic, ok := convertUnsupportedSyntax(checkResult.UnsupportedSyntax, entryFile, prefix, currentOffset); ok {
		return Result{
			Diagnostics: []model.Diagnostic{diagnostic},
			HasErrors:   true,
		}, nil
	}

	converted := convertDiagnostics(checkResult.Diagnostics, entryFile, prefix, currentOffset)
	emitted, err := emitCell(env.PreludeTS, src)
	if err != nil {
		return Result{}, err
	}
	return Result{
		Diagnostics: converted,
		HasErrors:   len(converted) > 0,
		EmittedJS:   emitted,
	}, nil
}

func normalizeEnv(env Env) Env {
	decls := builtinDeclarations
	if strings.TrimSpace(env.DeclarationsDTS) != "" {
		decls += "\n" + env.DeclarationsDTS
	}
	env.DeclarationsDTS = decls
	if env.Hash == "" {
		env.Hash = "builtin-v1"
	} else {
		env.Hash = "builtin-v1|" + env.Hash
	}
	return env
}

func buildTypecheckEntry(prelude string, history []string, current string) (string, int) {
	var b strings.Builder
	b.WriteString(moduleMarker)
	if prelude != "" {
		b.WriteString(prelude)
		if !strings.HasSuffix(prelude, "\n") {
			b.WriteString("\n")
		}
	}
	for _, src := range history {
		b.WriteString(src)
		if !strings.HasSuffix(src, "\n") {
			b.WriteString("\n")
		}
	}
	offset := b.Len()
	b.WriteString(current)
	return b.String(), offset
}

func filesForEnv(env Env, entry string) fs.FS {
	return fstest.MapFS{
		declarationsFile: &fstest.MapFile{Data: []byte(env.DeclarationsDTS)},
		entryFile:        &fstest.MapFile{Data: []byte(entry)},
	}
}

func emitCell(prelude string, src string) (string, error) {
	var input strings.Builder
	if prelude != "" {
		input.WriteString(prelude)
		if !strings.HasSuffix(prelude, "\n") {
			input.WriteString("\n")
		}
	}
	input.WriteString(src)
	result := api.Transform(input.String(), api.TransformOptions{
		Loader:     api.LoaderTS,
		Target:     api.ES2023,
		LogLevel:   api.LogLevelSilent,
		Sourcefile: entryFile,
	})
	if len(result.Errors) > 0 {
		return "", fmt.Errorf("typescript emit failed: %s", result.Errors[0].Text)
	}
	return string(result.Code), nil
}

func convertDiagnostics(in []tstoolbox.Diagnostic, entry string, fullSource string, currentOffset int) []model.Diagnostic {
	if len(in) == 0 {
		return nil
	}
	currentLine, currentColumn := lineColumnAt(fullSource, currentOffset)
	out := make([]model.Diagnostic, 0, len(in))
	for _, diagnostic := range in {
		item := model.Diagnostic{
			Message:  diagnostic.Message,
			Severity: "error",
		}
		if diagnostic.File == entry || filepath.Base(diagnostic.File) == entry {
			line, column := lineColumnAt(fullSource, diagnostic.Pos)
			item.Line = line - currentLine + 1
			if line == currentLine {
				item.Column = column - currentColumn + 1
			} else {
				item.Column = column
			}
			if item.Line < 1 {
				item.Line = 1
			}
			if item.Column < 1 {
				item.Column = 1
			}
		}
		out = append(out, item)
	}
	return out
}

func lineColumnAt(src string, pos int) (line int, column int) {
	line = 1
	column = 1
	if pos <= 0 {
		return line, column
	}
	if pos > len(src) {
		pos = len(src)
	}
	for i := 0; i < pos; i++ {
		if src[i] == '\n' {
			line++
			column = 1
			continue
		}
		column++
	}
	return line, column
}

func convertUnsupportedSyntax(in []tstoolbox.UnsupportedSyntax, entry string, fullSource string, currentOffset int) (model.Diagnostic, bool) {
	for _, item := range in {
		if item.File != entry && filepath.Base(item.File) != entry {
			continue
		}
		line, column := lineColumnAt(fullSource, item.Pos)
		currentLine, currentColumn := lineColumnAt(fullSource, currentOffset)
		line = line - currentLine + 1
		if line == 1 {
			column = column - currentColumn + 1
		}
		if line < 1 {
			line = 1
		}
		if column < 1 {
			column = 1
		}
		return model.Diagnostic{
			Message:  "TypeScript namespace/module declarations are not supported in REPL cells because their runtime emit is not replay-safe across cells. Use plain objects, functions, or classes instead.",
			Severity: "error",
			Line:     line,
			Column:   column,
		}, true
	}
	return model.Diagnostic{}, false
}
