package tsextractor

import (
	"github.com/enola-labs/enola/internal/extractors/tsutil"
	sitter "github.com/tree-sitter/go-tree-sitter"
)

// The hierarchical-loop rule, as the Go extractor applies it.
//
//	for (const job of jobs) {
//	    for (const need of job.needs) { … }
//	}
//
// The inner loop walks the children of the element the outer loop is on, so the
// nest visits every `need` once: its cost is the number of needs, not jobs squared.
// Such a loop adds no factor of n to scaling_loop_depth. It still repeats, so a
// call inside it stays an N+1 candidate.
//
// The proof is syntactic and deliberately narrower than "mentions the outer
// variable". The inner collection has to be REACHED THROUGH the element: a member
// of it, a subscript of it, a lookup keyed by it. `xs.slice(i + 1)` mentions the
// outer index and is the all-pairs loop this analysis exists to find, so a call
// on some other receiver that merely takes the variable as an argument proves
// nothing and keeps counting.

// tsLoopScope is one enclosing loop's contribution to that proof.
type tsLoopScope struct {
	// elems are names bound to the element the loop is on: the for..of variable, an
	// iterator callback's first parameter, and locals derived from either.
	elems map[string]bool
	// indexes are names that select the element without being it: a C-style
	// counter, a for..in key, a callback's index parameter. `xs[i]` is an element.
	indexes map[string]bool
	// amortizes is false under a loop with a constant trip count, where there is no
	// factor of n for a hierarchical inner loop to cancel.
	amortizes bool
}

func (s *tsLoopScope) addElem(name string) {
	if name == "" || name == "_" {
		return
	}
	if s.elems == nil {
		s.elems = make(map[string]bool)
	}
	s.elems[name] = true
}

func (s *tsLoopScope) addIndex(name string) {
	if name == "" || name == "_" {
		return
	}
	if s.indexes == nil {
		s.indexes = make(map[string]bool)
	}
	s.indexes[name] = true
}

func tsScopesHave(scopes []tsLoopScope, name string, index bool) bool {
	for i := range scopes {
		if !scopes[i].amortizes {
			continue
		}
		if index && scopes[i].indexes[name] {
			return true
		}
		if !index && scopes[i].elems[name] {
			return true
		}
	}
	return false
}

// tsWrapsElement lists the calls that hand back a view of their argument rather
// than a new collection: iterating the result is iterating the argument.
var tsWrapsElement = map[string]bool{
	"Object.keys": true, "Object.values": true, "Object.entries": true, "Array.from": true,
}

// tsViewMethods are the methods that hand back a view of their receiver whatever
// they are passed. Any other method of the element counts only when it takes no
// arguments, the shape of an accessor: `task.get_flat_relatives(upstream=False)`
// returns everything reachable from the task, not what belongs to it, and a nest
// over it is quadratic.
var tsViewMethods = map[string]bool{
	"filter": true, "map": true, "flatMap": true, "slice": true, "sort": true, "toSorted": true,
	"reverse": true, "toReversed": true, "concat": true, "values": true, "keys": true, "entries": true,
}

// tsReachedThroughElement reports whether expr is reached through the element of
// an enclosing loop. See the rule at the top of this file.
func tsReachedThroughElement(kinds *tsutil.KindTable, n *sitter.Node, src []byte, scopes []tsLoopScope) bool {
	if n == nil || len(scopes) == 0 {
		return false
	}
	switch kindOf(kinds, n) {
	case "identifier":
		return tsScopesHave(scopes, nodeText(n, src), false)
	case "parenthesized_expression", "non_null_expression", "as_expression", "satisfies_expression", "await_expression":
		if n.NamedChildCount() > 0 {
			return tsReachedThroughElement(kinds, n.NamedChild(0), src, scopes)
		}
	case "member_expression":
		return tsReachedThroughElement(kinds, n.ChildByFieldName("object"), src, scopes)
	case "subscript_expression":
		if tsReachedThroughElement(kinds, n.ChildByFieldName("object"), src, scopes) {
			return true
		}
		// `groups[key]`, `rows[i]`: selected by the loop's own variable, and nothing
		// else. `rows[i + 1]` is a neighbour, not the element.
		idx := n.ChildByFieldName("index")
		if idx != nil && kindOf(kinds, idx) == "identifier" {
			name := nodeText(idx, src)
			return tsScopesHave(scopes, name, true) || tsScopesHave(scopes, name, false)
		}
	case "binary_expression":
		// `job.needs ?? []`, `job.needs || []`: the fallback is empty or constant.
		if op := n.ChildByFieldName("operator"); op != nil {
			if t := nodeText(op, src); t == "??" || t == "||" {
				return tsReachedThroughElement(kinds, n.ChildByFieldName("left"), src, scopes)
			}
		}
	case "call_expression":
		fn := n.ChildByFieldName("function")
		if fn == nil {
			return false
		}
		args := n.ChildByFieldName("arguments")
		var first *sitter.Node
		if args != nil && args.NamedChildCount() > 0 {
			first = args.NamedChild(0)
		}
		if tsWrapsElement[nodeText(fn, src)] {
			return tsReachedThroughElement(kinds, first, src, scopes)
		}
		if kindOf(kinds, fn) != "member_expression" {
			return false
		}
		// `job.needs.filter(…)`, `node.children()`: a view or an accessor of the element.
		if tsReachedThroughElement(kinds, fn.ChildByFieldName("object"), src, scopes) {
			prop := fn.ChildByFieldName("property")
			return first == nil || (prop != nil && tsViewMethods[nodeText(prop, src)])
		}
		// `byId.get(job.id)`: a keyed lookup, the Map spelling of `byId[job.id]`.
		if prop := fn.ChildByFieldName("property"); prop != nil && nodeText(prop, src) == "get" {
			return tsReachedThroughElement(kinds, first, src, scopes)
		}
	}
	return false
}

// tsBindPattern adds every name a binding pattern introduces. A destructured
// element is still the element: `for (const {id, tags} of rows)` puts `tags` one
// step from `row.tags`.
func tsBindPattern(kinds *tsutil.KindTable, n *sitter.Node, src []byte, add func(string)) {
	if n == nil {
		return
	}
	switch kindOf(kinds, n) {
	case "identifier", "shorthand_property_identifier_pattern":
		add(nodeText(n, src))
		return
	case "assignment_pattern":
		// `{tags = []}`: the default is an expression, not a binding.
		tsBindPattern(kinds, n.ChildByFieldName("left"), src, add)
		return
	case "pair_pattern":
		tsBindPattern(kinds, n.ChildByFieldName("value"), src, add)
		return
	case "type_annotation":
		return
	}
	for i := range n.NamedChildCount() {
		tsBindPattern(kinds, n.NamedChild(i), src, add)
	}
}

// tsForInScope is the scope a for..of / for..in loop opens. A for..in variable is
// a key, so it selects an element and is not one.
func tsForInScope(kinds *tsutil.KindTable, n *sitter.Node, src []byte, amortizes bool) tsLoopScope {
	sc := tsLoopScope{amortizes: amortizes}
	isOf := false
	for i := range n.ChildCount() {
		if kindOf(kinds, n.Child(i)) == "of" {
			isOf = true
			break
		}
	}
	add := sc.addIndex
	if isOf {
		add = sc.addElem
	}
	tsBindPattern(kinds, n.ChildByFieldName("left"), src, add)
	return sc
}

// tsForScope is the scope a C-style for loop opens: its counters.
func tsForScope(kinds *tsutil.KindTable, n *sitter.Node, src []byte) tsLoopScope {
	sc := tsLoopScope{amortizes: true}
	init := n.ChildByFieldName("initializer")
	if init == nil {
		return sc
	}
	for i := range init.NamedChildCount() {
		if d := init.NamedChild(i); kindOf(kinds, d) == "variable_declarator" {
			tsBindPattern(kinds, d.ChildByFieldName("name"), src, sc.addIndex)
		}
	}
	return sc
}

// tsForBoundThroughElement reports whether a C-style loop's bound is a size of
// the enclosing element: `j < rows[i].length`, `k < node.children.length`.
func tsForBoundThroughElement(kinds *tsutil.KindTable, n *sitter.Node, src []byte, scopes []tsLoopScope) bool {
	cond := n.ChildByFieldName("condition")
	for cond != nil && kindOf(kinds, cond) != "binary_expression" && cond.NamedChildCount() == 1 {
		cond = cond.NamedChild(0)
	}
	if cond == nil || kindOf(kinds, cond) != "binary_expression" {
		return false
	}
	for _, side := range []string{"left", "right"} {
		e := cond.ChildByFieldName(side)
		if e == nil || kindOf(kinds, e) != "member_expression" {
			continue
		}
		prop := e.ChildByFieldName("property")
		if prop == nil {
			continue
		}
		if t := nodeText(prop, src); t != "length" && t != "size" {
			continue
		}
		if tsReachedThroughElement(kinds, e.ChildByFieldName("object"), src, scopes) {
			return true
		}
	}
	return false
}

// tsCallbackScope is the scope an iterator callback opens: `(row, i) => …` binds
// the element and its index.
func tsCallbackScope(kinds *tsutil.KindTable, cb *sitter.Node, src []byte, amortizes bool) tsLoopScope {
	sc := tsLoopScope{amortizes: amortizes}
	if p := cb.ChildByFieldName("parameter"); p != nil { // `row => …`
		sc.addElem(nodeText(p, src))
		return sc
	}
	params := cb.ChildByFieldName("parameters")
	if params == nil {
		return sc
	}
	for i := range params.NamedChildCount() {
		p := params.NamedChild(i)
		target := p
		if pat := p.ChildByFieldName("pattern"); pat != nil {
			target = pat
		}
		switch i {
		case 0:
			tsBindPattern(kinds, target, src, sc.addElem)
		case 1:
			tsBindPattern(kinds, target, src, sc.addIndex)
		}
	}
	return sc
}
