package dotnetextractor

// Conventional MVC routing — the URLs that come from a registration rather than
// from an attribute.
//
// A controller with verb attributes but no `[Route]` anywhere in its hierarchy is
// conventionally routed, and its URL is assembled from a template registered in a
// startup file. This extractor used to emit nothing for those, correctly: without
// reading the registration, composing from what is visible produced `/` for every
// action, and because facts are name-keyed several actions collapsed onto a single
// root node.
//
// Reading the registration is what makes them safe to emit. OrchardCore declares
// 288 verb attributes across 114 controllers and only 7 of those carry a
// `[Route]`, so this is the largest single source of missing .NET routes.
//
//	routes.MapAreaControllerRoute(
//	    name: "ListFeed",
//	    areaName: "OrchardCore.Feeds",
//	    pattern: "Contents/Lists/{contentItemId}/rss",
//	    defaults: new { controller = "Feed", action = "Index" });
//
// A registration whose template still contains `{controller}` or `{action}` after
// substitution is NOT emitted. OrchardCore's default is
// `{area}/{controller}/{action}/{id?}`, and expanding it would need each
// controller's area — which comes from an `[Area]` attribute or a module
// convention this extractor does not read. A route at a literal `/{area}/…` would
// be a URL the application never serves.

import (
	"strings"

	sitter "github.com/enola-labs/enola/internal/treesitter"

	"github.com/enola-labs/enola/internal/facts"
)

// conventionalMappers are the registration methods that carry a route template.
var conventionalMappers = map[string]bool{
	"MapControllerRoute":     true,
	"MapAreaControllerRoute": true,
}

// conventionalRoute is one resolved registration.
type conventionalRoute struct {
	Path       string
	Controller string // without the "Controller" suffix
	Action     string
	File       string
	Dir        string
	Line       int
}

// collectConventionalRoutes reads the registrations in one file. It reuses the
// httpClientScan literal environment because a template is routinely a `const`
// field rather than an inline string — OrchardCore's default route is exactly
// that.
func collectConventionalRoutes(root *sitter.Node, src []byte, relFile, dir string) ([]conventionalRoute, int) {
	s := &httpClientScan{src: src, relFile: relFile, dir: dir, literals: map[string]string{}}
	s.collectLiterals(root)

	var out []conventionalRoute
	skipped := 0
	var walk func(n *sitter.Node, locals map[string]string)
	walk = func(n *sitter.Node, locals map[string]string) {
		if memberBodies[kindOf(n)] {
			locals = s.localsOf(n)
		}
		if kindOf(n) == "invocation_expression" {
			if r, ok, skip := conventionalRegistration(s, n, locals); ok {
				out = append(out, r)
			} else if skip {
				skipped++
			}
		}
		for i := uint(0); i < n.NamedChildCount(); i++ {
			walk(n.NamedChild(i), locals)
		}
	}
	walk(root, nil)
	return out, skipped
}

// conventionalRegistration resolves one call. The third result reports a
// registration that WAS a route mapping but could not be resolved, so the count
// of what is missing stays visible rather than silently absorbed.
func conventionalRegistration(s *httpClientScan, node *sitter.Node, locals map[string]string) (conventionalRoute, bool, bool) {
	fn := node.ChildByFieldName("function")
	if fn == nil {
		return conventionalRoute{}, false, false
	}
	nameNode := fn.ChildByFieldName("name")
	if nameNode == nil {
		return conventionalRoute{}, false, false
	}
	if !conventionalMappers[nodeText(nameNode, s.src)] {
		return conventionalRoute{}, false, false
	}
	args := node.ChildByFieldName("arguments")
	if args == nil {
		return conventionalRoute{}, false, false
	}

	named := map[string]*sitter.Node{}
	var positional []*sitter.Node
	for i := uint(0); i < args.NamedChildCount(); i++ {
		a := args.NamedChild(i)
		if kindOf(a) != "argument" {
			continue
		}
		// A named argument carries its label in the `name` FIELD; there is no
		// name_colon node in this grammar. Reading it wrong made every named
		// argument look positional, so `pattern` picked up `areaName`.
		if nc := a.ChildByFieldName("name"); nc != nil {
			named[strings.TrimSpace(nodeText(nc, s.src))] = a
			continue
		}
		positional = append(positional, a)
	}

	patternArg := named["pattern"]
	if patternArg == nil && len(positional) >= 2 {
		// MapControllerRoute(name, pattern, …) — the unnamed spelling.
		patternArg = positional[1]
	}
	if patternArg == nil {
		return conventionalRoute{}, false, true
	}
	pattern, ok := s.resolve(lastNamedChild(patternArg), locals)
	if !ok {
		return conventionalRoute{}, false, true
	}

	r := conventionalRoute{
		File: s.relFile, Dir: s.dir, Line: int(node.StartPosition().Row) + 1,
	}
	if d := named["defaults"]; d != nil {
		r.Controller, r.Action = anonymousObjectRoute(lastNamedChild(d), s.src)
	}
	if a := named["areaName"]; a != nil {
		if area, ok := s.resolve(lastNamedChild(a), locals); ok {
			pattern = strings.ReplaceAll(pattern, "{area}", area)
			pattern = strings.ReplaceAll(pattern, "{area:exists}", area)
		}
	}
	if r.Controller != "" {
		pattern = strings.ReplaceAll(pattern, "{controller}", r.Controller)
		pattern = strings.ReplaceAll(pattern, "{controller=Home}", r.Controller)
	}
	if r.Action != "" {
		pattern = strings.ReplaceAll(pattern, "{action}", r.Action)
		pattern = strings.ReplaceAll(pattern, "{action=Index}", r.Action)
	}

	// Still generic: the URL depends on which controller happens to match, and
	// emitting a literal `{controller}` segment would name a URL nothing serves.
	if strings.Contains(pattern, "{controller") || strings.Contains(pattern, "{action") ||
		strings.Contains(pattern, "{area") {
		return conventionalRoute{}, false, true
	}

	p := strings.TrimSpace(pattern)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	p = strings.TrimSuffix(p, "/")
	if p == "" {
		return conventionalRoute{}, false, true
	}
	r.Path = p
	return r, true, false
}

// anonymousObjectRoute reads `new { controller = "Feed", action = "Index" }`.
//
// The grammar FLATTENS an anonymous object: its members are alternating
// `identifier` and value children of the creation expression, with no assignment
// or member-declarator node between them. Looking for an assignment_expression
// found nothing and every registration came out with no controller.
func anonymousObjectRoute(node *sitter.Node, src []byte) (controller, action string) {
	if node == nil {
		return "", ""
	}
	if kindOf(node) != "anonymous_object_creation_expression" {
		if inner := findChildByKind(node, "anonymous_object_creation_expression"); inner != nil {
			node = inner
		} else {
			return "", ""
		}
	}
	key := ""
	for i := uint(0); i < node.NamedChildCount(); i++ {
		c := node.NamedChild(i)
		if kindOf(c) == "identifier" {
			key = strings.ToLower(nodeText(c, src))
			continue
		}
		lit, ok := stringLiteralText(c, src)
		if !ok {
			key = ""
			continue
		}
		switch key {
		case "controller":
			controller = lit
		case "action":
			action = lit
		}
		key = ""
	}
	return controller, action
}

func lastNamedChild(n *sitter.Node) *sitter.Node {
	if n == nil || n.NamedChildCount() == 0 {
		return n
	}
	return n.NamedChild(n.NamedChildCount() - 1)
}

// conventionalRouteFacts turns resolved registrations into route facts, binding
// each to its action when that action names a real symbol.
//
// The method is GET: a conventional registration declares a URL, not a verb, and
// the verb lives on the action's own `[HttpPost]`. GET is what ASP.NET serves for
// an action with no verb attribute, and it is what makes the fact comparable with
// a client route.
func conventionalRouteFacts(routes []conventionalRoute, symbols map[string]bool) []facts.Fact {
	seen := map[string]bool{}
	out := make([]facts.Fact, 0, len(routes))
	for _, r := range routes {
		if seen[r.Path] {
			continue
		}
		seen[r.Path] = true
		props := map[string]any{
			"method":    "GET",
			"framework": "aspnetcore",
			"language":  "csharp",
			"routing":   "conventional",
		}
		rels := []facts.Relation{{Kind: facts.RelDeclares, Target: r.Dir}}
		if r.Controller != "" && r.Action != "" {
			props["controller"] = r.Controller
			props["action"] = r.Action
			if h := findActionSymbol(symbols, r.Controller, r.Action); h != "" {
				props["handler"] = h
				rels = append(rels, facts.Relation{Kind: facts.RelHandledBy, Target: h})
			}
		}
		out = append(out, facts.Fact{
			Kind:      facts.KindRoute,
			Name:      r.Path,
			File:      r.File,
			Line:      r.Line,
			Props:     props,
			Relations: rels,
		})
	}
	return out
}

// findActionSymbol locates `…<Controller>Controller.<Action>` among the known
// symbol names. A registration names a controller by its short name, and the fact
// is directory-anchored, so the match is on the suffix.
func findActionSymbol(symbols map[string]bool, controller, action string) string {
	suffix := "." + controller + "Controller." + action
	match := ""
	for name := range symbols {
		if strings.HasSuffix(name, suffix) {
			if match != "" {
				return "" // two controllers of the same name: binding one would be a guess
			}
			match = name
		}
	}
	return match
}
