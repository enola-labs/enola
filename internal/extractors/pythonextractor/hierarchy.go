package pythonextractor

import (
	sitter "github.com/tree-sitter/go-tree-sitter"
)

// The hierarchical-loop rule, as the Go, TypeScript and PHP extractors apply it.
//
//	for dag in dags:
//	    for task in dag.tasks:
//	        ...
//
// The inner loop walks what belongs to the element the outer loop is on, so the
// nest visits every task once and adds no factor of n to scaling_loop_depth. It
// still repeats, so a call inside it stays an N+1 candidate.
//
// The collection has to be REACHED THROUGH the element (an attribute, a method,
// a subscript, a lookup keyed by it). Mentioning the outer variable proves
// nothing: `xs[i + 1:]` and `range(i)` are the all-pairs loops.

// pyLoopScope is one enclosing loop's contribution to that proof.
type pyLoopScope struct {
	// vars are the loop's targets and the locals assigned from them.
	vars map[string]bool
	// amortizes is false under a loop with a constant trip count, where there is no
	// factor of n for a hierarchical inner loop to cancel.
	amortizes bool
}

func (s *pyLoopScope) add(name string) {
	if name == "" || name == "_" {
		return
	}
	if s.vars == nil {
		s.vars = make(map[string]bool)
	}
	s.vars[name] = true
}

func pyScopesHave(scopes []pyLoopScope, name string) bool {
	for i := range scopes {
		if scopes[i].amortizes && scopes[i].vars[name] {
			return true
		}
	}
	return false
}

// pyWrapsElement lists the builtins that return a view of their first argument:
// iterating the result is iterating the argument.
var pyWrapsElement = map[string]bool{
	"enumerate": true, "sorted": true, "reversed": true, "list": true, "tuple": true,
	"set": true, "iter": true,
}

// pyViewMethods are the methods that hand back a view of their receiver whatever
// they are passed. Any other method of the element counts only when it takes no
// arguments, the shape of an accessor: `task.get_flat_relatives(upstream=False)`
// returns everything reachable from the task, not what belongs to it, and a nest
// over it is quadratic.
var pyViewMethods = map[string]bool{
	"items": true, "values": true, "keys": true, "filter": true, "exclude": true,
	"order_by": true, "all": true,
}

// pyReachedThroughElement reports whether expr is reached through the element of
// an enclosing loop. See the rule at the top of this file.
func pyReachedThroughElement(n *sitter.Node, src []byte, scopes []pyLoopScope) bool {
	if n == nil || len(scopes) == 0 {
		return false
	}
	switch kindOf(n) {
	case "identifier":
		return pyScopesHave(scopes, pyText(n, src))
	case "parenthesized_expression", "await":
		if n.NamedChildCount() > 0 {
			return pyReachedThroughElement(n.NamedChild(0), src, scopes)
		}
	case "attribute":
		return pyReachedThroughElement(n.ChildByFieldName("object"), src, scopes)
	case "subscript":
		if pyReachedThroughElement(n.ChildByFieldName("value"), src, scopes) {
			return true
		}
		// `groups[key]`: selected by the loop's own variable and nothing else.
		// `rows[i + 1]` is a neighbour and `xs[i:]` a slice, not the element.
		if sub := n.ChildByFieldName("subscript"); sub != nil && kindOf(sub) == "identifier" {
			return pyScopesHave(scopes, pyText(sub, src))
		}
	case "boolean_operator":
		// `dag.tasks or []`: the fallback is empty or constant.
		if op := n.ChildByFieldName("operator"); op != nil && pyText(op, src) == "or" {
			return pyReachedThroughElement(n.ChildByFieldName("left"), src, scopes)
		}
	case "call":
		fn := n.ChildByFieldName("function")
		if fn == nil {
			return false
		}
		var first *sitter.Node
		if args := n.ChildByFieldName("arguments"); args != nil && args.NamedChildCount() > 0 {
			first = args.NamedChild(0)
		}
		switch kindOf(fn) {
		case "identifier":
			if pyWrapsElement[pyText(fn, src)] {
				return pyReachedThroughElement(first, src, scopes)
			}
		case "attribute":
			// `dag.tasks.values()`, `node.children()`: a view or an accessor of the element.
			if pyReachedThroughElement(fn.ChildByFieldName("object"), src, scopes) {
				attr := fn.ChildByFieldName("attribute")
				return first == nil || (attr != nil && pyViewMethods[pyText(attr, src)])
			}
			// `by_id.get(task.id)`: a keyed lookup, the dict spelling of `by_id[task.id]`.
			if attr := fn.ChildByFieldName("attribute"); attr != nil && pyText(attr, src) == "get" {
				return pyReachedThroughElement(first, src, scopes)
			}
		}
	}
	return false
}

// pyBindTargetNames adds every name a loop or assignment target binds:
// `for key, (a, b) in …` binds three.
func pyBindTargetNames(n *sitter.Node, src []byte, sc *pyLoopScope) {
	if n == nil {
		return
	}
	switch kindOf(n) {
	case "identifier":
		sc.add(pyText(n, src))
		return
	case "attribute", "subscript", "call":
		return // `self.x = …`, `d[k] = …`: not a local
	}
	for i := uint(0); i < uint(n.NamedChildCount()); i++ {
		pyBindTargetNames(n.NamedChild(i), src, sc)
	}
}
