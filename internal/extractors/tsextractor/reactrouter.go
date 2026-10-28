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
// splat (`*`) and a v5 regex path (`/(a|b)?`) name no one page and are skipped.
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
	routeTags, creators := reactRouterImports(kinds, root, src)
	if len(routeTags) == 0 && len(creators) == 0 {
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
	return rr.out
}

// reactRouterImports returns the local names `Route` and the route-object creators
// are imported under from react-router.
func reactRouterImports(kinds *tsutil.KindTable, root *sitter.Node, src []byte) (routeTags, creators map[string]bool) {
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
			}
		}
	}
	return routeTags, creators
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
		if open != nil && rr.routeTags[rr.tagName(open)] {
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
			continue
		}
		rr.objectRoutes(children, rr.emit(parent, path, hasPath, index, handler, obj))
	}
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
	if strings.ContainsAny(full, "*()?") {
		return full
	}
	key := full + "\x00" + handler
	if rr.seen[key] {
		return full
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
	return full
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
