package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
)

// undocumentedFlagsByDesign are flags the binary accepts and `--help` deliberately
// does not advertise, each with the reason. They are diagnostics for someone already
// debugging enola itself, not part of the surface a user is meant to discover — and
// printing them would spend the most-read lines of --help on the least-used flags.
//
// Anything NOT listed here has to be documented. That is the whole point: the default
// is visible, and hiding a flag is a decision somebody wrote down.
var undocumentedFlagsByDesign = map[string]string{
	"--memstats":   "prints heap statistics while the server runs; a debugging aid for enola's own memory work, handled by splitMemFlags before normal parsing",
	"--memprofile": "writes a pprof heap profile to a path; same audience as --memstats",
	"-h":           "the short form of --help, documented on the same line as `--help, -h`",
	"--json":       "a modifier, documented on the lines it modifies (`--version --json`)",
	"--all":        "a modifier, documented on the line it modifies (`--status --all`)",
}

// TestAcceptedFlagsAreDocumented reads main.go's own argument switches and asserts
// that every flag they accept is one `enola --help` tells you about.
//
// pkg/command already asserts this in both directions for SUBCOMMANDS, and it exists
// because the help once advertised commands the binary could not run. Flags
// are the other half of the same surface and had no such tie: --memstats and
// --memprofile were accepted and unmentioned, which is fine, and nothing recorded
// that it was deliberate — so the next undocumented flag would have looked identical.
//
// It parses the AST rather than grepping because a `case "--x":` label is exactly what
// "the binary accepts this" means, and a regex over the file would equally match the
// flag named in an error message.
func TestAcceptedFlagsAreDocumented(t *testing.T) {
	accepted := acceptedFlags(t)
	if len(accepted) < 8 {
		t.Fatalf("found %d flags in main.go's switches (%v); the parsing or the file changed",
			len(accepted), accepted)
	}

	documented := map[string]bool{}
	for _, f := range helpSpec().Flags {
		// A doc line may name a flag and its modifier ("--status --all"), or a flag
		// and an alias ("--help, -h"). Every token that starts with a dash counts.
		for _, tok := range strings.FieldsFunc(f.Flag, func(r rune) bool { return r == ' ' || r == ',' }) {
			if strings.HasPrefix(tok, "-") {
				documented[tok] = true
			}
		}
	}

	for _, flag := range accepted {
		if documented[flag] {
			continue
		}
		if _, ok := undocumentedFlagsByDesign[flag]; ok {
			continue
		}
		t.Errorf("main.go accepts %q, but `enola --help` never mentions it — "+
			"document it in cli.DefaultHelp, or record why it is hidden in undocumentedFlagsByDesign", flag)
	}
}

// TestNoStaleUndocumentedFlags is the converse. A flag listed as hidden that the
// binary no longer accepts is a note about nothing; one that --help has since started
// documenting no longer needs excusing.
func TestNoStaleUndocumentedFlags(t *testing.T) {
	accepted := map[string]bool{}
	for _, f := range acceptedFlags(t) {
		accepted[f] = true
	}
	for flag, why := range undocumentedFlagsByDesign {
		if !accepted[flag] {
			t.Errorf("undocumentedFlagsByDesign lists %q (%s), but main.go no longer accepts it", flag, why)
		}
	}
}

// TestDocumentedFlagsAreAccepted is the direction that catches a promise the binary
// cannot keep: --help offering a flag nothing parses, which fails as an unrecognised
// argument — or worse, is taken for a repository path.
func TestDocumentedFlagsAreAccepted(t *testing.T) {
	accepted := map[string]bool{}
	for _, f := range acceptedFlags(t) {
		accepted[f] = true
	}
	for _, f := range helpSpec().Flags {
		for _, tok := range strings.FieldsFunc(f.Flag, func(r rune) bool { return r == ' ' || r == ',' }) {
			if !strings.HasPrefix(tok, "-") || strings.HasPrefix(tok, "-—") {
				continue
			}
			if !accepted[tok] {
				t.Errorf("`enola --help` documents %q, but no switch in main.go accepts it — "+
					"typing it falls through to the positional-argument branch and is read as a path", tok)
			}
		}
	}
}

// acceptedFlags returns every dash-prefixed string literal used as a `case` label in
// cmd/enola/main.go — the binary's complete flag vocabulary.
func acceptedFlags(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing main.go: %v", err)
	}

	seen := map[string]bool{}
	var out []string
	ast.Inspect(file, func(n ast.Node) bool {
		clause, ok := n.(*ast.CaseClause)
		if !ok {
			return true
		}
		for _, expr := range clause.List {
			lit, ok := expr.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			val, err := strconv.Unquote(lit.Value)
			if err != nil || !strings.HasPrefix(val, "-") {
				continue
			}
			if !seen[val] {
				seen[val] = true
				out = append(out, val)
			}
		}
		return true
	})
	return out
}
