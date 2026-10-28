package tsextractor

import (
	"bytes"
	"strings"

	"github.com/enola-labs/enola/internal/extractors/tsutil"
	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/tree-sitter/go-tree-sitter"
)

// React Router: the pages an SPA declares, so a test's cy.visit or page.goto into it
// has a route to exercise. Both forms are read:
//
//   - JSX, `<Route path="/signup" element={<SignUp />}>` (v6) or with a `component`
//     or a child element (v5), nested routes inside it;
//   - route objects, `createBrowserRouter([{ path, element, children }])` and its
//     hash, memory and useRoutes siblings.
//
// The evidence is the import, not a package.json flag: `Route` or the creator must
// come from react-router itself, so an app of a monorepo is read wherever its
// manifest is, and a `Route` of another library is never one. Nesting follows the
// library: a child path without a leading slash joins its parent (v6), a path with
// one stands alone (v5 writes every nested path in full). An index route is its
// parent's path; a route with neither a path nor index only groups its children. A
// splat (`*`) and a v5 regex path (`/(a|b)`) name no one page and are skipped; an
// optional segment (`/:tag?`) names two, with and without it.
//
// Two forms reach past the library's own names. A component whose name ends in
// `Route` and is given a literal `path` (`<PrivateRoute path="/x" component={X}>`) is
// a wrapper that forwards to `<Route>`, and is read as one. A file that imports the
// `RouteObject` type builds route trees it hands to a creator elsewhere
// (`createBrowserRouter(getRouteMap())`), so the arrays its functions return and its
// consts hold are read as roots, when their objects carry a key only a route object
// has: a nav list of `{ path, label }` is not a router.
//
// JSX parses only in .tsx and .jsx, so JSX routes in a plain .js file are not read.

var reactRouterModules = map[string]bool{"react-router": true, "react-router-dom": true}

var reactRouterCreators = map[string]bool{
	"createBrowserRouter": true, "createHashRouter": true, "createMemoryRouter": true, "useRoutes": true,
}

func extractReactRouterRoutes(kinds *tsutil.KindTable, root *sitter.Node, src []byte, relFile string, aliases map[string]tsAlias) []facts.Fact {
	if !bytes.Contains(src, []byte("react-router")) {
		return nil
	}
	routeTags, creators, routeObject := reactRouterImports(kinds, root, src)
	if len(routeTags) == 0 && len(creators) == 0 && !routeObject {
		return nil
	}
	rr := &reactRouterScan{
		kinds: kinds, src: src, relFile: relFile, routeTags: routeTags, creators: creators,
		components: buildEmberImportBindings(kinds, root, src, relFile, aliases).internal,
		arrays:     map[string]*sitter.Node{},
		seen:       map[string]bool{},
	}
	rr.indexArrays(root)
	rr.walk(root, "")
	if routeObject {
		rr.routeObjectRoots(root)
	}
	return rr.out
}

// reactRouterImports returns the local names `Route` and the route-object creators
// are imported under from react-router, and whether the `RouteObject` type is.
func reactRouterImports(kinds *tsutil.KindTable, root *sitter.Node, src []byte) (routeTags, creators map[string]bool, routeObject bool) {
	routeTags, creators = map[string]bool{}, map[string]bool{}
	for i := range root.ChildCount() {
		stmt := root.Child(i)
		if kindOf(kinds, stmt) != "import_statement" {
			continue
		}
		source := findChildByKind(kinds, stmt, "string")
		if source == nil || !reactRouterModules[strings.Trim(nodeText(source, src), `"'`)] {
			continue
		}
		named := findChildByKind(kinds, findChildByKind(kinds, stmt, "import_clause"), "named_imports")
		if named == nil {
			continue
		}
		for j := range named.ChildCount() {
			spec := named.Child(j)
			if kindOf(kinds, spec) != "import_specifier" {
				continue
			}
			name := spec.ChildByFieldName("name")
			if name == nil {
				continue
			}
			local := nodeText(name, src)
			if a := spec.ChildByFieldName("alias"); a != nil {
				local = nodeText(a, src)
			}
			switch imported := nodeText(name, src); {
			case imported == "Route":
				routeTags[local] = true
			case reactRouterCreators[imported]:
				creators[local] = true
			case imported == "RouteObject":
				routeObject = true
			}
		}
	}
	return routeTags, creators, routeObject
}

type reactRouterScan struct {
	kinds      *tsutil.KindTable
	src        []byte
	relFile    string
	routeTags  map[string]bool
	creators   map[string]bool
	components map[string]string       // local name -> symbol, for the imported components a route renders
	arrays     map[string]*sitter.Node // module-scope `const routes = [...]`, for a creator given a name
	seen       map[string]bool
	out        []facts.Fact
}

func (rr *reactRouterScan) indexArrays(n *sitter.Node) {
	for i := range n.ChildCount() {
		c := n.Child(i)
		switch kindOf(rr.kinds, c) {
		case "lexical_declaration", "variable_declaration", "export_statement":
			rr.indexArrays(c)
		case "variable_declarator":
			if name, value := c.ChildByFieldName("name"), c.ChildByFieldName("value"); name != nil && value != nil && kindOf(rr.kinds, value) == "array" {
				rr.arrays[nodeText(name, rr.src)] = value
			}
		}
	}
}

func (rr *reactRouterScan) walk(n *sitter.Node, parent string) {
	switch kindOf(rr.kinds, n) {
	case "jsx_element", "jsx_self_closing_element":
		open := n
		if kindOf(rr.kinds, n) == "jsx_element" {
			open = n.ChildByFieldName("open_tag")
		}
		if open != nil && rr.isRouteTag(open) {
			parent = rr.jsxRoute(n, open, parent)
		}
	case "call_expression":
		if fn := n.ChildByFieldName("function"); fn != nil && kindOf(rr.kinds, fn) == "identifier" && rr.creators[nodeText(fn, rr.src)] {
			if args := n.ChildByFieldName("arguments"); args != nil && args.NamedChildCount() > 0 {
				rr.objectRoutes(rr.array(args.NamedChild(0)), "")
			}
		}
	}
	for i := range n.ChildCount() {
		rr.walk(n.Child(i), parent)
	}
}

// isRouteTag reports whether a JSX tag is react-router's Route, or a wrapper of it:
// a component named *Route given a literal path.
func (rr *reactRouterScan) isRouteTag(open *sitter.Node) bool {
	name := rr.tagName(open)
	if rr.routeTags[name] {
		return true
	}
	if !strings.HasSuffix(name, "Route") || strings.Contains(name, ".") {
		return false
	}
	for i := range open.NamedChildCount() {
		attr := open.NamedChild(i)
		if kindOf(rr.kinds, attr) == "jsx_attribute" && attr.NamedChildCount() > 1 &&
			nodeText(attr.NamedChild(0), rr.src) == "path" && kindOf(rr.kinds, attr.NamedChild(1)) == "string" {
			return true
		}
	}
	return false
}

// jsxRoute records one <Route> and returns the path its children nest under.
func (rr *reactRouterScan) jsxRoute(n, open *sitter.Node, parent string) string {
	var path, handler string
	hasPath, index := false, false
	for i := range open.NamedChildCount() {
		attr := open.NamedChild(i)
		if kindOf(rr.kinds, attr) != "jsx_attribute" || attr.NamedChildCount() == 0 {
			continue
		}
		var value *sitter.Node
		if attr.NamedChildCount() > 1 {
			value = attr.NamedChild(1)
		}
		switch nodeText(attr.NamedChild(0), rr.src) {
		case "path":
			if value == nil || kindOf(rr.kinds, value) != "string" {
				return parent // a computed path: neither it nor its children can be placed
			}
			path, hasPath = strings.Trim(nodeText(value, rr.src), `"'`), true
		case "index":
			index = value == nil || nodeText(value, rr.src) == "{true}"
		case "element", "component", "Component":
			if handler == "" {
				handler = rr.handler(value)
			}
		}
	}
	if handler == "" && kindOf(rr.kinds, n) == "jsx_element" {
		// v5: the page is the child element.
		for i := range n.NamedChildCount() {
			c := n.NamedChild(i)
			if k := kindOf(rr.kinds, c); k == "jsx_element" || k == "jsx_self_closing_element" {
				handler = rr.handler(c)
				break
			}
		}
	}
	return rr.emit(parent, path, hasPath, index, handler, n)
}

func (rr *reactRouterScan) objectRoutes(array *sitter.Node, parent string) {
	if array == nil {
		return
	}
	for i := range array.NamedChildCount() {
		obj := array.NamedChild(i)
		if kindOf(rr.kinds, obj) != "object" {
			continue
		}
		rr.objectRoute(obj, parent)
	}
}

// routeObjectKeys are the keys only a route object has, beside path.
var routeObjectKeys = map[string]bool{
	"element": true, "Component": true, "component": true, "lazy": true, "children": true,
	"index": true, "loader": true, "action": true, "errorElement": true, "ErrorBoundary": true,
}

// routeObjectRoots reads, in a file that imports RouteObject, the arrays its functions
// return and its consts hold. An array another route names as its children is read
// there, under its parent, and not again as a root.
func (rr *reactRouterScan) routeObjectRoots(root *sitter.Node) {
	nested := map[uint]bool{} // by start byte: tree-sitter hands out a new *Node per lookup
	var roots []*sitter.Node
	var visit func(n *sitter.Node)
	visit = func(n *sitter.Node) {
		switch kindOf(rr.kinds, n) {
		case "pair":
			if key := n.ChildByFieldName("key"); key != nil && strings.Trim(nodeText(key, rr.src), `"'`) == "children" {
				if a := rr.array(n.ChildByFieldName("value")); a != nil {
					nested[a.StartByte()] = true
				}
			}
		case "return_statement":
			if n.NamedChildCount() > 0 && kindOf(rr.kinds, n.NamedChild(0)) == "array" {
				roots = append(roots, n.NamedChild(0))
			}
		case "variable_declarator":
			if v := n.ChildByFieldName("value"); v != nil && kindOf(rr.kinds, v) == "array" {
				roots = append(roots, v)
			}
		}
		for i := range n.ChildCount() {
			visit(n.Child(i))
		}
	}
	visit(root)
	for _, a := range roots {
		if nested[a.StartByte()] {
			continue
		}
		for i := range a.NamedChildCount() {
			if obj := a.NamedChild(i); kindOf(rr.kinds, obj) == "object" && rr.hasRouteKey(obj) {
				rr.objectRoute(obj, "")
			}
		}
	}
}

func (rr *reactRouterScan) hasRouteKey(obj *sitter.Node) bool {
	for j := range obj.NamedChildCount() {
		if pair := obj.NamedChild(j); kindOf(rr.kinds, pair) == "pair" {
			if key := pair.ChildByFieldName("key"); key != nil && routeObjectKeys[strings.Trim(nodeText(key, rr.src), `"'`)] {
				return true
			}
		}
	}
	return false
}

// objectRoute records one route object and the routes nested in its children.
func (rr *reactRouterScan) objectRoute(obj *sitter.Node, parent string) {
	var path, handler string
	hasPath, index, computed := false, false, false
	var children *sitter.Node
	for j := range obj.NamedChildCount() {
		pair := obj.NamedChild(j)
		if kindOf(rr.kinds, pair) != "pair" {
			continue
		}
		key, value := pair.ChildByFieldName("key"), pair.ChildByFieldName("value")
		if key == nil || value == nil {
			continue
		}
		switch strings.Trim(nodeText(key, rr.src), `"'`) {
		case "path":
			if kindOf(rr.kinds, value) != "string" {
				computed = true
				continue
			}
			path, hasPath = strings.Trim(nodeText(value, rr.src), `"'`), true
		case "index":
			index = nodeText(value, rr.src) == "true"
		case "element", "Component", "component":
			if handler == "" {
				handler = rr.handler(value)
			}
		case "children":
			children = rr.array(value)
		}
	}
	if computed {
		return
	}
	rr.objectRoutes(children, rr.emit(parent, path, hasPath, index, handler, obj))
}

// emit records the page a route declares, if it names one, and returns the path its
// children nest under.
func (rr *reactRouterScan) emit(parent, path string, hasPath, index bool, handler string, at *sitter.Node) string {
	full := parent
	switch {
	case hasPath && strings.HasPrefix(path, "/"):
		full = facts.JoinRoutePath("", path)
	case hasPath:
		full = facts.JoinRoutePath(parent, path)
	case !index:
		return parent // a layout route: it only groups its children
	}
	if full == "" {
		full = "/"
	}
	if strings.ContainsAny(full, "*()") {
		return full
	}
	for _, page := range optionalExpansions(full) {
		rr.page(page, handler, at)
	}
	return full
}

// maxOptionalSegments bounds the pages one path expands to (2^n).
const maxOptionalSegments = 3

// optionalExpansions is every page a path with optional segments names: `/a/:b?`
// is `/a` and `/a/:b`. A `?` anywhere but a segment's end is not a page path.
func optionalExpansions(path string) []string {
	if !strings.Contains(path, "?") {
		return []string{path}
	}
	segs := strings.Split(path, "/")
	var optional []int
	for i, seg := range segs {
		if q := strings.Index(seg, "?"); q >= 0 {
			if q != len(seg)-1 || q == 0 {
				return nil
			}
			optional = append(optional, i)
		}
	}
	if len(optional) > maxOptionalSegments {
		return nil
	}
	var out []string
	for mask := 0; mask < 1<<len(optional); mask++ {
		var kept []string
		o := 0
		for i, seg := range segs {
			if o < len(optional) && optional[o] == i {
				if mask&(1<<o) != 0 {
					kept = append(kept, strings.TrimSuffix(seg, "?"))
				}
				o++
				continue
			}
			kept = append(kept, seg)
		}
		p := strings.Join(kept, "/")
		if p == "" {
			p = "/"
		}
		out = append(out, p)
	}
	return out
}

func (rr *reactRouterScan) page(full, handler string, at *sitter.Node) {
	key := full + "\x00" + handler
	if rr.seen[key] {
		return
	}
	rr.seen[key] = true
	f := facts.Fact{
		Kind: facts.KindRoute,
		Name: full,
		File: rr.relFile,
		Line: int(at.StartPosition().Row) + 1,
		Props: map[string]any{
			"method": "GET", "type": "page", "router": "react-router",
			"framework": "react-router", "language": "typescript",
		},
	}
	if handler != "" {
		f.Props["handler"] = handler
		f.Relations = []facts.Relation{{Kind: facts.RelHandledBy, Target: handler}}
	}
	rr.out = append(rr.out, f)
}

// handler is the imported component a route value renders: `<SignUp />`, `{<SignUp />}`,
// `{SignUp}`. A component declared in the file, or one from a package, is not bound.
func (rr *reactRouterScan) handler(v *sitter.Node) string {
	for v != nil && kindOf(rr.kinds, v) == "jsx_expression" && v.NamedChildCount() > 0 {
		v = v.NamedChild(0)
	}
	if v == nil {
		return ""
	}
	name := ""
	switch kindOf(rr.kinds, v) {
	case "identifier":
		name = nodeText(v, rr.src)
	case "jsx_element":
		if open := v.ChildByFieldName("open_tag"); open != nil {
			name = rr.tagName(open)
		}
	case "jsx_self_closing_element":
		name = rr.tagName(v)
	}
	if rr.routeTags[name] {
		return ""
	}
	return rr.components[name]
}

func (rr *reactRouterScan) tagName(open *sitter.Node) string {
	if name := open.ChildByFieldName("name"); name != nil {
		return nodeText(name, rr.src)
	}
	return ""
}

func (rr *reactRouterScan) array(v *sitter.Node) *sitter.Node {
	switch kindOf(rr.kinds, v) {
	case "array":
		return v
	case "identifier":
		return rr.arrays[nodeText(v, rr.src)]
	}
	return nil
}
