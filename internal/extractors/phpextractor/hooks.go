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
}

func (h *hookWalker) walk(node *sitter.Node) {
	if node == nil {
		return
	}
	if kindOf(node) == "function_call_expression" {
		h.handleCall(node)
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
	// For handler registrations, capture the callback target so the dead-code /
	// coverage explainers can connect the hook to the function it invokes.
	if registrationHooks[phpText(fn, h.src)] {
		if cb := callbackName(args, h.src); cb != "" {
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

// callbackName returns the second argument of an add_action/add_filter call as a
// callback target: a plain string ('my_func'), or "" for array/closure callbacks
// (their handler is better captured by the call graph).
func callbackName(args *sitter.Node, src []byte) string {
	if args == nil {
		return ""
	}
	var argNodes []*sitter.Node
	for i := uint(0); i < args.ChildCount(); i++ {
		if a := args.Child(i); kindOf(a) == "argument" {
			argNodes = append(argNodes, a)
		}
	}
	if len(argNodes) < 2 {
		return ""
	}
	return stringLiteral(argNodes[1].Child(0), src)
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

// bindWordPressHooks binds each hook registration to the function or method its
// callback names. A PHP symbol is named by its fully qualified PHP name, so a string
// callback ('wp_widgets_init', 'WP_Embed::autoembed', '\Acme\boot') is that name
// with any leading backslash dropped; one that names no symbol of the repository
// (a PHP built-in, another plugin's function) binds nothing. Runs once every file's
// symbols are known, since a callback is usually declared in another file.
func bindWordPressHooks(allFacts []facts.Fact) {
	var symbols map[string]bool
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
		if symbols[cb] && !f.HasRelation(facts.RelHandledBy, cb) {
			f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelHandledBy, Target: cb})
			f.SetProp("handler", cb)
		}
	}
}
