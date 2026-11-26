package pythonextractor

import (
	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/enola-labs/enola/internal/treesitter"
)

// A method read through self or cls without a call is handed on as a value:
// `return self.test_func`, `register(callback=self.on_done)`. Its call edge is
// missing, so a method used only that way read as dead. Such a read is recorded as
// `names` when the class declares the method, and otherwise left to the
// inherited-calls binder (PropMemberNames), which looks it up through the class's
// bases and adds an edge only when it lands on a method. Nothing binds by the
// name alone: a same-named method of another class is never reached.

// noteSelfMemberRead records `self.<attr>` or `cls.<attr>` read in a method's body.
func (w *pyWalker) noteSelfMemberRead(node *sitter.Node) {
	owner := w.currentOwner()
	if owner == nil || w.selfRebound > 0 || w.enclosingType() == "" || owner.PropString("symbol_kind") != facts.SymbolMethod {
		return
	}
	obj, attr := node.ChildByFieldName("object"), node.ChildByFieldName("attribute")
	if obj == nil || attr == nil || kindOf(obj) != "identifier" {
		return
	}
	if o := pyText(obj, w.src); o != "self" && o != "cls" {
		return
	}
	if !pyReadNotCalled(node) {
		return
	}
	name := pyText(attr, w.src)
	if !w.currentMethods()[name] {
		owner.AddPropStrings(facts.PropMemberNames, name)
		return
	}
	target := w.module + "." + w.enclosingType() + "." + name
	if target == owner.Name || owner.HasRelation(facts.RelNames, target) {
		return
	}
	owner.Relations = append(owner.Relations, facts.Relation{Kind: facts.RelNames, Target: target})
}

// pyReadNotCalled reports an attribute that is read and not called: neither the
// callee of a call nor a target an assignment or del writes.
func pyReadNotCalled(node *sitter.Node) bool {
	cur, p := node, node.Parent()
	for p != nil && pyGroupingKinds[kindOf(p)] {
		cur, p = p, p.Parent()
	}
	if p == nil {
		return true
	}
	same := func(field string) bool {
		c := p.ChildByFieldName(field)
		return c != nil && c.StartByte() == cur.StartByte() && c.EndByte() == cur.EndByte()
	}
	switch kindOf(p) {
	case "call":
		return !same("function")
	case "assignment", "augmented_assignment":
		return !same("left")
	case "for_statement", "for_in_clause":
		return !same("left")
	case "delete_statement":
		return false
	}
	return true
}

// pyGroupingKinds are the nodes between an attribute and the statement using it
// that do not change whether it is read or written.
var pyGroupingKinds = map[string]bool{
	"pattern_list": true, "tuple_pattern": true, "list_pattern": true, "parenthesized_expression": true,
	"tuple": true, "list": true, "expression_list": true,
}

// pyRebindsSelf reports a nested definition in which self or cls names something
// else: a class, or a function taking self or cls as a parameter.
func pyRebindsSelf(def *sitter.Node, src []byte) bool {
	switch kindOf(def) {
	case "class_definition":
		return true
	case "function_definition":
		bound := map[string]bool{}
		pyParamBoundNames(def.ChildByFieldName("parameters"), src, bound)
		return bound["self"] || bound["cls"]
	}
	return false
}
