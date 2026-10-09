package rustextractor

import (
	sitter "github.com/tree-sitter/go-tree-sitter"
)

// The hierarchical-loop rule, as the Go, TypeScript, PHP, Python and Kotlin
// extractors apply it.
//
//	for trace in traces {
//	    for frame in trace.frames() { … }
//	}
//
// The inner loop walks what belongs to the element the outer loop is on, so the
// nest visits every frame once and adds no factor of n to scaling_loop_depth. A
// call inside it is still inside the outer, scaling loop, so it stays an N+1
// candidate.
//
// The collection has to be REACHED THROUGH the element (a field, a method, an
// index by it). Mentioning the outer variable proves nothing: `&xs[i + 1..]` is
// the all-pairs loop.

// rustLoopScope is one enclosing loop's contribution to that proof.
type rustLoopScope struct {
	// vars are the names the loop's pattern binds and the locals bound from them.
	vars map[string]bool
	// amortizes is false under a loop with a constant trip count, where there is no
	// factor of n for a hierarchical inner loop to cancel.
	amortizes bool
}

func (s *rustLoopScope) add(name string) {
	if name == "" || name == "_" {
		return
	}
	if s.vars == nil {
		s.vars = make(map[string]bool)
	}
	s.vars[name] = true
}

func rustScopesHave(scopes []rustLoopScope, name string) bool {
	for i := range scopes {
		if scopes[i].amortizes && scopes[i].vars[name] {
			return true
		}
	}
	return false
}

// rustViewMethods are the methods that hand back a view of their receiver whatever
// they are passed. Any other method of the element counts only when it takes no
// arguments, the shape of an accessor: `task.get_flat_relatives(upstream=False)`
// returns everything reachable from the task, not what belongs to it, and a nest
// over it is quadratic.
var rustViewMethods = map[string]bool{
	"filter": true, "map": true, "filter_map": true, "drain": true, "get": true,
}

// rustReachedThroughElement reports whether expr is reached through the element of
// an enclosing loop. See the rule at the top of this file.
func rustReachedThroughElement(n *sitter.Node, src []byte, scopes []rustLoopScope) bool {
	if n == nil || len(scopes) == 0 {
		return false
	}
	switch kindOf(n) {
	case "identifier":
		return rustScopesHave(scopes, nodeText(n, src))
	case "parenthesized_expression", "try_expression", "await_expression", "unary_expression":
		if n.NamedChildCount() > 0 {
			return rustReachedThroughElement(n.NamedChild(0), src, scopes)
		}
	case "reference_expression", "field_expression":
		return rustReachedThroughElement(n.ChildByFieldName("value"), src, scopes)
	case "index_expression":
		if n.NamedChildCount() == 0 {
			return false
		}
		if rustReachedThroughElement(n.NamedChild(0), src, scopes) {
			return true
		}
		// `groups[key]`: selected by the loop's own variable and nothing else.
		// `rows[i + 1]` is a neighbour and `xs[i..]` a slice, not the element.
		if n.NamedChildCount() == 2 && kindOf(n.NamedChild(1)) == "identifier" {
			return rustScopesHave(scopes, nodeText(n.NamedChild(1), src))
		}
	case "call_expression":
		// `trace.frames()`, `node.children.iter()`: a method of the element. A free
		// function taking the element is not, and neither is another receiver's
		// method that takes it as an argument.
		fn := n.ChildByFieldName("function")
		if fn == nil || kindOf(fn) != "field_expression" ||
			!rustReachedThroughElement(fn.ChildByFieldName("value"), src, scopes) {
			return false
		}
		args := n.ChildByFieldName("arguments")
		field := fn.ChildByFieldName("field")
		return args == nil || args.NamedChildCount() == 0 || (field != nil && rustViewMethods[nodeText(field, src)])
	}
	return false
}

// rustBindPattern adds every name a pattern binds: `(key, value)`, `&item`,
// `Some(frame)`.
func rustBindPattern(n *sitter.Node, src []byte, sc *rustLoopScope) {
	if n == nil {
		return
	}
	if kindOf(n) == "identifier" {
		// A capitalized identifier in a pattern is a variant or constant, not a binding.
		if name := nodeText(n, src); !isCapitalized(name) {
			sc.add(name)
		}
		return
	}
	for i := uint(0); i < uint(n.NamedChildCount()); i++ {
		rustBindPattern(n.NamedChild(i), src, sc)
	}
}

// rustLoopSource returns the pattern a loop binds and the expression it draws
// from: `for <pattern> in <value>`, or `while let <pattern> = <value>`.
func rustLoopSource(node *sitter.Node) (pattern, value *sitter.Node) {
	switch kindOf(node) {
	case "for_expression":
		return node.ChildByFieldName("pattern"), node.ChildByFieldName("value")
	case "while_expression":
		if c := node.ChildByFieldName("condition"); c != nil && kindOf(c) == "let_condition" {
			return c.ChildByFieldName("pattern"), c.ChildByFieldName("value")
		}
	}
	return nil, nil
}
