package rubyextractor

import (
	"regexp"
	"strings"
	"unicode"

	sitter "github.com/enola-labs/enola/internal/treesitter"

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
//	Engine        a constant, looked up from the caller's namespace
//	@engine       an instance variable assigned elsewhere in the class
//	User.find()   what a class method of a constant returns: for a finder on a model,
//	              an instance of the model
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
	// propReturnType holds, on a method, the type token of what it returns. It does
	// not leave the extractor.
	propReturnType = "rb_return_type"
	// propDeclaredReturn holds, on a method, the type token its YARD `@return` tag
	// names. It does not leave the extractor.
	propDeclaredReturn = "rb_declared_return"
	// tokNil is the token of `nil`, which neither types a variable nor untypes it.
	tokNil = "\x00nil"
	// tokSelf is an instance of the class the method is declared in.
	tokSelf = "self"
	// callMark follows a call whose result a token is.
	callMark = "()"
)

// modelFinders are the class methods of an ActiveRecord or Sequel model that return
// one instance of it. `where`, `all` and `count` return something else.
var modelFinders = map[string]bool{
	"find": true, "find_by": true, "find_by!": true, "find_sole_by": true, "sole": true,
	"first": true, "first!": true, "last": true, "last!": true, "take": true, "take!": true,
	"create": true, "create!": true, "find_or_create_by": true, "find_or_create_by!": true,
	"find_or_initialize_by": true, "create_or_find_by": true, "create_or_find_by!": true,
	"with_pk": true, "with_pk!": true,
}

// beginTyping starts the variable types of a method body.
func (w *rubyWalker) beginTyping(method *sitter.Node, isClassMethod bool, locals map[string]bool) {
	w.types = map[string]string{}
	w.untyped = map[string]bool{}
	w.locals = locals
	w.inSingle = isClassMethod
	w.returns = nil
	collectBlockParams(method.ChildByFieldName("body"), w.src, w.untyped)
}

func (w *rubyWalker) endTyping() {
	w.types, w.untyped, w.locals, w.returns = nil, nil, nil, nil
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

// valueToken returns the type token of a value, tokNil for `nil`, and "" for a value
// of no known type.
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
	case "self":
		// In a class method self is the class, which is no instance of it.
		if !w.inSingle {
			return tokSelf
		}
	case "assignment", "operator_assignment":
		// `@engine ||= Engine.new` is the value it stores.
		if n.Kind() == "assignment" || rubyText(n.ChildByFieldName("operator"), w.src) == "||=" {
			return w.valueToken(n.ChildByFieldName("right"))
		}
	case "identifier":
		name := rubyText(n, w.src)
		if w.locals[name] {
			return w.types[name]
		}
		if name == "new" {
			// `new` with no parentheses parses as an identifier.
			if w.inSingle {
				return tokSelf
			}
			return ""
		}
		// An identifier that is no local is a call of the method of that name.
		if !rubyNonCalls[name] {
			return "#" + name + callMark
		}
	case "instance_variable":
		name := rubyText(n, w.src)
		if t, seen := w.types[name]; seen {
			return t
		}
		return name
	case "call":
		return w.callToken(n)
	}
	return ""
}

// callToken returns the type token of what a call returns.
func (w *rubyWalker) callToken(n *sitter.Node) string {
	recv, meth := n.ChildByFieldName("receiver"), n.ChildByFieldName("method")
	if meth == nil || kindOf(meth) != "identifier" {
		return ""
	}
	name := rubyText(meth, w.src)
	if recv == nil {
		switch {
		case name == "new":
			// `new(...)` in a class method builds an instance of the class.
			if w.inSingle {
				return tokSelf
			}
			return ""
		case w.locals[name] || rubyNonCalls[name]:
			return ""
		}
		return "#" + name + callMark
	}
	switch kindOf(recv) {
	case "constant", "scope_resolution":
		class := stripLeadingColons(rubyText(recv, w.src))
		if name == "new" {
			return class
		}
		return class + "." + name + callMark
	case "self":
		if name == "new" {
			if w.inSingle {
				return tokSelf
			}
			return ""
		}
		return "#" + name + callMark
	}
	rt := w.valueToken(recv)
	switch {
	case rt == "" || rt == tokNil || name == "new":
		return ""
	case name == "tap":
		return rt
	}
	return rt + "#" + name + callMark
}

// receiverToken returns the type token of a call's receiver, "" when it has none.
func (w *rubyWalker) receiverToken(recv *sitter.Node) string {
	if t := w.valueToken(recv); t != tokNil {
		return t
	}
	return ""
}

// noteReturn records the type of what a `return` returns. One in a lambda returns
// from the lambda.
func (w *rubyWalker) noteReturn(node *sitter.Node) {
	if w.types == nil {
		return
	}
	for p := node.Parent(); p != nil; p = p.Parent() {
		k := kindOf(p)
		if k == "lambda" {
			return
		}
		if k == "method" || k == "singleton_method" {
			break
		}
	}
	tok := tokNil
	if node.NamedChildCount() > 0 {
		tok = ""
		if args := node.NamedChild(0); kindOf(args) == "argument_list" && args.NamedChildCount() == 1 {
			tok = w.valueToken(args.NamedChild(0))
		}
	}
	w.returns = append(w.returns, tok)
}

// recordReturnType writes onto a method the type token of what it returns: the one
// type of its last expression and every `return`, nil aside. A body with a rescue,
// ensure or else clause, or ending in a branch, records none.
func (w *rubyWalker) recordReturnType(method *sitter.Node, ownerIdx int) {
	if typ := w.declaredReturn(method); typ != "" {
		w.out[ownerIdx].SetProp(propDeclaredReturn, typ)
	}
	body := method.ChildByFieldName("body")
	if body == nil {
		return
	}
	last := body
	if kindOf(body) == "body_statement" {
		last = nil
		for i := uint(0); i < body.NamedChildCount(); i++ {
			c := body.NamedChild(i)
			switch kindOf(c) {
			case "rescue", "else", "ensure":
				return
			case "comment":
				continue
			}
			last = c
		}
	}
	toks := w.returns
	if last != nil && kindOf(last) != "return" {
		switch kindOf(last) {
		case "if", "unless", "case", "begin", "while", "until", "for", "if_modifier", "unless_modifier", "conditional":
			return
		}
		toks = append(toks, w.valueToken(last))
	}
	typ := ""
	for _, t := range toks {
		switch {
		case t == tokNil:
		case t == "" || (typ != "" && t != typ):
			return
		default:
			typ = t
		}
	}
	if typ != "" {
		w.out[ownerIdx].SetProp(propReturnType, typ)
	}
}

// noteTypedCall records a call on a typed receiver for resolveTypedCalls.
func (w *rubyWalker) noteTypedCall(node *sitter.Node, ownerIdx int) {
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
	tok := w.receiverToken(recv)
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
	// models holds the ActiveRecord and Sequel models.
	models map[string]bool
	// returns maps a method to the type token of what it returns, read from the
	// method's own class. readers holds the attr_reader and attr_accessor methods.
	returns  map[string]string
	declared map[string]string
	readers  map[string]bool
	// ivars maps a class to the token of each instance variable its instance methods
	// assign, "" for one assigned two types or a value of no known type.
	ivars map[string]map[string]string
}

// resolveTypedCalls adds the edge of every call on a typed receiver whose class
// declares the method, and removes both temporary props.
func resolveTypedCalls(allFacts []facts.Fact, ix *handlerIndex) {
	r := typeResolver{ix: ix, models: map[string]bool{}, ivars: map[string]map[string]string{},
		returns: map[string]string{}, declared: map[string]string{}, readers: map[string]bool{}}
	for i := range allFacts {
		f := &allFacts[i]
		if f.Kind == facts.KindStorage && f.PropString("storage_kind") == "model" {
			r.models[f.Name] = true
		}
		if f.Kind == facts.KindSymbol && f.PropString("symbol_kind") == facts.SymbolVariable {
			if k := f.PropString("attr_kind"); k == "reader" || k == "accessor" {
				r.readers[f.Name] = true
			}
		}
		if t, ok := f.PropAny(propDeclaredReturn).(string); ok {
			delete(f.Props, propDeclaredReturn)
			r.declared[f.Name] = t
		}
		if t, ok := f.PropAny(propReturnType).(string); ok {
			delete(f.Props, propReturnType)
			if _, seen := r.returns[f.Name]; seen && r.returns[f.Name] != t {
				t = "" // reopened with another body
			}
			r.returns[f.Name] = t
		}
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
	if tok == tokSelf {
		return owner
	}
	if strings.HasSuffix(tok, callMark) {
		path := strings.TrimSuffix(tok, callMark)
		at := strings.LastIndexAny(path, "#.")
		if at < 0 {
			return ""
		}
		recv, name := path[:at], path[at+1:]
		var fn string
		switch {
		case path[at] == '#' && recv == "":
			// `make` / `self.make`: a method of the caller's own kind.
			if owner == "" {
				return ""
			}
			fn = r.ix.member(owner, sep, name)
		case path[at] == '#':
			class := r.typeOf(recv, owner, sep, depth+1)
			if class == "" {
				return ""
			}
			fn = r.ix.member(class, "#", name)
		default:
			class := r.ix.lexical(recv, owner)
			if class == "" {
				return ""
			}
			if modelFinders[name] && r.isModel(class) {
				return class
			}
			fn = r.ix.member(class, ".", name)
		}
		return r.returnOf(fn, depth+1)
	}
	return r.ix.lexical(tok, owner)
}

// returnOf returns the class of what method fn returns, or "". A reader returns its
// instance variable.
func (r typeResolver) returnOf(fn string, depth int) string {
	if fn == "" {
		return ""
	}
	owner, sep := methodOwner(fn)
	if r.readers[fn] {
		return r.typeOf("@"+fn[strings.LastIndexByte(fn, '#')+1:], owner, "#", depth)
	}
	if d := r.declared[fn]; d != "" {
		if class := r.typeOf(d, owner, sep, depth); class != "" {
			return class
		}
	}
	return r.typeOf(r.returns[fn], owner, sep, depth)
}

// isModel reports whether class is a model or inherits from one, as an STI subclass
// does.
func (r typeResolver) isModel(class string) bool {
	for range maxAncestors {
		if r.models[class] {
			return true
		}
		super, ok := r.ix.superclass[class]
		if !ok {
			return false
		}
		next := r.ix.lexical(super, class)
		if next == "" || next == class {
			return false
		}
		class = next
	}
	return false
}

// constantPath matches a type YARD names that a constant can resolve: `Engine`,
// `Ci::Pipeline`, `::Engine`. A generic (`Array<Engine>`) or duck type does not.
var constantPath = regexp.MustCompile(`^(::)?[A-Z][A-Za-z0-9_]*(::[A-Z][A-Za-z0-9_]*)*$`)

// declaredReturn returns the type token of the type a method's YARD `@return [T]`
// tag declares, `nil` aside: `[Engine]`, `[Engine, nil]`, and `[self]` in an instance
// method. Where it names a class of the repository it takes precedence over what the
// body is inferred to return; a core or library type (`[Hash]`) leaves the inferred
// one, since tags are often loose (`@return [Hash]` on a method returning a class
// that behaves like one).
func (w *rubyWalker) declaredReturn(method *sitter.Node) string {
	for _, l := range rubyDocLines(method, w.src) {
		rest, ok := strings.CutPrefix(l, "@return")
		if !ok {
			continue
		}
		rest = strings.TrimSpace(rest)
		if !strings.HasPrefix(rest, "[") {
			return ""
		}
		end := strings.IndexByte(rest, ']')
		if end < 0 {
			return ""
		}
		typ := ""
		for _, t := range strings.Split(rest[1:end], ",") {
			switch t = strings.TrimSpace(t); t {
			case "nil", "NilClass", "void", "":
				continue
			}
			if typ != "" {
				return ""
			}
			typ = t
		}
		switch {
		case typ == "self":
			if w.inSingle {
				return ""
			}
			return tokSelf
		case constantPath.MatchString(typ):
			return stripLeadingColons(typ)
		}
		return ""
	}
	return ""
}
