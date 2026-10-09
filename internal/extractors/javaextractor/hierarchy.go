package javaextractor

import (
	sitter "github.com/tree-sitter/go-tree-sitter"
)

// The hierarchical-loop rule, as the other extractors apply it.
//
//	for (Schema schema : schemas) {
//	    for (Schema part : schema.getAllOf()) { … }
//	}
//
// The inner loop walks what belongs to the element the outer loop is on, so the
// nest visits every part once and adds no factor of n to scaling_loop_depth. It
// still repeats, so a call inside it stays an N+1 candidate: for the loop's class
// that is exactly what javaLoopInfinite already means.
//
// The collection has to be REACHED THROUGH the element (a field, a method, an
// index, a lookup keyed by it). Mentioning the outer variable proves nothing:
// `xs.subList(i + 1, n)` is the all-pairs loop.
//
// Statement loops only. A stream lambda's parameter is not tracked, so a nest
// written as `xs.forEach(x -> x.kids().forEach(…))` keeps counting.

// javaLoopScope is one enclosing loop's contribution to that proof.
type javaLoopScope struct {
	// vars are the loop variable and the locals initialised from it.
	vars map[string]bool
	// amortizes is false under a loop with a constant trip count, where there is no
	// factor of n for a hierarchical inner loop to cancel.
	amortizes bool
}

func (s *javaLoopScope) add(name string) {
	if name == "" {
		return
	}
	if s.vars == nil {
		s.vars = make(map[string]bool)
	}
	s.vars[name] = true
}

func javaScopesHave(scopes []javaLoopScope, name string) bool {
	for i := range scopes {
		if scopes[i].amortizes && scopes[i].vars[name] {
			return true
		}
	}
	return false
}

// javaKeyedLookups are the Map reads that select by key.
var javaKeyedLookups = map[string]bool{"get": true, "getOrDefault": true}

// javaViewMethods are the methods that hand back a view of their receiver whatever
// they are passed. Any other method of the element counts only when it takes no
// arguments, the shape of an accessor: `task.get_flat_relatives(upstream=False)`
// returns everything reachable from the task, not what belongs to it, and a nest
// over it is quadratic.
var javaViewMethods = map[string]bool{"filter": true, "map": true, "sorted": true}

// javaReachedThroughElement reports whether expr is reached through the element of
// an enclosing loop. See the rule at the top of this file.
func javaReachedThroughElement(n *sitter.Node, src []byte, scopes []javaLoopScope) bool {
	if n == nil || len(scopes) == 0 {
		return false
	}
	switch kindOf(n) {
	case "identifier":
		return javaScopesHave(scopes, nodeText(n, src))
	case "parenthesized_expression":
		if n.NamedChildCount() > 0 {
			return javaReachedThroughElement(n.NamedChild(0), src, scopes)
		}
	case "field_access":
		return javaReachedThroughElement(n.ChildByFieldName("object"), src, scopes)
	case "array_access":
		if javaReachedThroughElement(n.ChildByFieldName("array"), src, scopes) {
			return true
		}
		// `rows[i]`: selected by the loop's own variable and nothing else.
		if idx := n.ChildByFieldName("index"); idx != nil && kindOf(idx) == "identifier" {
			return javaScopesHave(scopes, nodeText(idx, src))
		}
	case "method_invocation":
		// `schema.getAllOf()`, `node.children().values()`: a view or an accessor of
		// the element.
		name := n.ChildByFieldName("name")
		args := n.ChildByFieldName("arguments")
		if javaReachedThroughElement(n.ChildByFieldName("object"), src, scopes) {
			return args == nil || args.NamedChildCount() == 0 || (name != nil && javaViewMethods[nodeText(name, src)])
		}
		// `byId.get(schema.getId())`: a keyed lookup.
		if name != nil && javaKeyedLookups[nodeText(name, src)] && args != nil && args.NamedChildCount() > 0 {
			return javaReachedThroughElement(args.NamedChild(0), src, scopes)
		}
	}
	return false
}
