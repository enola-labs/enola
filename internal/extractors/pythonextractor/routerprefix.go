package pythonextractor

import (
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/tree-sitter/go-tree-sitter"
)

// FastAPI route paths are declared relative to the router they are registered on,
// and that router is mounted somewhere else entirely:
//
//	# api/v1/cognify/routers/get_cognify_router.py
//	def get_cognify_router() -> APIRouter:
//	    router = APIRouter()
//	    @router.post("/")            # <- declared as "/"
//	    async def cognify(...): ...
//
//	# api/client.py
//	app.include_router(get_cognify_router(), prefix="/api/v1/cognify")
//
// The decorator alone yields "/", which is useless downstream: the cross-repo
// linker matches client calls against route Names as paths, so a whole FastAPI
// backend collapses to a handful of bare leaves ("/", "/{id}") that match nothing
// — and its own frontend's calls then bind to whatever OTHER loaded repo happens
// to serve that shape.
//
// This file composes the true runtime path by resolving include_router mounts
// across the module graph, the Python analog of goextractor/routeprefix.go
// (mux/chi subrouters) and rustextractor/axum.go (.nest). It follows the axum
// design — routes are emitted bare during the walk and rewritten in a post-pass —
// but keys routes by the RECEIVER they were registered on (`@router.get`) rather
// than by line span, because Python hands us that name for free and one module
// may declare several routers.
//
// Safety invariant, as in both analogs: resolution is name-based and may only
// MISS a mount, never fabricate one. An unresolved or ambiguous mount leaves the
// route at its bare path, exactly as before this pass existed.
//
// Flask Blueprint url_prefix / Flask-AppBuilder route_base are deliberately NOT
// folded here: only routers built by APIRouter()/FastAPI() form groups, so Flask
// routes keep their historical bare-leaf Name (GAP-PY-06).

// pyRouteRef ties an emitted route fact to the router variable it was registered
// on. idx indexes into the fact slice the route was emitted into; extractPython
// rebases it onto the repo-wide slice before composeRouterPrefixes runs.
type pyRouteRef struct {
	idx   int
	group string
}

// pyRouterGroup is one router object: a module-level `router = APIRouter()`, or a
// factory function that builds and returns one (keyed by the function, since that
// is what include_router calls).
type pyRouterGroup struct {
	key    string // "<module>.<var>" or "<module>.<factory-fn>"
	prefix string // constructor prefix, e.g. APIRouter(prefix="/items")
	// prefixParts is a computed constructor prefix (a constant, an f-string),
	// filled into prefix by resolveRouterPrefixes. nil for a literal or none.
	prefixParts []pyPathPart
	isApp       bool // FastAPI() application root — mounts things, is never mounted
}

// pyRouterMount is one `parent.include_router(child, prefix=...)` call.
type pyRouterMount struct {
	parent    string // canonical key of the mounting router/app
	child     string // child key: canonical, dotted import target, or bare name
	childName string // child's bare name, for the unique-name fallback
	prefix    string
	// prefixParts is a computed mount prefix, as for pyRouterGroup.
	prefixParts []pyPathPart
}

// pyRouterAlias is a module-level `router = build_router()`: the variable names
// the router its factory builds, which is the group routes are keyed to. A mount
// of the variable (`app.include_router(account.router)`) resolves through it.
type pyRouterAlias struct {
	key        string // "<module>.<var>"
	target     string // the factory call's target, as childRouterKey reads it
	targetName string
}

// pyRouterTopology is one file's router wiring, collected alongside its facts.
type pyRouterTopology struct {
	relFile string
	groups  []pyRouterGroup
	mounts  []pyRouterMount
	routes  []pyRouteRef
	aliases []pyRouterAlias
	paths   []pyPathRef
	consts  []pyConst
}

// routerGroupKey maps the receiver of a route decorator (`@router.get`) to the
// router group that owns it. A receiver bound inside a function body belongs to
// that function — the factory pattern above — and is keyed by the function, which
// is the name include_router mounts. Anything else is a module-level router.
// A `self.router` receiver inside a class is the class-based controller pattern
// and is keyed by the class, as collectRouterTopology keys `self.router = ...`.
// Other dotted receivers are not tracked.
func (w *pyWalker) routerGroupKey(receiver string) string {
	if attr, ok := strings.CutPrefix(receiver, "self."); ok && len(w.typeStack) > 0 && attr != "" && !strings.Contains(attr, ".") {
		return w.module + "." + w.enclosingType() + "." + attr
	}
	if receiver == "" || strings.Contains(receiver, ".") {
		return ""
	}
	if w.funcScope != "" && w.localBound[receiver] {
		return w.funcScope
	}
	return w.module + "." + receiver
}

// collectRouterTopology walks a parsed file for router constructors and
// include_router mounts. It runs as its own pass rather than hooking the main
// walker: the wiring lives in plain module-level and function-body statements
// that the fact walk has no reason to inspect.
func collectRouterTopology(root *sitter.Node, src []byte, relFile, module string, importMap map[string]string) pyRouterTopology {
	c := &routerCollector{
		src:       src,
		module:    module,
		importMap: importMap,
		varKeys:   map[string]string{},
		topo:      pyRouterTopology{relFile: relFile},
	}
	c.walk(root)
	return c.topo
}

type routerCollector struct {
	src       []byte
	module    string
	importMap map[string]string

	classStack []string
	funcScope  string // outermost enclosing function's qualified name

	// locals holds, per enclosing function, the names it binds (parameters and
	// locals), which can never name a module constant.
	locals []map[string]bool

	// varKeys maps "<scope>\x00<var>" to the group key that variable names, so a
	// later `x.include_router(...)` resolves x to the group `x = APIRouter()` made.
	varKeys map[string]string

	topo pyRouterTopology
}

func (c *routerCollector) walk(node *sitter.Node) {
	switch kindOf(node) {
	case "class_definition":
		name := pyFuncName(node, c.src)
		c.classStack = append(c.classStack, name)
		c.walkChildren(node)
		c.classStack = c.classStack[:len(c.classStack)-1]
		return
	case "function_definition":
		saved := c.funcScope
		if c.funcScope == "" {
			// Mirrors pyWalker.funcScope: only the outermost function establishes
			// the scope, so a router built in a factory and used by a def nested
			// inside it share one key.
			c.funcScope = c.qualify(pyFuncName(node, c.src))
		}
		c.locals = append(c.locals, collectLocalBoundNames(node.ChildByFieldName("parameters"), node.ChildByFieldName("body"), c.src))
		c.walkChildren(node)
		c.locals = c.locals[:len(c.locals)-1]
		c.funcScope = saved
		return
	case "assignment":
		c.handleAssign(node)
	case "call":
		c.handleCall(node)
	}
	c.walkChildren(node)
}

func (c *routerCollector) walkChildren(node *sitter.Node) {
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		c.walk(node.Child(i))
	}
}

// qualify builds the qualified symbol name the fact walker would give a
// definition in the current class nesting, so keys computed here and by
// routerGroupKey refer to the same thing.
func (c *routerCollector) qualify(name string) string {
	if len(c.classStack) == 0 {
		return c.module + "." + name
	}
	return c.module + "." + strings.Join(append(append([]string{}, c.classStack...), name), ".")
}

// selfAttr returns X for a `self.X` assignment target inside a class, else "".
func (c *routerCollector) selfAttr(left *sitter.Node) string {
	if kindOf(left) != "attribute" || len(c.classStack) == 0 {
		return ""
	}
	obj := left.ChildByFieldName("object")
	attr := left.ChildByFieldName("attribute")
	if obj == nil || attr == nil || kindOf(obj) != "identifier" || pyText(obj, c.src) != "self" {
		return ""
	}
	return pyText(attr, c.src)
}

// scopeKey is the group key a variable assigned in the current scope belongs to:
// the enclosing factory function, or the module-level variable itself.
func (c *routerCollector) scopeKey(varName string) string {
	if c.funcScope != "" {
		return c.funcScope
	}
	return c.module + "." + varName
}

// handleAssign records `x = APIRouter(prefix="/p")` and `app = FastAPI()`.
func (c *routerCollector) handleAssign(node *sitter.Node) {
	left := node.ChildByFieldName("left")
	right := node.ChildByFieldName("right")
	if left == nil || right == nil || kindOf(right) != "call" {
		return
	}
	selfAttr := c.selfAttr(left)
	if kindOf(left) != "identifier" && selfAttr == "" {
		return
	}
	fn := right.ChildByFieldName("function")
	if fn == nil {
		return
	}
	ctor := lastComponent(pyText(fn, c.src))
	if ctor != "APIRouter" && ctor != "FastAPI" {
		if c.funcScope == "" && selfAttr == "" {
			if target, name := c.childRouterKey(right); target != "" {
				c.topo.aliases = append(c.topo.aliases, pyRouterAlias{key: c.module + "." + pyText(left, c.src), target: target, targetName: name})
			}
		}
		return
	}
	varName := pyText(left, c.src)
	key := c.scopeKey(varName)
	if selfAttr != "" {
		// `self.router = APIRouter(prefix=...)` in a controller method: the router
		// belongs to the class, the key routerGroupKey gives `@self.router.get`.
		key = c.module + "." + strings.Join(c.classStack, ".") + "." + selfAttr
	}
	c.varKeys[c.funcScope+"\x00"+varName] = key
	prefix, parts := c.prefixArg(right)
	c.topo.groups = append(c.topo.groups, pyRouterGroup{
		key:         key,
		prefix:      prefix,
		prefixParts: parts,
		isApp:       ctor == "FastAPI",
	})
}

// handleCall records `parent.include_router(child, prefix="/p")`.
func (c *routerCollector) handleCall(node *sitter.Node) {
	fn := node.ChildByFieldName("function")
	if fn == nil || kindOf(fn) != "attribute" {
		return
	}
	attr := fn.ChildByFieldName("attribute")
	obj := fn.ChildByFieldName("object")
	if attr == nil || obj == nil || pyText(attr, c.src) != "include_router" {
		return
	}
	args := node.ChildByFieldName("arguments")
	if args == nil {
		return
	}
	var first *sitter.Node
	for i := uint(0); i < uint(args.ChildCount()); i++ {
		a := args.Child(i)
		if a.IsNamed() && kindOf(a) != "keyword_argument" && kindOf(a) != "comment" {
			first = a
			break
		}
	}
	if first == nil {
		return
	}
	child, childName := c.childRouterKey(first)
	if child == "" {
		return
	}
	prefix, parts := c.prefixArg(node)
	c.topo.mounts = append(c.topo.mounts, pyRouterMount{
		parent:      c.parentRouterKey(obj),
		child:       child,
		childName:   childName,
		prefix:      prefix,
		prefixParts: parts,
	})
}

// prefixArg reads a call's prefix= argument: a literal as the prefix itself, or
// a computed one (`settings.API_V1_STR`, `API + "/v1"`) as parts for
// resolveRouterPrefixes. Neither when it is absent or unreadable.
func (c *routerCollector) prefixArg(call *sitter.Node) (string, []pyPathPart) {
	if p := kwargString(call, c.src, "prefix"); p != "" {
		return p, nil
	}
	v := keywordArg(call, c.src, "prefix")
	if v == nil {
		return "", nil
	}
	parts, ok := pathTemplate(v, c.src, c.refKey)
	if !ok {
		return "", nil
	}
	if lit, isLit := literalPath(parts); isLit {
		return lit, nil
	}
	return "", parts
}

// refKey is constRefKey that refuses names an enclosing function binds.
func (c *routerCollector) refKey(name string) string {
	head := name
	if i := strings.IndexByte(name, '.'); i >= 0 {
		head = name[:i]
	}
	for _, bound := range c.locals {
		if bound[head] {
			return ""
		}
	}
	return constRefKey(name, c.module, c.importMap)
}

// parentRouterKey resolves the receiver of an include_router call to a group key,
// preferring a router assigned in the current function scope over a module-level
// one. An unrecognized receiver still yields a key: composeRouterPrefixes treats
// an unknown parent as an unmounted root, so `app = create_app()` still composes
// its children from "".
func (c *routerCollector) parentRouterKey(obj *sitter.Node) string {
	if kindOf(obj) != "identifier" {
		return ""
	}
	name := pyText(obj, c.src)
	if k, ok := c.varKeys[c.funcScope+"\x00"+name]; ok {
		return k
	}
	if k, ok := c.varKeys["\x00"+name]; ok {
		return k
	}
	return c.module + "." + name
}

// childRouterKey resolves the mounted argument to a key plus its bare name. It
// handles the three FastAPI idioms: a factory call `get_x_router()`, a bare
// imported router `router`, and a module-qualified one `users.router`.
func (c *routerCollector) childRouterKey(arg *sitter.Node) (key, name string) {
	if kindOf(arg) == "call" {
		fn := arg.ChildByFieldName("function")
		if fn == nil {
			return "", ""
		}
		arg = fn
	}
	switch kindOf(arg) {
	case "identifier":
		name = pyText(arg, c.src)
		if t, ok := c.importMap[name]; ok && t != "" {
			return t, name
		}
		return c.module + "." + name, name
	case "attribute":
		obj := arg.ChildByFieldName("object")
		at := arg.ChildByFieldName("attribute")
		if obj == nil || at == nil {
			return "", ""
		}
		name = pyText(at, c.src)
		base := pyText(obj, c.src)
		if t, ok := c.importMap[base]; ok && t != "" {
			return t + "." + name, name
		}
		return c.module + "." + base + "." + name, name
	}
	return "", ""
}

// kwargString reads a string-literal keyword argument from a call, e.g. the
// "/api/v1" of include_router(r, prefix="/api/v1"). Non-literal values (an f-string
// or a settings lookup) yield "" — a missed prefix, never a wrong one.
func kwargString(call *sitter.Node, src []byte, want string) string {
	args := call.ChildByFieldName("arguments")
	if args == nil {
		return ""
	}
	for i := uint(0); i < uint(args.ChildCount()); i++ {
		a := args.Child(i)
		if kindOf(a) != "keyword_argument" {
			continue
		}
		n := a.ChildByFieldName("name")
		v := a.ChildByFieldName("value")
		if n == nil || v == nil || pyText(n, src) != want {
			continue
		}
		// An f-string carries string_content children too, around its
		// interpolations — `f"{API_PREFIX}/items"` would read as "/items". Only a
		// plain literal is a usable prefix.
		if kindOf(v) != "string" || firstChildOfKind(v, "interpolation") != nil {
			return ""
		}
		if content := firstChildOfKind(v, "string_content"); content != nil {
			return pyText(content, src)
		}
		return ""
	}
	return ""
}

// composeRouterPrefixes rewrites every FastAPI route fact with the full path its
// router is mounted at, resolving include_router mounts repo-wide via a fixpoint.
// Deterministic: groups, mounts and emitted prefixes are all processed in sorted
// order. Route facts whose mount chain does not resolve are returned untouched.
func composeRouterPrefixes(allFacts []facts.Fact, topos []pyRouterTopology, fileModules map[string]bool, pkgDirs map[string]bool) []facts.Fact {
	groups := map[string]*pyRouterGroup{}
	byName := map[string][]string{}
	var routes []pyRouteRef
	for i := range topos {
		for _, g := range topos[i].groups {
			gg := g
			if prev, ok := groups[g.key]; ok {
				// Two routers keyed alike (several APIRouter()s in one factory).
				// Keep the first and drop the prefix: composing one router's prefix
				// onto another's routes would be a fabricated path.
				if prev.prefix != g.prefix {
					prev.prefix = ""
				}
				continue
			}
			groups[g.key] = &gg
			byName[baseName(g.key)] = append(byName[baseName(g.key)], g.key)
		}
		routes = append(routes, topos[i].routes...)
	}
	if len(groups) == 0 || len(routes) == 0 {
		return allFacts
	}

	fileIdx := buildSuffixIndex(fileModules, pkgDirs)
	topPkgs := importableRoots(fileModules, pkgDirs)
	// A router imported through a package __init__ now resolves by its dotted path
	// rather than only via the unique-bare-name fallback below, so a re-exported
	// router whose short name is not repo-wide unique still finds its group.
	reexports := buildReexportIndex(allFacts, pkgDirs)

	// `router = build_router()` makes the variable another name for the factory's
	// group. Only an alias whose factory resolves to a known group is kept, and a
	// variable that is itself a group keeps that meaning.
	alias := map[string]string{}
	for i := range topos {
		importerDir := fileDir(topos[i].relFile)
		for _, a := range topos[i].aliases {
			if _, isGroup := groups[a.key]; isGroup {
				continue
			}
			if target := resolveRouterKey(a.target, a.targetName, groups, byName, fileIdx, topPkgs, importerDir, reexports); target != "" {
				alias[a.key] = target
			}
		}
	}
	// Mounts resolve against the groups and the aliases alike, then name the
	// group itself, since that is the key its routes carry.
	resolvable, byNameR := groups, byName
	if len(alias) > 0 {
		resolvable = make(map[string]*pyRouterGroup, len(groups)+len(alias))
		for k, g := range groups {
			resolvable[k] = g
		}
		byNameR = make(map[string][]string, len(byName))
		for k, v := range byName {
			byNameR[k] = v
		}
		aliasKeys := make([]string, 0, len(alias))
		for k := range alias {
			aliasKeys = append(aliasKeys, k)
		}
		sort.Strings(aliasKeys)
		for _, k := range aliasKeys {
			resolvable[k] = groups[alias[k]]
			byNameR[baseName(k)] = append(byNameR[baseName(k)], k)
		}
	}
	canonical := func(k string) string {
		if t, ok := alias[k]; ok {
			return t
		}
		return k
	}

	// Resolve mounts to group keys, in file then source order.
	type mountEdge struct{ parent, child, prefix string }
	var edges []mountEdge
	mounted := map[string]bool{}
	for i := range topos {
		importerDir := fileDir(topos[i].relFile)
		for _, m := range topos[i].mounts {
			child := canonical(resolveRouterKey(m.child, m.childName, resolvable, byNameR, fileIdx, topPkgs, importerDir, reexports))
			parent := canonical(m.parent)
			if child == "" || child == parent {
				continue
			}
			if _, ok := groups[parent]; !ok {
				// Unknown receiver (`app = create_app()`): treat it as a root so its
				// children still compose, but give it no prefix of its own.
				groups[parent] = &pyRouterGroup{key: parent, isApp: true}
			}
			edges = append(edges, mountEdge{parent: parent, child: child, prefix: m.prefix})
			mounted[child] = true
		}
	}

	byParent := map[string][]mountEdge{}
	for _, e := range edges {
		byParent[e.parent] = append(byParent[e.parent], e)
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range byParent {
		sort.Slice(k, func(a, b int) bool { return k[a].child+k[a].prefix < k[b].child+k[b].prefix })
	}

	// Fixpoint: roots (mounted by nobody) sit at "", propagating each parent's own
	// constructor prefix plus the mount prefix down to every child.
	mountedAt := map[string]map[string]bool{}
	type work struct{ key, at string }
	seen := map[work]bool{}
	var queue []work
	enqueue := func(w work) {
		if seen[w] {
			return
		}
		seen[w] = true
		if mountedAt[w.key] == nil {
			mountedAt[w.key] = map[string]bool{}
		}
		mountedAt[w.key][w.at] = true
		queue = append(queue, w)
	}
	for _, k := range keys {
		if !mounted[k] {
			enqueue(work{k, ""})
		}
	}
	for len(queue) > 0 {
		w := queue[0]
		queue = queue[1:]
		base := facts.JoinRoutePath(w.at, groups[w.key].prefix)
		for _, e := range byParent[w.key] {
			enqueue(work{e.child, facts.JoinRoutePath(base, e.prefix)})
		}
	}

	// Rewrite: one copy of the route per distinct mount path.
	prefixes := map[int][]string{}
	for _, r := range routes {
		g, ok := groups[r.group]
		if !ok || r.idx < 0 || r.idx >= len(allFacts) {
			continue
		}
		set := mountedAt[r.group]
		if len(set) == 0 {
			continue // unresolved mount chain -> keep the bare path
		}
		var ps []string
		for at := range set {
			if full := facts.JoinRoutePath(at, g.prefix); full != "" {
				ps = append(ps, full)
			}
		}
		if len(ps) == 0 {
			continue
		}
		sort.Strings(ps)
		prefixes[r.idx] = ps
	}
	if len(prefixes) == 0 {
		return allFacts
	}

	out := make([]facts.Fact, 0, len(allFacts))
	for i, f := range allFacts {
		ps, ok := prefixes[i]
		if !ok {
			out = append(out, f)
			continue
		}
		for _, p := range ps {
			nf := f
			nf.Name = facts.JoinRoutePath(p, f.Name)
			nf.Props = f.CloneProps()
			nf.SetProp("path", nf.Name)
			out = append(out, nf)
		}
	}
	return out
}

// resolveRouterKey maps a mount's child target to a known group key: exact match
// first, then the dotted import target resolved to a slash symbol, then a unique
// repo-wide match on the bare name (which is what carries a router re-exported
// through a package __init__). Ambiguous or unknown -> "", leaving the bare path.
func resolveRouterKey(raw, name string, groups map[string]*pyRouterGroup, byName map[string][]string, fileIdx suffixIndex, topPkgs map[string]bool, importerDir string, reexports reexportIndex) string {
	if _, ok := groups[raw]; ok {
		return raw
	}
	if isDottedCallTarget(raw) {
		// `from app.routers import account` then `account.router`: account is a
		// package, whose router is defined in its __init__ module.
		candidates := []string{raw}
		if mod, sym, ok := splitConstKey(raw); ok {
			candidates = append(candidates, mod+".__init__."+sym)
		}
		for _, cand := range candidates {
			if resolved, keep := resolveDottedTarget(cand, fileIdx, topPkgs, importerDir, reexports, nil); keep {
				if _, ok := groups[resolved]; ok {
					return resolved
				}
			}
		}
	}
	// A relative import binds a submodule by path ("app/routers/account" +
	// ".router"); for a package that is its __init__ module.
	if mod, sym, ok := splitConstKey(raw); ok && strings.IndexByte(raw, '/') >= 0 {
		if k := mod + "/__init__." + sym; groups[k] != nil {
			return k
		}
	}
	if cands := byName[name]; len(cands) == 1 {
		return cands[0]
	}
	return ""
}

// baseName is the trailing identifier of a group key ("a/b.get_x_router" -> "get_x_router").
func baseName(key string) string {
	if i := strings.LastIndexByte(key, '.'); i >= 0 {
		return key[i+1:]
	}
	return key
}
