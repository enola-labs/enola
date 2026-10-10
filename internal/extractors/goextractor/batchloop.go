package goextractor

import (
	"go/ast"
	"go/token"
)

// Batch loops: a loop that takes the input a batch at a time.
//
//	for left > 0 {                                   // one round per chunk of IDs
//	    rows, err := db.In("id", ids[:limit]).Rows() // ONE query for the chunk
//	    for rows.Next() { … }                        // drained here
//	    left -= limit
//	}
//
//	for {                                            // one round per page
//	    page, resp, err := client.List(ctx, opts)
//	    for _, item := range page { … }
//	    if resp.NextPage == 0 { break }
//	}
//
// Read as ordinary nesting this is a quadratic loop with a query in it, and the
// advice attached to that finding is to batch the query. It is the batching. Each
// row is visited once, and the number of round trips is the row count divided by
// the batch size.
//
// Two things follow for a batch loop. The loop that drains what a round fetched
// adds no factor of n: it is the hierarchical rule, with the batch as the element.
// And a call made at the batch loop's own level, once per round, is not an N+1
// candidate. A call inside the drain loop still runs once per row and still is.
//
// The shape is syntactic. The outer loop has to be a condition loop (`for cond {}`,
// `for {}`) or a stepped one (`i += size`), and its body has to BOTH bind a local
// from a call or a slice AND loop over that local. A condition that advances a
// cursor (`for rows.Next()`) is not one: that loop takes an element a round, and
// what its body fetches it fetches per row. A `for {}` that only walks a
// chain (`for { id = parent(id) }`) has no drain loop and is not one: its calls stay
// candidates, a query per level.

// goBatchLoop reports whether x is a batch loop and, when it is, the locals each
// round binds to what it fetched or sliced off.
//
// walk is set instead of ok for a loop of the same shape whose condition advances
// a cursor (`for rows.Next()`). That loop takes an element a round: its fetch is
// a fetch per row and stays an N+1 candidate. What the fetch returned still
// belongs to the row, so the loop that drains it is as hierarchical as it would
// be under a batch loop, and pageVars is returned for that.
func goBatchLoop(x *ast.ForStmt) (pageVars []string, ok, walk bool) {
	if x.Body == nil {
		return nil, false, false
	}
	condition := x.Init == nil && x.Post == nil
	stepped := false
	if as, isAssign := x.Post.(*ast.AssignStmt); isAssign && as.Tok == token.ADD_ASSIGN {
		stepped = true
	}
	if !condition && !stepped {
		return nil, false, false
	}
	walk = condition && goTakesElement(x.Cond)
	bound := map[string]bool{}
	for _, st := range x.Body.List {
		as, isAssign := st.(*ast.AssignStmt)
		if !isAssign || len(as.Rhs) != 1 {
			continue
		}
		switch as.Rhs[0].(type) {
		case *ast.CallExpr, *ast.SliceExpr:
		default:
			continue
		}
		for _, lhs := range as.Lhs {
			if id, isIdent := lhs.(*ast.Ident); isIdent && id.Name != "_" && id.Name != "err" {
				bound[id.Name] = true
			}
		}
	}
	if len(bound) == 0 {
		return nil, false, false
	}
	drained := false
	for _, st := range x.Body.List {
		var source ast.Expr
		switch inner := st.(type) {
		case *ast.RangeStmt:
			source = inner.X
		case *ast.ForStmt:
			source = inner.Cond // `for rows.Next()`
		}
		if name, isRooted := drainRoot(source); isRooted && bound[name] {
			drained = true
			break
		}
	}
	if !drained {
		return nil, false, false
	}
	for name := range bound {
		pageVars = append(pageVars, name)
	}
	return pageVars, !walk, walk
}

// goElementTakers are the methods a cursor is advanced by, one element a call.
var goElementTakers = map[string]bool{"Next": true, "Scan": true, "HasNext": true, "More": true}

// goTakesElement reports whether a loop condition advances a cursor: `rows.Next()`,
// `scanner.Scan()`, `dec.More()`, alone or negated.
func goTakesElement(cond ast.Expr) bool {
	for {
		switch x := cond.(type) {
		case *ast.ParenExpr:
			cond = x.X
			continue
		case *ast.UnaryExpr:
			cond = x.X
			continue
		case *ast.CallExpr:
			sel, ok := x.Fun.(*ast.SelectorExpr)
			return ok && goElementTakers[sel.Sel.Name]
		}
		return false
	}
}

// drainRoot returns the variable a drain loop's source is reached through:
// `page`, `page.Items`, `rows.Next()`, `resp.Data[1:]`.
func drainRoot(e ast.Expr) (string, bool) {
	for e != nil {
		switch x := e.(type) {
		case *ast.CallExpr:
			e = x.Fun
		case *ast.SliceExpr:
			e = x.X
		default:
			return rootIdent(e)
		}
	}
	return "", false
}

// drainsBatch reports whether a for loop's condition reads a local an enclosing
// batch loop, or cursor walk, bound: `for rows.Next()` under the round that produced `rows`.
func drainsBatch(x *ast.ForStmt, scopes []loopScope) bool {
	name, ok := drainRoot(x.Cond)
	if !ok {
		return false
	}
	for _, s := range scopes {
		if !s.fetches {
			continue
		}
		for _, v := range s.vars {
			if v == name {
				return true
			}
		}
	}
	return false
}
