package phpextractor

import (
	"regexp"
	"strings"

	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/tree-sitter/go-tree-sitter"
	php "github.com/tree-sitter/tree-sitter-php/bindings/go"
)

// extractSymfonyRoutes parses Symfony controllers and emits a server-route fact per
// #[Route(...)] attribute (PHP 8) or legacy @Route(...) docblock annotation. A
// class-level Route contributes a path prefix that is prepended to each method route.
func extractSymfonyRoutes(src []byte, relFile string) []facts.Fact {
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(php.LanguagePHP())); err != nil {
		return nil
	}
	tree := parser.Parse(src, nil)
	defer tree.Close()

	w := &symfonyRouteWalker{src: src, relFile: relFile, dir: factpath.Dir(relFile)}
	w.walk(tree.RootNode())
	return w.out
}

// bindSymfonyHandlers adds handled_by to each route whose handler (namespace\Class::
// method) is a symbol the file's own walk declared. Both walks qualify the class the
// same way, so a match is the method the attribute sits on; a handler with no such
// symbol keeps only its prop, because a wrong edge feeds impact_analysis and
// find_path and a missing one only leaves them short.
func bindSymfonyHandlers(routes, fileFacts []facts.Fact) []facts.Fact {
	declared := map[string]bool{}
	for _, f := range fileFacts {
		if f.Kind == facts.KindSymbol {
			declared[f.Name] = true
		}
	}
	for i := range routes {
		if h := routes[i].PropString("handler"); h != "" && declared[h] {
			routes[i].Relations = append(routes[i].Relations, facts.Relation{Kind: facts.RelHandledBy, Target: h})
		}
	}
	return routes
}

type symfonyRouteWalker struct {
	src       []byte
	relFile   string
	dir       string
	namespace string
	out       []facts.Fact
}

func (w *symfonyRouteWalker) walk(node *sitter.Node) {
	if node == nil {
		return
	}
	switch kindOf(node) {
	case "namespace_definition":
		w.namespace = phpText(node.ChildByFieldName("name"), w.src)
	case "class_declaration":
		w.handleClass(node)
		return // handleClass walks methods itself
	}
	for i := uint(0); i < node.ChildCount(); i++ {
		w.walk(node.Child(i))
	}
}

// handleClass reads the class-level Route prefix (if any) and emits a route for each
// method carrying a Route attribute or annotation.
func (w *symfonyRouteWalker) handleClass(node *sitter.Node) {
	className := phpText(node.ChildByFieldName("name"), w.src)
	fqClass := className
	if w.namespace != "" {
		fqClass = w.namespace + "\\" + className
	}

	classPrefix := ""
	if r, ok := w.routeOf(node); ok {
		classPrefix = r.path
	}

	body := node.ChildByFieldName("body")
	if body == nil {
		return
	}
	for i := uint(0); i < body.ChildCount(); i++ {
		m := body.Child(i)
		if kindOf(m) != "method_declaration" {
			continue
		}
		r, ok := w.routeOf(m)
		if !ok {
			continue
		}
		methodName := phpText(m.ChildByFieldName("name"), w.src)
		handler := fqClass + "::" + methodName
		methods := r.methods
		if len(methods) == 0 {
			methods = []string{"ANY"}
		}
		path := facts.JoinRoutePath(classPrefix, r.path)
		for _, verb := range methods {
			w.emit(m, strings.ToUpper(verb), path, handler, r.name)
		}
	}
}

func (w *symfonyRouteWalker) emit(node *sitter.Node, method, path, handler, name string) {
	if path == "" {
		path = "/"
	}
	props := map[string]any{
		facts.PropRole: facts.RoleServer,
		"method":       method,
		"framework":    "symfony",
		"language":     "php",
		"path":         path,
		"handler":      handler,
	}
	if name != "" {
		props["name"] = name
	}
	w.out = append(w.out, facts.Fact{
		Kind:      facts.KindRoute,
		Name:      path,
		File:      w.relFile,
		Line:      line(node),
		Props:     props,
		Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: w.dir}},
	})
}

// symfonyRoute is a parsed Route attribute/annotation.
type symfonyRoute struct {
	path    string
	methods []string
	name    string
}

// routeOf returns the Route attribute/annotation on a class or method declaration.
// It prefers a PHP 8 #[Route(...)] attribute and falls back to a preceding @Route
// docblock annotation.
func (w *symfonyRouteWalker) routeOf(node *sitter.Node) (symfonyRoute, bool) {
	if attr := findRouteAttribute(node, w.src); attr != nil {
		return parseRouteAttribute(attr, w.src), true
	}
	if r, ok := parseRouteAnnotation(node, w.src); ok {
		return r, true
	}
	return symfonyRoute{}, false
}

// findRouteAttribute returns the `attribute` node named Route in a declaration's
// attribute list, or nil.
func findRouteAttribute(node *sitter.Node, src []byte) *sitter.Node {
	attrs := node.ChildByFieldName("attributes")
	if attrs == nil {
		return nil
	}
	var found *sitter.Node
	var scan func(n *sitter.Node)
	scan = func(n *sitter.Node) {
		if found != nil || n == nil {
			return
		}
		if kindOf(n) == "attribute" {
			for i := uint(0); i < n.ChildCount(); i++ {
				c := n.Child(i)
				if (kindOf(c) == "name" || kindOf(c) == "qualified_name") &&
					lastNsSegment(phpText(c, src)) == "Route" {
					found = n
					return
				}
			}
		}
		for i := uint(0); i < n.ChildCount(); i++ {
			scan(n.Child(i))
		}
	}
	scan(attrs)
	return found
}

// parseRouteAttribute extracts path / methods / name from a #[Route(...)] attribute's
// argument list. The path is the first positional argument or the `path:` named arg.
func parseRouteAttribute(attr *sitter.Node, src []byte) symfonyRoute {
	args := attr.ChildByFieldName("parameters")
	var r symfonyRoute
	if args == nil {
		return r
	}
	positional := 0
	for i := uint(0); i < args.ChildCount(); i++ {
		a := args.Child(i)
		if kindOf(a) != "argument" {
			continue
		}
		nameNode := a.ChildByFieldName("name")
		val := argValue(a)
		if nameNode == nil {
			if positional == 0 {
				r.path = stringLiteral(val, src)
			}
			positional++
			continue
		}
		switch phpText(nameNode, src) {
		case "path":
			r.path = stringLiteral(val, src)
		case "methods":
			r.methods = symfonyMethods(val, src)
		case "name":
			r.name = stringLiteral(val, src)
		}
	}
	return r
}

var (
	symfonyAnnPathRe    = regexp.MustCompile(`@Route\(\s*["']([^"']*)["']`)
	symfonyAnnMethodsRe = regexp.MustCompile(`methods\s*=\s*\{([^}]*)\}`)
	symfonyAnnNameRe    = regexp.MustCompile(`name\s*=\s*["']([^"']*)["']`)
	symfonyAnnTokenRe   = regexp.MustCompile(`["']([^"']+)["']`)
)

// parseRouteAnnotation reads a legacy @Route(...) docblock from the comment node
// immediately preceding a declaration.
func parseRouteAnnotation(node *sitter.Node, src []byte) (symfonyRoute, bool) {
	prev := node.PrevNamedSibling()
	if prev == nil || kindOf(prev) != "comment" {
		return symfonyRoute{}, false
	}
	text := phpText(prev, src)
	m := symfonyAnnPathRe.FindStringSubmatch(text)
	if m == nil {
		return symfonyRoute{}, false
	}
	r := symfonyRoute{path: m[1]}
	if mm := symfonyAnnMethodsRe.FindStringSubmatch(text); mm != nil {
		for _, tok := range symfonyAnnTokenRe.FindAllStringSubmatch(mm[1], -1) {
			r.methods = append(r.methods, tok[1])
		}
	}
	if nm := symfonyAnnNameRe.FindStringSubmatch(text); nm != nil {
		r.name = nm[1]
	}
	return r, true
}

// symfonyMethods extracts the HTTP verbs from a #[Route(methods: …)] value. It
// accepts a single string/constant or an array of them, resolving Symfony's
// HttpFoundation Request::METHOD_* constants (e.g. Request::METHOD_GET -> "GET")
// in addition to plain string literals — the constant form is idiomatic Symfony.
func symfonyMethods(val *sitter.Node, src []byte) []string {
	if val == nil {
		return nil
	}
	if s := stringLiteral(val, src); s != "" {
		return []string{s}
	}
	if v := methodConst(val, src); v != "" {
		return []string{v}
	}
	if kindOf(val) != "array_creation_expression" {
		return nil
	}
	var out []string
	for i := uint(0); i < val.ChildCount(); i++ {
		el := val.Child(i)
		if kindOf(el) != "array_element_initializer" {
			continue
		}
		for j := uint(0); j < el.ChildCount(); j++ {
			c := el.Child(j)
			if s := stringLiteral(c, src); s != "" {
				out = append(out, s)
			} else if v := methodConst(c, src); v != "" {
				out = append(out, v)
			}
		}
	}
	return out
}

// methodConst maps a Symfony Request::METHOD_* class constant to its verb, returning
// "" for any other class constant (e.g. SomeController::class). The constant name is
// the last name child of the class_constant_access_expression.
func methodConst(node *sitter.Node, src []byte) string {
	if node == nil || kindOf(node) != "class_constant_access_expression" {
		return ""
	}
	constName := ""
	for i := uint(0); i < node.ChildCount(); i++ {
		if c := node.Child(i); kindOf(c) == "name" {
			constName = phpText(c, src)
		}
	}
	if strings.HasPrefix(constName, "METHOD_") {
		return strings.ToUpper(strings.TrimPrefix(constName, "METHOD_"))
	}
	return ""
}
