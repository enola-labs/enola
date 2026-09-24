package pythonextractor

import (
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/tree-sitter/go-tree-sitter"
)

// FastAPI and Starlette register routes in more shapes than `@router.get("/x")`,
// and a real service rarely sticks to one of them:
//
//	@router.api_route("/x", methods=["GET", "POST"])   # several verbs, one handler
//	@router.websocket("/ws")                           # a websocket endpoint
//	router.add_api_route("/x", handler, methods=[...]) # imperative, typical of class-based controllers
//	@router.get(HEALTH_PATH)                           # path held in a constant, often imported
//	@router.get(f"{PREFIX}/items/{{item_id}}")         # path built from constants
//
// Every one of these used to produce no route fact, so a service written in one
// of them reported zero routes. This file extracts them.
//
// A computed path is resolved against the string constants the repository
// declares (module- and class-level, across files through imports), after every
// file has been walked. The rule the router-prefix pass follows holds here too:
// resolution may MISS, never fabricate. A path with any part that does not
// resolve to a constant is dropped rather than emitted half-known, because the
// cross-repo linker reads a route's Name as its full path.

// pyPathPart is one piece of a computed path: literal text, or a reference to a
// string constant. ref is either a canonical "<module>.<NAME>" key or a dotted
// import target ("app.core.paths.HEALTH") that resolution maps onto one.
type pyPathPart struct {
	lit string
	ref string
}

// pyPathRef ties a route fact emitted with a computed path to that path's parts.
// idx indexes into the fact slice, like pyRouteRef, and is rebased the same way.
type pyPathRef struct {
	idx   int
	parts []pyPathPart
}

// pyConst is one string constant a file declares, kept as parts because a
// constant is often composed from others (`USERS = API_PREFIX + "/users"`).
type pyConst struct {
	key   string
	parts []pyPathPart
}

// pathPendingProp marks a route fact whose Name is not known until the repo-wide
// constant pass runs. It never survives extraction: the pass either resolves the
// path and removes the mark, or drops the fact.
const pathPendingProp = "path_pending"

// pyRouteCallMethods are the imperative registration methods on a FastAPI router
// or app and on a Starlette app/router. The value says whether the method
// registers a websocket endpoint.
var pyRouteCallMethods = map[string]bool{
	"add_api_route":           false,
	"add_route":               false,
	"add_api_websocket_route": true,
	"add_websocket_route":     true,
}

// pathTemplate reads a path expression into parts. It understands string
// literals, implicit concatenation, f-strings whose interpolations are names,
// `+` concatenation, and bare or dotted names. ok is false for anything else (a
// call, a subscript, a format spec) — a computed path enola cannot read.
//
// ref maps a name as written to its resolution key. It returns "" for a name
// that cannot be a constant (a function parameter or local), which fails the
// whole template.
func pathTemplate(node *sitter.Node, src []byte, ref func(string) string) ([]pyPathPart, bool) {
	if node == nil {
		return nil, false
	}
	switch kindOf(node) {
	case "string":
		fstring := false
		if start := firstChildOfKind(node, "string_start"); start != nil {
			fstring = strings.ContainsAny(pyText(start, src), "fF")
		}
		var parts []pyPathPart
		for i := uint(0); i < uint(node.ChildCount()); i++ {
			c := node.Child(i)
			switch kindOf(c) {
			case "string_start", "string_end":
			case "string_content", "escape_interpolation":
				// `{{` / `}}` in an f-string is a literal brace: how a FastAPI path
				// parameter is written inside one (f"{PREFIX}/{{item_id}}"). The
				// grammar leaves it inside string_content, so undo it here.
				text := pyText(c, src)
				if fstring {
					text = strings.NewReplacer("{{", "{", "}}", "}").Replace(text)
				}
				parts = append(parts, pyPathPart{lit: text})
			case "interpolation":
				expr := c.ChildByFieldName("expression")
				if expr == nil || firstChildOfKind(c, "format_specifier") != nil || firstChildOfKind(c, "type_conversion") != nil {
					return nil, false
				}
				sub, ok := pathTemplate(expr, src, ref)
				if !ok {
					return nil, false
				}
				parts = append(parts, sub...)
			default:
				// An escape sequence or anything else not plain path text.
				return nil, false
			}
		}
		return parts, true
	case "concatenated_string":
		var parts []pyPathPart
		for i := uint(0); i < uint(node.ChildCount()); i++ {
			c := node.Child(i)
			if !c.IsNamed() {
				continue
			}
			sub, ok := pathTemplate(c, src, ref)
			if !ok {
				return nil, false
			}
			parts = append(parts, sub...)
		}
		return parts, true
	case "binary_operator":
		op := node.ChildByFieldName("operator")
		if op == nil || pyText(op, src) != "+" {
			return nil, false
		}
		left, ok := pathTemplate(node.ChildByFieldName("left"), src, ref)
		if !ok {
			return nil, false
		}
		right, ok := pathTemplate(node.ChildByFieldName("right"), src, ref)
		if !ok {
			return nil, false
		}
		return append(left, right...), true
	case "parenthesized_expression":
		for i := uint(0); i < uint(node.ChildCount()); i++ {
			if c := node.Child(i); c.IsNamed() {
				return pathTemplate(c, src, ref)
			}
		}
		return nil, false
	case "identifier", "attribute":
		key := ref(pyText(node, src))
		if key == "" {
			return nil, false
		}
		return []pyPathPart{{ref: key}}, true
	}
	return nil, false
}

// literalPath reports the path when every part is literal text.
func literalPath(parts []pyPathPart) (string, bool) {
	var b strings.Builder
	for _, p := range parts {
		if p.ref != "" {
			return "", false
		}
		b.WriteString(p.lit)
	}
	return b.String(), true
}

// constRefKey maps a name as written at module or class level to the key of the
// constant it would name: through the import map when its first segment is
// imported, otherwise in the current module.
func constRefKey(name, module string, importMap map[string]string) string {
	head, tail := name, ""
	if i := strings.IndexByte(name, '.'); i >= 0 {
		head, tail = name[:i], name[i:]
	}
	if head == "self" || head == "cls" {
		return ""
	}
	if t, ok := importMap[head]; ok {
		if t == "" {
			return "" // external import: a third-party constant is not in the repo
		}
		return t + tail
	}
	return module + "." + name
}

// walkerRefKey is constRefKey for a name read inside the walk, where a name bound
// by the enclosing function (a parameter, a local) is not a constant.
func (w *pyWalker) walkerRefKey(name string) string {
	head := name
	if i := strings.IndexByte(name, '.'); i >= 0 {
		head = name[:i]
	}
	if w.localBound[head] {
		return ""
	}
	return constRefKey(name, w.module, w.importMap)
}

// callPathArg returns a route registration call's path argument: the first
// positional argument, or the path= keyword.
func callPathArg(call *sitter.Node, src []byte) *sitter.Node {
	args := call.ChildByFieldName("arguments")
	if args == nil {
		return nil
	}
	for i := uint(0); i < uint(args.ChildCount()); i++ {
		a := args.Child(i)
		if !a.IsNamed() || kindOf(a) == "comment" {
			continue
		}
		if kindOf(a) == "keyword_argument" {
			if n := a.ChildByFieldName("name"); n != nil && pyText(n, src) == "path" {
				return a.ChildByFieldName("value")
			}
			continue
		}
		return a
	}
	return nil
}

// positionalArgs returns a call's positional arguments, in order.
func positionalArgs(call *sitter.Node) []*sitter.Node {
	args := call.ChildByFieldName("arguments")
	if args == nil {
		return nil
	}
	var out []*sitter.Node
	for i := uint(0); i < uint(args.ChildCount()); i++ {
		a := args.Child(i)
		if !a.IsNamed() || kindOf(a) == "comment" || kindOf(a) == "keyword_argument" {
			continue
		}
		if kindOf(a) == "list_splat" || kindOf(a) == "dictionary_splat" {
			break
		}
		out = append(out, a)
	}
	return out
}

// keywordArg returns the value of a call's keyword argument, or nil.
func keywordArg(call *sitter.Node, src []byte, want string) *sitter.Node {
	args := call.ChildByFieldName("arguments")
	if args == nil {
		return nil
	}
	for i := uint(0); i < uint(args.ChildCount()); i++ {
		a := args.Child(i)
		if kindOf(a) != "keyword_argument" {
			continue
		}
		if n := a.ChildByFieldName("name"); n != nil && pyText(n, src) == want {
			return a.ChildByFieldName("value")
		}
	}
	return nil
}

// emitRouteAt emits route facts for a registration whose path is given as parts.
// A literal path is emitted as is; a computed one is emitted pending and recorded
// for the repo-wide constant pass.
func (w *pyWalker) emitRouteAt(parts []pyPathPart, methods []string, framework string, websocket bool, receiver string, line int, pending *[]int) {
	path, literal := literalPath(parts)
	before := len(w.out)
	w.emitRoutes(path, methods, framework, line, pending)
	for i := before; i < len(w.out); i++ {
		if websocket {
			w.out[i].SetProp("protocol", "websocket")
		}
		if !literal {
			w.out[i].SetProp(pathPendingProp, true)
			w.pathRefs = append(w.pathRefs, pyPathRef{idx: i, parts: parts})
		}
	}
	if group := w.routerGroupKey(receiver); group != "" {
		for i := before; i < len(w.out); i++ {
			w.routeRefs = append(w.routeRefs, pyRouteRef{idx: i, group: group})
		}
	}
}

// emitComputedDecoratorRoute handles a route decorator whose path the literal
// regex cannot read (`@router.get(HEALTH_PATH)`), by reading the decorator call
// from the tree. It reports whether routes were emitted (pending resolution).
func (w *pyWalker) emitComputedDecoratorRoute(dec *sitter.Node, pending *[]int) bool {
	call := firstChildOfKind(dec, "call")
	if call == nil {
		return false
	}
	fn := call.ChildByFieldName("function")
	if fn == nil || kindOf(fn) != "attribute" {
		return false
	}
	obj := fn.ChildByFieldName("object")
	attr := fn.ChildByFieldName("attribute")
	if obj == nil || attr == nil {
		return false
	}
	verb := pyText(attr, w.src)
	parts, ok := pathTemplate(callPathArg(call, w.src), w.src, w.walkerRefKey)
	if !ok {
		return false
	}
	methods, framework, websocket := w.decoratorVerbs(verb, pyText(call, w.src))
	if methods == nil {
		return false
	}
	w.emitRouteAt(parts, methods, framework, websocket, pyText(obj, w.src), int(dec.StartPosition().Row)+1, pending)
	return true
}

// decoratorVerbs maps a route decorator's method name to the HTTP methods it
// registers, the framework label, and whether it is a websocket endpoint. nil
// methods means the name is not a route decorator.
func (w *pyWalker) decoratorVerbs(verb, text string) ([]string, string, bool) {
	switch strings.ToLower(verb) {
	case "route":
		return routeMethods(text), "flask", false
	case "api_route":
		// FastAPI's api_route defaults to GET when methods= is absent.
		return routeMethods(text), w.verbShorthandFramework(), false
	case "websocket", "websocket_route":
		// A websocket handshake is an HTTP GET upgrade.
		return []string{"GET"}, w.verbShorthandFramework(), true
	case "get", "post", "put", "delete", "patch", "head", "options":
		return []string{strings.ToUpper(verb)}, w.verbShorthandFramework(), false
	}
	return nil, "", false
}

// emitCallRoute emits the route registered by an imperative call —
// `router.add_api_route("/x", handler, methods=["POST"])`, Starlette's
// `app.add_route("/x", endpoint)`, and their websocket forms. Called for every
// call node the walks reach; anything that is not such a call is ignored.
func (w *pyWalker) emitCallRoute(call *sitter.Node) {
	fn := call.ChildByFieldName("function")
	if fn == nil || kindOf(fn) != "attribute" {
		return
	}
	attr := fn.ChildByFieldName("attribute")
	obj := fn.ChildByFieldName("object")
	if attr == nil || obj == nil {
		return
	}
	name := pyText(attr, w.src)
	websocket, ok := pyRouteCallMethods[name]
	if !ok {
		return
	}
	if w.routeDecorators == nil {
		w.routeDecorators = map[uint]bool{}
	}
	if w.routeDecorators[call.StartByte()] {
		return // a method body is walked twice; emit once
	}
	parts, ok := pathTemplate(callPathArg(call, w.src), w.src, w.walkerRefKey)
	if !ok {
		return
	}
	// add_route is a generic name (aiohttp's router.add_route("GET", "/x", h) and
	// unrelated libraries use it), so only a path that reads as a URL path counts.
	if name == "add_route" || name == "add_websocket_route" {
		if len(parts) == 0 || parts[0].ref != "" || !strings.HasPrefix(parts[0].lit, "/") {
			return
		}
	}
	methods := []string{"GET"}
	if !websocket {
		if m := keywordArg(call, w.src, "methods"); m != nil {
			if verbs := httpMethodWordRe.FindAllString(pyText(m, w.src), -1); len(verbs) > 0 {
				methods = verbs
			}
		}
	}
	var pending []int
	w.emitRouteAt(parts, methods, w.verbShorthandFramework(), websocket, pyText(obj, w.src), int(call.StartPosition().Row)+1, &pending)
	w.routeDecorators[call.StartByte()] = true
	if handler := w.callRouteHandler(call); handler != "" {
		for _, idx := range pending {
			w.out[idx].SetProp("handler", handler)
		}
	}
}

// callRouteHandler names the endpoint an add_api_route call registers: the
// second positional argument or endpoint=, qualified the way the walker names
// symbols. "" when it is not a plain name.
func (w *pyWalker) callRouteHandler(call *sitter.Node) string {
	var ep *sitter.Node
	if pos := positionalArgs(call); len(pos) >= 2 {
		ep = pos[1]
	} else {
		ep = keywordArg(call, w.src, "endpoint")
	}
	if ep == nil {
		return ""
	}
	text := pyText(ep, w.src)
	switch kindOf(ep) {
	case "identifier":
		if target := w.resolveCall(text); target != "" {
			return target
		}
		return w.module + "." + text
	case "attribute":
		if rest, ok := strings.CutPrefix(text, "self."); ok && len(w.typeStack) > 0 && !strings.Contains(rest, ".") {
			return w.module + "." + w.enclosingType() + "." + rest
		}
	}
	return ""
}

// walkGuardedRoutes finds route-decorated definitions under a module-level
// if/try. walkStatement deliberately gives guarded defs no symbol (they are
// usually shims bound by a sibling branch), but a route decorator registers its
// handler with the framework the moment the branch runs — `if settings.DEBUG:
// @router.get("/debug")` serves a real endpoint. Only those definitions are
// walked; every other guarded def keeps the old treatment.
func (w *pyWalker) walkGuardedRoutes(node *sitter.Node) {
	switch kindOf(node) {
	case "function_definition", "class_definition":
		return
	case "decorated_definition":
		for i := uint(0); i < uint(node.ChildCount()); i++ {
			c := node.Child(i)
			if kindOf(c) != "decorator" {
				continue
			}
			text := pyText(c, w.src)
			if routeMethodRe.MatchString(text) || exposeDecoratorRe.MatchString(text) {
				w.handleDecoratedDefinition(node)
				return
			}
		}
		return
	}
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		w.walkGuardedRoutes(node.Child(i))
	}
}

// collectConsts reads the string constants a file declares at module and class
// level. Function bodies are skipped: a local is not importable, and a value
// computed at call time is not a constant.
func collectConsts(root *sitter.Node, src []byte, module string, importMap map[string]string) []pyConst {
	var out []pyConst
	var walk func(n *sitter.Node, classPath string)
	walk = func(n *sitter.Node, classPath string) {
		for i := uint(0); i < uint(n.ChildCount()); i++ {
			c := n.Child(i)
			switch kindOf(c) {
			case "function_definition", "decorated_definition":
				continue
			case "class_definition":
				if body := c.ChildByFieldName("body"); body != nil {
					walk(body, classPath+pyFuncName(c, src)+".")
				}
				continue
			case "expression_statement":
				walk(c, classPath)
				continue
			case "assignment":
				left := c.ChildByFieldName("left")
				right := c.ChildByFieldName("right")
				if left == nil || right == nil || kindOf(left) != "identifier" {
					continue
				}
				switch kindOf(right) {
				case "string", "concatenated_string", "binary_operator", "identifier", "attribute", "parenthesized_expression":
				default:
					continue
				}
				ref := func(name string) string {
					// Inside a class body a bare name reads the class's own
					// attribute first, which is how constants reference each other there.
					if classPath != "" && !strings.Contains(name, ".") {
						if _, imported := importMap[name]; !imported {
							return module + "." + classPath + name
						}
					}
					return constRefKey(name, module, importMap)
				}
				if parts, ok := pathTemplate(right, src, ref); ok {
					out = append(out, pyConst{key: module + "." + classPath + pyText(left, src), parts: parts})
				}
				continue
			case "if_statement", "try_statement", "block", "else_clause", "elif_clause", "except_clause", "finally_clause":
				walk(c, classPath)
				continue
			}
		}
	}
	walk(root, "")
	return out
}

// resolveRoutePaths resolves every pending computed route path against the
// repository's string constants.
// It runs BEFORE composeRouterPrefixes so the mount prefix is joined onto the
// resolved leaf. Unresolved routes keep pathPendingProp and are removed by
// dropPendingRoutes once composing is done (composing rebuilds the slice, so the
// drop is by mark, not by index).
func resolveRoutePaths(allFacts []facts.Fact, topos []pyRouterTopology, fileModules, pkgDirs map[string]bool, reexports reexportIndex) {
	type constDef struct {
		parts []pyPathPart
		dir   string // defining file's directory, for its own relative refs
	}
	consts := map[string]constDef{}
	keys := map[string]bool{}
	for i := range topos {
		dir := fileDir(topos[i].relFile)
		for _, c := range topos[i].consts {
			if _, dup := consts[c.key]; dup {
				continue // first definition wins, in file order
			}
			consts[c.key] = constDef{parts: c.parts, dir: dir}
			keys[c.key] = true
			// A package's constants are imported as pkg.NAME, not pkg.__init__.NAME.
			if mod, name, ok := splitConstKey(c.key); ok && strings.HasSuffix(mod, "/__init__") {
				alias := strings.TrimSuffix(mod, "/__init__") + "." + name
				if _, dup := consts[alias]; !dup {
					consts[alias] = constDef{parts: c.parts, dir: dir}
					keys[alias] = true
				}
			}
		}
	}

	fileIdx := buildSuffixIndex(fileModules, pkgDirs)
	topPkgs := importableRoots(fileModules, pkgDirs)
	lookup := func(ref, importerDir string) (constDef, bool) {
		if c, ok := consts[ref]; ok {
			return c, true
		}
		if !isDottedCallTarget(ref) {
			return constDef{}, false
		}
		// `from app.core import ROOT` names a package, whose constants live in its
		// __init__ module; the module index knows that file, not the directory.
		candidates := []string{ref}
		if mod, name, ok := splitConstKey(ref); ok {
			candidates = append(candidates, mod+".__init__."+name)
		}
		for _, cand := range candidates {
			if res, keep := resolveDottedTarget(cand, fileIdx, topPkgs, importerDir, reexports, keys); keep {
				if c, ok := consts[res]; ok {
					return c, true
				}
			}
		}
		return constDef{}, false
	}

	memo := map[string]string{}
	failed := map[string]bool{}
	var render func(parts []pyPathPart, dir string, depth int) (string, bool)
	render = func(parts []pyPathPart, dir string, depth int) (string, bool) {
		if depth > 8 {
			return "", false // a constant cycle, or a chain too deep to trust
		}
		var b strings.Builder
		for _, p := range parts {
			if p.ref == "" {
				b.WriteString(p.lit)
				continue
			}
			memoKey := dir + "\x00" + p.ref
			if v, ok := memo[memoKey]; ok {
				b.WriteString(v)
				continue
			}
			if failed[memoKey] {
				return "", false
			}
			c, ok := lookup(p.ref, dir)
			if !ok {
				failed[memoKey] = true
				return "", false
			}
			v, ok := render(c.parts, c.dir, depth+1)
			if !ok {
				failed[memoKey] = true
				return "", false
			}
			memo[memoKey] = v
			b.WriteString(v)
		}
		return b.String(), true
	}

	for i := range topos {
		dir := fileDir(topos[i].relFile)
		for _, pr := range topos[i].paths {
			if pr.idx < 0 || pr.idx >= len(allFacts) {
				continue
			}
			path, ok := render(pr.parts, dir, 0)
			// FastAPI only accepts "" or a "/"-rooted path; anything else is a
			// constant that happens to share the name, not a route path.
			if !ok || (path != "" && !strings.HasPrefix(path, "/")) {
				continue
			}
			f := &allFacts[pr.idx]
			f.Name = path
			f.SetProp("path", path)
			delete(f.Props, pathPendingProp)
		}
	}
}

// dropPendingRoutes removes the route facts whose computed path never resolved.
func dropPendingRoutes(allFacts []facts.Fact) []facts.Fact {
	out := allFacts[:0]
	for _, f := range allFacts {
		if f.Kind == facts.KindRoute && f.Props[pathPendingProp] == true {
			continue
		}
		out = append(out, f)
	}
	return out
}

// splitConstKey splits "<module>.<NAME>" at the last dot.
func splitConstKey(key string) (module, name string, ok bool) {
	i := strings.LastIndexByte(key, '.')
	if i <= 0 {
		return "", "", false
	}
	return key[:i], key[i+1:], true
}
