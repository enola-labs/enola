package pythonextractor

import (
	"bytes"
	"regexp"
	"slices"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
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

	groups := map[string]string{}  // group symbol → declared name
	parent := map[string]string{}  // command or group symbol → its group
	renamed := map[string]string{} // symbol → the name add_command gives it
	var commands []int
	for i := range allFacts {
		f := &allFacts[i]
		leaf, isGroup := f.PropString(propCommandLeaf), f.PropString(propCommandGroup)
		up, attaches := f.PropString(propCommandParent), stringSliceProp(f.Props, propCommandAttaches)
		if leaf == "" && isGroup == "" && up == "" && len(attaches) == 0 {
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
			if p := resolve(up, f.File); p != "" {
				parent[f.Name] = p
			}
		}
		for _, a := range attaches {
			parts := strings.SplitN(a, "\t", 3)
			if len(parts) != 3 {
				continue
			}
			g, c := resolve(parts[0], f.File), resolve(parts[1], f.File)
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
				if !genericGroups[name] {
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
		for _, p := range []string{propCommandLeaf, propCommandParent, propCommandGroup, propCommandAttaches} {
			allFacts[i].DelProp(p)
		}
	}
}
