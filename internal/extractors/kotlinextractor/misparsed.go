package kotlinextractor

import (
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"

	"github.com/enola-labs/enola/internal/facts"
)

// Declarations the grammar misreads as expressions.
//
// Two or more annotations stacked on a top-level declaration, with another
// top-level declaration after it, send the grammar down its script branch: the
// annotations become annotated expressions, and what follows them is read as one.
//
//	@EntryPoint
//	@InstallIn(SingletonComponent::class)
//	interface WorkerEntryPoint { fun factory(): Factory }   // infix: interface WorkerEntryPoint {…}
//
//	@Module @InstallIn(S::class)
//	abstract class DataModule { … }                          // infix: abstract class DataModule {…}
//
//	@Module @InstallIn(S::class)
//	object NetworkModule { … }                               // kept, its annotations split off
//
// A body-less `annotation class` under annotations is misread the same way. The
// first three lose the class: no symbol, and its members, still declarations
// inside the lambda the body became, belonged to nothing. The last keeps the
// declaration and loses its annotations, and with them its Hilt, Room or entry-point
// role. Each is recovered here from the shape the grammar left.

// kotlinDeclModifiers are the modifier words that can stand before `class` in the
// misread shape.
var kotlinDeclModifiers = map[string]bool{
	"abstract": true, "open": true, "data": true, "sealed": true, "enum": true,
	"inner": true, "value": true, "annotation": true, "private": true,
	"internal": true, "public": true, "protected": true, "final": true,
}

// pendingAnnotations are annotations the grammar split off a declaration, for the
// next declaration to take.
type pendingAnnotations struct {
	names []string
	nodes []*sitter.Node // annotations, and the arguments the grammar split off them
}

// takePendingAnnotations hands the split-off annotations to the declaration being
// read, once.
func (w *astWalker) takePendingAnnotations() *pendingAnnotations {
	p := w.pending
	w.pending = nil
	return p
}

// handleMisparsedDeclaration reads a top-level annotated expression: a declaration
// the grammar misread, or annotations split off the declaration after it.
func (w *astWalker) handleMisparsedDeclaration(node *sitter.Node) {
	var annNodes, argNodes []*sitter.Node
	var inner *sitter.Node
	for cur := node; cur != nil && kindOf(cur) == "annotated_expression"; {
		var next *sitter.Node
		for i := uint(0); i < cur.NamedChildCount(); i++ {
			switch c := cur.NamedChild(i); kindOf(c) {
			case "annotation":
				annNodes = append(annNodes, c)
			case "annotated_expression":
				next = c
			default:
				inner = c
			}
		}
		cur = next
	}
	var names []string
	for _, a := range annNodes {
		names = append(names, kotlinAnnotationName(a, w.src))
	}
	if inner != nil && kindOf(inner) == "infix_expression" && inner.NamedChildCount() == 3 {
		if w.recoverInfixDeclaration(inner, names, annNodes) {
			return
		}
	}
	// `@B(1)` before a declaration the grammar kept: B's arguments came out as an
	// expression of their own.
	if inner != nil && kindOf(inner) == "parenthesized_expression" {
		argNodes = append(argNodes, inner)
	}
	w.pending = &pendingAnnotations{names: names, nodes: append(annNodes, argNodes...)}
}

// recoverInfixDeclaration emits the class or interface an infix expression stands
// for, and reports whether it was one.
func (w *astWalker) recoverInfixDeclaration(inner *sitter.Node, names []string, annNodes []*sitter.Node) bool {
	a, b, c := inner.NamedChild(0), inner.NamedChild(1), inner.NamedChild(2)
	if !kotlinIsIdentifier(a) || !kotlinIsIdentifier(b) {
		return false
	}
	first, second := nodeText(a, w.src), nodeText(b, w.src)
	keyword, name, modifiers := "", "", ""
	var body *sitter.Node
	var rest *sitter.Node
	switch {
	case (first == "class" || first == "interface") && kindOf(c) == "lambda_literal":
		// `interface WorkerEntryPoint { … }`: the name is the infix operator.
		keyword, name, body = first, second, c
	case (first == "class" || first == "interface") && kindOf(c) == "annotated_expression":
		// Body-less `class TargetA` under annotations, followed by the next
		// annotated declaration: the rest of the chain is the right operand.
		keyword, name, rest = first, second, c
	case kotlinDeclModifiers[first] && (second == "class" || second == "interface"):
		keyword, modifiers = second, first
		switch kindOf(c) {
		case "call_expression": // `abstract class DataModule { … }`
			callee := firstNamedChild(c)
			if callee == nil || !kotlinIsIdentifier(callee) {
				return false
			}
			name = nodeText(callee, w.src)
			if al := findChildByKind(c, "annotated_lambda"); al != nil {
				body = findChildByKind(al, "lambda_literal")
			}
		case "identifier", "simple_identifier": // `annotation class Scope`
			name = nodeText(c, w.src)
		default:
			return false
		}
	default:
		return false
	}
	if name == "" || !isCapitalized(name) {
		return false
	}
	w.emitRecoveredClass(inner, keyword, name, modifiers, names, annNodes, body)
	if rest != nil {
		w.handleMisparsedDeclaration(rest)
	}
	return true
}

// emitRecoveredClass declares a recovered class as handleClassDeclaration would,
// and reads its body and annotations the same way.
func (w *astWalker) emitRecoveredClass(at *sitter.Node, keyword, name, modifiers string, annotations []string, annNodes []*sitter.Node, body *sitter.Node) {
	kind := facts.SymbolClass
	if keyword == "interface" {
		kind = facts.SymbolInterface
	}
	f := facts.Fact{
		Kind: facts.KindSymbol,
		Name: w.dir + "." + w.qualify(name),
		File: w.relFile,
		Line: int(at.StartPosition().Row) + 1,
		Props: map[string]any{
			"symbol_kind": kind,
			"exported":    !privateOrInternalRe.MatchString(modifiers),
			"language":    "kotlin",
		},
		Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: w.dir}},
	}
	switch modifiers {
	case "data":
		f.SetProp("data_class", true)
	case "sealed":
		f.SetProp("sealed", true)
		f.SetProp("abstract", true)
	case "enum":
		f.SetProp("enum", true)
	case "abstract":
		f.SetProp("abstract", true)
	case "annotation":
		f.SetProp("annotation_class", true)
	}
	if w.isAndroid {
		addAndroidProps(&f, name, annotations, "")
	}
	w.out = append(w.out, f)
	w.pushOwner(len(w.out) - 1)
	for _, a := range annNodes {
		w.noteAnnotationArgs(a)
	}
	w.pushType(name, collectMethodNames(body, w.src))
	if body != nil {
		into := w.memberTypes[len(w.memberTypes)-1]
		for i := uint(0); i < body.NamedChildCount(); i++ {
			if m := body.NamedChild(i); kindOf(m) == "property_declaration" {
				w.bindProperty(into, m)
			}
		}
		w.walkForCalls(body)
	}
	w.popType()
	w.popOwner()
}

// kotlinAnnotationName is the simple name of one annotation node.
func kotlinAnnotationName(ann *sitter.Node, src []byte) string {
	var typ *sitter.Node
	if inv := findChildByKind(ann, "constructor_invocation"); inv != nil {
		typ = firstTypeChild(inv)
	} else {
		typ = findChildByKind(ann, "user_type")
	}
	if typ == nil {
		return ""
	}
	t := nodeText(typ, src)
	if i := strings.LastIndexByte(t, '.'); i >= 0 {
		t = t[i+1:]
	}
	return t
}

// noteSplitAnnotations records what split-off annotations name, against the
// declaration that took them.
func (w *astWalker) noteSplitAnnotations(p *pendingAnnotations) {
	if p == nil {
		return
	}
	for _, n := range p.nodes {
		if kindOf(n) == "annotation" {
			w.noteAnnotationArgs(n)
			continue
		}
		var walk func(x *sitter.Node)
		walk = func(x *sitter.Node) {
			w.noteValueRef(x)
			for j := uint(0); j < x.ChildCount(); j++ {
				walk(x.Child(j))
			}
		}
		walk(n)
	}
}
