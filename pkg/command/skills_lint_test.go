package command

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/enola-labs/enola/pkg/cli"
	"github.com/enola-labs/enola/plugins/enola/skills"
)

// skillNonTools are snake_case words the skills write in backticks that are not MCP
// tools: fact properties and kinds, explainer and status names. Anything else in that
// shape must be a tool the server registers.
var skillNonTools = map[string]bool{
	"body_hash": true, "decl_hash": true, "inner_hash": true, "cited_hash": true, "cited_lines": true,
	"span_start": true, "symbol_kind": true, "unmatched_by_clients": true, "unreached_by_clients": true,
	"test_ref": true, "member_of": true, "enola_intent": true,
	// Rule vocabulary in enola-intent.yaml.
	"depends_on": true, "forbid_cycles": true,
}

var (
	codeSpan  = regexp.MustCompile("`([^`\n]+)`")
	toolShape = regexp.MustCompile(`^[a-z]+(?:_[a-z]+)+$`)
)

// TestSkillsNameRealCommandsAndTools — a skill is instructions an agent follows to the
// letter, so a flag that was renamed or a tool that was merged turns it into a recipe
// for an error. Every `enola ...` line in a skill's code must name a command, subcommand
// and flags the binary has, and every tool-shaped word must be a tool the server
// registers. The skills ship with the binary, so this is checked against the binary
// they ship with.
func TestSkillsNameRealCommandsAndTools(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	all, err := skills.All()
	if err != nil {
		t.Fatal(err)
	}
	tools := map[string]bool{}
	for _, e := range cli.OSSTools() {
		tools[e.Name] = true
	}

	bin := buildEnola(t)
	helpCache := map[string]string{}
	help := func(path []string) string {
		key := strings.Join(path, " ")
		if h, ok := helpCache[key]; ok {
			return h
		}
		res := runEnola(t, bin, t.TempDir(), t.TempDir(), append(slices.Clone(path), "--help")...)
		helpCache[key] = res.stdout + res.stderr
		return helpCache[key]
	}

	checked := 0
	for _, s := range all {
		for _, f := range s.Files {
			if !strings.HasSuffix(f.Path, ".md") {
				continue
			}
			where := s.Dir + "/" + f.Path
			for _, code := range codeOf(string(f.Content)) {
				if toolShape.MatchString(code) && !tools[code] && !skillNonTools[code] {
					t.Errorf("%s: `%s` looks like a tool and is none; add it to skillNonTools if it is a property", where, code)
				}
				for _, inv := range invocations(code) {
					checked++
					if err := checkInvocation(inv, help); err != nil {
						t.Errorf("%s: %q: %v", where, strings.Join(inv, " "), err)
					}
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no enola invocations found in any skill, so this test checked nothing")
	}
}

// codeOf returns a markdown file's inline code spans and the lines of its fenced blocks.
func codeOf(md string) []string {
	var out []string
	inFence := false
	for _, line := range strings.Split(md, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			out = append(out, strings.TrimSpace(line))
			continue
		}
		for _, m := range codeSpan.FindAllStringSubmatch(line, -1) {
			out = append(out, m[1])
		}
	}
	return out
}

// invocations splits a code snippet into the enola command lines it holds: the words
// after each `enola`, up to a shell operator.
func invocations(code string) [][]string {
	var out [][]string
	words := strings.Fields(code)
	for i, w := range words {
		if w != "enola" {
			continue
		}
		var inv []string
		for _, a := range words[i+1:] {
			if strings.ContainsAny(a, "|;&>") {
				break
			}
			inv = append(inv, a)
		}
		out = append(out, inv)
	}
	return out
}

// checkInvocation checks the command path and every flag of one invocation.
func checkInvocation(args []string, help func([]string) string) error {
	// `enola help <command> [<sub>]` is checked as the command it names.
	if len(args) > 0 && args[0] == "help" {
		return checkInvocation(args[1:], help)
	}
	var path []string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd := args[0]
		if !slices.Contains(Subcommands(), cmd) && cmd != "upgrade" {
			if looksLikeArgument(cmd) {
				return nil // `enola <path>`: the server, not a command
			}
			return fmt.Errorf("no command %q", cmd)
		}
		path = []string{cmd}
		if subs := subcommandsOf[cmd]; len(subs) > 0 && len(args) > 1 && !strings.HasPrefix(args[1], "-") {
			if slices.Contains(subs, args[1]) {
				path = append(path, args[1])
			} else if !looksLikeArgument(args[1]) {
				return fmt.Errorf("%s has no subcommand %q (it has %s)", cmd, args[1], strings.Join(subs, ", "))
			}
		}
	}
	page := help(path)
	for _, a := range args {
		if !strings.HasPrefix(a, "-") || a == "-" {
			continue
		}
		name, _, _ := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if !regexp.MustCompile(`(?m)(^|\s)--?` + regexp.QuoteMeta(name) + `\b`).MatchString(page) {
			return fmt.Errorf("flag --%s is not on the help page of `enola %s`", name, strings.Join(path, " "))
		}
	}
	return nil
}

// looksLikeArgument tells a positional argument (a path, a placeholder, a file:line)
// from a misspelt command word.
func looksLikeArgument(w string) bool {
	return strings.ContainsAny(w, "/.:<>'\"$*") || !regexp.MustCompile(`^[a-z][a-z-]*$`).MatchString(w)
}
