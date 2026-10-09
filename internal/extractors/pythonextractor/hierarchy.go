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
	// batch marks a loop that takes its input a batch at a time; see batchloop.go.
	batch bool
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
	return pyElementRoot(n, src, scopes) != ""
}

// pyElementRoot returns the scope variable expr is reached through, or "" when it
// is not reached through one. `dag.tasks.values()` is rooted at `dag`.
func pyElementRoot(n *sitter.Node, src []byte, scopes []pyLoopScope) string {
	if n == nil || len(scopes) == 0 {
		return ""
	}
	switch kindOf(n) {
	case "identifier":
		if name := pyText(n, src); pyScopesHave(scopes, name) {
			return name
		}
	case "parenthesized_expression", "await":
		if n.NamedChildCount() > 0 {
			return pyElementRoot(n.NamedChild(0), src, scopes)
		}
	case "attribute":
		return pyElementRoot(n.ChildByFieldName("object"), src, scopes)
	case "subscript":
		if root := pyElementRoot(n.ChildByFieldName("value"), src, scopes); root != "" {
			return root
		}
		// `groups[key]`: selected by the loop's own variable and nothing else.
		// `rows[i + 1]` is a neighbour and `xs[i:]` a slice, not the element.
		if sub := n.ChildByFieldName("subscript"); sub != nil && kindOf(sub) == "identifier" {
			if name := pyText(sub, src); pyScopesHave(scopes, name) {
				return name
			}
		}
	case "boolean_operator":
		// `dag.tasks or []`: the fallback is empty or constant.
		if op := n.ChildByFieldName("operator"); op != nil && pyText(op, src) == "or" {
			return pyElementRoot(n.ChildByFieldName("left"), src, scopes)
		}
	case "call":
		fn := n.ChildByFieldName("function")
		if fn == nil {
			return ""
		}
		var first *sitter.Node
		if args := n.ChildByFieldName("arguments"); args != nil && args.NamedChildCount() > 0 {
			first = args.NamedChild(0)
		}
		switch kindOf(fn) {
		case "identifier":
			if pyWrapsElement[pyText(fn, src)] {
				return pyElementRoot(first, src, scopes)
			}
		case "attribute":
			// `dag.tasks.values()`, `node.children()`: a view or an accessor of the element.
			if root := pyElementRoot(fn.ChildByFieldName("object"), src, scopes); root != "" {
				attr := fn.ChildByFieldName("attribute")
				if first == nil || (attr != nil && pyViewMethods[pyText(attr, src)]) {
					return root
				}
				return ""
			}
			// `by_id.get(task.id)`: a keyed lookup, the dict spelling of `by_id[task.id]`.
			if attr := fn.ChildByFieldName("attribute"); attr != nil && pyText(attr, src) == "get" {
				return pyElementRoot(first, src, scopes)
			}
		}
	}
	return ""
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

// The cross-call form of the rule. A loop that hands each element to a callee is
// the same walk as a nested loop when the callee loops over what it was handed:
//
//	for dag in dags:
//	    validate(dag)
//
//	def validate(dag):
//	    for task in dag.tasks: ...
//
// Two facts decide it, one on each side, and the analyzer joins them:
//
//	calls_on_loop_element(_arg)  the caller passes an element, and in which position
//	loops_over_param(_index)     the callee has a scaling loop over that parameter
//
// The position matters: a callee looping over a different parameter multiplies.

// pyReceiverParam is the position that stands for self / cls on both sides.
const pyReceiverParam = -1

// pyParamScope is a function's parameters as a scope, with each name's position.
// A leading self or cls is the receiver and does not count: `obj.method(a)` puts
// `a` in position 0 on the call side, so it has to be 0 on this side too.
func pyParamScope(params *sitter.Node, src []byte) (pyLoopScope, map[string]int) {
	sc := pyLoopScope{amortizes: true}
	idx := map[string]int{}
	if params == nil {
		return sc, idx
	}
	pos := 0
	for i := uint(0); i < uint(params.NamedChildCount()); i++ {
		p := params.NamedChild(i)
		name := ""
		switch kindOf(p) {
		case "identifier":
			name = pyText(p, src)
		case "default_parameter", "typed_default_parameter":
			if n := p.ChildByFieldName("name"); n != nil {
				name = pyText(n, src)
			}
		case "typed_parameter":
			if p.NamedChildCount() > 0 && kindOf(p.NamedChild(0)) == "identifier" {
				name = pyText(p.NamedChild(0), src)
			}
		}
		if name == "" {
			pos++ // *args, **kwargs, a bare `*`: holds a position, binds nothing we follow
			continue
		}
		if i == 0 && (name == "self" || name == "cls") {
			sc.add(name)
			idx[name] = pyReceiverParam
			continue
		}
		sc.add(name)
		idx[name] = pos
		pos++
	}
	return sc, idx
}
