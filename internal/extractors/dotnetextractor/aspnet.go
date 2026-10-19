package dotnetextractor

import (
	"log"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/tree-sitter/go-tree-sitter"
)

// ASP.NET Core attribute routing.
//
// A controller action's URL is assembled from two attributes in two places — a
// class-level [Route(...)] and a method-level [HttpGet("...")] — and frequently
// from a THIRD place the file cannot see: most controllers declare no [Route] of
// their own and inherit one from a base class in another file. In jellyfin, 40 of
// 64 controllers do exactly that, inheriting [Route("[controller]")] from a shared
// BaseJellyfinApiController, so a per-file composition would give the right path to
// a minority of the API and no path at all to the rest.
//
// The walker therefore records raw evidence per file (aspnetScaffold) and
// composeControllerRoutes assembles the routes once the whole fact set exists and
// resolveCSharpTargets has canonicalised the inheritance edges it has to walk.

// aspnetScaffold is one file's raw ASP.NET routing evidence, composed later.
type aspnetScaffold struct {
	controllers []controllerDecl
	actions     []actionDecl
	// minimal holds resolved minimal-API registrations. They need no cross-file
	// composition — a group and the endpoints using it live in one body — but they
	// do need the merged symbol set to bind a handler, so they are carried here
	// and materialised alongside the controller routes.
	minimal []minimalRoute
	// storage holds persistence evidence, decided after the walk for the same
	// reason routing is: whether a class is a DbContext depends on a base list
	// naming a type declared in another file.
	storage storageScaffold
	// clients holds resolved OUTBOUND requests: HttpClient verb calls and Refit
	// attributes. They need no cross-file composition, only the file-local literal
	// environment the scan already built.
	clients []clientCall
	// conventional holds MVC route registrations, and the count of those that were
	// registrations but could not be resolved — kept so the gap stays visible.
	conventional        []conventionalRoute
	conventionalSkipped int
}

func (s *aspnetScaffold) empty() bool {
	return len(s.controllers) == 0 && len(s.actions) == 0 && len(s.minimal) == 0
}

func (s *aspnetScaffold) merge(o aspnetScaffold) {
	s.controllers = append(s.controllers, o.controllers...)
	s.actions = append(s.actions, o.actions...)
	s.minimal = append(s.minimal, o.minimal...)
	s.storage.merge(o.storage)
	s.clients = append(s.clients, o.clients...)
	s.conventional = append(s.conventional, o.conventional...)
	s.conventionalSkipped += o.conventionalSkipped
}

// controllerDecl is a type that may serve routes: it carries a [Route] template, an
// [ApiController]/[Controller] marker, or both. A type with neither is still
// recorded when it declares verb-attributed methods, because it is then a
// controller whose base holds the template.
type controllerDecl struct {
	Fact     string // canonical fact name, e.g. "Jellyfin.Api/Controllers.ItemsController"
	Type     string // simple type name, for [controller] substitution
	Template string // its own [Route(...)] template, "" when it declares none
	HasRoute bool   // whether [Route] was present at all — "" is a real template
	IsAPI    bool   // [ApiController] or [Controller]
}

// actionDecl is a method carrying at least one HTTP verb attribute.
type actionDecl struct {
	Controller string // canonical fact name of the declaring type
	Symbol     string // canonical fact name of the method
	Method     string // simple method name, for [action] substitution
	File       string
	Line       int
	Verbs      []verbAttr
}

// verbAttr is one [HttpGet("x")] — the HTTP method and its route template.
type verbAttr struct {
	HTTPMethod string
	Template   string
	HasRoute   bool
}

// verbAttributes maps an ASP.NET routing attribute to the HTTP method it serves.
// [AcceptVerbs] is deliberately absent: it takes its verbs as a variadic argument
// list, and a route whose method is guessed is worse than one not emitted.
var verbAttributes = map[string]string{
	"HttpGet":     "GET",
	"HttpPost":    "POST",
	"HttpPut":     "PUT",
	"HttpDelete":  "DELETE",
	"HttpPatch":   "PATCH",
	"HttpHead":    "HEAD",
	"HttpOptions": "OPTIONS",
}

// csAttribute is one parsed attribute: its short name and its first positional
// string-literal argument, which is the route template for every attribute here.
type csAttribute struct {
	Name    string
	Arg     string
	HasArg  bool
	Present bool
}

// parseAttributes reads the attribute_list children of a declaration. Attributes
// may be written qualified ([Microsoft.AspNetCore.Mvc.HttpGet]) or with the
// conventional `Attribute` suffix ([HttpGetAttribute]), and both mean the same
// thing, so the name is reduced to its short form.
func parseAttributes(node *sitter.Node, src []byte) []csAttribute {
	var out []csAttribute
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		list := node.Child(i)
		if kindOf(list) != "attribute_list" {
			continue
		}
		for j := uint(0); j < uint(list.NamedChildCount()); j++ {
			a := list.NamedChild(j)
			if kindOf(a) != "attribute" {
				continue
			}
			name := simpleTypeName(typeFullName(a.ChildByFieldName("name"), src))
			name = strings.TrimSuffix(name, "Attribute")
			att := csAttribute{Name: name, Present: true}
			if arg, ok := firstStringArgument(a, src); ok {
				att.Arg, att.HasArg = arg, true
			}
			out = append(out, att)
		}
	}
	return out
}

// firstStringArgument returns an attribute's route template: the FIRST argument,
// and only when it is a string literal.
//
// Only the first argument can be the template, and that single rule is also what
// keeps a named argument out of the path. jellyfin writes
// [HttpGet("{itemId}/stream", Name = "GetAudioStream")] throughout and
// [HttpHead(Name = "HeadAudioStream")] alongside it; a named argument carries a
// `name` field, so the first-argument rule rejects it and the route keeps the
// controller's own path. Scanning further
// arguments for "a string literal" would put a route's DISPLAY NAME into its URL —
// which is why this stops at the first argument rather than searching.
//
// The same rule covers a positional argument that is not a literal at all (a
// nameof(), a constant, an enum member): unreadable is not guessable, so the
// attribute contributes no template.
func firstStringArgument(attr *sitter.Node, src []byte) (string, bool) {
	list := findChildByKind(attr, "attribute_argument_list")
	if list == nil {
		return "", false
	}
	for i := uint(0); i < uint(list.NamedChildCount()); i++ {
		arg := list.NamedChild(i)
		if kindOf(arg) != "attribute_argument" {
			continue
		}
		// A named argument (`Name = "..."`) carries its label in the `name` field
		// and is never the template, whatever its value.
		if arg.ChildByFieldName("name") != nil {
			return "", false
		}
		for j := uint(0); j < uint(arg.NamedChildCount()); j++ {
			if s, ok := stringLiteralText(arg.NamedChild(j), src); ok {
				return s, true
			}
		}
		return "", false
	}
	return "", false
}

// stringLiteralText unquotes a C# string literal. Interpolated and raw strings are
// not templates in practice and are left alone.
func stringLiteralText(node *sitter.Node, src []byte) (string, bool) {
	switch kindOf(node) {
	case "string_literal":
		t := nodeText(node, src)
		if len(t) >= 2 && strings.HasPrefix(t, `"`) && strings.HasSuffix(t, `"`) {
			return t[1 : len(t)-1], true
		}
	case "verbatim_string_literal":
		t := nodeText(node, src)
		if len(t) >= 3 && strings.HasPrefix(t, `@"`) && strings.HasSuffix(t, `"`) {
			return t[2 : len(t)-1], true
		}
	}
	return "", false
}

func findAttribute(atts []csAttribute, name string) csAttribute {
	for _, a := range atts {
		if a.Name == name {
			return a
		}
	}
	return csAttribute{}
}

// ── Walker hooks ────────────────────────────────────────────────────────────

// noteController records a type's routing attributes. Everything is recorded and
// nothing is decided here: whether the type serves routes at all depends on its
// methods and on its base classes, neither of which this file necessarily holds.
func (w *astWalker) noteController(node *sitter.Node, factName, typeName string) {
	atts := parseAttributes(node, w.src)
	route := findAttribute(atts, "Route")
	isAPI := findAttribute(atts, "ApiController").Present || findAttribute(atts, "Controller").Present
	if !route.Present && !isAPI {
		// Still recorded: a controller with no attributes of its own inherits its
		// template, and the composer needs its simple name for [controller].
		w.scaffold.controllers = append(w.scaffold.controllers, controllerDecl{
			Fact: factName, Type: typeName,
		})
		return
	}
	w.scaffold.controllers = append(w.scaffold.controllers, controllerDecl{
		Fact:     factName,
		Type:     typeName,
		Template: route.Arg,
		HasRoute: route.Present,
		IsAPI:    isAPI,
	})
}

// noteAction records a method's HTTP verb attributes. A method with none is not an
// action and is not recorded.
func (w *astWalker) noteAction(node *sitter.Node, controllerFact, symbolFact, methodName string) {
	if controllerFact == "" {
		return
	}
	atts := parseAttributes(node, w.src)
	// [Route] on a method supplies the template when the verb attribute carries
	// none — `[HttpGet] [Route("x")]` is equivalent to `[HttpGet("x")]`.
	methodRoute := findAttribute(atts, "Route")

	var verbs []verbAttr
	for _, a := range atts {
		verb, ok := verbAttributes[a.Name]
		if !ok {
			continue
		}
		v := verbAttr{HTTPMethod: verb, Template: a.Arg, HasRoute: a.HasArg}
		if !v.HasRoute && methodRoute.HasArg {
			v.Template, v.HasRoute = methodRoute.Arg, true
		}
		verbs = append(verbs, v)
	}
	if len(verbs) == 0 {
		return
	}
	w.scaffold.actions = append(w.scaffold.actions, actionDecl{
		Controller: controllerFact,
		Symbol:     symbolFact,
		Method:     methodName,
		File:       w.relFile,
		Line:       int(node.StartPosition().Row) + 1,
		Verbs:      verbs,
	})
}

// ── Composition ─────────────────────────────────────────────────────────────

// maxBaseWalk bounds the inheritance walk looking for an inherited [Route]. Real
// controller hierarchies are one or two deep; the bound is what keeps a cyclic or
// pathological `implements` chain from being a hang.
const maxBaseWalk = 8

// composeControllerRoutes turns the per-file scaffold into route facts, resolving
// each action's full URL from its controller's template — inherited from a base
// class when the controller declares none — and binding the route to the method
// that serves it.
//
// It must run AFTER resolveCSharpTargets: the base-class walk follows RelImplements
// targets, which are bare type names until that pass canonicalises them.
func composeControllerRoutes(allFacts []facts.Fact, sc aspnetScaffold) []facts.Fact {
	if sc.empty() {
		return nil
	}

	ctrl := make(map[string]controllerDecl, len(sc.controllers))
	for _, c := range sc.controllers {
		// A partial controller declares itself in several files; the half carrying
		// the [Route] wins over the half carrying none.
		if prev, ok := ctrl[c.Fact]; ok && prev.HasRoute && !c.HasRoute {
			continue
		}
		ctrl[c.Fact] = c
	}

	bases := make(map[string][]string, len(sc.controllers))
	symbols := make(map[string]bool, len(allFacts))
	dirOf := make(map[string]string, len(sc.controllers))
	for i := range allFacts {
		f := &allFacts[i]
		if f.Kind != facts.KindSymbol {
			continue
		}
		symbols[f.Name] = true
		if _, ok := ctrl[f.Name]; !ok {
			continue
		}
		dirOf[f.Name] = routeDir(f.Name)
		for _, r := range f.Relations {
			if r.Kind == facts.RelImplements {
				bases[f.Name] = append(bases[f.Name], r.Target)
			}
		}
	}

	out := minimalRouteFacts(sc.minimal, symbols)
	unrouted := 0
	for _, a := range sc.actions {
		c, ok := ctrl[a.Controller]
		if !ok {
			continue
		}
		base, ok := effectiveTemplate(a.Controller, ctrl, bases)
		if !ok {
			// CONVENTIONAL ROUTING — no [Route] anywhere in the hierarchy. The URL
			// then comes from a template registered in Program.cs
			// (MapControllerRoute("{controller}/{action}/{id?}")), which this
			// extractor does not read, so the path is genuinely unknown.
			//
			// Emitting the action's own template alone is what this used to do, and
			// it was wrong twice over: a bare [HttpGet] carries no template, so
			// every action in an MVC controller came out as "/" — the wrong path,
			// AND, because facts are name-keyed, five actions collapsing into one
			// root-route node. On eShop's Identity.API that turned 14 real endpoints
			// into a handful of phantom roots.
			//
			// Guessing "/{controller}/{action}" instead would be inventing a
			// template that lives in a file we did not parse and can be anything.
			// So: no route. A missing route is visible in the count below; a wrong
			// one is invisible and gets acted on.
			unrouted += len(a.Verbs)
			continue
		}
		for _, v := range a.Verbs {
			path := composeRoutePath(base, v.Template, c.Type, a.Method)
			props := map[string]any{
				"method":    v.HTTPMethod,
				"framework": "aspnetcore",
				"language":  "csharp",
				"handler":   a.Symbol,
			}
			rels := []facts.Relation{{Kind: facts.RelDeclares, Target: dirOf[a.Controller]}}
			// handled_by is emitted only against a symbol that exists. The target is
			// built from the same walk that emitted the method fact, so it normally
			// does — but a wrong handled_by feeds impact_analysis and find_path, and
			// is worse than the missing edge it would replace.
			if symbols[a.Symbol] {
				rels = append(rels, facts.Relation{Kind: facts.RelHandledBy, Target: a.Symbol})
			}
			out = append(out, facts.Fact{
				Kind:      facts.KindRoute,
				Name:      path,
				File:      a.File,
				Line:      a.Line,
				Props:     props,
				Relations: rels,
			})
		}
	}
	if unrouted > 0 {
		log.Printf("[dotnet-extractor] %d controller action(s) use conventional routing; "+
			"their paths come from Program.cs and were not extracted", unrouted)
	}
	return out
}

// routeDir returns the module directory a controller's fact name is anchored to.
// The fact name is "<dir>.<Type>", and a dir can itself contain dots
// (Jellyfin.Api/Controllers), so the split is on the last "/" then the first "."
// after it — not on the last "." in the whole string.
func routeDir(factName string) string {
	slash := strings.LastIndex(factName, "/")
	dot := strings.Index(factName[slash+1:], ".")
	if dot < 0 {
		return factName
	}
	return factName[:slash+1+dot]
}

// effectiveTemplate returns the [Route] template governing a controller: its own
// when it declares one, otherwise the nearest ancestor's.
//
// The walk is breadth-first over the resolved inheritance edges, which for a class
// hold its base class AND its interfaces indistinguishably — C# does not separate
// the two syntactically and neither does RelImplements. That is harmless here: an
// interface carries no [Route], so it contributes nothing and the search continues.
func effectiveTemplate(start string, ctrl map[string]controllerDecl, bases map[string][]string) (string, bool) {
	if c, ok := ctrl[start]; ok && c.HasRoute {
		return c.Template, true
	}
	seen := map[string]bool{start: true}
	queue := append([]string(nil), bases[start]...)
	for steps := 0; steps < maxBaseWalk && len(queue) > 0; steps++ {
		var next []string
		for _, b := range queue {
			if seen[b] {
				continue
			}
			seen[b] = true
			if c, ok := ctrl[b]; ok && c.HasRoute {
				return c.Template, true
			}
			next = append(next, bases[b]...)
		}
		queue = next
	}
	return "", false
}

// composeRoutePath assembles an action's URL from the controller template and the
// action template, applying ASP.NET's token replacement and absolute-path rule.
func composeRoutePath(base, sub, typeName, methodName string) string {
	base = replaceRouteTokens(base, typeName, methodName)
	sub = replaceRouteTokens(sub, typeName, methodName)

	// A template beginning with "/" or "~/" is absolute: it replaces the
	// controller's template rather than extending it.
	if strings.HasPrefix(sub, "~/") {
		return facts.JoinRoutePath("", strings.TrimPrefix(sub, "~"))
	}
	if strings.HasPrefix(sub, "/") {
		return facts.JoinRoutePath("", sub)
	}
	return facts.JoinRoutePath(base, sub)
}

// replaceRouteTokens applies ASP.NET's `[controller]` and `[action]` replacement.
// `[controller]` is the type name with its conventional `Controller` suffix
// removed — the whole point of the token, and the reason a repo like jellyfin can
// give 40 controllers their distinct paths from one shared base attribute.
func replaceRouteTokens(t, typeName, methodName string) string {
	if t == "" || !strings.Contains(t, "[") {
		return t
	}
	controller := strings.TrimSuffix(typeName, "Controller")
	t = strings.ReplaceAll(t, "[controller]", controller)
	t = strings.ReplaceAll(t, "[action]", methodName)
	return t
}
