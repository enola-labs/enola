package swiftextractor

import (
	sitter "github.com/enola-labs/enola/internal/treesitter"
)

// Where the Objective-C runtime enters Swift code.
//
// UIKit sends target-action, gesture, notification and timer messages by
// selector, and Interface Builder connects a storyboard or nib to a method by its
// selector. None of those is a call in source. Two pieces of evidence are read:
//
//   - #selector(self.m), #selector(Type.m), #selector(m(_:)) names the method a
//     message will be sent to. It is a use of that method, recorded as a method
//     value is (funcvalues.go): a calls edge listed in value_refs, bound the same
//     narrow way. #selector(getter:)/(setter:) names a property and is skipped.
//   - @IBAction and @IBSegueAction exist only for Interface Builder to connect, and
//     @objc(name:) gives a method the runtime name a string selector or an
//     Objective-C caller uses. Such a method is framework_registered: the runtime
//     invokes it, nothing calls it by name.
//
// A bare @objc is not evidence. It makes a method reachable by selector, and a
// method nothing selects is as dead as any other, so it is judged by its
// #selector references alone.

// objcEntryAttributes are the attributes whose sole purpose is to be entered by
// Interface Builder.
var objcEntryAttributes = map[string]bool{"IBAction": true, "IBSegueAction": true}

// isObjCRuntimeEntry reports a declaration whose modifiers carry @IBAction,
// @IBSegueAction or @objc with an explicit runtime name.
func isObjCRuntimeEntry(modifiers *sitter.Node, src []byte) bool {
	if modifiers == nil {
		return false
	}
	for i := uint(0); i < modifiers.ChildCount(); i++ {
		attr := modifiers.Child(i)
		if kindOf(attr) != "attribute" {
			continue
		}
		name := simpleTypeName(firstNamedChild(attr), src)
		if objcEntryAttributes[name] {
			return true
		}
		// @objc(doThing:) parses as the attribute name followed by the selector's
		// identifiers; a bare @objc has the name alone.
		if name == "objc" && attr.NamedChildCount() > 1 {
			return true
		}
	}
	return false
}

// selectorOperand returns the expression a #selector names as a method, or nil
// for a getter or setter selector. A selector spelled with its argument labels,
// m(_:with:), parses as a call whose arguments are labels only; the method is its
// callee.
func selectorOperand(sel *sitter.Node, src []byte) *sitter.Node {
	var expr *sitter.Node
	for i := uint(0); i < sel.ChildCount(); i++ {
		c := sel.Child(i)
		switch {
		case !c.IsNamed():
			if t := nodeText(c, src); t == "getter:" || t == "setter:" {
				return nil
			}
		case expr == nil:
			expr = c
		}
	}
	if kindOf(expr) == "call_expression" && labelsOnly(expr) {
		return expr.NamedChild(0)
	}
	return expr
}

// labelsOnly reports a call whose every argument is a bare label (`_:`, `with:`),
// the form a selector names an overload by.
func labelsOnly(call *sitter.Node) bool {
	suffix := findChildByKind(call, "call_suffix")
	args := findChildByKind(suffix, "value_arguments")
	if args == nil {
		return false
	}
	for i := uint(0); i < args.NamedChildCount(); i++ {
		if args.NamedChild(i).ChildByFieldName("value") != nil {
			return false
		}
	}
	return true
}
