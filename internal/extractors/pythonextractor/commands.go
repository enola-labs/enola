package pythonextractor

import (
	"bytes"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/tree-sitter/go-tree-sitter"
)

// Commands declared with click or typer.
//
//	@superset.command()
//	def load_examples(...): ...            superset load-examples
//
//	@app.command("init")
//	def initialize(...): ...               init
//
// The function is a command of the program, and the flow analysis names its flow by
// the command. The group a command is declared on is the program's name for it when
// the group has a name of its own; `app`, `cli` and the like name nothing.
//
// Where the groups are click groups the repository declares, the name is composed
// after the walk (composeCommandNames): a group is named by its declared name, a
// group declared on a group or attached with add_command is a subcommand of it, and
// a root group that is generic names nothing.
//
//	@click.group(name="testing")
//	def testing_group(): ...
//
//	@testing_group.command(name="tests")
//	def run_tests(): ...                   testing tests   (main.add_command(testing_group))

var (
	commandDecoratorRe = regexp.MustCompile(`^@\s*([\w.]+)\.command\s*\(`)
	groupDecoratorRe   = regexp.MustCompile(`^@\s*([\w.]+)\.group\s*\(`)
	commandNameArgRe   = regexp.MustCompile(`\.(?:command|group)\s*\(\s*(?:name\s*=\s*)?["']([^"']+)["']`)
	commandNameKwRe    = regexp.MustCompile(`\bname\s*=\s*["']([^"']+)["']`)
)

// The working props that carry a command's parts from the walk to
// composeCommandNames, which removes them.
const (
	propCommandLeaf     = "command_leaf"     // the command's own name
	propCommandParent   = "command_parent"   // the group it is declared on, as a target
	propCommandGroup    = "command_group"    // a group's declared name
	propCommandAttaches = "command_attaches" // "group\tcommand\tname" per add_command
	propConsoleScripts  = "console_scripts"  // "program\ttarget" per pyproject console script
)

// commandLibraries are the receivers of a decorator that declares a command or a
// group on no group of the repository.
var commandLibraries = map[string]bool{"click": true, "typer": true}

// genericGroups are group variable names that say nothing about the program.
var genericGroups = map[string]bool{
	"click": true, "typer": true, "app": true, "cli": true, "main": true, "group": true,
	"commands": true, "command": true, "root": true, "run": true,
}

// importsCommandLibrary reports whether a file imports click or typer, the libraries
// whose `.command()` decorator declares a command-line command.
func importsCommandLibrary(src []byte) bool {
	for _, imp := range [][]byte{[]byte("import click"), []byte("from click"), []byte("import typer"), []byte("from typer")} {
		if bytes.Contains(src, imp) {
			return true
		}
	}
	return false
}

// commandName returns the command a @<group>.command(...) decorator declares on the
// function fn: the name it is given, or the function's name with dashes for
// underscores, as both libraries derive it, prefixed by the group when the group has
// a name of its own.
func commandName(decorator, fn string) string {
	m := commandDecoratorRe.FindStringSubmatch(decorator)
	if m == nil {
		return ""
	}
	name := declaredCommandName(decorator, fn)
	if name == "" {
		return ""
	}
	group := m[1]
	if i := strings.LastIndexByte(group, '.'); i >= 0 {
		group = group[i+1:]
	}
	if genericGroups[group] {
		return name
	}
	return strings.ReplaceAll(group, "_", "-") + " " + name
}

// declaredCommandName is the name a .command(...) or .group(...) decorator gives fn:
// its first string argument or name=, or the function's name with dashes.
func declaredCommandName(decorator, fn string) string {
	if a := commandNameArgRe.FindStringSubmatch(decorator); a != nil {
		return a[1]
	}
	if k := commandNameKwRe.FindStringSubmatch(decorator); k != nil {
		return k[1]
	}
	return strings.ReplaceAll(strings.Trim(fn, "_"), "_", "-")
}

// decoratorGroup is the group a command or group decorator is declared on, when
// that is a plain name and not the library itself: "testing_group" for
// @testing_group.command(), "" for @click.command() or @cli.sub.command().
func decoratorGroup(decorator string, re *regexp.Regexp) string {
	m := re.FindStringSubmatch(decorator)
	if m == nil || strings.Contains(m[1], ".") || commandLibraries[m[1]] {
		return ""
	}
	return m[1]
}

// MCP tools declared with a decorator.
//
//	@mcp.tool()                 FastMCP
//	@registry.tool(tags={...})  a project's wrapper around it
//	@tool(...)                  a decorator imported from an MCP module
//
// The function is a tool of the server, and the flow analysis names its flow by the
// tool. A `.tool` decorator counts in a file that imports an MCP library, and a bare
// `@tool` only when it is imported from one.

var (
	toolDecoratorRe = regexp.MustCompile(`^@\s*(?:[\w.]+\.)?tool\b\s*(?:\(|$)`)
	toolNameKwRe    = regexp.MustCompile(`\bname\s*=\s*["']([^"']+)["']`)
	mcpImportRe     = regexp.MustCompile(`(?m)^\s*(?:from|import)\s+(?:fastmcp|[\w.]*\bmcp\b)`)
	mcpToolImportRe = regexp.MustCompile(`(?m)^\s*from\s+[\w.]*mcp[\w.]*\s+import\s+[^\n]*\btool\b`)
)

// importsMCP reports whether a file's decorator is an MCP tool declaration: a bare
// `@tool` imported from an MCP module, or `@x.tool` in a file importing one.
func importsMCP(src []byte, decorator string) bool {
	if strings.HasPrefix(strings.TrimSpace(strings.TrimPrefix(decorator, "@")), "tool") {
		return mcpToolImportRe.Match(src)
	}
	return mcpImportRe.Match(src)
}

// toolName returns the tool a decorator declares on fn: the name it is given, or the
// function's own name, as FastMCP derives it.
func toolName(decorator, fn string) string {
	if m := toolNameKwRe.FindStringSubmatch(decorator); m != nil {
		return m[1]
	}
	return fn
}

// composeCommandNames names each click command by the path of groups it is reached
// through, once call targets resolve across files, and removes the working props.
// A command whose path does not end at a root group the repository declares keeps
// the name commandName gave it.
func composeCommandNames(allFacts []facts.Fact, fileModules, pkgDirs map[string]bool) {
	var resolve func(target, file string) string
	ready := func() {
		fileIdx := buildSuffixIndex(fileModules, pkgDirs)
		topPkgs := importableRoots(fileModules, pkgDirs)
		reexports := buildReexportIndex(allFacts, pkgDirs)
		symbols := make(map[string]bool)
		for i := range allFacts {
			if allFacts[i].Kind == facts.KindSymbol {
				symbols[allFacts[i].Name] = true
			}
		}
		resolve = func(target, file string) string {
			if !isDottedCallTarget(target) {
				return target
			}
			r, keep := resolveDottedTarget(target, fileIdx, topPkgs, fileDir(file), reexports, symbols)
			if !keep {
				return ""
			}
			return r
		}
	}

	groups := map[string]string{}        // group symbol → declared name
	parent := map[string]string{}        // command or group symbol → its group
	renamed := map[string]string{}       // symbol → the name add_command gives it
	tableCommands := map[string]string{} // symbol → its argparse or table command
	programs := map[string]string{}      // a console script's target symbol → the program
	var commands []int
	for i := range allFacts {
		f := &allFacts[i]
		leaf, isGroup := f.PropString(propCommandLeaf), f.PropString(propCommandGroup)
		up, attaches := f.PropString(propCommandParent), stringSliceProp(f.Props, propCommandAttaches)
		table := stringSliceProp(f.Props, propCommandTable)
		scripts := stringSliceProp(f.Props, propConsoleScripts)
		if leaf == "" && isGroup == "" && up == "" && len(attaches) == 0 && len(table) == 0 && len(scripts) == 0 {
			continue
		}
		if resolve == nil {
			ready()
		}
		if isGroup != "" {
			groups[f.Name] = isGroup
		}
		if leaf != "" {
			commands = append(commands, i)
		}
		if up != "" {
			if p := importedDefinition(allFacts, resolve(up, f.File)); p != "" {
				parent[f.Name] = p
			}
		}
		for _, c := range scripts {
			name, target, ok := strings.Cut(c, "\t")
			if t := importedDefinition(allFacts, resolve(target, f.File)); ok && t != "" {
				programs[t] = name
			}
		}
		for _, c := range table {
			name, target, ok := strings.Cut(c, "\t")
			if t := resolve(target, f.File); ok && t != "" {
				tableCommands[t] = name
			}
		}
		for _, a := range attaches {
			parts := strings.SplitN(a, "\t", 3)
			if len(parts) != 3 {
				continue
			}
			g, c := importedDefinition(allFacts, resolve(parts[0], f.File)), importedDefinition(allFacts, resolve(parts[1], f.File))
			if g == "" || c == "" {
				continue
			}
			if _, ok := parent[c]; !ok {
				parent[c] = g
			}
			if parts[2] != "" {
				renamed[c] = parts[2]
			}
		}
	}

	// rootProgram is the console script that runs a root group: one whose target is
	// the group, or a function that calls it (`entrypoint()` wrapping
	// `dispatch_cli()` in error handling).
	var callers map[string][]string // symbol → the console-script targets that call it
	rootProgram := func(group string) string {
		if p := programs[group]; p != "" {
			return p
		}
		if callers == nil {
			callers = map[string][]string{}
			for i := range allFacts {
				s := &allFacts[i]
				if s.Kind != facts.KindSymbol || programs[s.Name] == "" {
					continue
				}
				for _, r := range s.Relations {
					if r.Kind == facts.RelCalls && !slices.Contains(callers[r.Target], s.Name) {
						callers[r.Target] = append(callers[r.Target], s.Name)
					}
				}
			}
		}
		if by := callers[group]; len(by) == 1 {
			return programs[by[0]]
		}
		return ""
	}

	for _, i := range commands {
		f := &allFacts[i]
		path := []string{f.PropString(propCommandLeaf)}
		if n := renamed[f.Name]; n != "" {
			path[0] = n
		}
		at, ok := parent[f.Name]
		for depth := 0; ok && depth < 16; depth++ {
			name, isGroup := groups[at]
			if !isGroup {
				ok = false
				break
			}
			if n := renamed[at]; n != "" {
				name = n
			}
			next, nested := parent[at]
			if !nested {
				if program := rootProgram(at); program != "" {
					path = append(path, program)
				} else if !genericGroups[name] {
					path = append(path, name)
				}
				break
			}
			path = append(path, name)
			at = next
		}
		if ok {
			slices.Reverse(path)
			f.SetProp("command", strings.Join(path, " "))
		}
	}

	for i := range allFacts {
		f := &allFacts[i]
		if name, ok := tableCommands[f.Name]; ok && f.Kind == facts.KindSymbol {
			f.SetProp("command", name)
		}
		for _, p := range []string{propCommandLeaf, propCommandParent, propCommandGroup, propCommandAttaches, propCommandTable, propConsoleScripts} {
			allFacts[i].DelProp(p)
		}
	}
}

// Commands declared with argparse, and command tables.
//
//	build = sub.add_parser("build")            build
//	build.set_defaults(func=run_build)
//
//	DAGS_COMMANDS = (ActionCommand(name="list", func=lazy_load_command("app.cli.dag_command.dag_list")), ...)
//	core_commands = [GroupCommand(name="dags", subcommands=DAGS_COMMANDS), ...]
//	                                           dags list
//
// The second is how a large CLI keeps its argparse tree as data that one builder
// walks, which leaves nothing for the argparse rule to read. Both are read only in a
// file that imports argparse: elsewhere a call with name= and func= is as often an
// agent's tool. A handler is a function name, or a dotted import path handed to a
// loader as its one string argument; the walk records each command as
// "<name>\t<target>" and composeCommandNames resolves the target.

// propCommandTable carries a file's argparse and table commands to
// composeCommandNames, which removes it.
const propCommandTable = "command_table"

var argparseImportRe = regexp.MustCompile(`(?m)^\s*(?:import argparse|from argparse import)`)

// argparseCommands returns the file's argparse and command-table commands.
func (w *pyWalker) argparseCommands(root *sitter.Node) []string {
	if !argparseImportRe.Match(w.src) {
		return nil
	}
	var out []string
	w.commandTables(root, &out)
	var visit func(n *sitter.Node, paths, subs map[string][]string)
	visit = func(n *sitter.Node, paths, subs map[string][]string) {
		for i := uint(0); i < n.NamedChildCount(); i++ {
			c := n.NamedChild(i)
			switch kindOf(c) {
			case "function_definition":
				visit(c, map[string][]string{}, map[string][]string{})
				continue
			case "assignment":
				left, right := c.ChildByFieldName("left"), c.ChildByFieldName("right")
				if left != nil && right != nil && kindOf(left) == "identifier" && kindOf(right) == "call" {
					if recv, method, args := w.methodCall(right); recv != "" {
						switch method {
						case "add_parser":
							if name := w.firstStringArg(args); name != "" {
								paths[pyText(left, w.src)] = append(slices.Clone(subs[recv]), name)
							}
						case "add_subparsers":
							subs[pyText(left, w.src)] = slices.Clone(paths[recv])
						}
					}
				}
			case "call":
				if path, target := w.setDefaultsCommand(c, paths, subs); len(path) > 0 && target != "" {
					out = append(out, strings.Join(path, " ")+"\t"+target)
				}
			}
			visit(c, paths, subs)
		}
	}
	visit(root, map[string][]string{}, map[string][]string{})
	return out
}

// methodCall splits `recv.method(args)` with a plain-name receiver.
func (w *pyWalker) methodCall(call *sitter.Node) (recv, method string, args *sitter.Node) {
	fn := call.ChildByFieldName("function")
	if fn == nil || kindOf(fn) != "attribute" {
		return "", "", nil
	}
	obj, attr := fn.ChildByFieldName("object"), fn.ChildByFieldName("attribute")
	if obj == nil || attr == nil || kindOf(obj) != "identifier" {
		return "", "", nil
	}
	return pyText(obj, w.src), pyText(attr, w.src), call.ChildByFieldName("arguments")
}

// setDefaultsCommand reads `p.set_defaults(func=f)` and
// `sub.add_parser("n").set_defaults(func=f)`: the parser's command path and f.
func (w *pyWalker) setDefaultsCommand(call *sitter.Node, paths, subs map[string][]string) ([]string, string) {
	fn := call.ChildByFieldName("function")
	if fn == nil || kindOf(fn) != "attribute" {
		return nil, ""
	}
	obj, attr := fn.ChildByFieldName("object"), fn.ChildByFieldName("attribute")
	if obj == nil || attr == nil || pyText(attr, w.src) != "set_defaults" {
		return nil, ""
	}
	value := w.keywordArg(call.ChildByFieldName("arguments"), "func")
	if value == nil {
		return nil, ""
	}
	var path []string
	switch kindOf(obj) {
	case "identifier":
		path = paths[pyText(obj, w.src)]
	case "call":
		if recv, method, args := w.methodCall(obj); method == "add_parser" {
			if name := w.firstStringArg(args); name != "" {
				path = append(slices.Clone(subs[recv]), name)
			}
		}
	}
	return path, w.handlerTarget(value)
}

// commandTables reads the lists and tuples of command calls in the file, at module
// level or in a function that builds or returns them (`commands = [GroupCommand(...)]`,
// `commands.append(GroupCommand(...))`, `return [GroupCommand(...)]`).
func (w *pyWalker) commandTables(root *sitter.Node, out *[]string) {
	type entry struct{ name, target, sublist string }
	lists := map[string][]entry{}
	var order []string
	read := func(list string, el *sitter.Node) {
		if kindOf(el) != "call" {
			return
		}
		args := el.ChildByFieldName("arguments")
		nameNode := w.keywordArg(args, "name")
		if nameNode == nil || kindOf(nameNode) != "string" {
			return
		}
		e := entry{name: unquotePyType(pyText(nameNode, w.src))}
		if f := w.keywordArg(args, "func"); f != nil {
			e.target = w.handlerTarget(f)
		} else if sub := w.keywordArg(args, "subcommands"); sub != nil && kindOf(sub) == "identifier" {
			e.sublist = pyText(sub, w.src)
		}
		if e.name == "" || (e.target == "" && e.sublist == "") {
			return
		}
		if _, seen := lists[list]; !seen {
			order = append(order, list)
		}
		lists[list] = append(lists[list], e)
	}
	var visit func(n *sitter.Node)
	visit = func(n *sitter.Node) {
		switch kindOf(n) {
		case "list", "tuple":
			// A list assigned to a name is keyed by it, so a group can name it as its
			// subcommands; any other (`return [GroupCommand(...)]`) by its position.
			key := "@" + strconv.Itoa(int(n.StartByte()))
			if p := n.Parent(); p != nil && kindOf(p) == "assignment" {
				if left := p.ChildByFieldName("left"); left != nil && kindOf(left) == "identifier" {
					key = pyText(left, w.src)
				}
			}
			for j := uint(0); j < n.NamedChildCount(); j++ {
				read(key, n.NamedChild(j))
			}
		case "call":
			if recv, method, args := w.methodCall(n); method == "append" && args != nil && args.NamedChildCount() == 1 {
				read(recv, args.NamedChild(0))
			}
		}
		for i := uint(0); i < n.NamedChildCount(); i++ {
			visit(n.NamedChild(i))
		}
	}
	visit(root)
	under := map[string]string{} // sublist -> the group name it is listed under
	parent := map[string]string{}
	for _, l := range order {
		for _, e := range lists[l] {
			if e.sublist != "" {
				under[e.sublist], parent[e.sublist] = e.name, l
			}
		}
	}
	for _, l := range order {
		var prefix []string
		for at, depth := l, 0; depth < 8; depth++ {
			g, ok := under[at]
			if !ok {
				break
			}
			prefix = append([]string{g}, prefix...)
			at = parent[at]
		}
		for _, e := range lists[l] {
			if e.target != "" {
				*out = append(*out, strings.Join(append(slices.Clone(prefix), e.name), " ")+"\t"+e.target)
			}
		}
	}
}

// keywordArg is the value of a call's keyword argument, or nil.
func (w *pyWalker) keywordArg(args *sitter.Node, key string) *sitter.Node {
	if args == nil {
		return nil
	}
	for i := uint(0); i < args.NamedChildCount(); i++ {
		a := args.NamedChild(i)
		if kindOf(a) != "keyword_argument" {
			continue
		}
		if k := a.ChildByFieldName("name"); k != nil && pyText(k, w.src) == key {
			return a.ChildByFieldName("value")
		}
	}
	return nil
}

// firstStringArg is a call's first positional argument when it is a string.
func (w *pyWalker) firstStringArg(args *sitter.Node) string {
	if args == nil || args.NamedChildCount() == 0 {
		return ""
	}
	if a := args.NamedChild(0); kindOf(a) == "string" {
		return unquotePyType(pyText(a, w.src))
	}
	return ""
}

// handlerTarget names the function a func= value hands over: a name, or the dotted
// import path a loader is given as its one string argument.
func (w *pyWalker) handlerTarget(v *sitter.Node) string {
	switch kindOf(v) {
	case "identifier":
		return w.valueRefTarget(pyText(v, w.src))
	case "call":
		args := v.ChildByFieldName("arguments")
		if args == nil || args.NamedChildCount() != 1 {
			return ""
		}
		if path := w.firstStringArg(args); strings.Count(path, ".") >= 1 && isPyDottedName(path) {
			return path
		}
	}
	return ""
}

// importedDefinition follows a resolved "<module>.<name>" that names no symbol
// through the module's own import of that name, and on through a module that
// imports it in turn: breeze's console script points at airflow_breeze.breeze:main,
// a name breeze.py only imports from main_command. It returns the target unchanged
// when it is a symbol or no import of it is found.
func importedDefinition(allFacts []facts.Fact, target string) string {
	for hop := 0; hop < 4 && target != ""; hop++ {
		next := importedOnce(allFacts, target)
		if next == target {
			break
		}
		target = next
	}
	return target
}

func importedOnce(allFacts []facts.Fact, target string) string {
	i := strings.LastIndexByte(target, '.')
	if i < 0 {
		return target
	}
	module, name := target[:i], target[i+1:]
	file := module + ".py"
	for j := range allFacts {
		if allFacts[j].Kind == facts.KindSymbol && allFacts[j].Name == target {
			return target
		}
	}
	for j := range allFacts {
		d := &allFacts[j]
		if d.Kind != facts.KindDependency || d.File != file {
			continue
		}
		imported := bindingOf(d.PropAny("bindings"), name)
		if imported == "" {
			continue
		}
		for _, r := range d.Relations {
			if r.Kind == facts.RelImports && strings.ContainsRune(r.Target, '/') {
				return strings.TrimSuffix(r.Target, ".py") + "." + imported
			}
		}
	}
	return target
}

// bindingOf reads a dependency's local-to-imported name map, as the walk wrote it
// or as JSON read it back.
func bindingOf(v any, local string) string {
	switch m := v.(type) {
	case map[string]string:
		return m[local]
	case map[string]any:
		s, _ := m[local].(string)
		return s
	}
	return ""
}
