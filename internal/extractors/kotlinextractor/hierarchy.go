package kotlinextractor

import (
	sitter "github.com/tree-sitter/go-tree-sitter"
)

// The hierarchical-loop rule, as the Go, TypeScript, PHP and Python extractors
// apply it.
//
//	for (config in configurations) {
//	    for (dep in config.dependencies) { … }
//	}
//
// The inner loop walks what belongs to the element the outer loop is on, so the
// nest visits every dependency once and adds no factor of n to scaling_loop_depth.
// It still repeats, so a call inside it stays an N+1 candidate: for the loop's
// class that is exactly what kotlinLoopInfinite already means.
//
// The collection has to be REACHED THROUGH the element (a member, a method, an
// index, a lookup keyed by it). Mentioning the outer variable proves nothing:
// `xs.drop(i + 1)` is the all-pairs loop.

// kotlinLoopScope is one enclosing loop's contribution to that proof.
type kotlinLoopScope struct {
	// vars are the names the loop binds (the for variable, a lambda's parameters or
	// its implicit `it`) and the locals initialised from them.
	vars map[string]bool
	// amortizes is false under a loop with a constant trip count, where there is no
	// factor of n for a hierarchical inner loop to cancel.
	amortizes bool
}

func (s *kotlinLoopScope) add(name string) {
	if name == "" || name == "_" {
		return
	}
	if s.vars == nil {
		s.vars = make(map[string]bool)
	}
	s.vars[name] = true
}

func kotlinScopesHave(scopes []kotlinLoopScope, name string) bool {
	for i := range scopes {
		if scopes[i].amortizes && scopes[i].vars[name] {
			return true
		}
	}
	return false
}

// kotlinKeyedLookups are the Map reads that are `m[key]` under another name.
var kotlinKeyedLookups = map[string]bool{
	"get": true, "getValue": true, "getOrDefault": true, "getOrElse": true,
}

// kotlinViewMethods are the methods that hand back a view of their receiver whatever
// they are passed. Any other method of the element counts only when it takes no
// arguments, the shape of an accessor: `task.get_flat_relatives(upstream=False)`
// returns everything reachable from the task, not what belongs to it, and a nest
// over it is quadratic.
//
// A trailing lambda is not a value argument, so `c.deps.filter { … }` is already
// an accessor by this test.
var kotlinViewMethods = map[string]bool{
	"sortedWith": true, "sortedBy": true, "filterIsInstance": true,
}

// kotlinReachedThroughElement reports whether expr is reached through the element
// of an enclosing loop. See the rule at the top of this file.
func kotlinReachedThroughElement(n *sitter.Node, src []byte, scopes []kotlinLoopScope) bool {
	if n == nil || len(scopes) == 0 {
		return false
	}
	switch kindOf(n) {
	case "identifier", "simple_identifier":
		return kotlinScopesHave(scopes, nodeText(n, src))
	case "parenthesized_expression":
		return kotlinReachedThroughElement(firstNamedChild(n), src, scopes)
	case "unary_expression", "postfix_expression":
		// `config.dependencies!!`
		if arg := n.ChildByFieldName("argument"); arg != nil {
			return kotlinReachedThroughElement(arg, src, scopes)
		}
		return kotlinReachedThroughElement(firstNamedChild(n), src, scopes)
	case "navigation_expression":
		return kotlinReachedThroughElement(firstNamedChild(n), src, scopes)
	case "index_expression", "indexing_expression":
		if kotlinReachedThroughElement(firstNamedChild(n), src, scopes) {
			return true
		}
		// `groups[key]`: selected by the loop's own variable and nothing else.
		// `rows[i + 1]` is a neighbour, not the element.
		if n.NamedChildCount() == 2 && kotlinIsIdentifier(n.NamedChild(1)) {
			return kotlinScopesHave(scopes, nodeText(n.NamedChild(1), src))
		}
	case "binary_expression":
		// `config.dependencies ?: emptyList()`: the fallback is empty or constant.
		for i := uint(0); i < uint(n.ChildCount()); i++ {
			if kindOf(n.Child(i)) == "?:" {
				return kotlinReachedThroughElement(n.ChildByFieldName("left"), src, scopes)
			}
		}
	case "call_expression":
		callee := firstNamedChild(n)
		if callee == nil || kindOf(callee) != "navigation_expression" {
			return false
		}
		// `config.dependencies.filter { … }`, `node.children()`: a view or an
		// accessor of the element.
		if kotlinReachedThroughElement(firstNamedChild(callee), src, scopes) {
			name, _ := calleeName(callee, src)
			return kotlinCallArgCount(n) == 0 || kotlinViewMethods[name]
		}
		// `byId.getValue(config.id)`: a keyed lookup.
		if name, _ := calleeName(callee, src); kotlinKeyedLookups[name] {
			if args := findChildByKind(n, "value_arguments"); args != nil {
				if a := findChildByKind(args, "value_argument"); a != nil {
					return kotlinReachedThroughElement(lastNamedChild(a), src, scopes)
				}
			}
		}
	}
	return false
}

// kotlinBindDeclared adds every name a variable_declaration or a destructuring
// multi_variable_declaration under n binds, without descending into expressions.
func kotlinBindDeclared(n *sitter.Node, src []byte, sc *kotlinLoopScope) {
	if n == nil {
		return
	}
	switch kindOf(n) {
	case "variable_declaration":
		for i := uint(0); i < uint(n.ChildCount()); i++ {
			if kotlinIsIdentifier(n.Child(i)) {
				sc.add(nodeText(n.Child(i), src))
				return
			}
		}
	case "multi_variable_declaration", "lambda_parameters":
		for i := uint(0); i < n.NamedChildCount(); i++ {
			kotlinBindDeclared(n.NamedChild(i), src, sc)
		}
	}
}

// kotlinForScope is the scope a for statement opens.
func kotlinForScope(node *sitter.Node, src []byte, amortizes bool) kotlinLoopScope {
	sc := kotlinLoopScope{amortizes: amortizes}
	for i := uint(0); i < node.NamedChildCount(); i++ {
		switch c := node.NamedChild(i); kindOf(c) {
		case "variable_declaration", "multi_variable_declaration":
			kotlinBindDeclared(c, src, &sc)
		}
	}
	return sc
}

// kotlinLambdaScope is the scope an iterator's lambda opens: its declared
// parameters, or the implicit `it` when it declares none.
func kotlinLambdaScope(lit *sitter.Node, src []byte, amortizes bool) kotlinLoopScope {
	sc := kotlinLoopScope{amortizes: amortizes}
	if params := findChildByKind(lit, "lambda_parameters"); params != nil {
		kotlinBindDeclared(params, src, &sc)
		return sc
	}
	sc.add("it")
	return sc
}
