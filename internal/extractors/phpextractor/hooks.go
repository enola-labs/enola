package phpextractor

import (
	"strings"

	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/tree-sitter/go-tree-sitter"
	php "github.com/tree-sitter/tree-sitter-php/bindings/go"
)

// hookMethods maps a WordPress hook function to the route "method" it produces.
// add_action/add_filter register a handler against a hook; do_action/apply_filters
// are the hook points that fire it; register_rest_route declares a REST endpoint.
var hookMethods = map[string]string{
	"add_action":                 "ACTION",
	"add_filter":                 "FILTER",
	"do_action":                  "ACTION",
	"do_action_ref_array":        "ACTION",
	"apply_filters":              "FILTER",
	"apply_filters_ref_array":    "FILTER",
	"register_activation_hook":   "ACTIVATION",
	"register_deactivation_hook": "DEACTIVATION",
	"register_rest_route":        "REST",
}

// hookPoints fire a hook rather than register on one: the firing side, which has
// no handler of its own.
var hookPoints = map[string]bool{
	"do_action": true, "do_action_ref_array": true,
	"apply_filters": true, "apply_filters_ref_array": true,
}

// registrationHooks fire when a handler is being attached (vs. a hook point being
// triggered). Their first string argument is the hook name and, for the add_*
// family, the second argument is the callback.
var registrationHooks = map[string]bool{
	"add_action": true, "add_filter": true,
}

// extractHooks scans a PHP file for WordPress hook calls and emits one route fact
// per occurrence. It walks the entire tree (hooks appear at file scope and inside
// functions/methods), independent of the symbol/call walk in extractFileAST.
func extractHooks(src []byte, relFile string) []facts.Fact {
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(php.LanguagePHP())); err != nil {
		return nil
	}
	tree := parser.Parse(src, nil)
	defer tree.Close()

	h := &hookWalker{src: src, relFile: relFile, dir: factpath.Dir(relFile)}
	h.walk(tree.RootNode())
	return h.out
}

type hookWalker struct {
	src     []byte
	relFile string
	dir     string
	out     []facts.Fact
	// namespace and class are where the walk is, qualified as the declaration walk
	// names a class, so `array( $this, 'm' )` names the method's symbol.
	namespace string
	class     []string
}

func (h *hookWalker) walk(node *sitter.Node) {
	if node == nil {
		return
	}
	switch kindOf(node) {
	case "function_call_expression":
		h.handleCall(node)
	case "namespace_definition":
		name := phpText(node.ChildByFieldName("name"), h.src)
		body := node.ChildByFieldName("body")
		if body == nil {
			h.namespace = name // `namespace Foo;`: the rest of the file
			return
		}
		prev := h.namespace
		h.namespace = name
		h.walk(body)
		h.namespace = prev
		return
	case "class_declaration", "trait_declaration", "enum_declaration", "interface_declaration":
		if name := phpText(node.ChildByFieldName("name"), h.src); name != "" {
			if h.namespace != "" {
				name = h.namespace + "\\" + name
			}
			h.class = append(h.class, name)
			defer func() { h.class = h.class[:len(h.class)-1] }()
		}
	}
	for i := uint(0); i < node.ChildCount(); i++ {
		h.walk(node.Child(i))
	}
}

func (h *hookWalker) handleCall(node *sitter.Node) {
	fn := node.ChildByFieldName("function")
	if fn == nil || kindOf(fn) != "name" {
		return
	}
	method, ok := hookMethods[phpText(fn, h.src)]
	if !ok {
		return
	}
	args := node.ChildByFieldName("arguments")
	hookName := firstStringArg(args, h.src)
	if hookName == "" {
		return // dynamic hook name (variable / concatenation) — not statically known
	}

	props := map[string]any{
		"method":    method,
		"framework": "wordpress",
		"language":  "php",
		"hook":      phpText(fn, h.src),
	}
	if method != "REST" {
		// A hook is in-process dispatch, and a hook point the side that fires it.
		props[facts.PropRouteType] = facts.RouteTypeHook
		if hookPoints[phpText(fn, h.src)] {
			props[facts.PropRole] = facts.RoleClient
		}
	}
	// For handler registrations, capture the callback target so the dead-code /
	// coverage explainers can connect the hook to the function it invokes.
	if registrationHooks[phpText(fn, h.src)] {
		if cb := h.callbackTarget(argumentAt(args, 1)); cb != "" {
			props["callback"] = cb
		}
	}
	if method == "REST" {
		if cb := h.callbackTarget(h.restCallback(argumentAt(args, 2))); cb != "" {
			props["callback"] = cb
		}
	}

	h.out = append(h.out, facts.Fact{
		Kind:      facts.KindRoute,
		Name:      method + " " + hookName,
		File:      h.relFile,
		Line:      line(node),
		Props:     props,
		Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: h.dir}},
	})
}

// firstStringArg returns the literal content of the first string argument in an
// arguments node, or "" when the first argument is not a string literal.
func firstStringArg(args *sitter.Node, src []byte) string {
	if args == nil {
		return ""
	}
	for i := uint(0); i < args.ChildCount(); i++ {
		a := args.Child(i)
		if kindOf(a) != "argument" {
			continue
		}
		return stringLiteral(a.Child(0), src)
	}
	return ""
}

// stringLiteral returns the content of a PHP string node ('...' or "...") when it is
// a plain, non-interpolated literal, or "" otherwise. An interpolated string such as
// "prefix_{$suffix}" carries variable children and is treated as dynamic (skipped),
// so only statically-known hook names are emitted.
func stringLiteral(node *sitter.Node, src []byte) string {
	if node == nil {
		return ""
	}
	switch kindOf(node) {
	case "string", "encapsed_string":
		content := ""
		for i := uint(0); i < node.ChildCount(); i++ {
			c := node.Child(i)
			if !c.IsNamed() {
				continue // quote tokens
			}
			if kindOf(c) != "string_content" {
				return "" // interpolation (variable_name, ${...}, etc.) -> dynamic
			}
			content += phpText(c, src)
		}
		return content // empty for "" or '' (no string_content child)
	}
	return ""
}

// bySuffix returns the one symbol whose name ends in suffix, or "" when none or
// several do.
func bySuffix(symbols map[string]bool, suffix string) string {
	if !strings.Contains(suffix, "::") {
		return ""
	}
	found := ""
	for name := range symbols {
		if strings.HasSuffix(name, suffix) {
			if found != "" {
				return ""
			}
			found = name
		}
	}
	return found
}

// bindWordPressHooks binds each hook registration to the function or method its
// callback names. A PHP symbol is named by its fully qualified PHP name, so a string
// callback ('wp_widgets_init', 'WP_Embed::autoembed', '\Acme\boot') is that name
// with any leading backslash dropped; one that names no symbol of the repository
// (a PHP built-in, another plugin's function) binds nothing. Runs once every file's
// symbols are known, since a callback is usually declared in another file.
func bindWordPressHooks(allFacts []facts.Fact) {
	var symbols map[string]bool
	var ix *classIndex
	for i := range allFacts {
		f := &allFacts[i]
		if f.Kind != facts.KindRoute || f.PropString("framework") != "wordpress" {
			continue
		}
		cb := strings.TrimPrefix(f.PropString("callback"), `\`)
		if cb == "" {
			continue
		}
		if symbols == nil {
			symbols = map[string]bool{}
			for j := range allFacts {
				if allFacts[j].Kind == facts.KindSymbol {
					symbols[allFacts[j].Name] = true
				}
			}
		}
		target := cb
		if !symbols[target] {
			// A class named as written ('Foo::m', Foo::class) is relative to the
			// file's namespace and imports, which this pass does not see: it binds
			// when exactly one symbol of the repository ends in \Foo::m.
			target = bySuffix(symbols, "\\"+cb)
		}
		if class, method, ok := strings.Cut(cb, "::"); target == "" && ok {
			// A method the class inherits is declared on a parent or a trait.
			if ix == nil {
				ix = newClassIndex(allFacts)
			}
			target = ix.declaringMethod(class, method)
		}
		if target != "" && !f.HasRelation(facts.RelHandledBy, target) {
			f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelHandledBy, Target: target})
			f.SetProp("handler", target)
		}
	}
}

// argumentAt returns the value of the i-th argument of a call, or nil.
func argumentAt(args *sitter.Node, i int) *sitter.Node {
	if args == nil {
		return nil
	}
	n := 0
	for j := uint(0); j < args.ChildCount(); j++ {
		a := args.Child(j)
		if kindOf(a) != "argument" {
			continue
		}
		if n == i {
			for k := uint(0); k < a.NamedChildCount(); k++ {
				if c := a.NamedChild(k); kindOf(c) != "name" || a.NamedChildCount() == 1 {
					return c
				}
			}
			return nil
		}
		n++
	}
	return nil
}

// callbackTarget names the function or method a PHP callable value refers to, as a
// symbol name: 'fn' and 'Class::m' as written, and the array forms
// array( $this, 'm' ), [ self::class, 'm' ], [ __CLASS__, 'm' ] (the enclosing
// class) and [ Foo::class, 'm' ], array( 'Foo', 'm' ) (the named class, which
// bindWordPressHooks resolves). A callable on another object, `array( $obj, 'm' )`,
// names no class here and yields "".
func (h *hookWalker) callbackTarget(v *sitter.Node) string {
	if v == nil {
		return ""
	}
	switch kindOf(v) {
	case "string", "encapsed_string":
		return stringLiteral(v, h.src)
	case "array_creation_expression":
		var elems []*sitter.Node
		for i := uint(0); i < v.NamedChildCount(); i++ {
			if e := v.NamedChild(i); kindOf(e) == "array_element_initializer" {
				elems = append(elems, e)
			}
		}
		if len(elems) != 2 || elems[0].NamedChildCount() != 1 || elems[1].NamedChildCount() != 1 {
			return ""
		}
		method := stringLiteral(elems[1].NamedChild(0), h.src)
		class := h.callableClass(elems[0].NamedChild(0))
		if method == "" || class == "" {
			return ""
		}
		return class + "::" + method
	}
	return ""
}

// callableClass reads the class half of an array callable.
func (h *hookWalker) callableClass(n *sitter.Node) string {
	text := phpText(n, h.src)
	switch kindOf(n) {
	case "variable_name":
		if text == "$this" && len(h.class) > 0 {
			return h.class[len(h.class)-1]
		}
		return ""
	case "name":
		if text == "__CLASS__" && len(h.class) > 0 {
			return h.class[len(h.class)-1]
		}
	case "string", "encapsed_string":
		return stringLiteral(n, h.src)
	case "class_constant_access_expression":
		if !strings.HasSuffix(text, "::class") {
			return ""
		}
		class := strings.TrimSpace(strings.TrimSuffix(text, "::class"))
		if (class == "self" || class == "static") && len(h.class) > 0 {
			return h.class[len(h.class)-1]
		}
		return class
	}
	return ""
}

// restCallback returns the 'callback' option of register_rest_route's options
// array, which is either the options themselves or a list of them (one per method).
func (h *hookWalker) restCallback(opts *sitter.Node) *sitter.Node {
	if opts == nil || kindOf(opts) != "array_creation_expression" {
		return nil
	}
	for i := uint(0); i < opts.NamedChildCount(); i++ {
		e := opts.NamedChild(i)
		if kindOf(e) != "array_element_initializer" {
			continue
		}
		if e.NamedChildCount() == 2 && stringLiteral(e.NamedChild(0), h.src) == "callback" {
			return e.NamedChild(1)
		}
		if e.NamedChildCount() == 1 {
			if cb := h.restCallback(e.NamedChild(0)); cb != nil {
				return cb
			}
		}
	}
	return nil
}
