package pythonextractor

import (
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"
)

// Batch loops: a loop that takes the input a batch at a time.
//
//	for chunk in chunked(ids, 500):                   # one round per chunk
//	    rows = session.execute(q.where(col.in_(chunk)))  # ONE query for the chunk
//	    for row in rows: ...                          # drained here
//
//	for i in range(0, total, batch_size):             # the same, stepped
//	    batch = ids[i : i + batch_size]
//	    session.execute(delete(Log).where(Log.id.in_(batch)))
//
//	while True:                                       # one round per page
//	    page = client.list(token)
//	    for item in page.items: ...
//
// Read as ordinary nesting this is a quadratic loop with a query in it, and the
// advice attached is to batch the query. It is the batching.
//
// Two things follow. The loop that drains what a round fetched adds no factor of
// n: it is the hierarchical rule, with the batch as the element. And a call made
// at the batch loop's own level, once per round, is not an N+1 candidate. A call
// inside the drain loop still runs once per row and still is.
//
// A `for` is a batch loop by its form: a three-argument range, or a call whose
// name says it chunks. A `while` has to prove it, by binding a local from a call
// and then looping over that local; one that only walks a chain is not one.

// pyChunkingName reports whether a callable's name says it yields batches.
func pyChunkingName(name string) bool {
	n := strings.ToLower(name)
	return strings.Contains(n, "chunk") || strings.Contains(n, "batch")
}

// pyBatchLoop reports whether a for/while statement is a batch loop and, when it
// is, the locals each round binds to what it fetched or sliced off.
func pyBatchLoop(node *sitter.Node, src []byte) (pageVars []string, ok bool) {
	body := node.ChildByFieldName("body")
	if body == nil {
		return nil, false
	}
	bound := map[string]bool{}
	pyRoundBindings(body, src, bound)

	switch kindOf(node) {
	case "for_statement":
		right := node.ChildByFieldName("right")
		if right == nil || kindOf(right) != "call" {
			return nil, false
		}
		fn, args := right.ChildByFieldName("function"), right.ChildByFieldName("arguments")
		if fn == nil {
			return nil, false
		}
		name := pyText(fn, src)
		if kindOf(fn) == "attribute" {
			if attr := fn.ChildByFieldName("attribute"); attr != nil {
				name = pyText(attr, src)
			}
		}
		stepped := name == "range" && args != nil && args.NamedChildCount() == 3
		if !stepped && !pyChunkingName(name) {
			return nil, false
		}
	case "while_statement":
		if !pyDrainsABinding(body, src, bound) {
			return nil, false
		}
	default:
		return nil, false
	}
	for name := range bound {
		pageVars = append(pageVars, name)
	}
	return pageVars, true
}

// pyRoundBindings collects the locals a loop body assigns from a call or a
// subscript, looking through with/try/if blocks and not into nested loops or
// definitions.
func pyRoundBindings(n *sitter.Node, src []byte, out map[string]bool) {
	for i := uint(0); i < uint(n.NamedChildCount()); i++ {
		c := n.NamedChild(i)
		switch kindOf(c) {
		case "for_statement", "while_statement", "function_definition", "class_definition", "decorated_definition":
			continue
		case "expression_statement":
			if c.NamedChildCount() == 0 || kindOf(c.NamedChild(0)) != "assignment" {
				continue
			}
			as := c.NamedChild(0)
			right := as.ChildByFieldName("right")
			if right != nil && kindOf(right) == "await" && right.NamedChildCount() > 0 {
				right = right.NamedChild(0)
			}
			if right == nil || (kindOf(right) != "call" && kindOf(right) != "subscript") {
				continue
			}
			var sc pyLoopScope
			pyBindTargetNames(as.ChildByFieldName("left"), src, &sc)
			for name := range sc.vars {
				out[name] = true
			}
		default:
			pyRoundBindings(c, src, out)
		}
	}
}

// pyDrainsABinding reports whether a loop body has a for statement over one of the
// locals the round bound.
func pyDrainsABinding(n *sitter.Node, src []byte, bound map[string]bool) bool {
	if len(bound) == 0 {
		return false
	}
	for i := uint(0); i < uint(n.NamedChildCount()); i++ {
		c := n.NamedChild(i)
		switch kindOf(c) {
		case "function_definition", "class_definition", "decorated_definition", "while_statement":
			continue
		case "for_statement":
			if bound[pyRootName(c.ChildByFieldName("right"), src)] {
				return true
			}
		default:
			if pyDrainsABinding(c, src, bound) {
				return true
			}
		}
	}
	return false
}

// pyRootName returns the variable an expression is reached through: `page`,
// `page.items`, `rows.all()`, `resp["data"]`.
func pyRootName(n *sitter.Node, src []byte) string {
	for n != nil {
		switch kindOf(n) {
		case "identifier":
			return pyText(n, src)
		case "attribute":
			n = n.ChildByFieldName("object")
		case "call":
			n = n.ChildByFieldName("function")
		case "subscript":
			n = n.ChildByFieldName("value")
		case "parenthesized_expression", "await":
			if n.NamedChildCount() == 0 {
				return ""
			}
			n = n.NamedChild(0)
		default:
			return ""
		}
	}
	return ""
}
