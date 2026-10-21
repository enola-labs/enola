package goextractor

import (
	"go/ast"
	"go/token"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// Commands.
//
// A command-line program has one main and many ways in: `enola check`, `gitea web`.
// As a flow, the main is one entry reaching everything every command reaches. The
// command is the entry a reader means, and the function that runs it is the symbol to
// walk from. Three ways of declaring one are read:
//
//	switch args[0] { case "check": r.Check(ctx, args[1:]) }      a string-keyed dispatch
//	&cobra.Command{Use: "check [flags]", RunE: runCheck}          cobra
//	&cli.Command{Name: "web", Action: runWeb}                     urfave/cli
//
// The function a command runs gets a `command` prop, "<program> <name>", which is what
// the flow analysis names its flow. The program is the base of the module path.

// commandProp names the command a function runs, as a user types it.
const commandProp = "command"

// dispatchProp carries a function's string-keyed dispatches from extractFunc to
// bindCommands, which removes it: each entry is "<name>\x00<callee>".
const dispatchProp = "_dispatches"

// stringDispatches returns the commands a function dispatches by a string switch on
// an argument: `switch args[0]` or `switch os.Args[1]`, with two or more cases that
// are words and not flags.
func stringDispatches(body *ast.BlockStmt, ctx resolveCtx) []string {
	var out []string
	ast.Inspect(body, func(n ast.Node) bool {
		sw, ok := n.(*ast.SwitchStmt)
		if !ok || !isArgumentIndex(sw.Tag) {
			return true
		}
		var found []string
		words := 0
		for _, stmt := range sw.Body.List {
			clause, ok := stmt.(*ast.CaseClause)
			if !ok {
				continue
			}
			var names []string
			for _, e := range clause.List {
				lit, ok := e.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				name, err := strconv.Unquote(lit.Value)
				if err != nil || name == "" || strings.HasPrefix(name, "-") {
					continue
				}
				names = append(names, name)
			}
			if len(names) == 0 {
				continue
			}
			words++
			// A case that runs a command hands it to a function first thing:
			// `r.Check(ctx, args[1:])`, `return run(args)`, `err = run(args)`. A case
			// that does its work inline, `for _, n := range names() { ... }`, runs
			// no function a flow could start from, and its first call is a helper.
			call := leadingCall(clause.Body)
			if call == nil {
				continue
			}
			if key := resolveCalleeKey(call, ctx); key != "" {
				for _, name := range names {
					found = append(found, name+"\x00"+key)
				}
			}
		}
		if words >= 2 {
			out = append(out, found...)
		}
		return true
	})
	return out
}

// leadingCall returns the call a case's first statement makes, or nil: a call
// statement, a return of a call, or an assignment from one.
func leadingCall(body []ast.Stmt) *ast.CallExpr {
	if len(body) == 0 {
		return nil
	}
	var e ast.Expr
	switch s := body[0].(type) {
	case *ast.ExprStmt:
		e = s.X
	case *ast.ReturnStmt:
		if len(s.Results) == 1 {
			e = s.Results[0]
		}
	case *ast.AssignStmt:
		if len(s.Rhs) == 1 {
			e = s.Rhs[0]
		}
	}
	call, _ := e.(*ast.CallExpr)
	return call
}

// isArgumentIndex reports whether a switch tag is an element of a program's
// arguments: `args[0]`, `argv[1]`, `os.Args[1]`.
func isArgumentIndex(tag ast.Expr) bool {
	idx, ok := tag.(*ast.IndexExpr)
	if !ok {
		return false
	}
	if lit, ok := idx.Index.(*ast.BasicLit); !ok || lit.Kind != token.INT {
		return false
	}
	switch x := idx.X.(type) {
	case *ast.Ident:
		return x.Name == "args" || x.Name == "argv"
	case *ast.SelectorExpr:
		pkg, ok := x.X.(*ast.Ident)
		return ok && pkg.Name == "os" && x.Sel.Name == "Args"
	}
	return false
}

// commandLiterals returns the commands a file declares as cobra or urfave/cli
// command literals, each "<name>\x00<function key>", for those whose run function is
// a function of the module named directly.
func commandLiterals(f *ast.File, pkgDir string, fileImports map[string]string) []string {
	cobra, urfave := "", ""
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		alias := importAlias(imp, p)
		switch {
		case p == "github.com/spf13/cobra":
			cobra = alias
		case strings.HasPrefix(p, "github.com/urfave/cli"):
			urfave = alias
		}
	}
	if cobra == "" && urfave == "" {
		return nil
	}
	// commandType reports which library a type expression is the Command of, "" for
	// none: `cobra.Command`, `*cli.Command`.
	commandType := func(t ast.Expr) string {
		if star, ok := t.(*ast.StarExpr); ok {
			t = star.X
		}
		sel, ok := t.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Command" {
			return ""
		}
		pkg, ok := sel.X.(*ast.Ident)
		switch {
		case !ok:
			return ""
		case cobra != "" && pkg.Name == cobra:
			return "cobra"
		case urfave != "" && pkg.Name == urfave:
			return "urfave"
		}
		return ""
	}
	var out []string
	read := func(lit *ast.CompositeLit, library string) {
		nameKey, runKeys := "Use", []string{"RunE", "Run"}
		if library == "urfave" {
			nameKey, runKeys = "Name", []string{"Action"}
		}
		fields := map[string]ast.Expr{}
		for _, elt := range lit.Elts {
			if kv, ok := elt.(*ast.KeyValueExpr); ok {
				if k, ok := kv.Key.(*ast.Ident); ok {
					fields[k.Name] = kv.Value
				}
			}
		}
		name := ""
		if v, ok := fields[nameKey].(*ast.BasicLit); ok && v.Kind == token.STRING {
			if s, err := strconv.Unquote(v.Value); err == nil {
				if words := strings.Fields(s); len(words) > 0 {
					name = words[0]
				}
			}
		}
		if name == "" {
			return
		}
		for _, k := range runKeys {
			if key := runFunctionKey(fields[k], pkgDir, fileImports); key != "" {
				out = append(out, name+"\x00"+key)
				break
			}
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		if library := commandType(lit.Type); library != "" {
			read(lit, library)
			return true
		}
		// `[]*cli.Command{{Name: "install", ...}}`: the elements leave the type out.
		if arr, ok := lit.Type.(*ast.ArrayType); ok {
			if library := commandType(arr.Elt); library != "" {
				for _, elt := range lit.Elts {
					if el, ok := elt.(*ast.CompositeLit); ok && el.Type == nil {
						read(el, library)
					}
				}
			}
		}
		return true
	})
	return out
}

// runFunctionKey names the function a command's run field runs: the function itself,
// the one function a wrapper is given (`runPluginCommand(installCommand)`), or the
// function a literal hands the work to first (`func(c) error { return RunServer(c) }`).
func runFunctionKey(v ast.Expr, pkgDir string, fileImports map[string]string) string {
	switch e := v.(type) {
	case *ast.CallExpr:
		if len(e.Args) == 1 {
			return functionValueKey(e.Args[0], pkgDir, fileImports)
		}
	case *ast.FuncLit:
		if e.Body != nil {
			if call := leadingCall(e.Body.List); call != nil {
				return functionValueKey(call.Fun, pkgDir, fileImports)
			}
		}
	default:
		return functionValueKey(v, pkgDir, fileImports)
	}
	return ""
}

// functionValueKey names the function a value refers to: one of this package by
// name, or one of an imported package of the module as `pkg.Func`.
func functionValueKey(v ast.Expr, pkgDir string, fileImports map[string]string) string {
	switch e := v.(type) {
	case *ast.Ident:
		return pkgDir + "." + e.Name
	case *ast.SelectorExpr:
		if pkg, ok := e.X.(*ast.Ident); ok {
			if dir := fileImports[pkg.Name]; dir != "" {
				return dir + "." + e.Sel.Name
			}
		}
	}
	return ""
}

// bindCommands gives each function a command runs its command prop. found holds the
// command literals of every file, and each dispatching function carries its own in
// dispatchProp. A command whose candidates name no declared function is dropped.
func bindCommands(allFacts []facts.Fact, found []string, mods goModules) {
	declared := map[string]bool{}
	for i := range allFacts {
		if allFacts[i].Kind == facts.KindSymbol {
			declared[allFacts[i].Name] = true
		}
	}
	command := map[string]string{} // function -> command name
	settled := map[string]bool{}   // command name -> bound
	take := func(entries []string) {
		for _, e := range entries {
			name, key, _ := strings.Cut(e, "\x00")
			if settled[name] || !declared[key] {
				continue
			}
			settled[name] = true
			if _, taken := command[key]; !taken {
				command[key] = name
			}
		}
	}
	sort.Strings(found)
	take(found)

	// A dispatch inside a function that itself runs a command is that command's
	// subcommands: `providers list` is dispatched by the function `providers` runs. So
	// a dispatcher is taken once its own command is known, and its names are prefixed
	// with it. One that runs no command is the program's own dispatch.
	dispatchers := map[string][]string{} // dispatching function -> its entries
	var order []string
	for i := range allFacts {
		f := &allFacts[i]
		if f.Kind != facts.KindSymbol {
			continue
		}
		if entries, ok := f.Props[dispatchProp].([]string); ok {
			if _, seen := dispatchers[f.Name]; !seen {
				order = append(order, f.Name)
			}
			dispatchers[f.Name] = append(dispatchers[f.Name], entries...)
			delete(f.Props, dispatchProp)
		}
	}
	sort.Strings(order)
	nested := map[string]bool{}
	for _, d := range order {
		nested[d] = isCommandTarget(d, dispatchers)
	}
	for _, d := range order {
		if !nested[d] {
			take(dispatchers[d])
			delete(dispatchers, d)
		}
	}
	for round := 0; round < 3 && len(dispatchers) > 0; round++ {
		for _, d := range order {
			parent, ok := command[d]
			entries, pending := dispatchers[d]
			if !ok || !pending {
				continue
			}
			prefixed := make([]string, len(entries))
			for i, e := range entries {
				prefixed[i] = parent + " " + e
			}
			take(prefixed)
			delete(dispatchers, d)
		}
	}
	if len(command) == 0 {
		return
	}
	// The program is the module's last path element, without a domain: `gitea.dev`
	// is gitea.
	program := path.Base(mods.rootPath())
	if i := strings.IndexByte(program, '.'); i > 0 {
		program = program[:i]
	}
	if program == "." || program == "/" || program == "" {
		program = ""
	} else {
		program += " "
	}
	for i := range allFacts {
		f := &allFacts[i]
		if f.Kind == facts.KindSymbol {
			if name, ok := command[f.Name]; ok {
				f.SetProp(commandProp, program+name)
			}
		}
	}
}

// isCommandTarget reports whether a dispatching function is among the candidates of
// another dispatch, which makes its own dispatch a level of subcommands.
func isCommandTarget(fn string, dispatchers map[string][]string) bool {
	for d, entries := range dispatchers {
		if d == fn {
			continue
		}
		for _, e := range entries {
			if _, key, _ := strings.Cut(e, "\x00"); key == fn {
				return true
			}
		}
	}
	return false
}
