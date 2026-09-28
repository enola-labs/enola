// Package callsite names the function that makes each hand-written client call.
//
// A client route fact records WHERE a call is made (its file and line) but not which
// function makes it, and no extractor records where a function ends, so the answer
// cannot be recovered from the facts afterwards. It has to be read from the syntax
// tree while the extractor still holds it. This package is the part of that which
// does not depend on the language: an extractor supplies the node kinds that are
// functions in its grammar and the declaration nodes a function can sit inside, and
// Attribute pairs the call sites with the file's own symbols.
package callsite

import (
	sitter "github.com/tree-sitter/go-tree-sitter"

	"github.com/enola-labs/enola/internal/facts"
)

// Grammar describes one language's tree to Attribute.
type Grammar struct {
	// KindOf returns a node's kind name.
	KindOf func(*sitter.Node) string
	// Functions are the kinds whose body is a function.
	Functions map[string]bool
	// Wrappers are the kinds a function can sit inside while still being the value
	// a declaration names, so its symbol's Line is the wrapper's start: a variable
	// declarator, an export, a class property, an annotated declaration.
	Wrappers map[string]bool
}

// Span is one function's row range (0-based, inclusive) and the symbol it was
// emitted as.
type Span struct {
	Start, End uint
	Symbol     string
}

// Attribute sets facts.PropCaller on every hand-written client route in result: the
// symbol of the innermost function containing the call site that the file emitted a
// symbol for. result must hold the file's symbol facts as well as its routes.
//
// A function node and its symbol are paired by line. A symbol's Line is the start of
// the node the declaration walk emitted it for, which is the function itself or a
// declaration wrapping it, so a node is matched on its own start row or that of a
// wrapping declaration, and on no other ancestor's. Two symbols starting on one of
// those rows leave the node unmatched rather than guessed at.
//
// A call inside a closure with no symbol of its own is credited to the nearest
// enclosing function that has one, which is how the call walks credit the closure's
// other calls. A call at file scope gets no caller.
func Attribute(g Grammar, root *sitter.Node, result []facts.Fact) {
	var calls []int
	for i := range result {
		f := &result[i]
		if f.Kind == facts.KindRoute && f.Line > 0 && f.PropAny(facts.PropRole) == facts.RoleClient &&
			facts.HandWrittenClientSources[f.PropString(facts.PropSource)] {
			calls = append(calls, i)
		}
	}
	if len(calls) == 0 || root == nil {
		return
	}
	AttributeSpans(treeSpans(g, root, result), result)
}

// treeSpans pairs the function nodes of a tree with the symbols in result.
func treeSpans(g Grammar, root *sitter.Node, result []facts.Fact) []Span {
	byLine := map[uint][]string{}
	for _, f := range result {
		if f.Kind == facts.KindSymbol && f.Line > 0 {
			byLine[uint(f.Line-1)] = append(byLine[uint(f.Line-1)], f.Name)
		}
	}
	symbolAt := func(n *sitter.Node) string {
		for {
			if names := byLine[n.StartPosition().Row]; len(names) > 0 {
				if len(names) == 1 {
					return names[0]
				}
				return ""
			}
			p := n.Parent()
			if p == nil || !g.Wrappers[g.KindOf(p)] {
				return ""
			}
			n = p
		}
	}

	var spans []Span
	stack := []*sitter.Node{root}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if g.Functions[g.KindOf(n)] {
			if sym := symbolAt(n); sym != "" {
				spans = append(spans, Span{Start: n.StartPosition().Row, End: n.EndPosition().Row, Symbol: sym})
			}
		}
		for i := range n.ChildCount() {
			stack = append(stack, n.Child(i))
		}
	}
	return spans
}

// AttributeSpans sets facts.PropCaller on every hand-written client route in result
// to the symbol of the narrowest span containing its line. It is Attribute for an
// extractor that knows its functions' extents directly: one whose grammar makes a
// function's signature and body siblings rather than one node, so no single node
// contains a call and names its function.
func AttributeSpans(spans []Span, result []facts.Fact) {
	if len(spans) == 0 {
		return
	}
	for i := range result {
		f := &result[i]
		if f.Kind != facts.KindRoute || f.Line <= 0 || f.PropAny(facts.PropRole) != facts.RoleClient ||
			!facts.HandWrittenClientSources[f.PropString(facts.PropSource)] {
			continue
		}
		row := uint(f.Line - 1)
		best := -1
		for j, s := range spans {
			if row < s.Start || row > s.End {
				continue
			}
			if best < 0 || s.End-s.Start < spans[best].End-spans[best].Start {
				best = j
			}
		}
		if best >= 0 {
			f.SetProp(facts.PropCaller, spans[best].Symbol)
		}
	}
}
