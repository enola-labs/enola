package goextractor

import (
	"go/ast"
	"go/token"

	"github.com/enola-labs/enola/internal/facts"
)

// nestedFuncs returns the function literals a function body binds to a name, `onClose :=
// func() {...}` or `var onClose = func() {...}`, at any depth. An unbound literal is left
// out, since nothing can refer to it by name.
func nestedFuncs(fset *token.FileSet, f *ast.File) []facts.Nested {
	var out []facts.Nested
	add := func(name *ast.Ident, lit *ast.FuncLit) {
		if name.Name == "_" {
			return
		}
		out = append(out, facts.Nested{Name: name.Name, Line: fset.Position(name.Pos()).Line, EndLine: fset.Position(lit.End()).Line})
	}
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch s := n.(type) {
			case *ast.AssignStmt:
				if len(s.Lhs) != len(s.Rhs) {
					break
				}
				for i, rhs := range s.Rhs {
					name, isName := s.Lhs[i].(*ast.Ident)
					if lit, isLit := rhs.(*ast.FuncLit); isName && isLit {
						add(name, lit)
					}
				}
			case *ast.ValueSpec:
				if len(s.Names) != len(s.Values) {
					break
				}
				for i, v := range s.Values {
					if lit, isLit := v.(*ast.FuncLit); isLit {
						add(s.Names[i], lit)
					}
				}
			}
			return true
		})
	}
	return out
}
