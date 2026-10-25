package kotlinextractor

import (
	"slices"
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"

	"github.com/enola-labs/enola/internal/facts"
)

// A class, object or enum of the repository used as a value or tested against.
//
//	when (effect) {
//	    ShutdownConnection -> …       // an object as a when condition
//	    is SocketEffect.Close -> …    // a type test
//	}
//	logger.level = Level.BODY         // an enum's entry, through the enum
//	register(Defaults)                // an object passed on
//
// None of these is a call or a constructor, so nothing recorded that the code uses
// the type, and an object reached only this way read as unused. Each becomes a
// names edge to the type, where the repository declares it. The walker records the
// names a written path could have, longest first, and resolveValueRefs keeps the
// first one that names a symbol.

// propValueRefs lists, for a function, the candidate names of each type it uses as
// a value. It does not leave the extractor.
const propValueRefs = "value_refs"

// kotlinValueParents are the places an expression stands as a value.
var kotlinValueParents = map[string]bool{
	"when_entry": true, "when_subject": true, "property_declaration": true,
	"value_argument": true, "return_expression": true, "jump_expression": true,
	"parenthesized_expression": true, "equality_expression": true,
	"comparison_expression": true, "conjunction_expression": true,
	"disjunction_expression": true, "elvis_expression": true,
	"collection_literal": true, "if_expression": true, "statements": true,
}

// noteValueRef records a type the node uses as a value, if it is one: a
// capitalized identifier or a capitalized path in a value position, the right side
// of an assignment, or the type of a type test.
func (w *astWalker) noteValueRef(node *sitter.Node) {
	parent := node.Parent()
	if parent == nil {
		return
	}
	var segs []string
	switch kindOf(node) {
	case "identifier", "simple_identifier":
		if kotlinValuePosition(node, parent) {
			segs = []string{nodeText(node, w.src)}
		}
	case "navigation_expression":
		if kotlinValuePosition(node, parent) {
			segs = kotlinPathSegments(node, w.src)
		}
	case "type_test":
		if ut := findChildByKind(node, "user_type"); ut != nil {
			for i := uint(0); i < ut.NamedChildCount(); i++ {
				if c := ut.NamedChild(i); kotlinIsIdentifier(c) {
					segs = append(segs, nodeText(c, w.src))
				}
			}
		}
	}
	w.noteValueSegments(segs)
}

// kotlinValuePosition reports whether node stands as a value of parent: not a
// callee, not a member name after a dot, not a declared name.
func kotlinValuePosition(node, parent *sitter.Node) bool {
	switch kindOf(parent) {
	case "assignment":
		r := parent.ChildByFieldName("right")
		return r != nil && r.Id() == node.Id()
	case "navigation_expression":
		// The receiver of a navigation that is itself a call's member is a type
		// whose member is called (`Analytics.log()`), already a call edge.
		return false
	}
	return kotlinValueParents[kindOf(parent)]
}

// kotlinPathSegments returns the identifiers of a plain dotted path, or nil.
func kotlinPathSegments(nav *sitter.Node, src []byte) []string {
	var segs []string
	var walk func(n *sitter.Node) bool
	walk = func(n *sitter.Node) bool {
		switch {
		case kotlinIsIdentifier(n):
			segs = append(segs, nodeText(n, src))
			return true
		case kindOf(n) == "navigation_expression" && n.NamedChildCount() == 2:
			return walk(n.NamedChild(0)) && walk(n.NamedChild(1))
		}
		return false
	}
	if !walk(nav) {
		return nil
	}
	return segs
}

// noteValueSegments records the names a written path could have: the capitalized
// prefix of the path, longest first, rooted where its first segment resolves.
func (w *astWalker) noteValueSegments(segs []string) {
	n := 0
	for n < len(segs) && isCapitalized(segs[n]) {
		n++
	}
	if n == 0 {
		return
	}
	base := w.typeCandidate(segs[0])
	if base == "" {
		return
	}
	owner := w.currentOwner()
	if owner == noOwner {
		return
	}
	var cands []string
	for k := n; k >= 1; k-- {
		cands = append(cands, base+strings.Join(append([]string{""}, segs[1:k]...), "."))
	}
	notes, _ := w.out[owner].PropAny(propValueRefs).([][]string)
	w.out[owner].SetProp(propValueRefs, append(notes, cands))
}

// resolveValueRefs turns the walker's notes into names edges to the first
// candidate that names a symbol of the repository. A nested type imported by its
// own name (`import pkg.Outer.Inner`) is written "<dir>/Outer.Inner" and declared
// "<dir>.Outer.Inner", so a path segment that names a type is tried as one.
func resolveValueRefs(all []facts.Fact) {
	// A value or a type test names a type or a constant, never a function: a
	// capitalized function of the same name (`fun String(vararg cp: Int)`) is no
	// target for `String.CASE_INSENSITIVE_ORDER`.
	exists := make(map[string]bool, len(all))
	for i := range all {
		if all[i].Kind != facts.KindSymbol {
			continue
		}
		switch all[i].PropString("symbol_kind") {
		case facts.SymbolFunc, facts.SymbolMethod, facts.SymbolGetter:
			continue
		}
		exists[all[i].Name] = true
	}
	resolve := func(c string) string {
		for {
			if exists[c] {
				return c
			}
			slash := strings.LastIndexByte(c, '/')
			if slash < 0 || slash+1 >= len(c) || !isCapitalized(c[slash+1:]) {
				return ""
			}
			c = c[:slash] + "." + c[slash+1:]
		}
	}
	for i := range all {
		f := &all[i]
		notes, ok := f.PropAny(propValueRefs).([][]string)
		if !ok {
			continue
		}
		f.DelProp(propValueRefs)
		for _, cands := range notes {
			for _, c := range cands {
				if t := resolve(c); t != "" {
					if t != f.Name && !slices.ContainsFunc(f.Relations, func(r facts.Relation) bool { return r.Target == t }) {
						f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelNames, Target: t})
					}
					break
				}
			}
		}
	}
}

// noteAnnotationRefs records the types an annotation's arguments name, against the
// current owner: `@ExtendWith(StartStopExtension::class)` registers the extension,
// `@RunWith(Parameterized::class)` the runner. The grammar reads `X::class` as a
// path, so the value positions inside the arguments are all there is to read.
func (w *astWalker) noteAnnotationRefs(holder *sitter.Node) {
	if holder == nil {
		return
	}
	for i := uint(0); i < holder.ChildCount(); i++ {
		if ann := holder.Child(i); kindOf(ann) == "annotation" {
			w.noteAnnotationArgs(ann)
		}
	}
}

// noteAnnotationArgs records the types one annotation's arguments name.
func (w *astWalker) noteAnnotationArgs(ann *sitter.Node) {
	inv := findChildByKind(ann, "constructor_invocation")
	if inv == nil {
		return
	}
	args := findChildByKind(inv, "value_arguments")
	if args == nil {
		return
	}
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		w.noteValueRef(n)
		for j := uint(0); j < n.ChildCount(); j++ {
			walk(n.Child(j))
		}
	}
	walk(args)
}
