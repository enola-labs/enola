// Angular's router: the second half of what an Angular application declares about
// itself, and the half a per-file pass cannot read.
//
// A route array states a path fragment; the prefix it hangs under is decided
// somewhere else — by a parent's `children:`, by the `path:` of the entry whose
// `loadChildren` lazily loads the module the array belongs to, or by nothing at all
// when the array is a library's and no application mounts it. Measured before this
// existed: the corpus declares 284 `RouterModule.forRoot`, 242 `forChild`, 262
// `provideRouter` and 350 `loadChildren` sites, and produced zero route facts.
//
// The shape is the one three extractors already share — Express mounts
// (routermount.go), gorilla/mux subrouters (goextractor/routeprefix.go, v125) and
// Axum's .nest() (rustextractor/axum.go, v130): every file reports the route arrays
// it declares and the mounts it writes, and a repo-wide pass walks outward from the
// application roots, emitting each route at its true runtime path.
//
// Three properties hold it to that standard:
//
//   - Reachable routes only. An array no root reaches emits NOTHING — never a
//     fragment. A component library whose only router call is forChild therefore
//     contributes no routes, which is the correct reading of a library.
//   - One candidate required. A lazy `loadChildren` names a module, not an array;
//     the array is found by an exact export name, else by the target file having
//     exactly one forChild array, else by exactly one of that file's own imports
//     having one. Anything ambiguous is counted, not guessed.
//   - Page routes, never endpoints. Every fact carries type=page, so it is excluded
//     from cross-repo HTTP matching and can never surface as an unused route — the
//     same contract Ember's router map and Nuxt's pages have.
package tsextractor

import (
	"bytes"
	"sort"
	"strconv"
	"strings"

	sitter "github.com/enola-labs/enola/internal/treesitter"

	"github.com/enola-labs/enola/internal/extractors/tsutil"
	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
)

// angularRouteRef names one route array: the file that declares it and the binding
// it is bound to. Two files may each declare `routes`, so neither half identifies it.
type angularRouteRef struct {
	file string
	name string
}

// angularLazyRef is a resolved `() => import('…')` target: the repo-relative file it
// names, and the export read off the `.then(m => m.X)` that follows, if any.
type angularLazyRef struct {
	file   string
	export string
}

// angularRouteEntry is one object in a route array, with everything that decides
// what it contributes: its own path fragment, what serves it, and what hangs below.
type angularRouteEntry struct {
	path          string
	line          int
	component     string
	lazyComponent *angularLazyRef
	guards        []string
	children      []angularRouteEntry
	childrenRef   *angularRouteRef
	lazyChildren  *angularLazyRef
	redirect      bool
	// declaresLazy is set when the entry writes a loadChildren whose target did not
	// resolve. Without it such an entry is indistinguishable from a redirect, and one
	// workspace's 64 unreadable mounts were being reported as configuration.
	declaresLazy bool
	// nonLiteralPath marks a path this pass refuses to read: an expression whose text
	// is not a URL and which no constant in the repository resolves.
	nonLiteralPath bool
	// pathRef is a path written as `SomeConst.Member`. The literal lives in another
	// declaration, so it is resolved during the repo-wide walk rather than here.
	pathRef *angularConstRef
}

// angularConstRef is a qualified constant reference: `DemoRoute.GettingStarted`.
type angularConstRef struct {
	qualifier string
	member    string
}

// angularRouterFile is what one file contributes to the repo-wide walk.
type angularRouterFile struct {
	relFile string
	dir     string
	arrays  map[string][]angularRouteEntry
	// roots are arrays mounted at the application root: RouterModule.forRoot(x) and
	// provideRouter(x). Both may name an array another file declares.
	roots []angularRouteRef
	// forChild are arrays this file passes to RouterModule.forChild — the candidates
	// a lazy loadChildren pointing at this file resolves to.
	forChild []string
	// imports maps a local name to the file and export it names, for resolving an
	// identifier that stands for an array or a component in another file. The export
	// matters: `import routes from './app.routes'` names the file's DEFAULT export,
	// which is a different binding from one called `routes`.
	imports map[string]angularLazyRef
	// importedFiles are the internal files this one imports, for the single hop a
	// lazy module that delegates its routing to a sibling routing module needs.
	importedFiles []string
	// isModule marks a file declaring an @NgModule. Such a file is kept even when it
	// declares no routes of its own, because it is what a lazy loadChildren names:
	// the routing it mounts is routinely one import away, in a sibling routing module.
	isModule bool
	// modules names the @NgModule classes this file declares, so a lazy mount that
	// resolves to a package barrel can be followed to the file that actually
	// declares the module the barrel re-exports.
	modules []string
	// constants holds string-valued members of the enums and `as const` objects this
	// file declares, so a path written `DemoRoute.GettingStarted` can be folded to the
	// literal it names. Derivation, not inference: the value is read off a
	// single-assignment declaration, and a member with a computed value is absent.
	constants map[string]map[string]string
}

// angularRoutingTokens gate the router walk. A route array is declared with the
// router's own vocabulary — the `Routes`/`Route[]` type annotation, or one of the
// three calls that mount an array — and walking every other file's whole tree a
// second time cost more than the pass itself on the largest repository.
var angularRoutingTokens = [][]byte{
	[]byte("@angular/router"), []byte("forRoot("), []byte("forChild("),
	[]byte("provideRouter("), []byte("loadChildren"),
	// An NgModule file routinely declares no routing of its own and exists in this
	// pass only as the thing a lazy loadChildren names, one import away from the
	// routing module that holds the array.
	[]byte("NgModule"),
	// A file that declares nothing but a constants map is in this pass for one
	// reason: it holds the literals the route paths name. Both forms it can take
	// are spelled with one of these.
	[]byte("as const"),
	[]byte("enum "),
}

// declaresAngularRouting reports whether a file mentions the router at all.
func declaresAngularRouting(src []byte) bool {
	for _, tok := range angularRoutingTokens {
		if bytes.Contains(src, tok) {
			return true
		}
	}
	return false
}

// collectAngularRouterFile reads one file's route arrays, router calls and imports.
// Returns nil when the file declares nothing the router pass could use.
func collectAngularRouterFile(kinds *tsutil.KindTable, root *sitter.Node, ctx *extractCtx, aliases map[string]tsAlias) *angularRouterFile {
	f := &angularRouterFile{
		relFile:   ctx.relFile,
		dir:       ctx.dir,
		arrays:    map[string][]angularRouteEntry{},
		imports:   map[string]angularLazyRef{},
		constants: map[string]map[string]string{},
	}
	collectAngularImportFiles(kinds, root, ctx, aliases, f)

	inline := 0
	defaultAlias := ""
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil {
			return
		}
		switch kindOf(kinds, n) {
		case "export_statement":
			// `export default [ … ]` is a route array with no name, and
			// `export default routes` renames one that has a name. A lazy import with
			// no `.then(m => m.X)` names exactly this binding.
			if !hasChildKind(kinds, n, "default") {
				break
			}
			if arr := angularArrayValue(kinds, n); arr != nil && looksLikeRouteArray(kinds, arr, ctx.src) {
				f.arrays[angularDefaultExport] = angularRouteEntries(kinds, arr, ctx, f)
			} else if id := findChildByKind(kinds, n, "identifier"); id != nil {
				defaultAlias = nodeText(id, ctx.src)
			}
		case "enum_declaration":
			if name := findChildByKind(kinds, n, "identifier"); name != nil {
				if members := angularEnumMembers(kinds, n, ctx.src); len(members) > 0 {
					f.constants[nodeText(name, ctx.src)] = members
				}
			}
		case "variable_declarator":
			// `const routes: Routes = [ … ]`. The type annotation is not required:
			// an array of objects carrying `path:` is a route array whatever it was
			// annotated, and plenty are annotated nothing at all.
			name := n.ChildByFieldName("name")
			if name == nil {
				break
			}
			value := n.ChildByFieldName("value")
			if arr := angularArrayValue(kinds, value); arr != nil && looksLikeRouteArray(kinds, arr, ctx.src) {
				f.arrays[nodeText(name, ctx.src)] = angularRouteEntries(kinds, arr, ctx, f)
				break
			}
			// `const DemoRoute = {GettingStarted: '/getting-started', …} as const`,
			// which is how one application spells every one of its route paths.
			if obj := angularConstObject(kinds, value); obj != nil {
				if members := angularStringMembers(kinds, obj, ctx.src); len(members) > 0 {
					f.constants[nodeText(name, ctx.src)] = members
				}
			}
		case "decorator":
			if name, _ := decoratorNameArgs(kinds, n, ctx.src); name == "NgModule" {
				f.isModule = true
				if cls := angularDecoratedClassName(kinds, n, ctx.src); cls != "" {
					f.modules = append(f.modules, cls)
				}
			}
		case "call_expression":
			kind, arg := angularRouterCall(kinds, n, ctx.src)
			if kind == "" || arg == nil {
				break
			}
			ref, ok := angularArrayArg(kinds, arg, ctx, f, &inline)
			if !ok {
				break
			}
			switch kind {
			case "forRoot", "provideRouter":
				f.roots = append(f.roots, ref)
			case "forChild":
				if ref.file == f.relFile {
					f.forChild = append(f.forChild, ref.name)
				}
			}
		}
		for i := range n.ChildCount() {
			walk(n.Child(i))
		}
	}
	walk(root)

	if defaultAlias != "" {
		if entries, ok := f.arrays[defaultAlias]; ok {
			f.arrays[angularDefaultExport] = entries
		}
	}

	if len(f.arrays) == 0 && len(f.roots) == 0 && len(f.constants) == 0 && !f.isModule {
		return nil
	}
	return f
}

// collectAngularImportFiles records where each imported name is declared, and which
// internal files this one imports.
func collectAngularImportFiles(kinds *tsutil.KindTable, root *sitter.Node, ctx *extractCtx, aliases map[string]tsAlias, f *angularRouterFile) {
	seen := map[string]bool{}
	for i := range root.ChildCount() {
		child := root.Child(i)
		if kindOf(kinds, child) != "import_statement" {
			continue
		}
		source := findChildByKind(kinds, child, "string")
		if source == nil {
			continue
		}
		resolved, isExternal := resolveImportPath(strings.Trim(nodeText(source, ctx.src), `"'`), ctx.dir, aliases)
		if isExternal {
			continue
		}
		file, _, ok := resolveModuleFile(resolved, ctx.knownFiles)
		if !ok {
			continue
		}
		if !seen[file] {
			seen[file] = true
			f.importedFiles = append(f.importedFiles, file)
		}
		clause := findChildByKind(kinds, child, "import_clause")
		if clause == nil {
			continue
		}
		// `import routes from './app.routes'` — the local name stands for the file's
		// default export, which is how one large application spells its root routes.
		if def := findChildByKind(kinds, clause, "identifier"); def != nil {
			f.imports[nodeText(def, ctx.src)] = angularLazyRef{file: file, export: angularDefaultExport}
		}
		named := findChildByKind(kinds, clause, "named_imports")
		if named == nil {
			continue
		}
		for j := range named.ChildCount() {
			spec := named.Child(j)
			if kindOf(kinds, spec) != "import_specifier" {
				continue
			}
			nameNode := spec.ChildByFieldName("name")
			if nameNode == nil {
				continue
			}
			local := nodeText(nameNode, ctx.src)
			if alias := spec.ChildByFieldName("alias"); alias != nil {
				local = nodeText(alias, ctx.src)
			}
			f.imports[local] = angularLazyRef{file: file, export: nodeText(nameNode, ctx.src)}
		}
	}
}

// angularRouterCall recognises the three calls that mount a route array, and returns
// the mount kind with the argument node holding the array.
func angularRouterCall(kinds *tsutil.KindTable, call *sitter.Node, src []byte) (string, *sitter.Node) {
	fn := call.ChildByFieldName("function")
	if fn == nil {
		return "", nil
	}
	name := nodeText(fn, src)
	switch {
	case strings.HasSuffix(name, "RouterModule.forRoot"), name == "forRoot":
		name = "forRoot"
	case strings.HasSuffix(name, "RouterModule.forChild"), name == "forChild":
		name = "forChild"
	case name == "provideRouter":
		name = "provideRouter"
	default:
		return "", nil
	}
	args := call.ChildByFieldName("arguments")
	if args == nil {
		return "", nil
	}
	for i := range args.ChildCount() {
		a := args.Child(i)
		switch kindOf(kinds, a) {
		case "array", "identifier":
			return name, a
		}
	}
	return "", nil
}

// angularArrayArg resolves a router call's argument to the array it names: an inline
// literal becomes an anonymous array of this file's, an identifier resolves locally
// or through the file's imports.
func angularArrayArg(kinds *tsutil.KindTable, arg *sitter.Node, ctx *extractCtx, f *angularRouterFile, inline *int) (angularRouteRef, bool) {
	switch kindOf(kinds, arg) {
	case "array":
		*inline++
		name := "#inline" + strconv.Itoa(*inline)
		f.arrays[name] = angularRouteEntries(kinds, arg, ctx, f)
		return angularRouteRef{file: f.relFile, name: name}, true
	case "identifier":
		return angularNameRef(f, nodeText(arg, ctx.src)), true
	}
	return angularRouteRef{}, false
}

// angularDefaultExport is the name a file's default export is filed under, so a
// lazy import with no `.then(m => m.X)` has something to resolve to.
const angularDefaultExport = "#default"

// angularEnumMembers reads the string-valued members of a TypeScript enum.
func angularEnumMembers(kinds *tsutil.KindTable, enum *sitter.Node, src []byte) map[string]string {
	body := findChildByKind(kinds, enum, "enum_body")
	if body == nil {
		return nil
	}
	out := map[string]string{}
	for i := range body.ChildCount() {
		member := body.Child(i)
		if kindOf(kinds, member) != "enum_assignment" {
			continue
		}
		name := member.ChildByFieldName("name")
		value := member.ChildByFieldName("value")
		if name == nil || value == nil {
			continue
		}
		if k := kindOf(kinds, value); k != "string" && k != "template_string" {
			continue
		}
		out[strings.Trim(nodeText(name, src), `"'`)] = strings.Trim(nodeText(value, src), "\"'`")
	}
	return out
}

// angularConstObject finds an object literal behind an `as const` or a plain
// assignment, for a constants map written as an object rather than an enum.
func angularConstObject(kinds *tsutil.KindTable, n *sitter.Node) *sitter.Node {
	for depth := 0; n != nil && depth < 4; depth++ {
		switch kindOf(kinds, n) {
		case "object":
			return n
		case "as_expression", "satisfies_expression", "parenthesized_expression":
			var next *sitter.Node
			for i := range n.ChildCount() {
				switch c := n.Child(i); kindOf(kinds, c) {
				case "object", "as_expression", "satisfies_expression", "parenthesized_expression":
					next = c
				}
			}
			if next == nil {
				return nil
			}
			n = next
		default:
			return nil
		}
	}
	return nil
}

// angularStringMembers reads the string-valued properties of an object literal.
func angularStringMembers(kinds *tsutil.KindTable, obj *sitter.Node, src []byte) map[string]string {
	out := map[string]string{}
	for i := range obj.ChildCount() {
		pair := obj.Child(i)
		if kindOf(kinds, pair) != "pair" {
			continue
		}
		k := pair.ChildByFieldName("key")
		v := pair.ChildByFieldName("value")
		if k == nil || v == nil {
			continue
		}
		if kv := kindOf(kinds, v); kv != "string" && kv != "template_string" {
			continue
		}
		text := strings.Trim(nodeText(v, src), "\"'`")
		if strings.Contains(text, "${") {
			continue
		}
		out[strings.Trim(nodeText(k, src), `"'`)] = text
	}
	return out
}

// angularResolveConstPath folds a `Const.Member` path to the literal it names,
// through this file's own declarations or the file it imported the constant from.
func angularResolveConstPath(byFile map[string]*angularRouterFile, f *angularRouterFile, ref *angularConstRef) (string, bool) {
	if members, ok := f.constants[ref.qualifier]; ok {
		if v, ok := members[ref.member]; ok {
			return v, true
		}
	}
	imp, ok := f.imports[ref.qualifier]
	if !ok {
		return "", false
	}
	owner := byFile[imp.file]
	if owner == nil {
		return "", false
	}
	name := imp.export
	if name == angularDefaultExport {
		name = ref.qualifier
	}
	if members, ok := owner.constants[name]; ok {
		if v, ok := members[ref.member]; ok {
			return v, true
		}
	}
	return "", false
}

// angularDecoratedClassName returns the name of the class a decorator annotates.
// The decorator is a sibling of the class inside an export statement, or a sibling
// of the class declaration itself.
func angularDecoratedClassName(kinds *tsutil.KindTable, dec *sitter.Node, src []byte) string {
	parent := dec.Parent()
	if parent == nil {
		return ""
	}
	for i := range parent.ChildCount() {
		c := parent.Child(i)
		switch kindOf(kinds, c) {
		case "class_declaration", "abstract_class_declaration", "class":
			if name := findChildByKind(kinds, c, "type_identifier"); name != nil {
				return nodeText(name, src)
			}
		}
	}
	return ""
}

// angularArrayValue finds the array literal a node stands for, descending through
// the wrappers TypeScript allows around one: `[…] as Routes`, `[…] satisfies
// Routes`, and parentheses. Without this an `export default [ … ] as Routes` — the
// form a routes file exported as a module's default uses — is not an array at all
// to a direct-child search, and every route behind that lazy import goes missing.
func angularArrayValue(kinds *tsutil.KindTable, n *sitter.Node) *sitter.Node {
	for depth := 0; n != nil && depth < 4; depth++ {
		switch kindOf(kinds, n) {
		case "array":
			return n
		case "as_expression", "satisfies_expression", "parenthesized_expression", "export_statement":
			var next *sitter.Node
			for i := range n.ChildCount() {
				c := n.Child(i)
				switch kindOf(kinds, c) {
				case "array", "as_expression", "satisfies_expression", "parenthesized_expression":
					next = c
				}
			}
			if next == nil {
				return nil
			}
			n = next
		default:
			return nil
		}
	}
	return nil
}

// angularNameRef resolves an identifier to the array it names, in this file or in
// the file it was imported from.
func angularNameRef(f *angularRouterFile, name string) angularRouteRef {
	if imp, ok := f.imports[name]; ok {
		return angularRouteRef{file: imp.file, name: imp.export}
	}
	return angularRouteRef{file: f.relFile, name: name}
}

// looksLikeRouteArray reports whether an array literal holds route objects. A route
// object is one carrying `path:` — the single key every Angular route has, including
// the empty-path and wildcard forms.
func looksLikeRouteArray(kinds *tsutil.KindTable, arr *sitter.Node, src []byte) bool {
	for i := range arr.ChildCount() {
		if obj := angularRouteObject(kinds, arr.Child(i), src); obj != nil {
			return true
		}
	}
	return false
}

// angularRouteObject returns the object literal an array element states a route
// with, unwrapping a single-argument factory call around it.
//
// One component library writes every route as `route({path: …, loadComponent: …})`,
// a helper that adds a page tab and returns the route. The object inside is the
// route in every sense that matters here, and requiring a bare literal read its
// whole application as having none. The unwrap is one level deep and still demands
// a `path:` key, so an ordinary call in an ordinary array is not mistaken for one.
func angularRouteObject(kinds *tsutil.KindTable, el *sitter.Node, src []byte) *sitter.Node {
	switch kindOf(kinds, el) {
	case "object":
	case "call_expression":
		args := el.ChildByFieldName("arguments")
		if args == nil {
			return nil
		}
		var only *sitter.Node
		for i := range args.ChildCount() {
			if a := args.Child(i); kindOf(kinds, a) == "object" {
				if only != nil {
					return nil
				}
				only = a
			}
		}
		el = only
	default:
		return nil
	}
	if el == nil || objectPropValue(kinds, el, src, "path") == nil {
		return nil
	}
	return el
}

// angularRouteEntries parses an array literal into route entries.
func angularRouteEntries(kinds *tsutil.KindTable, arr *sitter.Node, ctx *extractCtx, f *angularRouterFile) []angularRouteEntry {
	var out []angularRouteEntry
	for i := range arr.ChildCount() {
		el := angularRouteObject(kinds, arr.Child(i), ctx.src)
		if el == nil {
			continue
		}
		pathNode := objectPropValue(kinds, el, ctx.src, "path")
		e := angularRouteEntry{line: int(el.StartPosition().Row) + 1}
		switch kindOf(kinds, pathNode) {
		case "string", "template_string":
			e.path = strings.Trim(nodeText(pathNode, ctx.src), "\"'`")
			if strings.Contains(e.path, "${") {
				e.nonLiteralPath = true
			}
		case "member_expression":
			// `path: DemoRoute.GettingStarted`. The literal is a declaration away;
			// the walk resolves it, and refuses the entry if it cannot.
			obj := pathNode.ChildByFieldName("object")
			prop := pathNode.ChildByFieldName("property")
			if obj != nil && prop != nil && kindOf(kinds, obj) == "identifier" {
				e.pathRef = &angularConstRef{qualifier: nodeText(obj, ctx.src), member: nodeText(prop, ctx.src)}
			} else {
				e.nonLiteralPath = true
			}
		default:
			// An expression whose text is not a URL.
			e.nonLiteralPath = true
		}
		if v := objectPropValue(kinds, el, ctx.src, "component"); v != nil && kindOf(kinds, v) == "identifier" {
			e.component = nodeText(v, ctx.src)
		}
		if v := objectPropValue(kinds, el, ctx.src, "loadComponent"); v != nil {
			e.lazyComponent = angularLazyTarget(kinds, v, ctx, f)
		}
		if v := objectPropValue(kinds, el, ctx.src, "loadChildren"); v != nil {
			e.declaresLazy = true
			e.lazyChildren = angularLazyTarget(kinds, v, ctx, f)
		}
		if objectPropValue(kinds, el, ctx.src, "redirectTo") != nil {
			e.redirect = true
		}
		e.guards = angularGuardNames(kinds, el, ctx.src)
		if v := objectPropValue(kinds, el, ctx.src, "children"); v != nil {
			switch kindOf(kinds, v) {
			case "array":
				e.children = angularRouteEntries(kinds, v, ctx, f)
			case "identifier":
				ref := angularNameRef(f, nodeText(v, ctx.src))
				e.childrenRef = &ref
			}
		}
		out = append(out, e)
	}
	return out
}

// angularGuardNames returns the identifiers named by a route's guard and resolver
// properties, which are the classes and functions the router runs before activating it.
func angularGuardNames(kinds *tsutil.KindTable, el *sitter.Node, src []byte) []string {
	var out []string
	for _, key := range []string{"canActivate", "canActivateChild", "canDeactivate", "canMatch", "canLoad"} {
		v := objectPropValue(kinds, el, src, key)
		if v == nil || kindOf(kinds, v) != "array" {
			continue
		}
		for i := range v.ChildCount() {
			if g := v.Child(i); kindOf(kinds, g) == "identifier" {
				out = append(out, nodeText(g, src))
			}
		}
	}
	return out
}

// angularLazyTarget resolves `() => import('./x').then(m => m.Y)` to the file it
// names and the export it reads. A dynamic import path (a template with an
// interpolation, a variable) resolves to nothing.
func angularLazyTarget(kinds *tsutil.KindTable, n *sitter.Node, ctx *extractCtx, f *angularRouterFile) *angularLazyRef {
	var importArg, member string
	var walk func(x *sitter.Node)
	walk = func(x *sitter.Node) {
		if x == nil {
			return
		}
		if kindOf(kinds, x) == "call_expression" {
			fn := x.ChildByFieldName("function")
			if fn != nil && nodeText(fn, ctx.src) == "import" {
				if args := x.ChildByFieldName("arguments"); args != nil {
					for i := range args.ChildCount() {
						a := args.Child(i)
						if k := kindOf(kinds, a); k == "string" || k == "template_string" {
							importArg = strings.Trim(nodeText(a, ctx.src), "\"'`")
						}
					}
				}
			}
		}
		// `.then(m => m.AdminModule)` — the member access names the export.
		if kindOf(kinds, x) == "member_expression" {
			if prop := x.ChildByFieldName("property"); prop != nil {
				if obj := x.ChildByFieldName("object"); obj != nil && kindOf(kinds, obj) == "identifier" {
					member = nodeText(prop, ctx.src)
				}
			}
		}
		for i := range x.ChildCount() {
			walk(x.Child(i))
		}
	}
	walk(n)

	if importArg == "" || strings.Contains(importArg, "${") {
		return nil
	}
	resolved, isExternal := resolveImportPath(importArg, f.dir, ctx.aliases)
	if isExternal {
		return nil
	}
	file, _, ok := resolveModuleFile(resolved, ctx.knownFiles)
	if !ok {
		return nil
	}
	return &angularLazyRef{file: file, export: member}
}

// composeAngularRoutes walks outward from every application root and emits each
// reachable route at its full runtime path.
func composeAngularRoutes(files []*angularRouterFile) ([]facts.Fact, angularCounts) {
	var counts angularCounts
	byFile := map[string]*angularRouterFile{}
	names := make([]string, 0, len(files))
	for _, f := range files {
		if f == nil {
			continue
		}
		byFile[f.relFile] = f
		names = append(names, f.relFile)
	}
	sort.Strings(names)

	var out []facts.Fact
	visited := map[string]bool{}
	seen := map[string]bool{}

	// walkRef enters one route array; walkEntries walks a list of entries already in
	// hand. They are separate because an array is reached by REFERENCE (a root, a
	// `children: someRoutes`, a lazy module) while a nested `children: [ … ]` is
	// reached by value, and both have to continue the same way — a lazy mount nested
	// three levels down resolves exactly as one at the top.
	var walkRef func(ref angularRouteRef, prefix string, composed bool)
	var walkEntries func(f *angularRouterFile, entries []angularRouteEntry, prefix string, composed bool)

	walkRef = func(ref angularRouteRef, prefix string, composed bool) {
		key := ref.file + "\x00" + ref.name + "\x00" + prefix
		if visited[key] {
			return // a router that reaches itself is not a shape to follow forever
		}
		visited[key] = true

		f := byFile[ref.file]
		if f == nil {
			return
		}
		entries, ok := f.arrays[ref.name]
		if !ok {
			return
		}
		if len(entries) == 0 {
			// The array is there and holds nothing: the routes are supplied at
			// runtime, through a ROUTES provider or a factory. Naming that is the
			// difference between a route enola could not read and one that is not
			// written down anywhere to read.
			counts.miss("runtime_route_provider")
			return
		}
		walkEntries(f, entries, prefix, composed)
	}

	walkEntries = func(f *angularRouterFile, entries []angularRouteEntry, prefix string, composed bool) {
		for _, e := range entries {
			if e.pathRef != nil {
				if lit, ok := angularResolveConstPath(byFile, f, e.pathRef); ok {
					e.path = lit
				} else {
					e.nonLiteralPath = true
				}
			}
			full := facts.JoinRoutePath(prefix, e.path)
			emitAngularRoute(&out, f, e, full, composed, &counts, seen)
			if e.nonLiteralPath {
				continue // nothing below it has a path to hang under either
			}

			if len(e.children) > 0 {
				walkEntries(f, e.children, full, composed)
			}
			if e.childrenRef != nil {
				walkRef(*e.childrenRef, full, composed || e.childrenRef.file != f.relFile)
			}
			if e.lazyChildren != nil {
				if target, ok := resolveAngularLazyArray(byFile, e.lazyChildren); ok {
					walkRef(target, full, true)
				} else {
					counts.miss("unmounted_lazy_module")
				}
			}
		}
	}

	for _, name := range names {
		for _, root := range byFile[name].roots {
			walkRef(root, "", root.file != name)
		}
	}
	return out, counts
}

// resolveAngularLazyArray finds the route array a lazy module reference mounts.
//
// The reference names a MODULE, not an array, so three readings are tried in
// decreasing order of certainty and each requires exactly one candidate: an array
// exported under that name, the target file's single forChild array, or — for the
// very common module that delegates its routing to a sibling routing module — the
// single one among the files it imports.
func resolveAngularLazyArray(byFile map[string]*angularRouterFile, ref *angularLazyRef) (angularRouteRef, bool) {
	target := byFile[ref.file]
	if target == nil {
		// The mount resolved to a file this pass kept nothing for — a package barrel
		// that only re-exports. Its export name is still a lead worth following.
		return angularModuleByName(byFile, ref.export)
	}
	want := ref.export
	if want == "" {
		// `import('./admin/routes')` with no `.then` names the module's default
		// export, which for a routes file IS the array.
		want = angularDefaultExport
	}
	if _, ok := target.arrays[want]; ok {
		return angularRouteRef{file: target.relFile, name: want}, true
	}
	if len(target.forChild) == 1 {
		return angularRouteRef{file: target.relFile, name: target.forChild[0]}, true
	}
	if len(target.forChild) > 1 {
		return angularRouteRef{}, false
	}
	if ref, ok := angularForChildAmongImports(byFile, target); ok {
		return ref, true
	}
	return angularModuleByName(byFile, ref.export)
}

// angularModuleByName follows a lazily-mounted module's NAME to the file that
// declares it, for the case the import resolved to a package barrel that only
// re-exports the module — `import('@acme/admin').then(m => m.AdminModule)`. One
// candidate required, as everywhere else in this pass.
func angularModuleByName(byFile map[string]*angularRouterFile, export string) (angularRouteRef, bool) {
	if export == "" {
		return angularRouteRef{}, false
	}
	var declaring []*angularRouterFile
	for _, name := range sortedRouterFiles(byFile) {
		f := byFile[name]
		for _, m := range f.modules {
			if m == export {
				declaring = append(declaring, f)
				break
			}
		}
	}
	if len(declaring) != 1 {
		return angularRouteRef{}, false
	}
	owner := declaring[0]
	if len(owner.forChild) == 1 {
		return angularRouteRef{file: owner.relFile, name: owner.forChild[0]}, true
	}
	return angularForChildAmongImports(byFile, owner)
}

// angularForChildAmongImports finds the single forChild array among the files a
// module imports — the shape of a module that delegates its routing to a sibling
// routing module.
func angularForChildAmongImports(byFile map[string]*angularRouterFile, from *angularRouterFile) (angularRouteRef, bool) {
	var found []angularRouteRef
	for _, imported := range from.importedFiles {
		sib := byFile[imported]
		if sib == nil || len(sib.forChild) != 1 {
			continue
		}
		found = append(found, angularRouteRef{file: sib.relFile, name: sib.forChild[0]})
	}
	if len(found) == 1 {
		return found[0], true
	}
	return angularRouteRef{}, false
}

// sortedRouterFiles returns the router files' names in a fixed order, so a search
// across them cannot depend on map iteration.
func sortedRouterFiles(byFile map[string]*angularRouterFile) []string {
	names := make([]string, 0, len(byFile))
	for name := range byFile {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// emitAngularRoute writes one page route, with an edge to whatever serves it.
func emitAngularRoute(out *[]facts.Fact, f *angularRouterFile, e angularRouteEntry, full string, composed bool, counts *angularCounts, seen map[string]bool) {
	if e.nonLiteralPath {
		counts.miss("non_literal_path")
		return
	}
	if e.declaresLazy && e.lazyChildren == nil {
		// The entry mounts a module this snapshot could not resolve — a workspace
		// package behind a path alias, or a dynamic import. It is a missing mount,
		// not configuration, and the two must not be counted as one thing.
		counts.miss("unresolved_lazy_import")
		return
	}
	if e.component == "" && e.lazyComponent == nil {
		if len(e.children) > 0 || e.childrenRef != nil || e.lazyChildren != nil {
			// A mount point, not a page: nothing renders at it, and the routes that
			// do are the children about to be walked. Its own path reappears among
			// them whenever one of them has an empty path, which is how an Angular
			// application spells "the page at the mount".
			return
		}
		// A redirect, or an entry carrying only data, a title or a matcher. It
		// configures the router without being a page anything renders.
		counts.miss("redirect_or_config")
		return
	}
	// Two roots may reach the same array; the route is one route either way.
	key := f.relFile + "\x00" + full + "\x00" + strconv.Itoa(e.line)
	if seen[key] {
		return
	}
	seen[key] = true
	counts.resolved++

	props := map[string]any{
		"method":            "GET",
		"type":              "page",
		"language":          "typescript",
		facts.PropFramework: AngularFramework,
		facts.PropSource:    facts.RouteSourceAngularRouter,
	}
	if composed {
		// The prefix came from another file, so the fact records that its path was
		// composed rather than read off one line.
		props["mount_composed"] = true
	}
	rels := []facts.Relation{{Kind: facts.RelDeclares, Target: f.dir}}
	if target, ok := angularRouteTarget(f, e); ok {
		props["handler"] = target
		rels = append(rels, facts.Relation{Kind: facts.RelHandledBy, Target: target})
	} else if e.lazyComponent != nil {
		// `loadComponent: () => import('./page')` names the file's DEFAULT export,
		// which the import statement does not spell. The file is recorded here and
		// the class it declares is resolved once every symbol is in hand — without
		// it, a page component reachable only through a lazy route has no inbound
		// edge at all and reads as code nothing renders.
		props["angular_lazy_component_file"] = e.lazyComponent.file
	}
	for _, g := range e.guards {
		if target, ok := angularSymbolRef(f, g); ok {
			rels = append(rels, facts.Relation{Kind: facts.RelDependsOn, Target: target})
		}
	}
	*out = append(*out, facts.Fact{
		Kind:      facts.KindRoute,
		Name:      full,
		File:      f.relFile,
		Line:      e.line,
		Props:     props,
		Relations: rels,
	})
}

// angularRouteTarget names the symbol that serves a route: the component it states,
// or the one its loadComponent lazily imports.
func angularRouteTarget(f *angularRouterFile, e angularRouteEntry) (string, bool) {
	if e.component != "" {
		return angularSymbolRef(f, e.component)
	}
	if e.lazyComponent != nil && e.lazyComponent.export != "" {
		return factpath.Dir(e.lazyComponent.file) + "." + e.lazyComponent.export, true
	}
	return "", false
}

// angularSymbolRef names the symbol an identifier in this file refers to, through
// the file's own imports or its own declarations.
func angularSymbolRef(f *angularRouterFile, name string) (string, bool) {
	if imp, ok := f.imports[name]; ok {
		return factpath.Dir(imp.file) + "." + name, true
	}
	return f.dir + "." + name, true
}
