package javaextractor

import (
	sitter "github.com/tree-sitter/go-tree-sitter"
)

// Batch loops: a loop that takes its input a page at a time.
//
//	do {
//	    page = service.findDevices(tenantId, pageLink);   // ONE query for the page
//	    for (Device d : page.getData()) { … }             // drained here
//	    pageLink = pageLink.nextPageLink();
//	} while (page.hasNext());
//
// Read as ordinary nesting this is a quadratic loop with a query in it, and the
// advice attached to that finding is to batch the query. It is the batching: each
// row is visited once, and the round trips are the row count over the page size.
// So the loop that drains a page adds no factor, and a call made once per round,
// at the paging loop's own level, is not an N+1 candidate. A call inside the
// drain loop still runs once per row and still is.
//
// The shape has three parts, and all three are required. The loop is a condition
// loop (while, do-while). Its body binds a local from a call and loops over that
// local. And whether the loop goes round again depends on that local: its
// condition reads it, or an `if` that breaks or returns does.
//
// The third part is what tells paging from a walk that queries per element:
//
//	while (it.hasNext()) {
//	    Node n = it.next();
//	    List<Node> kids = dao.findChildren(n.getId());   // one query per node
//	    for (Node k : kids) { … }
//	}
//
// binds and drains too, but the loop runs for as long as there are nodes, whatever
// a round fetched. That one is an N+1 and stays one.

// javaBatchLoop returns the locals a paging loop binds each round to what it
// fetched, or nil when node is not one.
func javaBatchLoop(node *sitter.Node, src []byte) []string {
	if k := kindOf(node); k != "while_statement" && k != "do_statement" {
		return nil
	}
	body := node.ChildByFieldName("body")
	if body == nil {
		return nil
	}
	// What each round binds from a call, and what it derives from those.
	fetched := map[string]bool{}
	derived := map[string]bool{}
	bind := func(name, value *sitter.Node) {
		if name == nil || value == nil || kindOf(name) != "identifier" {
			return
		}
		n := nodeText(name, src)
		switch {
		case kindOf(value) == "method_invocation" && !javaRootedAt(value, src, fetched):
			fetched[n] = true
		case javaRootedAt(value, src, fetched) || javaRootedAt(value, src, derived):
			derived[n] = true
		}
	}
	for i := uint(0); i < body.NamedChildCount(); i++ {
		st := body.NamedChild(i)
		switch kindOf(st) {
		case "local_variable_declaration":
			for j := uint(0); j < st.NamedChildCount(); j++ {
				if d := st.NamedChild(j); kindOf(d) == "variable_declarator" {
					bind(d.ChildByFieldName("name"), d.ChildByFieldName("value"))
				}
			}
		case "expression_statement":
			if a := st.NamedChild(0); a != nil && kindOf(a) == "assignment_expression" {
				bind(a.ChildByFieldName("left"), a.ChildByFieldName("right"))
			}
		}
	}
	if len(fetched) == 0 {
		return nil
	}

	// Does going round again depend on what a round fetched?
	controls := func(cond *sitter.Node) bool {
		return javaMentions(cond, src, fetched) || javaMentions(cond, src, derived)
	}
	controlled := controls(node.ChildByFieldName("condition"))
	for i := uint(0); !controlled && i < body.NamedChildCount(); i++ {
		if st := body.NamedChild(i); kindOf(st) == "if_statement" && javaLeavesLoop(st.ChildByFieldName("consequence")) {
			controlled = controls(st.ChildByFieldName("condition"))
		}
	}
	if !controlled {
		return nil
	}

	// And is what it fetched drained here?
	var pages []string
	var find func(n *sitter.Node)
	find = func(n *sitter.Node) {
		if n == nil {
			return
		}
		var over *sitter.Node
		switch kindOf(n) {
		case "enhanced_for_statement":
			over = n.ChildByFieldName("value")
		case "method_invocation":
			if javaStreamLambda(n, src) != nil {
				over = n.ChildByFieldName("object")
			}
		}
		if name := javaRoot(over, src); name != "" && fetched[name] {
			pages = append(pages, name)
		}
		for i := uint(0); i < n.NamedChildCount(); i++ {
			find(n.NamedChild(i))
		}
	}
	find(body)
	return pages
}

// javaRoot returns the identifier an access chain starts at: `page` for
// `page.getData().stream()`. "" when the chain starts at anything else.
func javaRoot(n *sitter.Node, src []byte) string {
	for n != nil {
		switch kindOf(n) {
		case "identifier":
			return nodeText(n, src)
		case "method_invocation", "field_access":
			n = n.ChildByFieldName("object")
		case "parenthesized_expression":
			n = n.NamedChild(0)
		default:
			return ""
		}
	}
	return ""
}

func javaRootedAt(n *sitter.Node, src []byte, names map[string]bool) bool {
	root := javaRoot(n, src)
	return root != "" && names[root]
}

// javaMentions reports whether an expression names one of names anywhere in it.
func javaMentions(n *sitter.Node, src []byte, names map[string]bool) bool {
	if n == nil || len(names) == 0 {
		return false
	}
	if kindOf(n) == "identifier" {
		return names[nodeText(n, src)]
	}
	for i := uint(0); i < n.NamedChildCount(); i++ {
		if javaMentions(n.NamedChild(i), src, names) {
			return true
		}
	}
	return false
}

// javaLeavesLoop reports whether a statement is, or is a block ending in, a break
// or a return.
func javaLeavesLoop(n *sitter.Node) bool {
	if n == nil {
		return false
	}
	switch kindOf(n) {
	case "break_statement", "return_statement":
		return true
	case "block":
		if c := n.NamedChildCount(); c > 0 {
			return javaLeavesLoop(n.NamedChild(c - 1))
		}
	}
	return false
}
