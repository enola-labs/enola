package pythonextractor

import (
	"bytes"
	"regexp"
	"strings"
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

var (
	commandDecoratorRe = regexp.MustCompile(`^@\s*([\w.]+)\.command\s*\(`)
	commandNameArgRe   = regexp.MustCompile(`\.command\s*\(\s*(?:name\s*=\s*)?["']([^"']+)["']`)
	commandNameKwRe    = regexp.MustCompile(`\bname\s*=\s*["']([^"']+)["']`)
)

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
	name := ""
	if a := commandNameArgRe.FindStringSubmatch(decorator); a != nil {
		name = a[1]
	} else if k := commandNameKwRe.FindStringSubmatch(decorator); k != nil {
		name = k[1]
	} else {
		name = strings.ReplaceAll(strings.Trim(fn, "_"), "_", "-")
	}
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
