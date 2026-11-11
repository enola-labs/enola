package goextractor

import (
	"go/ast"
	"go/token"
	"slices"
	"strings"
)

// A query often names its table through a constant rather than in the literal:
//
//	const ordersTable = "orders"
//	q := fmt.Sprintf("SELECT id FROM %s WHERE user_id = ?", ordersTable)
//	q := "DELETE FROM " + ordersTable + " WHERE id = ?"
//
// The literal alone reads "FROM %s" or ends at "FROM ", and no table is found. The
// helpers here rebuild the query text the code assembles, from string literals and the
// package's string constants (buildStringConsts, constString), so the statement
// patterns can read it. A part that is not
// a constant, a placeholder list built at run time, becomes "?", which no pattern reads
// as a table.

// partString is constString for one operand of an assembled query: anything it cannot
// resolve is "?".
func partString(expr ast.Expr, consts map[string]string) string {
	if v, ok := constString(expr, consts); ok {
		return v
	}
	if b, ok := expr.(*ast.BinaryExpr); ok && b.Op == token.ADD {
		return partString(b.X, consts) + partString(b.Y, consts)
	}
	return "?"
}

// namesConst reports whether expr mentions one of the package's string constants.
func namesConst(expr ast.Expr, consts map[string]string) bool {
	found := false
	ast.Inspect(expr, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok {
			if _, ok := consts[id.Name]; ok {
				found = true
			}
		}
		return !found
	})
	return found
}

// concatQuery is the text of a string sum that names a constant, such as
// "DELETE FROM " + ordersTable. A sum of literals alone is left to the literals.
func concatQuery(b *ast.BinaryExpr, consts map[string]string) (string, bool) {
	if b.Op != token.ADD || !namesConst(b, consts) {
		return "", false
	}
	return partString(b, consts), true
}

// sprintfQuery is the text fmt.Sprintf builds, when its format is known and it is
// handed at least one constant: each verb takes its argument's value, or "?".
func sprintfQuery(call *ast.CallExpr, fmtName string, consts map[string]string) (string, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Sprintf" || len(call.Args) < 2 {
		return "", false
	}
	if x, ok := sel.X.(*ast.Ident); !ok || fmtName == "" || x.Name != fmtName {
		return "", false
	}
	format, ok := constString(call.Args[0], consts)
	if !ok {
		return "", false
	}
	args := call.Args[1:]
	if !slices.ContainsFunc(args, func(a ast.Expr) bool { return namesConst(a, consts) }) {
		return "", false
	}
	var sb strings.Builder
	for i := 0; i < len(format); i++ {
		c := format[i]
		if c != '%' {
			sb.WriteByte(c)
			continue
		}
		if i+1 < len(format) && format[i+1] == '%' {
			sb.WriteByte('%')
			i++
			continue
		}
		// Skip flags, width and precision to the verb.
		j := i + 1
		for j < len(format) && strings.IndexByte("+-# 0123456789.[]*", format[j]) >= 0 {
			j++
		}
		if len(args) > 0 {
			sb.WriteString(partString(args[0], consts))
			args = args[1:]
		} else {
			sb.WriteString("?")
		}
		i = j
	}
	return sb.String(), true
}

// importName is the name a file uses for an imported package path, "" when the file
// does not import it.
func importName(f *ast.File, path string) string {
	for _, imp := range f.Imports {
		if strings.Trim(imp.Path.Value, `"`) != path {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		return path[strings.LastIndex(path, "/")+1:]
	}
	return ""
}
