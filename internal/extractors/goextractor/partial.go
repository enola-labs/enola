package goextractor

import "go/ast"

// keepCleanDecls is what can still be read of a file the parser failed on: its
// declarations that parsed whole. A syntax error was the loss of the whole file, so a
// single declaration in a form the parser does not know (a construct of a newer Go)
// took every other function of the file out of the graph. The parser recovers at the
// next declaration and marks what it could not read with Bad nodes; a declaration
// holding one is left out, rather than read with a hole in it. nil when nothing is
// left: no package clause, or no declaration parsed whole.
func keepCleanDecls(f *ast.File) *ast.File {
	if f == nil || f.Name == nil {
		return nil
	}
	kept := f.Decls[:0:0]
	for _, d := range f.Decls {
		if !hasBadNode(d) {
			kept = append(kept, d)
		}
	}
	if len(kept) == 0 {
		return nil
	}
	f.Decls = kept
	return f
}

// hasBadNode reports a node the parser could not read anywhere under n.
func hasBadNode(n ast.Node) bool {
	bad := false
	ast.Inspect(n, func(n ast.Node) bool {
		switch n.(type) {
		case *ast.BadDecl, *ast.BadExpr, *ast.BadStmt:
			bad = true
		}
		return !bad
	})
	return bad
}
