package docslint

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/enola-labs/enola/internal/config"
	"github.com/enola-labs/enola/internal/explainers/layers"
	"github.com/enola-labs/enola/internal/intent"
	"github.com/enola-labs/enola/pkg/cli"
)

// Inventory is a closed vocabulary the documentation counts and enumerates.
//
// Every one is DERIVED from the value the running binary uses. That is the whole
// point: a hand-written copy here would be a second list to keep in step, which is
// the defect this package exists to catch.
type Inventory struct {
	// Key is how a check names it in a failure message.
	Key string
	// Source is where the value comes from, quoted verbatim in failures so the
	// reader knows which side to change.
	Source string
	// Items is the live vocabulary.
	Items []string
	// UserFacing marks a vocabulary whose members are IDENTIFIERS a reader types:
	// an explainer name goes in --fail-on, a tool name is what an agent calls, a
	// rule form is a YAML key. Those must each appear in the docs verbatim.
	//
	// Layer taxonomies are not: `go-standard` and `nextjs` are internal pattern
	// ids, and the pages that describe them quite correctly write "Go-standard"
	// and "Next.js". Demanding the raw key would push documentation toward
	// naming implementation details it has no reason to name.
	UserFacing bool
	// Quoted demands each item appear in backticks. Relation kinds such as `calls`
	// and `names` are ordinary English words, so a bare substring match would find
	// them on any page and prove nothing.
	Quoted bool
}

// Inventories returns every vocabulary the docs make counted claims about.
//
// Cross-repo signals and extractors are deliberately absent. Both are registered in
// pkg/bootstrap, which imports every extractor and therefore tree-sitter — importing
// it here would trade a millisecond check for a CGO build, and this package's value
// is that it is cheap enough to run in a git hook. Their doc contracts belong in a
// test that already pays for the engine.
func Inventories() []Inventory {
	tools := cli.OSSTools()
	toolNames := make([]string, 0, len(tools))
	for _, t := range tools {
		toolNames = append(toolNames, t.Name)
	}

	forms := make([]string, 0, len(intent.RuleForms))
	for _, f := range intent.RuleForms {
		forms = append(forms, f.Key)
	}

	return []Inventory{
		{Key: "explainers", Source: "config.KnownExplainers", Items: config.KnownExplainers, UserFacing: true},
		{Key: "MCP tools", Source: "cli.OSSTools()", Items: toolNames, UserFacing: true},
		{Key: "rule forms", Source: "intent.RuleForms", Items: forms, UserFacing: true},
		{Key: "layer taxonomies", Source: "layers.TaxonomyNames()", Items: layers.TaxonomyNames()},
		{Key: "fact kinds", Source: "the Kind* constants in internal/facts/model.go", Items: factConsts("Kind"), UserFacing: true, Quoted: true},
		{Key: "relation kinds", Source: "the Rel* constants in internal/facts/model.go", Items: factConsts("Rel"), UserFacing: true, Quoted: true},
	}
}

// factConsts returns the values of the string constants in internal/facts/model.go
// whose names start with prefix followed by an upper-case letter.
//
// It reads the source rather than a slice exported for the purpose: an exported list
// would be one more copy to keep in step with the constants, and the constants are
// what every extractor actually writes. A file that cannot be read yields one item no
// page can contain, so the contract fails loudly instead of checking an empty list.
func factConsts(prefix string) []string {
	path := filepath.Join(repoRoot, "internal", "facts", "model.go")
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		return []string{"<unreadable " + path + ": " + err.Error() + ">"}
	}
	var out []string
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, name := range vs.Names {
				rest, ok := strings.CutPrefix(name.Name, prefix)
				if !ok || rest == "" || rest[0] < 'A' || rest[0] > 'Z' || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				if v, err := strconv.Unquote(lit.Value); err == nil {
					out = append(out, v)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}
