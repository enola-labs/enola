package swiftextractor

import (
	"strings"

	sitter "github.com/enola-labs/enola/internal/treesitter"

	"github.com/enola-labs/enola/internal/facts"
)

// A function or method handed over by name rather than called.
//
//	.map(formattedEmailAddress)               // an argument
//	["/projects/pledge/destroy": pledgeDestroy] // a route table
//	label |> settingsLabelStyle               // an operand of a custom operator
//	let autoFill = emailFieldAutoFillStyle    // the right side of a let
//	.observeValues(self.configureBadge)       // a method value
//	.map(DiscoveryViewModel.initialParams)    // a static method value
//
// walkForCalls records call expressions only, so a function used this way had no
// incoming edge and read as dead. Each becomes a calls edge, the way the Go extractor
// records a function value, and its target is listed in value_refs: the function is
// reachable from here, not run here, so the performs_io closure does not follow it.
//
// Resolution is narrow, because a bare name in value position is far more often a
// local, a parameter or a property than a function. A name bound anywhere in the
// enclosing declaration (a parameter, a let or var, a closure parameter, a pattern, a
// nested function) is skipped, and so is a name the code there could mean as a
// variable: a top-level variable or constant of the repository, or a property of the
// type the name is looked up on or of one of its supertypes. What is left binds to the method the enclosing type or one of
// its extensions declares, else to the one top-level function of that
// name the file sees (the caller's module first). A member path binds only to a
// method of the type it is rooted at: self, a type name, or a receiver whose type the
// source states. The type must be the repository's, and the method its own or an
// extension's, never a supertype's: a framework supertype has members the graph
// cannot see. Anything else stays unrecorded: a missing edge beats a wrong one.

// propValueNotes holds the walker's notes to resolveValueRefs. It does not leave
// the extractor.
const propValueNotes = "swift_value_notes"

// propValueRefs lists the calls edges of a function that hand a function over
// rather than call it (the Go extractor's prop of the same name).
const propValueRefs = "value_refs"

// propDelegateCallback marks, for resolveValueRefs alone, a method named after the
// type of its first parameter: `tableView(_ tableView: UITableView, didSelectRowAt:)`.
// That is the Cocoa delegate convention, and the name is also the delegating
// object's property on the framework supertype (UITableViewController.tableView), so
// `self.tableView` is the property. It does not leave the extractor.
const propDelegateCallback = "swift_delegate_callback"

// isDelegateCallback reports a method whose first parameter's type ends in its name
// (`navigationController(_: UINavigationController, …)`).
func isDelegateCallback(fn *sitter.Node, name string, src []byte) bool {
	if name == "" {
		return false
	}
	for i := uint(0); i < fn.ChildCount(); i++ {
		p := fn.Child(i)
		if kindOf(p) != "parameter" {
			continue
		}
		t := statedType(parameterTypeNode(p), src)
		return t != "" && strings.HasSuffix(t, strings.ToUpper(name[:1])+name[1:])
	}
	return false
}

// swiftValueNote is one name a declaration uses as a value.
type swiftValueNote struct {
	typ    string // the type the name is looked up on: the enclosing one for a bare name
	name   string
	dir    string // the caller's module
	member bool   // written as a member path (self.x, Type.x, value.x)
}

// swiftValueFields names, per node kind, the fields whose child stands as a value.
var swiftValueFields = map[string][]string{
	"value_argument":             {"value"},
	"dictionary_literal":         {"value"},
	"array_literal":              {"element"},
	"property_declaration":       {"value"},
	"assignment":                 {"result"},
	"infix_expression":           {"lhs", "rhs"},
	"ternary_expression":         {"if_true", "if_false"},
	"control_transfer_statement": {"result"},
}

// noteValueOperands records the names node holds in a value position.
func (w *astWalker) noteValueOperands(node *sitter.Node) {
	fields, ok := swiftValueFields[kindOf(node)]
	if !ok || w.currentOwner() < 0 {
		return
	}
	for i := uint(0); i < node.ChildCount(); i++ {
		field := node.FieldNameForChild(uint32(i))
		if field == "" || !containsString(fields, field) {
			continue
		}
		w.noteValueOperand(node.Child(i))
	}
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// noteValueOperand records c when it is a bare name or a member path that can
// name a function.
func (w *astWalker) noteValueOperand(c *sitter.Node) {
	if c == nil {
		return
	}
	switch kindOf(c) {
	case "simple_identifier":
		name := nodeText(c, w.src)
		if !isValueName(name) || w.boundInScope(c, name) {
			return
		}
		w.addValueNote(swiftValueNote{typ: lastDotComponent(w.enclosingType()), name: name, dir: w.dir})
	case "navigation_expression":
		target, suffix := c.ChildByFieldName("target"), c.ChildByFieldName("suffix")
		if target == nil || suffix == nil {
			return
		}
		id := findFirstIdentifier(suffix, w.src)
		if id == nil {
			return
		}
		name := nodeText(id, w.src)
		if !isValueName(name) {
			return
		}
		typ := ""
		switch {
		case kindOf(target) == "self_expression":
			typ = lastDotComponent(w.enclosingType())
		case staticReceiver(target, w.src) != "":
			typ = staticReceiver(target, w.src)
		default:
			typ = w.receiverType(c)
		}
		if typ != "" {
			w.addValueNote(swiftValueNote{typ: typ, name: name, dir: w.dir, member: true})
		}
	}
}

// isValueName reports a name that could be a function's: lower case, not a
// keyword-like member, not a builtin.
func isValueName(name string) bool {
	if name == "" || name == "_" || isCapitalized(name) || swiftBuiltins[name] {
		return false
	}
	switch name {
	case "self", "init", "super", "error", "newValue", "oldValue":
		return false
	}
	return true
}

func (w *astWalker) addValueNote(n swiftValueNote) {
	idx := w.currentOwner()
	notes, _ := w.out[idx].PropAny(propValueNotes).([]swiftValueNote)
	if containsNote(notes, n) {
		return
	}
	w.out[idx].SetProp(propValueNotes, append(notes, n))
}

func containsNote(notes []swiftValueNote, n swiftValueNote) bool {
	for _, x := range notes {
		if x == n {
			return true
		}
	}
	return false
}

// swiftTypeBodies are the node kinds whose direct children are member declarations:
// a name bound under one of those children is local to it.
var swiftTypeBodies = map[string]bool{
	"source_file": true, "class_body": true, "protocol_body": true, "enum_class_body": true,
}

// boundInScope reports whether name is bound anywhere in the declaration enclosing
// node: the method, initializer, property or top-level statement it sits in.
// Lexical scope is not followed; a name bound anywhere in the declaration counts,
// which can only lose an edge.
func (w *astWalker) boundInScope(node *sitter.Node, name string) bool {
	root := node
	for p := node.Parent(); p != nil && !swiftTypeBodies[kindOf(p)]; p = p.Parent() {
		root = p
	}
	key := [2]uint{root.StartByte(), root.EndByte()}
	bound, ok := w.boundNames[key]
	if !ok {
		bound = map[string]bool{}
		collectBound(root, root, w.src, bound)
		if w.boundNames == nil {
			w.boundNames = map[[2]uint]map[string]bool{}
		}
		w.boundNames[key] = bound
	}
	return bound[name]
}

// collectBound adds the names n binds: parameters, closure parameters, pattern
// identifiers, if/guard/while bindings and nested function names.
func collectBound(n, root *sitter.Node, src []byte, out map[string]bool) {
	switch kindOf(n) {
	case "parameter", "lambda_parameter":
		for i := uint(0); i < n.ChildCount(); i++ {
			if c := n.Child(i); kindOf(c) == "simple_identifier" {
				out[nodeText(c, src)] = true
			}
		}
	case "pattern":
		addIdentifiers(n, src, out)
		return
	case "function_declaration":
		if n != root {
			if name := n.ChildByFieldName("name"); name != nil {
				out[nodeText(name, src)] = true
			}
		}
	}
	for i := uint(0); i < n.ChildCount(); i++ {
		c := n.Child(i)
		if n.FieldNameForChild(uint32(i)) == "bound_identifier" {
			out[nodeText(c, src)] = true
		}
		collectBound(c, root, src, out)
	}
}

// addIdentifiers adds every simple identifier under n.
func addIdentifiers(n *sitter.Node, src []byte, out map[string]bool) {
	if kindOf(n) == "simple_identifier" {
		out[nodeText(n, src)] = true
		return
	}
	for i := uint(0); i < n.ChildCount(); i++ {
		addIdentifiers(n.Child(i), src, out)
	}
}

// resolveValueRefs turns the walker's notes into calls edges, listed in value_refs,
// against the declarations the calling file sees. A note that names no function
// gets no edge. A file's top-level fact left with no edge is dropped, as the walker
// drops one whose statements call nothing.
func resolveValueRefs(allFacts []facts.Fact, index *swiftIndexes, hier *swiftHierarchies) []facts.Fact {
	callbacks := map[string]bool{}
	for i := range allFacts {
		if b, _ := allFacts[i].PropAny(propDelegateCallback).(bool); b {
			callbacks[allFacts[i].Name] = true
			allFacts[i].DelProp(propDelegateCallback)
		}
	}
	var empty map[int]bool
	var props *swiftProperties
	for i := range allFacts {
		f := &allFacts[i]
		notes, ok := f.PropAny(propValueNotes).([]swiftValueNote)
		if !ok {
			continue
		}
		f.DelProp(propValueNotes)
		if props == nil {
			props = newSwiftProperties(allFacts)
		}
		ix := index.forFile(f.File)
		h := hier.forFile(f.File)
		var refs []string
		for _, n := range notes {
			target := resolveValueNote(n, ix, h, props, f.Name)
			if target == "" || target == f.Name || callbacks[target] || f.HasRelation(facts.RelCalls, target) {
				continue
			}
			f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelCalls, Target: target})
			refs = append(refs, target)
		}
		if len(refs) > 0 {
			f.SetProp(propValueRefs, refs)
		}
		if f.Kind == facts.KindFileRef && len(f.Relations) == 0 {
			if empty == nil {
				empty = map[int]bool{}
			}
			empty[i] = true
		}
	}
	if len(empty) == 0 {
		return allFacts
	}
	out := allFacts[:0]
	for i := range allFacts {
		if !empty[i] {
			out = append(out, allFacts[i])
		}
	}
	return out
}

// resolveValueNote returns the function or method n names, or "".
func resolveValueNote(n swiftValueNote, ix *swiftNameIndex, h *swiftHierarchy, props *swiftProperties, from string) string {
	if props.couldMean(n, h) {
		return ""
	}
	// A method value binds to what the type itself or an extension of it declares.
	// Not to a supertype's: a supertype the repository does not declare (UIKit's
	// UIViewController) has members nobody can see, and `self.view` is its
	// property, not the `view(fromNib:)` a protocol extension adds. Nor on a type
	// the repository does not declare at all, for the same reason.
	if n.typ != "" && len(h.typeNames[n.typ]) > 0 {
		if names := h.methodByType[n.typ][n.name]; len(names) > 0 {
			return nearestName(names, from)
		}
	}
	if n.member {
		return ""
	}
	// File-private functions of one name in several files of a module share one
	// qualified name, so candidates are distinct names.
	var cands []string
	for _, c := range ix.funcs[n.name] {
		if !containsString(cands, c) {
			cands = append(cands, c)
		}
	}
	if len(cands) == 1 {
		return cands[0]
	}
	found := ""
	for _, c := range cands {
		if ownerOf(c) == n.dir {
			if found != "" {
				return ""
			}
			found = c
		}
	}
	return found
}

// swiftProperties indexes the variables and constants of the repository by where a
// bare name could reach them.
type swiftProperties struct {
	globals map[string]bool            // top-level variables and constants
	members map[string]map[string]bool // simple type name -> its property names
}

func newSwiftProperties(allFacts []facts.Fact) *swiftProperties {
	p := &swiftProperties{globals: map[string]bool{}, members: map[string]map[string]bool{}}
	for i := range allFacts {
		f := &allFacts[i]
		if f.Kind != facts.KindSymbol {
			continue
		}
		if sk, _ := f.PropAny("symbol_kind").(string); sk != facts.SymbolVariable && sk != facts.SymbolConstant {
			continue
		}
		short, owner := lastDotComponent(f.Name), ownerOf(f.Name)
		if f.HasRelation(facts.RelDeclares, owner) {
			p.globals[short] = true
			continue
		}
		t := lastDotComponent(owner)
		if p.members[t] == nil {
			p.members[t] = map[string]bool{}
		}
		p.members[t][short] = true
	}
	return p
}

// couldMean reports whether the name n notes could be a variable: a property of
// its type or a supertype, or for a bare name a top-level variable.
func (p *swiftProperties) couldMean(n swiftValueNote, h *swiftHierarchy) bool {
	if !n.member && p.globals[n.name] {
		return true
	}
	if n.typ == "" {
		return false
	}
	seen := map[string]bool{}
	queue := []string{n.typ}
	for len(queue) > 0 {
		t := queue[0]
		queue = queue[1:]
		if seen[t] {
			continue
		}
		seen[t] = true
		if p.members[t][n.name] {
			return true
		}
		queue = append(queue, h.typeSupers[t]...)
	}
	return false
}

// valueRefSet returns the targets a fact only hands over.
func valueRefSet(f *facts.Fact) map[string]bool {
	refs, _ := f.PropAny(propValueRefs).([]string)
	if len(refs) == 0 {
		return nil
	}
	out := make(map[string]bool, len(refs))
	for _, r := range refs {
		out[r] = true
	}
	return out
}
