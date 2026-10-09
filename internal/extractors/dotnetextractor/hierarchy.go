package dotnetextractor

import (
	sitter "github.com/tree-sitter/go-tree-sitter"
)

// The hierarchical-loop rule, as the other extractors apply it.
//
//	foreach (var path in document.Paths) {
//	    foreach (var operation in path.Value.Operations) { … }
//	}
//
// The inner loop walks what belongs to the element the outer loop is on, so the
// nest visits every operation once and adds no factor of n to scaling_loop_depth.
// It still repeats, so a call inside it stays an N+1 candidate: for the loop's
// class that is exactly what loopInfinite already means.
//
// The collection has to be REACHED THROUGH the element (a member, a method, an
// index, a lookup keyed by it). Mentioning the outer variable proves nothing:
// `xs.Skip(i + 1)` is the all-pairs loop.
//
// Statement loops only. A LINQ lambda's parameter is not tracked, so a nest
// written as `xs.Select(x => x.Kids.Select(…))` keeps counting.

// csLoopScope is one enclosing loop's contribution to that proof.
type csLoopScope struct {
	// vars are the loop variable and the locals initialised from it.
	vars map[string]bool
	// amortizes is false under a loop with a constant trip count, where there is no
	// factor of n for a hierarchical inner loop to cancel.
	amortizes bool
}

func (s *csLoopScope) add(name string) {
	if name == "" || name == "_" {
		return
	}
	if s.vars == nil {
		s.vars = make(map[string]bool)
	}
	s.vars[name] = true
}

func csScopesHave(scopes []csLoopScope, name string) bool {
	for i := range scopes {
		if scopes[i].amortizes && scopes[i].vars[name] {
			return true
		}
	}
	return false
}

// csViewMethods are the methods that hand back a view of their receiver whatever
// they are passed. Any other method of the element counts only when it takes no
// arguments, the shape of an accessor: `task.get_flat_relatives(upstream=False)`
// returns everything reachable from the task, not what belongs to it, and a nest
// over it is quadratic.
var csViewMethods = map[string]bool{
	"Where": true, "Select": true, "OrderBy": true, "OrderByDescending": true, "OfType": true,
}

// csReachedThroughElement reports whether expr is reached through the element of
// an enclosing loop. See the rule at the top of this file.
func csReachedThroughElement(n *sitter.Node, src []byte, scopes []csLoopScope) bool {
	if n == nil || len(scopes) == 0 {
		return false
	}
	switch kindOf(n) {
	case "identifier":
		return csScopesHave(scopes, nodeText(n, src))
	case "parenthesized_expression", "postfix_unary_expression", "await_expression":
		if n.NamedChildCount() > 0 {
			return csReachedThroughElement(n.NamedChild(0), src, scopes)
		}
	case "member_access_expression":
		return csReachedThroughElement(n.ChildByFieldName("expression"), src, scopes)
	case "conditional_access_expression":
		return csReachedThroughElement(n.ChildByFieldName("condition"), src, scopes)
	case "element_access_expression":
		if csReachedThroughElement(n.ChildByFieldName("expression"), src, scopes) {
			return true
		}
		// `groups[key]`: selected by the loop's own variable and nothing else.
		if sub := n.ChildByFieldName("subscript"); sub != nil && sub.NamedChildCount() == 1 {
			arg := sub.NamedChild(0)
			if kindOf(arg) == "argument" && arg.NamedChildCount() == 1 {
				arg = arg.NamedChild(0)
			}
			if kindOf(arg) == "identifier" {
				return csScopesHave(scopes, nodeText(arg, src))
			}
		}
	case "binary_expression":
		// `path.Operations ?? []`: the fallback is empty or constant.
		if op := n.ChildByFieldName("operator"); op != nil && nodeText(op, src) == "??" {
			return csReachedThroughElement(n.ChildByFieldName("left"), src, scopes)
		}
	case "invocation_expression":
		// `path.GetOperations()`, `node.Children.Where(…)`: a method of the element.
		fn := n.ChildByFieldName("function")
		if fn == nil || kindOf(fn) != "member_access_expression" ||
			!csReachedThroughElement(fn.ChildByFieldName("expression"), src, scopes) {
			return false
		}
		args := n.ChildByFieldName("arguments")
		name := fn.ChildByFieldName("name")
		return args == nil || args.NamedChildCount() == 0 || (name != nil && csViewMethods[nodeText(name, src)])
	}
	return false
}

// csBindIdentifiers adds every identifier under a foreach's left-hand side: a
// plain variable, or the names a `var (key, value)` deconstruction binds.
func csBindIdentifiers(n *sitter.Node, src []byte, sc *csLoopScope) {
	if n == nil {
		return
	}
	if kindOf(n) == "identifier" {
		sc.add(nodeText(n, src))
		return
	}
	for i := uint(0); i < uint(n.NamedChildCount()); i++ {
		csBindIdentifiers(n.NamedChild(i), src, sc)
	}
}
