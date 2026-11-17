package tsextractor

import (
	"github.com/enola-labs/enola/internal/extractors/tsutil"
	sitter "github.com/enola-labs/enola/internal/treesitter"
)

// typeOnlyImport reports an import or re-export statement the compiler erases:
// `import type …`, `export type … from`, or a named clause whose every specifier
// is marked `type`. A default, namespace or side-effect import, or one value
// among the names, is loaded at run time.
func typeOnlyImport(kinds *tsutil.KindTable, stmt *sitter.Node) bool {
	clause := ""
	switch kindOf(kinds, stmt) {
	case "import_statement":
		clause = "import_clause"
	case "export_statement":
		clause = "export_clause"
	default:
		return false
	}
	for i := range stmt.ChildCount() {
		child := stmt.Child(i)
		switch kindOf(kinds, child) {
		case "type":
			return true
		case clause:
			if clause == "export_clause" {
				return allSpecifiersTyped(kinds, child, "export_specifier")
			}
			// An import clause holds a default import, a namespace import or the
			// named imports; only the last can be all types.
			if child.ChildCount() != 1 {
				return false
			}
			named := child.Child(0)
			return kindOf(kinds, named) == "named_imports" && allSpecifiersTyped(kinds, named, "import_specifier")
		}
	}
	return false
}

func allSpecifiersTyped(kinds *tsutil.KindTable, list *sitter.Node, specifier string) bool {
	n := 0
	for i := range list.ChildCount() {
		spec := list.Child(i)
		if kindOf(kinds, spec) != specifier {
			continue
		}
		n++
		if spec.ChildCount() == 0 || kindOf(kinds, spec.Child(0)) != "type" {
			return false
		}
	}
	return n > 0
}
