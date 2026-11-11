package goextractor

import (
	"go/ast"
	"strings"
)

// typeUses returns the module's types code names outside a composite literal: in a
// function's parameters and results (sig, when there is one) and, within body, in
// a function literal's signature, a `var x T`, make and new, and a type assertion.
// Each is a use: a struct reached only
// as a parameter type, a decode target or the element of a slice built with make
// is otherwise left with no incoming edge and reported as dead code.
//
// Resolved as a struct field's type is: a predeclared name, a type parameter and a
// name the package does not declare (a type local to the function) have nothing to
// point at.
func typeUses(sig *ast.FuncType, body ast.Node, ctx resolveCtx, typeParams map[string]bool) []string {
	var out []string
	seen := map[string]bool{}
	add := func(expr ast.Expr) {
		for _, typeStr := range namedTypesIn(expr, nil) {
			if !strings.Contains(typeStr, ".") && (!ctx.pkgScope[typeStr] || typeParams[typeStr]) {
				continue
			}
			target := resolveTypeName(typeStr, ctx)
			if !seen[target] && isTrackedTypeTarget(target, ctx) {
				seen[target] = true
				out = append(out, target)
			}
		}
	}
	if sig != nil {
		add(sig)
	}
	if body == nil {
		return out
	}
	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncLit:
			add(x.Type)
		case *ast.ValueSpec:
			if x.Type != nil {
				add(x.Type)
			}
		case *ast.TypeAssertExpr:
			if x.Type != nil { // nil in a type switch's x.(type)
				add(x.Type)
			}
		case *ast.CallExpr:
			if id, ok := x.Fun.(*ast.Ident); ok && (id.Name == "make" || id.Name == "new") && len(x.Args) > 0 {
				add(x.Args[0])
			}
		}
		return true
	})
	return out
}
