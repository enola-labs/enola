package tree_sitter

import (
	"os"
	"strings"
	"testing"

	csharp "github.com/enola-labs/enola/internal/extractors/dotnetextractor/grammar"
)

// The two error-recovery hunks ATTRIBUTION.md says this copy restores. A re-vendor
// that copies a newer upstream parser.c over them fails here.
var restoredRecovery = []string{
	"static const unsigned MAX_COST_DIFFERENCE = 16 * ERROR_COST_PER_SKIPPED_TREE;",
	"    if (state == ERROR_STATE) {\n      ts_parser__recover(self, version, lookahead);\n      return true;\n    }\n",
}

func TestRecovery_SourceCarriesTheRestoredHunks(t *testing.T) {
	src, err := os.ReadFile("src/parser.c")
	if err != nil {
		t.Fatal(err)
	}
	for _, hunk := range restoredRecovery {
		if !strings.Contains(string(src), hunk) {
			t.Errorf("src/parser.c no longer carries the restored recovery hunk:\n%s", hunk)
		}
	}
}

// testdata/recovery.cs is an unbalanced method body cut from a real C# file. Stock
// adc13ff recovery turns the whole file into one ERROR node; the restored recovery
// keeps a compilation_unit. The case is fragile by nature (renaming its identifiers
// flips the outcome), so it pins which recovery is LINKED, not that one is better:
// Go's build cache does not track src/parser.c, and a build that reused an object
// compiled from the stock parser fails here. After a C# grammar bump that changes
// this outcome, cut a new fragment the same way rather than loosening the assertion.
func TestRecovery_LinkedParserUsesTheRestoredRecovery(t *testing.T) {
	src, err := os.ReadFile("testdata/recovery.cs")
	if err != nil {
		t.Fatal(err)
	}
	p := NewParser()
	defer p.Close()
	if err := p.SetLanguage(NewLanguage(csharp.Language())); err != nil {
		t.Fatal(err)
	}
	tree := p.Parse(src, nil)
	defer tree.Close()
	if got := tree.RootNode().Kind(); got != "compilation_unit" {
		t.Errorf("root = %s, want compilation_unit: the linked parser recovers like stock "+
			"upstream. Rebuild with -a if src/parser.c changed, else re-apply the hunks", got)
	}
}
