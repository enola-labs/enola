package phpextractor

import (
	sitter "github.com/tree-sitter/go-tree-sitter"
)

// The hierarchical-loop rule, as the Go and TypeScript extractors apply it.
//
//	foreach ($drivers as $driver) {
//	    foreach ($driver->models() as $model) { … }
//	}
//
// The inner loop walks what belongs to the element the outer loop is on, so the
// nest visits every model once and adds no factor of n to scaling_loop_depth. It
// still repeats, so a call inside it stays an N+1 candidate: for the loop's class
// that is exactly what phpLoopInfinite already means.
//
// The collection has to be REACHED THROUGH the element (a member, a method, a
// subscript, a lookup keyed by it). Merely mentioning the outer variable proves
// nothing: `array_slice($xs, $i + 1)` is the all-pairs loop.

// phpLoopScope is one enclosing loop's contribution to that proof.
type phpLoopScope struct {
	// vars are the names the loop binds, `$`-prefixed as written: the foreach value
	// and key, and locals assigned from either.
	vars map[string]bool
	// amortizes is false under a loop with a constant trip count, where there is no
	// factor of n for a hierarchical inner loop to cancel.
	amortizes bool
}

func (s *phpLoopScope) add(name string) {
	if name == "" {
		return
	}
	if s.vars == nil {
		s.vars = make(map[string]bool)
	}
	s.vars[name] = true
}

func phpScopesHave(scopes []phpLoopScope, name string) bool {
	for i := range scopes {
		if scopes[i].amortizes && scopes[i].vars[name] {
			return true
		}
	}
	return false
}

// phpWrapsElement lists the functions that return a view of their first argument:
// iterating the result is iterating the argument.
var phpWrapsElement = map[string]bool{
	"array_keys": true, "array_values": true, "array_filter": true, "array_unique": true,
	"array_reverse": true, "iterator_to_array": true,
}

// phpViewMethods are the methods that hand back a view of their receiver whatever
// they are passed. Any other method of the element counts only when it takes no
// arguments, the shape of an accessor: `task.get_flat_relatives(upstream=False)`
// returns everything reachable from the task, not what belongs to it, and a nest
// over it is quadratic.
var phpViewMethods = map[string]bool{
	"filter": true, "map": true, "values": true, "keys": true, "all": true, "toArray": true,
	"sortBy": true, "sort": true,
}

// phpReachedThroughElement reports whether expr is reached through the element of
// an enclosing loop. See the rule at the top of this file.
func phpReachedThroughElement(n *sitter.Node, src []byte, scopes []phpLoopScope) bool {
	if n == nil || len(scopes) == 0 {
		return false
	}
	switch kindOf(n) {
	case "variable_name":
		return phpScopesHave(scopes, phpText(n, src))
	case "parenthesized_expression":
		if n.NamedChildCount() > 0 {
			return phpReachedThroughElement(n.NamedChild(0), src, scopes)
		}
	case "member_access_expression", "nullsafe_member_access_expression":
		return phpReachedThroughElement(n.ChildByFieldName("object"), src, scopes)
	case "member_call_expression", "nullsafe_member_call_expression":
		// `$driver->models()`: a view or an accessor of the element.
		if !phpReachedThroughElement(n.ChildByFieldName("object"), src, scopes) {
			return false
		}
		args := n.ChildByFieldName("arguments")
		name := n.ChildByFieldName("name")
		return args == nil || args.NamedChildCount() == 0 || (name != nil && phpViewMethods[phpText(name, src)])
	case "subscript_expression":
		if n.NamedChildCount() == 0 {
			return false
		}
		if phpReachedThroughElement(n.NamedChild(0), src, scopes) {
			return true
		}
		// `$groups[$key]`: selected by the loop's own variable and nothing else.
		// `$rows[$i + 1]` is a neighbour, not the element.
		if n.NamedChildCount() > 1 && kindOf(n.NamedChild(1)) == "variable_name" {
			return phpScopesHave(scopes, phpText(n.NamedChild(1), src))
		}
	case "binary_expression":
		// `$driver->models ?? []`: the fallback is empty or constant.
		if op := n.ChildByFieldName("operator"); op != nil && phpText(op, src) == "??" {
			return phpReachedThroughElement(n.ChildByFieldName("left"), src, scopes)
		}
	case "function_call_expression":
		fn := n.ChildByFieldName("function")
		if fn == nil || !phpWrapsElement[phpText(fn, src)] {
			return false
		}
		args := n.ChildByFieldName("arguments")
		if args == nil || args.NamedChildCount() == 0 {
			return false
		}
		arg := args.NamedChild(0)
		if kindOf(arg) == "argument" && arg.NamedChildCount() > 0 {
			arg = arg.NamedChild(0)
		}
		return phpReachedThroughElement(arg, src, scopes)
	}
	return false
}

// phpForeachScope is the scope a foreach opens: its value, its key, and every
// name a list() destructuring binds.
func phpForeachScope(node *sitter.Node, src []byte, amortizes bool) phpLoopScope {
	sc := phpLoopScope{amortizes: amortizes}
	body := node.ChildByFieldName("body")
	// Named child 0 is the collection; what follows it, up to the body, binds.
	for i := uint(1); i < node.NamedChildCount(); i++ {
		c := node.NamedChild(i)
		if body != nil && c.StartByte() == body.StartByte() {
			break
		}
		phpBindVariables(c, src, &sc)
	}
	return sc
}

func phpBindVariables(n *sitter.Node, src []byte, sc *phpLoopScope) {
	if n == nil {
		return
	}
	if kindOf(n) == "variable_name" {
		sc.add(phpText(n, src))
		return
	}
	for i := uint(0); i < n.NamedChildCount(); i++ {
		phpBindVariables(n.NamedChild(i), src, sc)
	}
}
