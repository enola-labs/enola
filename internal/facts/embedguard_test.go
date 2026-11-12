package facts

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// No struct in the module may embed Fact. Fact has a MarshalJSON (it writes the identity
// props back into "props"), and an embedded type's method is promoted: the outer struct
// would encode as the fact alone, silently dropping every field declared beside it. That
// is how query_facts lost related_facts the first time. Embed FactJSON instead.
func TestNoStructEmbedsFact(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); path != root && (strings.HasPrefix(name, ".") || name == "testdata" || name == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil // not this test's concern
		}
		local := file.Name.Name == "facts" && strings.Contains(filepath.ToSlash(path), "/internal/facts/")
		ast.Inspect(file, func(n ast.Node) bool {
			st, ok := n.(*ast.StructType)
			if !ok {
				return true
			}
			for _, field := range st.Fields.List {
				if len(field.Names) != 0 {
					continue
				}
				typ := field.Type
				if star, ok := typ.(*ast.StarExpr); ok {
					typ = star.X
				}
				switch x := typ.(type) {
				case *ast.SelectorExpr:
					if id, ok := x.X.(*ast.Ident); ok && id.Name == "facts" && x.Sel.Name == "Fact" {
						t.Errorf("%s: a struct embeds facts.Fact; embed facts.FactJSON (see its doc)", fset.Position(field.Pos()))
					}
				case *ast.Ident:
					if local && x.Name == "Fact" {
						t.Errorf("%s: a struct embeds Fact; embed FactJSON (see its doc)", fset.Position(field.Pos()))
					}
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test")
		}
		dir = parent
	}
}
