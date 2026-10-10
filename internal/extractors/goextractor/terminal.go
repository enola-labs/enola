package goextractor

import (
	"go/ast"
	"go/token"
)

// A call the loop does not survive.
//
//	for _, id := range ids {
//	    if !valid(id) {
//	        http.Error(w, "bad id", 400)   // at most once: the function returns
//	        return
//	    }
//	}
//
// The call sits in a loop and runs at most once each time the loop is entered,
// because the block it is in ends by leaving the loop. Counted as a call per
// iteration, it was reported as an N+1 on the error path of every validating loop.
//
// A block leaves the loop when its last statement is a return, a panic, or a
// break that is the loop's own: not one inside a switch or a select, which it
// would leave instead, and not a labelled one, whose target is not read here. Only
// the innermost loop is considered. A return leaves the outer loops as well, and
// that is not claimed.
//
// And nothing in the block may go round again first. A body that ends in a return
// is the "first match" loop, and its opening call runs on every element that does
// not match:
//
//	for _, p := range principals {
//	    key, err := search(ctx, p)   // once per principal
//	    if err != nil {
//	        continue
//	    }
//	    return key
//	}

// posSpan is a half-open range of source positions.
type posSpan struct{ pos, end token.Pos }

// terminalBlocks returns the statement runs within a loop body that end by
// leaving the loop. Nested loops and function literals are not entered: a nested
// loop has blocks of its own, and a literal runs when it is called.
func terminalBlocks(body *ast.BlockStmt) []posSpan {
	if body == nil {
		return nil
	}
	var out []posSpan
	var walk func(list []ast.Stmt, inSwitch bool)
	walk = func(list []ast.Stmt, inSwitch bool) {
		if n := len(list); n > 0 && leavesLoop(list[n-1], inSwitch) && !continuesLoop(list) {
			out = append(out, posSpan{list[0].Pos(), list[n-1].End()})
		}
		for _, st := range list {
			for st != nil {
				switch s := st.(type) {
				case *ast.LabeledStmt:
					st = s.Stmt
					continue
				case *ast.BlockStmt:
					walk(s.List, inSwitch)
				case *ast.IfStmt:
					walk(s.Body.List, inSwitch)
					st = s.Else // a block, another if, or nil
					continue
				case *ast.SwitchStmt:
					walkClauses(s.Body, walk)
				case *ast.TypeSwitchStmt:
					walkClauses(s.Body, walk)
				case *ast.SelectStmt:
					walkClauses(s.Body, walk)
				}
				break
			}
		}
	}
	walk(body.List, false)
	return out
}

func walkClauses(body *ast.BlockStmt, walk func([]ast.Stmt, bool)) {
	if body == nil {
		return
	}
	for _, c := range body.List {
		switch cl := c.(type) {
		case *ast.CaseClause:
			walk(cl.Body, true)
		case *ast.CommClause:
			walk(cl.Body, true)
		}
	}
}

// leavesLoop reports whether a statement, as the last of its block, takes control
// out of the innermost loop.
func leavesLoop(st ast.Stmt, inSwitch bool) bool {
	switch s := st.(type) {
	case *ast.ReturnStmt:
		return true
	case *ast.BranchStmt:
		return s.Tok == token.BREAK && s.Label == nil && !inSwitch
	case *ast.ExprStmt:
		if call, ok := s.X.(*ast.CallExpr); ok {
			if id, ok := call.Fun.(*ast.Ident); ok {
				return id.Name == "panic"
			}
		}
	}
	return false
}

// continuesLoop reports whether a statement list can start the innermost loop's
// next iteration: it holds a continue that is not a nested loop's own. A labelled
// continue or a goto is taken to, since its target is not read here.
func continuesLoop(list []ast.Stmt) bool {
	found := false
	for _, st := range list {
		ast.Inspect(st, func(n ast.Node) bool {
			if found {
				return false
			}
			switch x := n.(type) {
			case *ast.FuncLit:
				return false
			case *ast.ForStmt:
				found = found || hasLabelledJump(x.Body)
				return false
			case *ast.RangeStmt:
				found = found || hasLabelledJump(x.Body)
				return false
			case *ast.BranchStmt:
				if x.Tok == token.CONTINUE || x.Tok == token.GOTO {
					found = true
				}
			}
			return true
		})
	}
	return found
}

// hasLabelledJump reports whether a nested loop's body can jump out to an
// enclosing loop's next iteration.
func hasLabelledJump(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if b, ok := n.(*ast.BranchStmt); ok && b.Label != nil && (b.Tok == token.CONTINUE || b.Tok == token.GOTO) {
			found = true
		}
		_, lit := n.(*ast.FuncLit)
		return !found && !lit
	})
	return found
}
