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
