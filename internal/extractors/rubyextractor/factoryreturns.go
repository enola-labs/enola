package rubyextractor

import (
	"strings"
	"unicode"

	sitter "github.com/tree-sitter/go-tree-sitter"

	"github.com/enola-labs/enola/internal/facts"
)

// What a variable holds.
//
//	def sync
//	  engine = Engine.new          # engine is an Engine
//	  engine.run                   # Engine#run
//	end
//
// Ruby declares no types, so a call on a variable was recorded by its spelling
// (`engine.run`) or, with no arguments, as a name only, and a flow through the method
// ended there. The walker now gives a local or instance variable a type token from
// what it is assigned:
//
//	Engine     a constant, looked up from the caller's namespace
//	@engine    an instance variable assigned elsewhere in the class
//
// and a call on a typed receiver records "<token>#<method>" with the target the call
// had before (propTypedCalls). Once every class is known, resolveTypedCalls turns the
// token into a class and, only where the class or an ancestor declares the method,
// adds the edge and drops the unresolved spelling. A token that names no class, or a
// method no class declares, leaves the method exactly as it was.
//
// A variable assigned two different types in one method has none, and so does an
// instance variable assigned two different types anywhere in its class. A block
// parameter shadows a local of the same name and is never typed.

const (
	// propTypedCalls holds, on a method, "<token>#<method>\x00<fallback>" for each call
	// on a typed receiver. It does not leave the extractor.
	propTypedCalls = "rb_typed_calls"
	// propIvarTypes holds, on an instance method, "@name=<token>" for each instance
	// variable it assigns, with an empty token for a value of no known type. It does
	// not leave the extractor.
	propIvarTypes = "rb_ivar_types"
	// tokNil is the token of `nil`, which neither types a variable nor untypes it.
	tokNil = "\x00nil"
)

// beginTyping starts the variable types of a method body.
func (w *rubyWalker) beginTyping(method *sitter.Node) {
	w.types = map[string]string{}
	w.untyped = map[string]bool{}
	collectBlockParams(method.ChildByFieldName("body"), w.src, w.untyped)
}

func (w *rubyWalker) endTyping() {
	w.types, w.untyped = nil, nil
}

// noteAssignment types the variable an assignment writes.
func (w *rubyWalker) noteAssignment(node *sitter.Node, ownerIdx int) {
	if w.types == nil {
		return
	}
	left := node.ChildByFieldName("left")
	if left == nil {
		return
	}
	switch kindOf(left) {
	case "identifier", "instance_variable":
	case "left_assignment_list":
		names := map[string]bool{}
		collectIdentifiers(left, w.src, names)
		for n := range names {
			w.types[n] = ""
		}
		return
	default:
		return
	}
	name := rubyText(left, w.src)
	tok := ""
	if node.Kind() == "assignment" || rubyText(node.ChildByFieldName("operator"), w.src) == "||=" {
		tok = w.valueToken(node.ChildByFieldName("right"))
	}
	if tok == tokNil {
		return
	}
	if w.untyped[name] {
		tok = ""
	}
	if prev, seen := w.types[name]; seen && prev != tok {
		tok = ""
	}
	w.types[name] = tok
	if kindOf(left) == "instance_variable" {
		appendPropOnce(&w.out[ownerIdx], propIvarTypes, name+"="+tok)
	}
}

// valueToken returns the type token of an assigned value, tokNil for `nil`, and ""
// for a value of no known type.
func (w *rubyWalker) valueToken(n *sitter.Node) string {
	if n == nil {
		return ""
	}
	switch kindOf(n) {
	case "parenthesized_statements":
		if n.NamedChildCount() == 1 {
			return w.valueToken(n.NamedChild(0))
		}
	case "nil":
		return tokNil
	case "call":
		recv, meth := n.ChildByFieldName("receiver"), n.ChildByFieldName("method")
		if recv != nil && meth != nil && rubyText(meth, w.src) == "new" {
			switch kindOf(recv) {
			case "constant", "scope_resolution":
				return stripLeadingColons(rubyText(recv, w.src))
			}
		}
	case "identifier":
		return w.types[rubyText(n, w.src)]
	case "instance_variable":
		name := rubyText(n, w.src)
		if t, seen := w.types[name]; seen {
			return t
		}
		return name
	}
	return ""
}

// receiverToken returns the type token of a call's receiver, "" when it has none.
func (w *rubyWalker) receiverToken(recv *sitter.Node, locals map[string]bool) string {
	switch kindOf(recv) {
	case "parenthesized_statements":
		if recv.NamedChildCount() == 1 {
			return w.receiverToken(recv.NamedChild(0), locals)
		}
	case "identifier":
		if name := rubyText(recv, w.src); locals[name] {
			return w.types[name]
		}
	case "instance_variable":
		name := rubyText(recv, w.src)
		if t, seen := w.types[name]; seen {
			return t
		}
		return name
	}
	return ""
}

// noteTypedCall records a call on a typed receiver for resolveTypedCalls.
func (w *rubyWalker) noteTypedCall(node *sitter.Node, ownerIdx int, locals map[string]bool) {
	if w.types == nil {
		return
	}
	recv, meth := node.ChildByFieldName("receiver"), node.ChildByFieldName("method")
	if recv == nil || meth == nil || kindOf(meth) != "identifier" {
		return
	}
	name := rubyText(meth, w.src)
	if name == "new" {
		return
	}
	tok := w.receiverToken(recv, locals)
	if tok == "" {
		return
	}
	// `engine.run(x)` was recorded as written. That spelling names nothing and is
	// replaced when the typed call resolves; a bare name is shared with other calls
	// and stays.
	fallback := w.callTarget(node)
	if fallback == "" || strings.Contains(fallback, "#") || !strings.Contains(fallback, ".") || !startsLower(fallback) {
		fallback = ""
	}
	appendPropOnce(&w.out[ownerIdx], propTypedCalls, tok+"#"+name+"\x00"+fallback)
}

func startsLower(s string) bool {
	for _, r := range s {
		return r == '_' || unicode.IsLower(r)
	}
	return false
}

func appendPropOnce(f *facts.Fact, key, entry string) {
	list, _ := f.PropAny(key).([]string)
	for _, e := range list {
		if e == entry {
			return
		}
	}
	f.SetProp(key, append(list, entry))
}

// typeResolver turns type tokens into the classes they name.
type typeResolver struct {
	ix *handlerIndex
	// ivars maps a class to the token of each instance variable its instance methods
	// assign, "" for one assigned two types or a value of no known type.
	ivars map[string]map[string]string
}

// resolveTypedCalls adds the edge of every call on a typed receiver whose class
// declares the method, and removes both temporary props.
func resolveTypedCalls(allFacts []facts.Fact, ix *handlerIndex) {
	r := typeResolver{ix: ix, ivars: map[string]map[string]string{}}
	for i := range allFacts {
		f := &allFacts[i]
		list, _ := f.PropAny(propIvarTypes).([]string)
		if list == nil {
			continue
		}
		delete(f.Props, propIvarTypes)
		owner, sep := methodOwner(f.Name)
		if owner == "" || sep != "#" {
			continue
		}
		m := r.ivars[owner]
		if m == nil {
			m = map[string]string{}
			r.ivars[owner] = m
		}
		for _, e := range list {
			name, tok, _ := strings.Cut(e, "=")
			if prev, seen := m[name]; seen && prev != tok {
				tok = ""
			}
			m[name] = tok
		}
	}
	for i := range allFacts {
		f := &allFacts[i]
		list, _ := f.PropAny(propTypedCalls).([]string)
		if list == nil {
			continue
		}
		delete(f.Props, propTypedCalls)
		owner, sep := methodOwner(f.Name)
		for _, e := range list {
			typed, fallback, _ := strings.Cut(e, "\x00")
			at := strings.LastIndexByte(typed, '#')
			class := r.typeOf(typed[:at], owner, sep, 0)
			if class == "" {
				continue
			}
			fn := ix.member(class, "#", typed[at+1:])
			if fn == "" {
				continue
			}
			f.Relations = replaceCall(f.Relations, fallback, fn)
		}
	}
}

// replaceCall adds a calls edge to fn and removes the one to fallback, if any.
func replaceCall(rels []facts.Relation, fallback, fn string) []facts.Relation {
	out := rels[:0]
	has := false
	for _, rel := range rels {
		if rel.Kind == facts.RelCalls {
			if fallback != "" && rel.Target == fallback && fallback != fn {
				continue
			}
			if rel.Target == fn {
				has = true
			}
		}
		out = append(out, rel)
	}
	if !has {
		out = append(out, facts.Relation{Kind: facts.RelCalls, Target: fn})
	}
	return out
}

// typeOf returns the class a token names from a method of owner (sep "#" for an
// instance method, "." for a singleton one), or "".
func (r typeResolver) typeOf(tok, owner, sep string, depth int) string {
	if tok == "" || depth > 8 {
		return ""
	}
	if strings.HasPrefix(tok, "@") {
		if sep != "#" {
			return ""
		}
		class := owner
		for range maxAncestors {
			if t, seen := r.ivars[class][tok]; seen {
				return r.typeOf(t, class, "#", depth+1)
			}
			super, ok := r.ix.superclass[class]
			if !ok {
				return ""
			}
			next := r.ix.lexical(super, class)
			if next == "" || next == class {
				return ""
			}
			class = next
		}
		return ""
	}
	return r.ix.lexical(tok, owner)
}
