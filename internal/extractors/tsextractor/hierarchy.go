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
	return tsElementRoot(kinds, n, src, scopes) != ""
}

// tsElementRoot returns the scope variable expr is reached through, or "" when it
// is not reached through one. `job.needs.filter(f)` is rooted at `job`.
func tsElementRoot(kinds *tsutil.KindTable, n *sitter.Node, src []byte, scopes []tsLoopScope) string {
	if n == nil || len(scopes) == 0 {
		return ""
	}
	switch kindOf(kinds, n) {
	case "identifier", "this":
		if name := nodeText(n, src); tsScopesHave(scopes, name, false) {
			return name
		}
	case "parenthesized_expression", "non_null_expression", "as_expression", "satisfies_expression", "await_expression":
		if n.NamedChildCount() > 0 {
			return tsElementRoot(kinds, n.NamedChild(0), src, scopes)
		}
	case "member_expression":
		return tsElementRoot(kinds, n.ChildByFieldName("object"), src, scopes)
	case "subscript_expression":
		if root := tsElementRoot(kinds, n.ChildByFieldName("object"), src, scopes); root != "" {
			return root
		}
		// `groups[key]`, `rows[i]`: selected by the loop's own variable, and nothing
		// else. `rows[i + 1]` is a neighbour, not the element.
		idx := n.ChildByFieldName("index")
		if idx != nil && kindOf(kinds, idx) == "identifier" {
			if name := nodeText(idx, src); tsScopesHave(scopes, name, true) || tsScopesHave(scopes, name, false) {
				return name
			}
		}
	case "binary_expression":
		// `job.needs ?? []`, `job.needs || []`: the fallback is empty or constant.
		if op := n.ChildByFieldName("operator"); op != nil {
			if t := nodeText(op, src); t == "??" || t == "||" {
				return tsElementRoot(kinds, n.ChildByFieldName("left"), src, scopes)
			}
		}
	case "call_expression":
		fn := n.ChildByFieldName("function")
		if fn == nil {
			return ""
		}
		args := n.ChildByFieldName("arguments")
		var first *sitter.Node
		if args != nil && args.NamedChildCount() > 0 {
			first = args.NamedChild(0)
		}
		if tsWrapsElement[nodeText(fn, src)] {
			return tsElementRoot(kinds, first, src, scopes)
		}
		if kindOf(kinds, fn) != "member_expression" {
			return ""
		}
		// `job.needs.filter(…)`, `node.children()`: a view or an accessor of the element.
		if root := tsElementRoot(kinds, fn.ChildByFieldName("object"), src, scopes); root != "" {
			prop := fn.ChildByFieldName("property")
			if first == nil || (prop != nil && tsViewMethods[nodeText(prop, src)]) {
				return root
			}
			return ""
		}
		// `byId.get(job.id)`: a keyed lookup, the Map spelling of `byId[job.id]`.
		if prop := fn.ChildByFieldName("property"); prop != nil && nodeText(prop, src) == "get" {
			return tsElementRoot(kinds, first, src, scopes)
		}
	}
	return ""
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
	return tsForBoundRoot(kinds, n, src, scopes) != ""
}

// tsForBoundRoot is tsForBoundThroughElement naming the variable the bound is
// reached through.
func tsForBoundRoot(kinds *tsutil.KindTable, n *sitter.Node, src []byte, scopes []tsLoopScope) string {
	cond := n.ChildByFieldName("condition")
	for cond != nil && kindOf(kinds, cond) != "binary_expression" && cond.NamedChildCount() == 1 {
		cond = cond.NamedChild(0)
	}
	if cond == nil || kindOf(kinds, cond) != "binary_expression" {
		return ""
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
		if root := tsElementRoot(kinds, e.ChildByFieldName("object"), src, scopes); root != "" {
			return root
		}
	}
	return ""
}

// tsCallbackScope is the scope an iterator callback opens: `(row, i) => …` binds
// the element and its index.
//
// elemAt is the position of the element: 0 for map/filter/forEach and the rest, 1
// for reduce and reduceRight, whose first parameter is the accumulator.
func tsCallbackScope(kinds *tsutil.KindTable, cb *sitter.Node, src []byte, amortizes bool, elemAt int) tsLoopScope {
	sc := tsLoopScope{amortizes: amortizes}
	if p := cb.ChildByFieldName("parameter"); p != nil { // `row => …`
		if elemAt == 0 {
			sc.addElem(nodeText(p, src))
		}
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
		switch int(i) {
		case elemAt:
			tsBindPattern(kinds, target, src, sc.addElem)
		case elemAt + 1:
			tsBindPattern(kinds, target, src, sc.addIndex)
		}
	}
	return sc
}

// The cross-call form of the rule. A loop that hands each element to a callee is
// the same walk as a nested loop when the callee loops over what it was handed:
//
//	for (const frame of frames) { render(frame) }
//	function render(frame) { for (const el of frame.elements) { … } }
//
// Two facts decide it, one on each side, and the analyzer joins them:
//
//	calls_on_loop_element(_arg)  the caller passes an element, and in which position
//	loops_over_param(_index)     the callee has a scaling loop over that parameter
//
// The position matters. A callee that loops over a DIFFERENT parameter, as
// `itemHasQuery(item, words)` does over `words`, multiplies.

// tsReceiverParam is the position that stands for `this` on both sides.
const tsReceiverParam = -1

// tsParamScope is a function's parameters as a scope, with each name's position.
// It is kept apart from the loop scopes: reaching a collection through a parameter
// says whose data a loop walks, and nothing about whether the loop scales.
func tsParamScope(kinds *tsutil.KindTable, fn *sitter.Node, src []byte) (tsLoopScope, map[string]int) {
	sc := tsLoopScope{amortizes: true}
	idx := map[string]int{"this": tsReceiverParam}
	sc.addElem("this")
	if fn == nil {
		return sc, idx
	}
	bind := func(target *sitter.Node, i int) {
		tsBindPattern(kinds, target, src, func(name string) {
			sc.addElem(name)
			idx[name] = i
		})
	}
	if p := fn.ChildByFieldName("parameter"); p != nil { // `row => …`
		bind(p, 0)
		return sc, idx
	}
	params := fn.ChildByFieldName("parameters")
	if params == nil {
		return sc, idx
	}
	for i := range params.NamedChildCount() {
		p := params.NamedChild(i)
		if pat := p.ChildByFieldName("pattern"); pat != nil {
			p = pat
		}
		bind(p, int(i))
	}
	return sc, idx
}

// tsEnclosingFunction returns node if it declares parameters, else the nearest
// ancestor that does: the metrics walk is handed a function in some call sites and
// its body in others.
func tsEnclosingFunction(node *sitter.Node) *sitter.Node {
	for n := node; n != nil; n = n.Parent() {
		if n.ChildByFieldName("parameters") != nil || n.ChildByFieldName("parameter") != nil {
			return n
		}
	}
	return nil
}

// tsCallbackElementAt returns which parameter of an iterator's callback receives
// the element.
func tsCallbackElementAt(kinds *tsutil.KindTable, call *sitter.Node, src []byte) int {
	if fn := call.ChildByFieldName("function"); fn != nil {
		if prop := fn.ChildByFieldName("property"); prop != nil {
			switch nodeText(prop, src) {
			case "reduce", "reduceRight":
				return 1
			}
		}
	}
	return 0
}
