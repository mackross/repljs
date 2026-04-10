package session

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
	"unsafe"

	"github.com/dop251/goja"
	jsast "github.com/dop251/goja/ast"
	"github.com/dop251/goja/token"
	"github.com/dop251/goja/unistring"
)

const replInternalCommitName = "__repljs_internal_commit__"

type topLevelDeclaration struct {
	Name string
	Kind string
}

type committedTopLevelDeclaration struct {
	LexicalKind        string
	LexicalTransformed bool
	VarLikeKind        string
	VarLikeTransformed bool
}

type topLevelAwaitPlan struct {
	Declarations []topLevelDeclaration
	Program      *goja.Program
	Transformed  bool
}

type topLevelAwaitState struct {
	mu        sync.Mutex
	committed map[string]committedTopLevelDeclaration
	pending   *topLevelAwaitCommitState
}

type topLevelAwaitCommitState struct {
	allowed map[string]struct{}
	varLike map[string]struct{}
}

func newTopLevelAwaitState() *topLevelAwaitState {
	return &topLevelAwaitState{committed: make(map[string]committedTopLevelDeclaration)}
}

func (s *topLevelAwaitState) install(rt *goja.Runtime) error {
	return rt.Set(replInternalCommitName, func(call goja.FunctionCall) goja.Value {
		return s.commit(rt, call)
	})
}

func declarationIsVarLike(kind string) bool {
	switch kind {
	case "var", "function":
		return true
	default:
		return false
	}
}

func redeclarationUnsupportedMessage(name, kind string) string {
	switch kind {
	case "function":
		return fmt.Sprintf("top-level name %q is already defined in this session; re-declaring it with `function` after a prior top-level-await declaration is not supported here. If you meant to replace it, assign a new function value instead, e.g. `%s = function () { ... }`.", name, name)
	case "var":
		fallthrough
	default:
		return fmt.Sprintf("top-level name %q is already defined in this session; re-declaring it with `%s` after a prior top-level-await declaration is not supported here. If you meant to overwrite the value, use `%s = ...` instead.", name, kind, name)
	}
}

func (d committedTopLevelDeclaration) conflictKind() string {
	if d.LexicalKind != "" {
		return d.LexicalKind
	}
	if d.VarLikeKind != "" {
		return d.VarLikeKind
	}
	return "declaration"
}

func (s *topLevelAwaitState) validateRedeclarations(decls []topLevelDeclaration, currentTransformed bool) error {
	if len(decls) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, decl := range decls {
		prev, ok := s.committed[decl.Name]
		if !ok {
			continue
		}
		if declarationIsVarLike(decl.Kind) {
			if !currentTransformed && prev.VarLikeTransformed {
				return fmt.Errorf("%s", redeclarationUnsupportedMessage(decl.Name, decl.Kind))
			}
			if prev.LexicalKind != "" {
				return fmt.Errorf("top-level declaration %q (%s) conflicts with prior committed %s", decl.Name, decl.Kind, prev.LexicalKind)
			}
			continue
		}
		if prev.LexicalKind != "" || prev.VarLikeKind != "" {
			return fmt.Errorf("top-level declaration %q (%s) conflicts with prior committed %s", decl.Name, decl.Kind, prev.conflictKind())
		}
	}
	return nil
}

func (s *topLevelAwaitState) commitDeclarations(decls []topLevelDeclaration, transformed bool) {
	if len(decls) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, decl := range decls {
		entry := s.committed[decl.Name]
		if declarationIsVarLike(decl.Kind) {
			entry.VarLikeKind = decl.Kind
			entry.VarLikeTransformed = transformed
		} else {
			entry.LexicalKind = decl.Kind
			entry.LexicalTransformed = transformed
		}
		s.committed[decl.Name] = entry
	}
}

func (s *topLevelAwaitState) beginCommit(decls []topLevelDeclaration) {
	allowed := make(map[string]struct{}, len(decls))
	varLike := make(map[string]struct{}, len(decls))
	for _, decl := range decls {
		allowed[decl.Name] = struct{}{}
		if declarationIsVarLike(decl.Kind) {
			varLike[decl.Name] = struct{}{}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending = &topLevelAwaitCommitState{allowed: allowed, varLike: varLike}
}

func (s *topLevelAwaitState) endCommit() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending = nil
}

func (s *topLevelAwaitState) pendingCommit() *topLevelAwaitCommitState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending == nil {
		return nil
	}
	allowed := make(map[string]struct{}, len(s.pending.allowed))
	for name := range s.pending.allowed {
		allowed[name] = struct{}{}
	}
	varLike := make(map[string]struct{}, len(s.pending.varLike))
	for name := range s.pending.varLike {
		varLike[name] = struct{}{}
	}
	return &topLevelAwaitCommitState{allowed: allowed, varLike: varLike}
}

func (s *topLevelAwaitState) commit(rt *goja.Runtime, call goja.FunctionCall) goja.Value {
	pending := s.pendingCommit()
	if pending == nil {
		panicJSError(rt, "Error", "internal commit hook called without pending top-level await state")
	}
	if len(pending.allowed) > 0 {
		if err := promoteCurrentWrapperStash(rt, pending.allowed, pending.varLike); err != nil {
			panicJSError(rt, "Error", "%v", err)
		}
	}
	if len(call.Arguments) == 0 {
		return goja.Undefined()
	}
	return call.Argument(0)
}

func buildTopLevelAwaitPlan(src string, redecls *topLevelAwaitState) (topLevelAwaitPlan, error) {
	rawPrg, rawErr := goja.Parse("cell.js", src)
	if rawErr == nil {
		decls := collectTopLevelDeclarations(rawPrg.Body)
		if redecls != nil {
			if err := redecls.validateRedeclarations(decls, false); err != nil {
				return topLevelAwaitPlan{}, err
			}
		}
		return topLevelAwaitPlan{Declarations: decls}, nil
	}

	if hintErr := unsupportedTopLevelAwaitSyntaxError(src); hintErr != nil {
		return topLevelAwaitPlan{}, hintErr
	}

	transformed, decls, ok, err := compileTopLevelAwaitCell(src)
	if err != nil {
		return topLevelAwaitPlan{}, err
	}
	if !ok {
		return topLevelAwaitPlan{}, rawErr
	}
	if redecls != nil {
		if err := redecls.validateRedeclarations(decls, true); err != nil {
			return topLevelAwaitPlan{}, err
		}
	}
	return topLevelAwaitPlan{Declarations: decls, Program: transformed, Transformed: true}, nil
}

func compileTopLevelAwaitCell(src string) (*goja.Program, []topLevelDeclaration, bool, error) {
	wrapped := "(async function __repljs_cell__(){\n" + src + "\nreturn " + replInternalCommitName + "(void 0);\n})()"
	prg, err := goja.Parse("cell.js", wrapped)
	if err != nil {
		return nil, nil, false, nil
	}

	fn, err := locateWrappedAsyncCell(prg)
	if err != nil {
		return nil, nil, false, err
	}
	if len(fn.Body.List) == 0 {
		return nil, nil, false, nil
	}

	userStatements := append([]jsast.Statement(nil), fn.Body.List[:len(fn.Body.List)-1]...)
	if containsTopLevelReturn(userStatements) {
		return nil, nil, false, nil
	}
	if !containsTopLevelAwait(userStatements) {
		return nil, nil, false, nil
	}
	if err := unsupportedTransformedCellSemantics(userStatements); err != nil {
		return nil, nil, false, err
	}

	decls := collectTopLevelDeclarations(userStatements)
	completionName := chooseInternalName(decls, "__repljs_completion__")
	patched := patchWrappedAsyncCell(userStatements, len(decls) > 0, completionName)
	fn.Body.List = patched

	compiled, err := goja.CompileAST(prg, false)
	if err != nil {
		return nil, nil, false, err
	}
	return compiled, decls, true, nil
}

func unsupportedTopLevelAwaitSyntaxError(src string) error {
	if strings.Contains(src, "for await") {
		return fmt.Errorf("top-level await does not support 'for await...of' in this REPL yet; for example: `for (const item of items) { const value = await item; /* body using value */ }`")
	}
	return nil
}

func unsupportedTransformedCellSemantics(stmts []jsast.Statement) error {
	if hasTopLevelUseStrictDirective(stmts) {
		return fmt.Errorf("top-level await cells do not support a top-level \"use strict\" directive in this REPL")
	}
	if containsEvalAnywhere(stmts) {
		return fmt.Errorf("eval is not supported in top-level await cells because the REPL runs them inside an async wrapper, which changes eval scope semantics")
	}
	if containsTopLevelThis(stmts) {
		return fmt.Errorf("top-level `this` is not supported in top-level await cells in this REPL; use globalThis instead")
	}
	return nil
}

func hasTopLevelUseStrictDirective(stmts []jsast.Statement) bool {
	for _, stmt := range stmts {
		exprStmt, ok := stmt.(*jsast.ExpressionStatement)
		if !ok {
			break
		}
		lit, ok := exprStmt.Expression.(*jsast.StringLiteral)
		if !ok {
			break
		}
		if lit.Value.String() == "use strict" {
			return true
		}
	}
	return false
}

func locateWrappedAsyncCell(prg *jsast.Program) (*jsast.FunctionLiteral, error) {
	if len(prg.Body) != 1 {
		return nil, fmt.Errorf("top-level await wrapper body count = %d, want 1", len(prg.Body))
	}
	exprStmt, ok := prg.Body[0].(*jsast.ExpressionStatement)
	if !ok {
		return nil, fmt.Errorf("top-level await wrapper root is %T, want *ast.ExpressionStatement", prg.Body[0])
	}
	call, ok := exprStmt.Expression.(*jsast.CallExpression)
	if !ok {
		return nil, fmt.Errorf("top-level await wrapper expression is %T, want *ast.CallExpression", exprStmt.Expression)
	}
	fn, ok := call.Callee.(*jsast.FunctionLiteral)
	if !ok {
		return nil, fmt.Errorf("top-level await wrapper callee is %T, want *ast.FunctionLiteral", call.Callee)
	}
	if !fn.Async {
		return nil, fmt.Errorf("top-level await wrapper function is not async")
	}
	return fn, nil
}

func chooseInternalName(decls []topLevelDeclaration, base string) string {
	used := make(map[string]struct{}, len(decls))
	for _, decl := range decls {
		used[decl.Name] = struct{}{}
	}
	if _, ok := used[base]; !ok {
		return base
	}
	for i := 1; ; i++ {
		candidate := fmt.Sprintf("%s_%d", base, i)
		if _, ok := used[candidate]; !ok {
			return candidate
		}
	}
}

func patchWrappedAsyncCell(userStatements []jsast.Statement, forceDynamic bool, completionName string) []jsast.Statement {
	patched := make([]jsast.Statement, 0, len(userStatements)+4)
	if forceDynamic {
		patched = append(patched, directEvalSentinel())
	}
	patched = append(patched, completionDeclarationStatement(completionName))
	patched = append(patched, rewriteTailStatementsForCompletion(userStatements, completionName)...)
	patched = append(patched, commitReturnStatement(identifierExpression(completionName)))
	return patched
}

func rewriteTailStatementsForCompletion(stmts []jsast.Statement, completionName string) []jsast.Statement {
	if len(stmts) == 0 {
		return nil
	}
	out := make([]jsast.Statement, 0, len(stmts))
	for i, stmt := range stmts {
		if i != len(stmts)-1 {
			out = append(out, stmt)
			continue
		}
		out = append(out, rewriteTailStatementForCompletion(stmt, completionName))
	}
	return out
}

func rewriteTailStatementForCompletion(stmt jsast.Statement, completionName string) jsast.Statement {
	switch s := stmt.(type) {
	case *jsast.ExpressionStatement:
		return completionAssignStatement(completionName, s.Expression)
	case *jsast.BlockStatement:
		s.List = rewriteTailStatementsForCompletion(s.List, completionName)
		return s
	case *jsast.IfStatement:
		s.Consequent = rewriteTailStatementForCompletion(s.Consequent, completionName)
		if s.Alternate != nil {
			s.Alternate = rewriteTailStatementForCompletion(s.Alternate, completionName)
		}
		return s
	case *jsast.SwitchStatement:
		for _, c := range s.Body {
			c.Consequent = rewriteSwitchConsequentForCompletion(c.Consequent, completionName)
		}
		return s
	case *jsast.TryStatement:
		if s.Body != nil {
			s.Body.List = rewriteTailStatementsForCompletion(s.Body.List, completionName)
		}
		if s.Catch != nil && s.Catch.Body != nil {
			s.Catch.Body.List = rewriteTailStatementsForCompletion(s.Catch.Body.List, completionName)
		}
		return s
	case *jsast.LabelledStatement:
		s.Statement = rewriteTailStatementForCompletion(s.Statement, completionName)
		return s
	case *jsast.WithStatement:
		s.Body = rewriteTailStatementForCompletion(s.Body, completionName)
		return s
	default:
		return stmt
	}
}

func rewriteSwitchConsequentForCompletion(stmts []jsast.Statement, completionName string) []jsast.Statement {
	if len(stmts) == 0 {
		return nil
	}
	last := stmts[len(stmts)-1]
	if branch, ok := last.(*jsast.BranchStatement); ok && branch.Token == token.BREAK && branch.Label == nil {
		out := rewriteSwitchConsequentForCompletion(stmts[:len(stmts)-1], completionName)
		out = append(out, branch)
		return out
	}
	return rewriteTailStatementsForCompletion(stmts, completionName)
}

func directEvalSentinel() jsast.Statement {
	return &jsast.ExpressionStatement{Expression: &jsast.CallExpression{
		Callee: identifierExpression("eval"),
		ArgumentList: []jsast.Expression{
			&jsast.StringLiteral{Literal: `""`, Value: unistring.NewFromString("")},
		},
	}}
}

func identifierExpression(name string) *jsast.Identifier {
	return &jsast.Identifier{Name: unistring.NewFromString(name)}
}

func completionDeclarationStatement(name string) jsast.Statement {
	return &jsast.LexicalDeclaration{
		Token: token.LET,
		List: []*jsast.Binding{{
			Target:      identifierExpression(name),
			Initializer: undefinedExpression(),
		}},
	}
}

func completionAssignStatement(name string, value jsast.Expression) jsast.Statement {
	return &jsast.ExpressionStatement{Expression: &jsast.AssignExpression{
		Operator: token.ASSIGN,
		Left:     identifierExpression(name),
		Right:    value,
	}}
}

func commitReturnStatement(value jsast.Expression) jsast.Statement {
	return &jsast.ReturnStatement{Argument: &jsast.CallExpression{
		Callee:       identifierExpression(replInternalCommitName),
		ArgumentList: []jsast.Expression{value},
	}}
}

func undefinedExpression() jsast.Expression {
	return &jsast.UnaryExpression{Operator: token.VOID, Operand: &jsast.NumberLiteral{Literal: "0", Value: int64(0)}}
}

func collectTopLevelDeclarations(stmts []jsast.Statement) []topLevelDeclaration {
	seen := make(map[string]bool)
	out := make([]topLevelDeclaration, 0)
	add := func(name, kind string) {
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		out = append(out, topLevelDeclaration{Name: name, Kind: kind})
	}
	for _, stmt := range stmts {
		switch s := stmt.(type) {
		case *jsast.VariableStatement:
			for _, b := range s.List {
				for _, name := range bindingTargetNames(b.Target) {
					add(name, "var")
				}
			}
		case *jsast.LexicalDeclaration:
			kind := s.Token.String()
			for _, b := range s.List {
				for _, name := range bindingTargetNames(b.Target) {
					add(name, kind)
				}
			}
		case *jsast.FunctionDeclaration:
			if s.Function != nil && s.Function.Name != nil {
				add(s.Function.Name.Name.String(), "function")
			}
		case *jsast.ClassDeclaration:
			if s.Class != nil && s.Class.Name != nil {
				add(s.Class.Name.Name.String(), "class")
			}
		}
	}
	return out
}

func bindingTargetNames(target jsast.BindingTarget) []string {
	switch t := target.(type) {
	case *jsast.Identifier:
		return []string{t.Name.String()}
	case *jsast.ArrayPattern:
		var out []string
		for _, el := range t.Elements {
			if bt, ok := el.(jsast.BindingTarget); ok {
				out = append(out, bindingTargetNames(bt)...)
			}
		}
		if bt, ok := t.Rest.(jsast.BindingTarget); ok {
			out = append(out, bindingTargetNames(bt)...)
		}
		return out
	case *jsast.ObjectPattern:
		var out []string
		for _, prop := range t.Properties {
			switch p := prop.(type) {
			case *jsast.PropertyShort:
				out = append(out, p.Name.Name.String())
			case *jsast.PropertyKeyed:
				if bt, ok := p.Value.(jsast.BindingTarget); ok {
					out = append(out, bindingTargetNames(bt)...)
				}
			}
		}
		if bt, ok := t.Rest.(jsast.BindingTarget); ok {
			out = append(out, bindingTargetNames(bt)...)
		}
		return out
	default:
		return nil
	}
}

func containsTopLevelReturn(stmts []jsast.Statement) bool {
	for _, stmt := range stmts {
		if statementContainsTopLevelReturn(stmt) {
			return true
		}
	}
	return false
}

func statementContainsTopLevelReturn(stmt jsast.Statement) bool {
	switch s := stmt.(type) {
	case *jsast.ReturnStatement:
		return true
	case *jsast.BlockStatement:
		return containsTopLevelReturn(s.List)
	case *jsast.IfStatement:
		return statementContainsTopLevelReturn(s.Consequent) || (s.Alternate != nil && statementContainsTopLevelReturn(s.Alternate))
	case *jsast.LabelledStatement:
		return statementContainsTopLevelReturn(s.Statement)
	case *jsast.WithStatement:
		return statementContainsTopLevelReturn(s.Body)
	case *jsast.WhileStatement:
		return statementContainsTopLevelReturn(s.Body)
	case *jsast.DoWhileStatement:
		return statementContainsTopLevelReturn(s.Body)
	case *jsast.ForStatement:
		return statementContainsTopLevelReturn(s.Body)
	case *jsast.ForInStatement:
		return statementContainsTopLevelReturn(s.Body)
	case *jsast.ForOfStatement:
		return statementContainsTopLevelReturn(s.Body)
	case *jsast.SwitchStatement:
		for _, c := range s.Body {
			if containsTopLevelReturn(c.Consequent) {
				return true
			}
		}
	case *jsast.TryStatement:
		if s.Body != nil && containsTopLevelReturn(s.Body.List) {
			return true
		}
		if s.Catch != nil && s.Catch.Body != nil && containsTopLevelReturn(s.Catch.Body.List) {
			return true
		}
		if s.Finally != nil && containsTopLevelReturn(s.Finally.List) {
			return true
		}
	}
	return false
}

func containsTopLevelAwait(stmts []jsast.Statement) bool {
	for _, stmt := range stmts {
		if statementContainsAwait(stmt) {
			return true
		}
	}
	return false
}

func statementContainsAwait(stmt jsast.Statement) bool {
	switch s := stmt.(type) {
	case *jsast.ExpressionStatement:
		return expressionContainsAwait(s.Expression)
	case *jsast.VariableStatement:
		for _, b := range s.List {
			if b.Initializer != nil && expressionContainsAwait(b.Initializer) {
				return true
			}
		}
	case *jsast.LexicalDeclaration:
		for _, b := range s.List {
			if b.Initializer != nil && expressionContainsAwait(b.Initializer) {
				return true
			}
		}
	case *jsast.BlockStatement:
		return containsTopLevelAwait(s.List)
	case *jsast.IfStatement:
		return expressionContainsAwait(s.Test) || statementContainsAwait(s.Consequent) || (s.Alternate != nil && statementContainsAwait(s.Alternate))
	case *jsast.WhileStatement:
		return expressionContainsAwait(s.Test) || statementContainsAwait(s.Body)
	case *jsast.DoWhileStatement:
		return expressionContainsAwait(s.Test) || statementContainsAwait(s.Body)
	case *jsast.ForStatement:
		if s.Initializer != nil && forLoopInitializerContainsAwait(s.Initializer) {
			return true
		}
		if s.Test != nil && expressionContainsAwait(s.Test) {
			return true
		}
		if s.Update != nil && expressionContainsAwait(s.Update) {
			return true
		}
		return statementContainsAwait(s.Body)
	case *jsast.ForInStatement:
		return forIntoContainsAwait(s.Into) || expressionContainsAwait(s.Source) || statementContainsAwait(s.Body)
	case *jsast.ForOfStatement:
		return forIntoContainsAwait(s.Into) || expressionContainsAwait(s.Source) || statementContainsAwait(s.Body)
	case *jsast.ReturnStatement:
		return s.Argument != nil && expressionContainsAwait(s.Argument)
	case *jsast.ThrowStatement:
		return expressionContainsAwait(s.Argument)
	case *jsast.SwitchStatement:
		if expressionContainsAwait(s.Discriminant) {
			return true
		}
		for _, c := range s.Body {
			if c.Test != nil && expressionContainsAwait(c.Test) {
				return true
			}
			if containsTopLevelAwait(c.Consequent) {
				return true
			}
		}
	case *jsast.TryStatement:
		if s.Body != nil && containsTopLevelAwait(s.Body.List) {
			return true
		}
		if s.Catch != nil && s.Catch.Body != nil && containsTopLevelAwait(s.Catch.Body.List) {
			return true
		}
		if s.Finally != nil && containsTopLevelAwait(s.Finally.List) {
			return true
		}
	case *jsast.WithStatement:
		return expressionContainsAwait(s.Object) || statementContainsAwait(s.Body)
	case *jsast.FunctionDeclaration, *jsast.ClassDeclaration:
		return false
	}
	return false
}

func forLoopInitializerContainsAwait(init jsast.ForLoopInitializer) bool {
	switch v := init.(type) {
	case *jsast.ForLoopInitializerExpression:
		return expressionContainsAwait(v.Expression)
	case *jsast.ForLoopInitializerVarDeclList:
		for _, b := range v.List {
			if b.Initializer != nil && expressionContainsAwait(b.Initializer) {
				return true
			}
		}
	case *jsast.ForLoopInitializerLexicalDecl:
		for _, b := range v.LexicalDeclaration.List {
			if b.Initializer != nil && expressionContainsAwait(b.Initializer) {
				return true
			}
		}
	}
	return false
}

func forIntoContainsAwait(into jsast.ForInto) bool {
	switch v := into.(type) {
	case *jsast.ForIntoExpression:
		return expressionContainsAwait(v.Expression)
	case *jsast.ForIntoVar:
		return v.Binding != nil && v.Binding.Initializer != nil && expressionContainsAwait(v.Binding.Initializer)
	case *jsast.ForDeclaration:
		return false
	default:
		return false
	}
}

func expressionContainsAwait(expr jsast.Expression) bool {
	switch e := expr.(type) {
	case nil:
		return false
	case *jsast.AwaitExpression:
		return true
	case *jsast.AssignExpression:
		return expressionContainsAwait(e.Left) || expressionContainsAwait(e.Right)
	case *jsast.ArrayLiteral:
		for _, v := range e.Value {
			if expressionContainsAwait(v) {
				return true
			}
		}
	case *jsast.ArrayPattern:
		for _, v := range e.Elements {
			if expressionContainsAwait(v) {
				return true
			}
		}
		return expressionContainsAwait(e.Rest)
	case *jsast.ObjectPattern:
		for _, prop := range e.Properties {
			switch p := prop.(type) {
			case *jsast.PropertyShort:
				if p.Initializer != nil && expressionContainsAwait(p.Initializer) {
					return true
				}
			case *jsast.PropertyKeyed:
				if expressionContainsAwait(p.Key) || expressionContainsAwait(p.Value) {
					return true
				}
			}
		}
		return expressionContainsAwait(e.Rest)
	case *jsast.BinaryExpression:
		return expressionContainsAwait(e.Left) || expressionContainsAwait(e.Right)
	case *jsast.UnaryExpression:
		return expressionContainsAwait(e.Operand)
	case *jsast.CallExpression:
		if expressionContainsAwait(e.Callee) {
			return true
		}
		for _, arg := range e.ArgumentList {
			if expressionContainsAwait(arg) {
				return true
			}
		}
	case *jsast.ConditionalExpression:
		return expressionContainsAwait(e.Test) || expressionContainsAwait(e.Consequent) || expressionContainsAwait(e.Alternate)
	case *jsast.BracketExpression:
		return expressionContainsAwait(e.Left) || expressionContainsAwait(e.Member)
	case *jsast.DotExpression:
		return expressionContainsAwait(e.Left)
	case *jsast.PrivateDotExpression:
		return expressionContainsAwait(e.Left)
	case *jsast.NewExpression:
		if expressionContainsAwait(e.Callee) {
			return true
		}
		for _, arg := range e.ArgumentList {
			if expressionContainsAwait(arg) {
				return true
			}
		}
	case *jsast.ObjectLiteral:
		for _, prop := range e.Value {
			switch p := prop.(type) {
			case *jsast.PropertyShort:
				if p.Initializer != nil && expressionContainsAwait(p.Initializer) {
					return true
				}
			case *jsast.PropertyKeyed:
				if expressionContainsAwait(p.Key) || expressionContainsAwait(p.Value) {
					return true
				}
			case *jsast.SpreadElement:
				if expressionContainsAwait(p.Expression) {
					return true
				}
			}
		}
	case *jsast.SequenceExpression:
		for _, item := range e.Sequence {
			if expressionContainsAwait(item) {
				return true
			}
		}
	case *jsast.TemplateLiteral:
		if expressionContainsAwait(e.Tag) {
			return true
		}
		for _, item := range e.Expressions {
			if expressionContainsAwait(item) {
				return true
			}
		}
	case *jsast.YieldExpression:
		return expressionContainsAwait(e.Argument)
	case *jsast.FunctionLiteral, *jsast.ArrowFunctionLiteral, *jsast.ClassLiteral:
		return false
	}
	return false
}

func containsTopLevelThis(stmts []jsast.Statement) bool {
	for _, stmt := range stmts {
		if statementContainsTopLevelThis(stmt) {
			return true
		}
	}
	return false
}

func statementContainsTopLevelThis(stmt jsast.Statement) bool {
	switch s := stmt.(type) {
	case *jsast.ExpressionStatement:
		return expressionContainsTopLevelThis(s.Expression)
	case *jsast.VariableStatement:
		for _, b := range s.List {
			if b.Initializer != nil && expressionContainsTopLevelThis(b.Initializer) {
				return true
			}
		}
	case *jsast.LexicalDeclaration:
		for _, b := range s.List {
			if b.Initializer != nil && expressionContainsTopLevelThis(b.Initializer) {
				return true
			}
		}
	case *jsast.BlockStatement:
		return containsTopLevelThis(s.List)
	case *jsast.IfStatement:
		return expressionContainsTopLevelThis(s.Test) || statementContainsTopLevelThis(s.Consequent) || (s.Alternate != nil && statementContainsTopLevelThis(s.Alternate))
	case *jsast.WhileStatement:
		return expressionContainsTopLevelThis(s.Test) || statementContainsTopLevelThis(s.Body)
	case *jsast.DoWhileStatement:
		return expressionContainsTopLevelThis(s.Test) || statementContainsTopLevelThis(s.Body)
	case *jsast.ForStatement:
		if s.Initializer != nil && forLoopInitializerContainsTopLevelThis(s.Initializer) {
			return true
		}
		if s.Test != nil && expressionContainsTopLevelThis(s.Test) {
			return true
		}
		if s.Update != nil && expressionContainsTopLevelThis(s.Update) {
			return true
		}
		return statementContainsTopLevelThis(s.Body)
	case *jsast.ForInStatement:
		return forIntoContainsTopLevelThis(s.Into) || expressionContainsTopLevelThis(s.Source) || statementContainsTopLevelThis(s.Body)
	case *jsast.ForOfStatement:
		return forIntoContainsTopLevelThis(s.Into) || expressionContainsTopLevelThis(s.Source) || statementContainsTopLevelThis(s.Body)
	case *jsast.ReturnStatement:
		return s.Argument != nil && expressionContainsTopLevelThis(s.Argument)
	case *jsast.ThrowStatement:
		return expressionContainsTopLevelThis(s.Argument)
	case *jsast.SwitchStatement:
		if expressionContainsTopLevelThis(s.Discriminant) {
			return true
		}
		for _, c := range s.Body {
			if c.Test != nil && expressionContainsTopLevelThis(c.Test) {
				return true
			}
			if containsTopLevelThis(c.Consequent) {
				return true
			}
		}
	case *jsast.TryStatement:
		if s.Body != nil && containsTopLevelThis(s.Body.List) {
			return true
		}
		if s.Catch != nil && s.Catch.Body != nil && containsTopLevelThis(s.Catch.Body.List) {
			return true
		}
		if s.Finally != nil && containsTopLevelThis(s.Finally.List) {
			return true
		}
	case *jsast.WithStatement:
		return expressionContainsTopLevelThis(s.Object) || statementContainsTopLevelThis(s.Body)
	case *jsast.FunctionDeclaration, *jsast.ClassDeclaration:
		return false
	}
	return false
}

func forLoopInitializerContainsTopLevelThis(init jsast.ForLoopInitializer) bool {
	switch v := init.(type) {
	case *jsast.ForLoopInitializerExpression:
		return expressionContainsTopLevelThis(v.Expression)
	case *jsast.ForLoopInitializerVarDeclList:
		for _, b := range v.List {
			if b.Initializer != nil && expressionContainsTopLevelThis(b.Initializer) {
				return true
			}
		}
	case *jsast.ForLoopInitializerLexicalDecl:
		for _, b := range v.LexicalDeclaration.List {
			if b.Initializer != nil && expressionContainsTopLevelThis(b.Initializer) {
				return true
			}
		}
	}
	return false
}

func forIntoContainsTopLevelThis(into jsast.ForInto) bool {
	switch v := into.(type) {
	case *jsast.ForIntoExpression:
		return expressionContainsTopLevelThis(v.Expression)
	case *jsast.ForIntoVar:
		return v.Binding != nil && v.Binding.Initializer != nil && expressionContainsTopLevelThis(v.Binding.Initializer)
	case *jsast.ForDeclaration:
		return false
	default:
		return false
	}
}

func expressionContainsTopLevelThis(expr jsast.Expression) bool {
	switch e := expr.(type) {
	case nil:
		return false
	case *jsast.ThisExpression:
		return true
	case *jsast.AssignExpression:
		return expressionContainsTopLevelThis(e.Left) || expressionContainsTopLevelThis(e.Right)
	case *jsast.ArrayLiteral:
		for _, v := range e.Value {
			if expressionContainsTopLevelThis(v) {
				return true
			}
		}
	case *jsast.ArrayPattern:
		for _, v := range e.Elements {
			if expressionContainsTopLevelThis(v) {
				return true
			}
		}
		return expressionContainsTopLevelThis(e.Rest)
	case *jsast.ObjectPattern:
		for _, prop := range e.Properties {
			switch p := prop.(type) {
			case *jsast.PropertyShort:
				if p.Initializer != nil && expressionContainsTopLevelThis(p.Initializer) {
					return true
				}
			case *jsast.PropertyKeyed:
				if expressionContainsTopLevelThis(p.Key) || expressionContainsTopLevelThis(p.Value) {
					return true
				}
			}
		}
		return expressionContainsTopLevelThis(e.Rest)
	case *jsast.BinaryExpression:
		return expressionContainsTopLevelThis(e.Left) || expressionContainsTopLevelThis(e.Right)
	case *jsast.UnaryExpression:
		return expressionContainsTopLevelThis(e.Operand)
	case *jsast.CallExpression:
		if expressionContainsTopLevelThis(e.Callee) {
			return true
		}
		for _, arg := range e.ArgumentList {
			if expressionContainsTopLevelThis(arg) {
				return true
			}
		}
	case *jsast.ConditionalExpression:
		return expressionContainsTopLevelThis(e.Test) || expressionContainsTopLevelThis(e.Consequent) || expressionContainsTopLevelThis(e.Alternate)
	case *jsast.BracketExpression:
		return expressionContainsTopLevelThis(e.Left) || expressionContainsTopLevelThis(e.Member)
	case *jsast.DotExpression:
		return expressionContainsTopLevelThis(e.Left)
	case *jsast.PrivateDotExpression:
		return expressionContainsTopLevelThis(e.Left)
	case *jsast.NewExpression:
		if expressionContainsTopLevelThis(e.Callee) {
			return true
		}
		for _, arg := range e.ArgumentList {
			if expressionContainsTopLevelThis(arg) {
				return true
			}
		}
	case *jsast.ObjectLiteral:
		for _, prop := range e.Value {
			switch p := prop.(type) {
			case *jsast.PropertyShort:
				if p.Initializer != nil && expressionContainsTopLevelThis(p.Initializer) {
					return true
				}
			case *jsast.PropertyKeyed:
				if expressionContainsTopLevelThis(p.Key) || expressionContainsTopLevelThis(p.Value) {
					return true
				}
			case *jsast.SpreadElement:
				if expressionContainsTopLevelThis(p.Expression) {
					return true
				}
			}
		}
	case *jsast.SequenceExpression:
		for _, item := range e.Sequence {
			if expressionContainsTopLevelThis(item) {
				return true
			}
		}
	case *jsast.TemplateLiteral:
		if expressionContainsTopLevelThis(e.Tag) {
			return true
		}
		for _, item := range e.Expressions {
			if expressionContainsTopLevelThis(item) {
				return true
			}
		}
	case *jsast.YieldExpression:
		return expressionContainsTopLevelThis(e.Argument)
	case *jsast.OptionalChain:
		return expressionContainsTopLevelThis(e.Expression)
	case *jsast.Optional:
		return expressionContainsTopLevelThis(e.Expression)
	case *jsast.FunctionLiteral, *jsast.ArrowFunctionLiteral, *jsast.ClassLiteral:
		return false
	}
	return false
}

func containsEvalAnywhere(stmts []jsast.Statement) bool {
	for _, stmt := range stmts {
		if statementContainsEvalAnywhere(stmt) {
			return true
		}
	}
	return false
}

func statementContainsEvalAnywhere(stmt jsast.Statement) bool {
	switch s := stmt.(type) {
	case *jsast.ExpressionStatement:
		return expressionContainsEvalAnywhere(s.Expression)
	case *jsast.VariableStatement:
		for _, b := range s.List {
			if bindingContainsEvalAnywhere(b) {
				return true
			}
		}
	case *jsast.LexicalDeclaration:
		for _, b := range s.List {
			if bindingContainsEvalAnywhere(b) {
				return true
			}
		}
	case *jsast.BlockStatement:
		return containsEvalAnywhere(s.List)
	case *jsast.IfStatement:
		return expressionContainsEvalAnywhere(s.Test) || statementContainsEvalAnywhere(s.Consequent) || (s.Alternate != nil && statementContainsEvalAnywhere(s.Alternate))
	case *jsast.WhileStatement:
		return expressionContainsEvalAnywhere(s.Test) || statementContainsEvalAnywhere(s.Body)
	case *jsast.DoWhileStatement:
		return expressionContainsEvalAnywhere(s.Test) || statementContainsEvalAnywhere(s.Body)
	case *jsast.ForStatement:
		if s.Initializer != nil && forLoopInitializerContainsEvalAnywhere(s.Initializer) {
			return true
		}
		if s.Test != nil && expressionContainsEvalAnywhere(s.Test) {
			return true
		}
		if s.Update != nil && expressionContainsEvalAnywhere(s.Update) {
			return true
		}
		return statementContainsEvalAnywhere(s.Body)
	case *jsast.ForInStatement:
		return forIntoContainsEvalAnywhere(s.Into) || expressionContainsEvalAnywhere(s.Source) || statementContainsEvalAnywhere(s.Body)
	case *jsast.ForOfStatement:
		return forIntoContainsEvalAnywhere(s.Into) || expressionContainsEvalAnywhere(s.Source) || statementContainsEvalAnywhere(s.Body)
	case *jsast.ReturnStatement:
		return s.Argument != nil && expressionContainsEvalAnywhere(s.Argument)
	case *jsast.ThrowStatement:
		return expressionContainsEvalAnywhere(s.Argument)
	case *jsast.SwitchStatement:
		if expressionContainsEvalAnywhere(s.Discriminant) {
			return true
		}
		for _, c := range s.Body {
			if c.Test != nil && expressionContainsEvalAnywhere(c.Test) {
				return true
			}
			if containsEvalAnywhere(c.Consequent) {
				return true
			}
		}
	case *jsast.TryStatement:
		if s.Body != nil && containsEvalAnywhere(s.Body.List) {
			return true
		}
		if s.Catch != nil {
			if bindingTargetContainsEvalAnywhere(s.Catch.Parameter) {
				return true
			}
			if s.Catch.Body != nil && containsEvalAnywhere(s.Catch.Body.List) {
				return true
			}
		}
		if s.Finally != nil && containsEvalAnywhere(s.Finally.List) {
			return true
		}
	case *jsast.WithStatement:
		return expressionContainsEvalAnywhere(s.Object) || statementContainsEvalAnywhere(s.Body)
	case *jsast.FunctionDeclaration:
		return functionLiteralContainsEvalAnywhere(s.Function)
	case *jsast.ClassDeclaration:
		return classLiteralContainsEvalAnywhere(s.Class)
	}
	return false
}

func forLoopInitializerContainsEvalAnywhere(init jsast.ForLoopInitializer) bool {
	switch v := init.(type) {
	case *jsast.ForLoopInitializerExpression:
		return expressionContainsEvalAnywhere(v.Expression)
	case *jsast.ForLoopInitializerVarDeclList:
		for _, b := range v.List {
			if bindingContainsEvalAnywhere(b) {
				return true
			}
		}
	case *jsast.ForLoopInitializerLexicalDecl:
		for _, b := range v.LexicalDeclaration.List {
			if bindingContainsEvalAnywhere(b) {
				return true
			}
		}
	}
	return false
}

func forIntoContainsEvalAnywhere(into jsast.ForInto) bool {
	switch v := into.(type) {
	case *jsast.ForIntoExpression:
		return expressionContainsEvalAnywhere(v.Expression)
	case *jsast.ForIntoVar:
		return v.Binding != nil && bindingContainsEvalAnywhere(v.Binding)
	case *jsast.ForDeclaration:
		return bindingTargetContainsEvalAnywhere(v.Target)
	default:
		return false
	}
}

func bindingContainsEvalAnywhere(binding *jsast.Binding) bool {
	if binding == nil {
		return false
	}
	if bindingTargetContainsEvalAnywhere(binding.Target) {
		return true
	}
	return expressionContainsEvalAnywhere(binding.Initializer)
}

func bindingTargetContainsEvalAnywhere(target jsast.BindingTarget) bool {
	switch t := target.(type) {
	case nil:
		return false
	case *jsast.Identifier:
		return t.Name.String() == "eval"
	case *jsast.ArrayPattern:
		for _, elem := range t.Elements {
			if expressionContainsEvalAnywhere(elem) {
				return true
			}
		}
		return expressionContainsEvalAnywhere(t.Rest)
	case *jsast.ObjectPattern:
		for _, prop := range t.Properties {
			switch p := prop.(type) {
			case *jsast.PropertyShort:
				if p.Name.Name.String() == "eval" {
					return true
				}
				if expressionContainsEvalAnywhere(p.Initializer) {
					return true
				}
			case *jsast.PropertyKeyed:
				if expressionContainsEvalAnywhere(p.Key) || expressionContainsEvalAnywhere(p.Value) {
					return true
				}
			}
		}
		return expressionContainsEvalAnywhere(t.Rest)
	default:
		return expressionContainsEvalAnywhere(t)
	}
}

func parameterListContainsEvalAnywhere(params *jsast.ParameterList) bool {
	if params == nil {
		return false
	}
	for _, binding := range params.List {
		if bindingContainsEvalAnywhere(binding) {
			return true
		}
	}
	return expressionContainsEvalAnywhere(params.Rest)
}

func functionLiteralContainsEvalAnywhere(fn *jsast.FunctionLiteral) bool {
	if fn == nil {
		return false
	}
	if fn.Name != nil && fn.Name.Name.String() == "eval" {
		return true
	}
	if parameterListContainsEvalAnywhere(fn.ParameterList) {
		return true
	}
	if fn.Body != nil {
		return containsEvalAnywhere(fn.Body.List)
	}
	return false
}

func arrowFunctionContainsEvalAnywhere(fn *jsast.ArrowFunctionLiteral) bool {
	if fn == nil {
		return false
	}
	if parameterListContainsEvalAnywhere(fn.ParameterList) {
		return true
	}
	switch body := fn.Body.(type) {
	case *jsast.BlockStatement:
		return containsEvalAnywhere(body.List)
	case *jsast.ExpressionBody:
		return expressionContainsEvalAnywhere(body.Expression)
	default:
		return false
	}
}

func classLiteralContainsEvalAnywhere(class *jsast.ClassLiteral) bool {
	if class == nil {
		return false
	}
	if class.Name != nil && class.Name.Name.String() == "eval" {
		return true
	}
	if expressionContainsEvalAnywhere(class.SuperClass) {
		return true
	}
	for _, elem := range class.Body {
		if classElementContainsEvalAnywhere(elem) {
			return true
		}
	}
	return false
}

func classElementContainsEvalAnywhere(elem jsast.ClassElement) bool {
	switch e := elem.(type) {
	case *jsast.FieldDefinition:
		return expressionContainsEvalAnywhere(e.Key) || expressionContainsEvalAnywhere(e.Initializer)
	case *jsast.MethodDefinition:
		return expressionContainsEvalAnywhere(e.Key) || functionLiteralContainsEvalAnywhere(e.Body)
	case *jsast.ClassStaticBlock:
		return e.Block != nil && containsEvalAnywhere(e.Block.List)
	default:
		return false
	}
}

func expressionContainsEvalAnywhere(expr jsast.Expression) bool {
	switch e := expr.(type) {
	case nil:
		return false
	case *jsast.Identifier:
		return e.Name.String() == "eval"
	case *jsast.AssignExpression:
		return expressionContainsEvalAnywhere(e.Left) || expressionContainsEvalAnywhere(e.Right)
	case *jsast.ArrayLiteral:
		for _, v := range e.Value {
			if expressionContainsEvalAnywhere(v) {
				return true
			}
		}
	case *jsast.ArrayPattern:
		for _, v := range e.Elements {
			if expressionContainsEvalAnywhere(v) {
				return true
			}
		}
		return expressionContainsEvalAnywhere(e.Rest)
	case *jsast.ObjectPattern:
		for _, prop := range e.Properties {
			switch p := prop.(type) {
			case *jsast.PropertyShort:
				if p.Name.Name.String() == "eval" {
					return true
				}
				if expressionContainsEvalAnywhere(p.Initializer) {
					return true
				}
			case *jsast.PropertyKeyed:
				if expressionContainsEvalAnywhere(p.Key) || expressionContainsEvalAnywhere(p.Value) {
					return true
				}
			}
		}
		return expressionContainsEvalAnywhere(e.Rest)
	case *jsast.BinaryExpression:
		return expressionContainsEvalAnywhere(e.Left) || expressionContainsEvalAnywhere(e.Right)
	case *jsast.UnaryExpression:
		return expressionContainsEvalAnywhere(e.Operand)
	case *jsast.CallExpression:
		if expressionContainsEvalAnywhere(e.Callee) {
			return true
		}
		for _, arg := range e.ArgumentList {
			if expressionContainsEvalAnywhere(arg) {
				return true
			}
		}
	case *jsast.ConditionalExpression:
		return expressionContainsEvalAnywhere(e.Test) || expressionContainsEvalAnywhere(e.Consequent) || expressionContainsEvalAnywhere(e.Alternate)
	case *jsast.BracketExpression:
		return expressionContainsEvalAnywhere(e.Left) || expressionContainsEvalAnywhere(e.Member)
	case *jsast.DotExpression:
		return e.Identifier.Name.String() == "eval" || expressionContainsEvalAnywhere(e.Left)
	case *jsast.PrivateDotExpression:
		return e.Identifier.Name.String() == "eval" || expressionContainsEvalAnywhere(e.Left)
	case *jsast.NewExpression:
		if expressionContainsEvalAnywhere(e.Callee) {
			return true
		}
		for _, arg := range e.ArgumentList {
			if expressionContainsEvalAnywhere(arg) {
				return true
			}
		}
	case *jsast.ObjectLiteral:
		for _, prop := range e.Value {
			switch p := prop.(type) {
			case *jsast.PropertyShort:
				if p.Name.Name.String() == "eval" {
					return true
				}
				if expressionContainsEvalAnywhere(p.Initializer) {
					return true
				}
			case *jsast.PropertyKeyed:
				if expressionContainsEvalAnywhere(p.Key) || expressionContainsEvalAnywhere(p.Value) {
					return true
				}
			case *jsast.SpreadElement:
				if expressionContainsEvalAnywhere(p.Expression) {
					return true
				}
			}
		}
	case *jsast.SequenceExpression:
		for _, item := range e.Sequence {
			if expressionContainsEvalAnywhere(item) {
				return true
			}
		}
	case *jsast.TemplateLiteral:
		if expressionContainsEvalAnywhere(e.Tag) {
			return true
		}
		for _, item := range e.Expressions {
			if expressionContainsEvalAnywhere(item) {
				return true
			}
		}
	case *jsast.YieldExpression:
		return expressionContainsEvalAnywhere(e.Argument)
	case *jsast.OptionalChain:
		return expressionContainsEvalAnywhere(e.Expression)
	case *jsast.Optional:
		return expressionContainsEvalAnywhere(e.Expression)
	case *jsast.FunctionLiteral:
		return functionLiteralContainsEvalAnywhere(e)
	case *jsast.ArrowFunctionLiteral:
		return arrowFunctionContainsEvalAnywhere(e)
	case *jsast.ClassLiteral:
		return classLiteralContainsEvalAnywhere(e)
	}
	return false
}

const (
	stashMaskConst  = uint32(1) << 31
	stashMaskVar    = uint32(1) << 30
	stashMaskStrict = uint32(1) << 29
	stashMaskType   = stashMaskConst | stashMaskVar | stashMaskStrict
)

func exposeReflectValue(v reflect.Value) reflect.Value {
	return reflect.NewAt(v.Type(), unsafe.Pointer(v.UnsafeAddr())).Elem()
}

func promotedStashSlotValue(stashValue reflect.Value, idx int) goja.Value {
	values := exposeReflectValue(stashValue.FieldByName("values")).Interface().([]goja.Value)
	if idx < 0 || idx >= len(values) {
		return goja.Undefined()
	}
	if values[idx] == nil {
		return goja.Undefined()
	}
	return values[idx]
}

func setPromotedStashSlotValue(stashValue reflect.Value, idx int, value goja.Value) {
	values := exposeReflectValue(stashValue.FieldByName("values")).Interface().([]goja.Value)
	if idx < 0 || idx >= len(values) {
		return
	}
	values[idx] = value
}

func bridgePromotedGlobals(rt *goja.Runtime, stashValue reflect.Value, names reflect.Value, varLike map[string]struct{}) error {
	globalObj := rt.GlobalObject()
	hasOwnValue := globalObj.Get("hasOwnProperty")
	hasOwn, ok := goja.AssertFunction(hasOwnValue)
	if !ok {
		return fmt.Errorf("promote wrapper stash: global object has no callable hasOwnProperty")
	}

	iter := names.MapRange()
	for iter.Next() {
		name := fmt.Sprint(iter.Key().Interface())
		if _, ok := varLike[name]; !ok {
			continue
		}
		idxRaw := uint32(iter.Value().Uint())
		exists, err := hasOwn(globalObj, rt.ToValue(name))
		if err != nil {
			return fmt.Errorf("promote wrapper stash: check global property %q: %w", name, err)
		}
		idx := int(idxRaw &^ stashMaskType)
		currentValue := promotedStashSlotValue(stashValue, idx)
		if exists.ToBoolean() {
			if err := globalObj.Set(name, currentValue); err != nil {
				return fmt.Errorf("promote wrapper stash: sync existing global %q: %w", name, err)
			}
			continue
		}
		getter := rt.ToValue(func(call goja.FunctionCall) goja.Value {
			return promotedStashSlotValue(stashValue, idx)
		})
		setter := rt.ToValue(func(call goja.FunctionCall) goja.Value {
			setPromotedStashSlotValue(stashValue, idx, call.Argument(0))
			return goja.Undefined()
		})
		if err := globalObj.DefineAccessorProperty(name, getter, setter, goja.FLAG_FALSE, goja.FLAG_TRUE); err != nil {
			return fmt.Errorf("promote wrapper stash: bridge global %q: %w", name, err)
		}
	}
	return nil
}

func promoteCurrentWrapperStash(rt *goja.Runtime, allowed map[string]struct{}, varLike map[string]struct{}) error {
	if rt == nil {
		return fmt.Errorf("promote wrapper stash: nil runtime")
	}
	rv := reflect.ValueOf(rt)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return fmt.Errorf("promote wrapper stash: invalid runtime value")
	}
	rv = rv.Elem()
	vmField := exposeReflectValue(rv.FieldByName("vm"))
	if !vmField.IsValid() || vmField.IsNil() {
		return fmt.Errorf("promote wrapper stash: runtime.vm missing")
	}
	vm := vmField.Elem()
	current := exposeReflectValue(vm.FieldByName("stash"))
	if !current.IsValid() || current.IsNil() {
		return fmt.Errorf("promote wrapper stash: vm.stash missing")
	}
	currentElem := current.Elem()
	names := exposeReflectValue(currentElem.FieldByName("names"))
	if !names.IsValid() || names.IsNil() {
		return fmt.Errorf("promote wrapper stash: current stash has no names map")
	}

	global := exposeReflectValue(rv.FieldByName("global"))
	globalStash := exposeReflectValue(global.FieldByName("stash"))
	stashType := globalStash.Type()
	oldCopy := reflect.New(stashType)
	oldCopy.Elem().Set(globalStash)

	filtered := reflect.MakeMapWithSize(names.Type(), len(allowed))
	it := names.MapRange()
	for it.Next() {
		nameValue := it.Key()
		name := fmt.Sprint(nameValue.Interface())
		if name == "arguments" || name == "this" {
			continue
		}
		if _, ok := allowed[name]; !ok {
			continue
		}
		filtered.SetMapIndex(nameValue, it.Value())
	}
	names.Set(filtered)
	exposeReflectValue(currentElem.FieldByName("obj")).Set(exposeReflectValue(oldCopy.Elem().FieldByName("obj")))
	exposeReflectValue(currentElem.FieldByName("funcType")).Set(exposeReflectValue(oldCopy.Elem().FieldByName("funcType")))
	exposeReflectValue(currentElem.FieldByName("outer")).Set(oldCopy)
	globalStash.Set(currentElem)
	if err := bridgePromotedGlobals(rt, globalStash, filtered, varLike); err != nil {
		return err
	}
	return nil
}
