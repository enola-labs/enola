package dartextractor

import (
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"

	"github.com/enola-labs/enola/internal/facts"
)

// Flutter navigation routes.
//
// These are `route` facts carrying type "page", which is a load-bearing choice rather
// than a label. A Flutter navigation path is a URL the APP navigates to internally, not
// an HTTP contract it serves to anybody — nothing outside the process can call
// `/profile/:id` on a mobile app. enola already models exactly this distinction for
// Next.js pages, Nuxt pages, SvelteKit and Ember, and `routeindex.IsUIRoute` keys on
// the type value: a page-type route is kept out of the cross-repo server-route index
// and out of the unused-routes explainer.
//
// Getting that wrong in the other direction would be quietly destructive. A Flutter app
// and its backend are frequently in one snapshot (two of the benchmark clusters are
// exactly that shape), both have a `/users/:id`, and indexing the app's screen as a
// served endpoint would match it against the backend's real route — manufacturing a
// cross-repo edge in the wrong direction and simultaneously reporting the backend's own
// endpoint as served by the phone.

// extractNavigationRoutes emits one page route per declared navigation destination.
func (w *walker) extractNavigationRoutes(root *sitter.Node) {
	switch {
	case w.importsAny("package:go_router/"):
		w.goRouterRoutes(root)
	case w.importsAny("package:auto_route/"):
		w.autoRouteRoutes(root)
	}
	// The Navigator `routes:` map is core Flutter, so it is not exclusive with the
	// packages above — an app can use both.
	if w.isFlutterFile() {
		w.navigatorRoutesMap(root)
	}
}

// goRouterRoutes walks a go_router configuration, composing nested paths.
//
// go_router nests sub-routes under a parent via `routes: [...]`, and a child path that
// does not begin with "/" is RELATIVE to its parent — `GoRoute(path: '/user', routes: [
// GoRoute(path: 'settings') ])` serves `/user/settings`. Composing is the same
// correction made for Go's subrouters, Rust's `.nest()` and FastAPI's `include_router`:
// storing the bare child path would put a destination in the graph the app never
// navigates to.
func (w *walker) goRouterRoutes(root *sitter.Node) {
	var visit func(n *sitter.Node, prefix string)
	visit = func(n *sitter.Node, prefix string) {
		kids := namedChildren(n)
		for i, c := range kids {
			if name, args := w.constructorCall(kids, i); name == "GoRoute" || name == "ShellRoute" {
				path, ref := w.routePathArg(args)
				full := composePath(prefix, path)
				if name == "GoRoute" && (path != "" || ref != "") {
					extra := map[string]any{
						"route_name": stringArg(args, w.src, "name", -1),
						"handler":    w.builderTarget(args),
					}
					if ref != "" {
						extra[pathRefProp] = ref
						extra[pathPrefixProp] = prefix
						full = ref
					}
					w.emitPageRoute(full, "go_router", c, extra)
				}
				// A ShellRoute contributes no path of its own but does nest children.
				if sub := namedArgOf(args, w.src, "routes"); sub != nil {
					visit(sub, full)
					continue
				}
			}
			visit(c, prefix)
		}
	}
	visit(root, "")
}

// autoRouteRoutes reads auto_route's declarative route list.
//
// The dominant idiom declares NO path at all — `AutoRoute(page: LoginRoute.page)` — and
// lets auto_route derive one from the page name. All 59 declarations in immich's router
// are this shape, so requiring an explicit `path:` would leave auto_route effectively
// unsupported. The derivation is auto_route's own documented rule and is applied here
// (see autoRoutePath), which is the same class of move as deriving a drift table name
// from its class or resolving C#'s `[controller]` token: reading a convention the
// framework guarantees, not guessing.
func (w *walker) autoRouteRoutes(root *sitter.Node) {
	var visit func(n *sitter.Node, prefix string)
	visit = func(n *sitter.Node, prefix string) {
		kids := namedChildren(n)
		for i, c := range kids {
			if name, args := w.constructorCall(kids, i); name == "AutoRoute" || name == "CustomRoute" ||
				name == "RedirectRoute" {
				page := w.namedArgIdentifier(args, "page")
				path, ref := w.routePathArg(args)
				initial := w.isInitialRoute(args)
				if path == "" && ref == "" {
					path = autoRoutePath(page, initial)
				}
				var full string
				switch {
				case ref != "":
					full = ref
				case initial:
					// An `initial: true` child mounts at its PARENT's path, not at the
					// application root. Composing "/" here instead produced two facts
					// both named "/" on immich — and since facts are name-keyed, the
					// nested one silently merged into the root screen's node.
					full = prefix
					if full == "" {
						full = "/"
					}
				default:
					full = composePath(prefix, path)
				}
				if (path != "" || ref != "") && name != "RedirectRoute" {
					extra := map[string]any{"handler": pageWidgetName(page)}
					if ref != "" {
						extra[pathRefProp] = ref
						extra[pathPrefixProp] = prefix
					}
					w.emitPageRoute(full, "auto_route", c, extra)
				}
				if sub := namedArgOf(args, w.src, "children"); sub != nil {
					visit(sub, full)
					continue
				}
			}
			visit(c, prefix)
		}
	}
	visit(root, "")
}

// isInitialRoute reports `initial: true`, which auto_route mounts at the parent root.
func (w *walker) isInitialRoute(args *sitter.Node) bool {
	v := namedArgOf(args, w.src, "initial")
	return v != nil && strings.TrimSpace(v.Utf8Text(w.src)) == "true"
}

// autoRoutePath derives the path auto_route generates for a pathless route.
//
// auto_route kebab-cases the route name with its `Route` suffix removed, so
// `SplashScreenRoute` is served at `/splash-screen`. An `initial: true` route is mounted
// at its parent's root instead.
func autoRoutePath(page string, initial bool) string {
	if initial {
		return "/"
	}
	base := strings.TrimSuffix(page, "Route")
	if base == "" || base == page {
		// Not the generated-route naming convention, so the derivation does not
		// apply and inventing a path from an arbitrary identifier would be a guess.
		return ""
	}
	return "/" + kebabCase(base)
}

// pageWidgetName maps a generated route class back to the widget it wraps.
//
// auto_route generates `LoginRoute` from `@RoutePage() class LoginPage`, so dropping
// the `Route` suffix names the widget — which is what makes the handler resolvable to a
// real symbol instead of to the generated class in the .gr.dart this extractor excludes.
func pageWidgetName(page string) string {
	if base := strings.TrimSuffix(page, "Route"); base != "" && base != page {
		return base + "Page"
	}
	return ""
}

// kebabCase converts PascalCase to kebab-case, the transform auto_route applies.
func kebabCase(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			if i > 0 {
				b.WriteByte('-')
			}
			b.WriteByte(c - 'A' + 'a')
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// navigatorRoutesMap reads the core-Flutter `routes: {'/home': (c) => Home()}` map that
// MaterialApp/CupertinoApp take.
func (w *walker) navigatorRoutesMap(root *sitter.Node) {
	var visit func(*sitter.Node)
	visit = func(n *sitter.Node) {
		for _, c := range namedChildren(n) {
			if kindOf(c) == "named_argument" {
				if lbl := childOfKind(c, "label"); lbl != nil {
					label := strings.TrimSuffix(strings.TrimSpace(lbl.Utf8Text(w.src)), ":")
					if label == "routes" {
						w.routesMapEntries(c)
					}
				}
			}
			visit(c)
		}
	}
	visit(root)
}

// routesMapEntries emits a page route per literal key of a routes map.
func (w *walker) routesMapEntries(n *sitter.Node) {
	lit := firstOfKind(n, "set_or_map_literal")
	if lit == nil {
		return
	}
	for _, entry := range namedChildren(lit) {
		if kindOf(entry) != "pair" {
			continue
		}
		kids := namedChildren(entry)
		if len(kids) == 0 {
			continue
		}
		path := stringLiteralValue(kids[0], w.src)
		if path == "" || !strings.HasPrefix(path, "/") {
			continue
		}
		w.emitPageRoute(path, "navigator", entry, nil)
	}
}

// constructorCall reports the constructor name and arguments when kids[i] is an
// invocation's argument selector.
//
// It exists because the grammar's selector-chain model means a constructor call is two
// sibling nodes, so "is this a GoRoute(...)" cannot be answered by looking at one node.
// The caller passes the index rather than the node: namedChildren allocates fresh
// wrappers on every call, so locating a node by pointer comparison inside here would
// never match.
func (w *walker) constructorCall(kids []*sitter.Node, i int) (string, *sitter.Node) {
	if i <= 0 || kindOf(kids[i]) != "selector" {
		return "", nil
	}
	args := childOfKind(kids[i], "argument_part")
	if args == nil {
		return "", nil
	}
	prev := kids[i-1]
	if kindOf(prev) != "identifier" && kindOf(prev) != "type_identifier" {
		return "", nil
	}
	return prev.Utf8Text(w.src), argumentsOf(args)
}

// namedArgIdentifier returns the bare identifier a named argument names, for the cases
// where the value is a symbol rather than a literal (`page: HomeRoute.page`).
func (w *walker) namedArgIdentifier(args *sitter.Node, label string) string {
	v := namedArgOf(args, w.src, label)
	if v == nil {
		return ""
	}
	txt := strings.TrimSpace(v.Utf8Text(w.src))
	if i := strings.IndexAny(txt, ".(<"); i > 0 {
		txt = txt[:i]
	}
	if isPlainIdentifier(txt) {
		return txt
	}
	return ""
}

// builderTarget names the widget a route builds, when the builder body makes it plain.
//
// The body is an expression, or a block whose one return is. The widget is the
// constructor that expression calls, read through every wrapper that hands its
// screen on as `child:`: `MaterialExtendedPage(child: const HomeScreen())` and
// `BlocProvider(create: ..., child: ProfileScreen())` are HomeScreen and
// ProfileScreen. Anything else (a conditional, several returns, a child chosen at run
// time) is left unnamed rather than guessed, because a handler prop is what
// `handled_by` binding would key on and a wrong one is worse than none.
func (w *walker) builderTarget(args *sitter.Node) string {
	for _, label := range []string{"builder", "pageBuilder"} {
		v := namedArgOf(args, w.src, label)
		if v == nil {
			continue
		}
		body := firstOfKind(v, "function_expression_body")
		if body == nil {
			continue
		}
		expr := namedChildren(body)
		if len(expr) == 1 && kindOf(expr[0]) == "block" {
			expr = soleReturn(expr[0])
		}
		if name := w.builtWidget(expr, 0); name != "" {
			return name
		}
	}
	return ""
}

// soleReturn is the expression of a block's one return statement, or nil when the
// block returns in more than one place or not at its top level.
func soleReturn(block *sitter.Node) []*sitter.Node {
	var ret *sitter.Node
	count := 0
	var visit func(n *sitter.Node)
	visit = func(n *sitter.Node) {
		for _, c := range namedChildren(n) {
			switch kindOf(c) {
			case "return_statement":
				count++
				ret = c
			case "function_expression", "function_body", "lambda_expression":
				continue // another function's returns
			default:
				visit(c)
			}
		}
	}
	visit(block)
	if count != 1 || childOfKind(block, "return_statement") == nil {
		return nil
	}
	return namedChildren(ret)
}

// builtWidget names the class an expression constructs, given as the sibling run
// that makes it up (`Name` then its selectors, or one const/new expression), and
// follows a `child:` argument into the widget it wraps.
func (w *walker) builtWidget(expr []*sitter.Node, depth int) string {
	if len(expr) == 0 || depth > 8 {
		return ""
	}
	var name string
	var args *sitter.Node
	switch first := expr[0]; kindOf(first) {
	case "identifier", "type_identifier":
		name = first.Utf8Text(w.src)
		for _, sel := range expr[1:] {
			if kindOf(sel) != "selector" {
				return ""
			}
			if args = firstOfKind(sel, "arguments"); args != nil {
				break
			}
		}
	case "const_object_expression", "new_expression":
		if t := childOfKind(first, "type_identifier"); t != nil {
			name = t.Utf8Text(w.src)
		}
		args = childOfKind(first, "arguments")
		if len(expr) > 1 {
			return ""
		}
	default:
		return ""
	}
	if name == "" || !isUpper(name[0]) || inlineScreens[name] {
		return ""
	}
	if child := namedArgValue(args, w.src, "child"); child != nil {
		return w.builtWidget(child, depth+1)
	}
	return name
}

// inlineScreens are the framework's page layouts: a builder that constructs one
// builds its screen inline, and the layout is no screen of the app's.
var inlineScreens = map[string]bool{"Scaffold": true, "CupertinoPageScaffold": true}

// namedArgValue is the whole value of a named argument, every node after its label.
func namedArgValue(args *sitter.Node, src []byte, label string) []*sitter.Node {
	for _, a := range namedChildren(args) {
		if kindOf(a) != "named_argument" {
			continue
		}
		kids := namedChildren(a)
		if len(kids) < 2 || kindOf(kids[0]) != "label" ||
			strings.TrimSuffix(strings.TrimSpace(kids[0].Utf8Text(src)), ":") != label {
			continue
		}
		return kids[1:]
	}
	return nil
}

// pathRefProp / pathPrefixProp carry an unresolved constant reference through to the
// repo-wide resolution pass. See resolveRoutePathRefs.
const (
	pathRefProp    = "path_ref"
	pathPrefixProp = "path_prefix"
)

// routePathArg reads a route's `path:` argument, falling back to a constant reference.
//
// `path: HomeScreen.routeName` is the dominant Flutter idiom — a screen declares its own
// path as a static const and the router refers to it — so reading only literals would
// drop most of a real app's navigation surface. The reference is carried on the fact and
// dereferenced repo-wide afterwards; one that never resolves takes its route with it,
// because a route named after a Dart constant is worse than no route at all.
func (w *walker) routePathArg(args *sitter.Node) (literal, ref string) {
	if s := stringArg(args, w.src, "path", -1); s != "" {
		return s, ""
	}
	txt := namedArgText(args, w.src, "path")
	if txt == "" {
		return "", ""
	}
	// Only a plain `Type.CONST` reference is followed. Anything else is an expression
	// whose value extraction cannot know.
	if typeName, member, ok := strings.Cut(txt, "."); ok &&
		isPlainIdentifier(typeName) && isPlainIdentifier(member) && isUpper(typeName[0]) {
		return "", txt
	}
	return "", ""
}

// emitPageRoute appends one navigation route fact.
func (w *walker) emitPageRoute(path, framework string, n *sitter.Node, extra map[string]any) {
	if path == "" {
		return
	}
	props := map[string]any{
		"language":          "dart",
		facts.PropRouteType: "page",
		facts.PropFramework: framework,
		facts.PropRole:      facts.RoleServer,
		"method":            "GET",
		"navigation":        true,
	}
	for k, v := range extra {
		if s, ok := v.(string); ok && s == "" {
			continue
		}
		if v != nil {
			props[k] = v
		}
	}
	var rels []facts.Relation
	if h, _ := props["handler"].(string); h != "" {
		rels = append(rels, facts.Relation{Kind: facts.RelHandledBy, Target: h})
	}
	w.out = append(w.out, facts.Fact{
		Kind: facts.KindRoute, Name: path, File: w.relFile, Line: lineOf(n),
		Props: props, Relations: rels,
	})
}

// composePath joins a parent mount with a child path.
//
// A child beginning with "/" is absolute and REPLACES the parent, which is go_router's
// own rule. Composing blindly would produce `/user/user/settings` for the very common
// case of a sub-route written absolute — the same trap the Scala Play-routes composer
// hit and solved with a segment-wise test.
func composePath(prefix, path string) string {
	switch {
	case path == "":
		return prefix
	case strings.HasPrefix(path, "/"):
		return path
	case prefix == "":
		return "/" + path
	case strings.HasSuffix(prefix, "/"):
		return prefix + path
	default:
		return prefix + "/" + path
	}
}
