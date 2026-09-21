package rustextractor

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/tree-sitter/go-tree-sitter"
)

// axumHTTPMethods are the Axum `MethodRouter` builder functions (`get`, `post`,
// …) that appear as the callee of a `.route(path, <chain>)` call's second
// argument, e.g. `.route("/users", get(list_users).post(create_user))`.
var axumHTTPMethods = map[string]bool{
	"get": true, "post": true, "put": true, "delete": true, "patch": true,
	"head": true, "options": true, "trace": true, "connect": true, "any": true,
}

// axumRouteEntry is one (method, handler) pair extracted from a MethodRouter
// builder chain.
type axumRouteEntry struct {
	method  string
	handler string
}

// extractAxumRoutes walks a whole parsed file for Axum-style
// `<router>.route("/path", get(handler))` registrations and emits one
// facts.KindRoute per (path, method) pair found.
//
// Detection is purely structural — a `.route(...)` call whose first argument
// is a string literal and second argument is (a chain of) HTTP-verb builder
// calls — rather than gated on first confirming the file's crate depends on
// axum. That combination is distinctive enough in practice (mirrors how the
// Python extractor's FastAPI decorator regex doesn't verify the import either)
// and avoids needing per-crate dependency plumbing through the whole walker
// just for this one signal.
//
// The bare in-router path is emitted here; `.nest(prefix, module::router())`
// mount prefixes are composed interprocedurally afterwards by composeAxumPrefixes
// (a crate-wide pass, since a router is commonly mounted in a different file than
// the one defining it). `.route_service(...)` (a tower `Service` value, not a
// plain handler function) is still not handled.
func extractAxumRoutes(root *sitter.Node, src []byte, relFile, dir string) []facts.Fact {
	var out []facts.Fact
	var walk func(node *sitter.Node)
	walk = func(node *sitter.Node) {
		if node == nil {
			return
		}
		if kindOf(node) == "call_expression" {
			if fn := node.ChildByFieldName("function"); fn != nil && kindOf(fn) == "field_expression" {
				if field := fn.ChildByFieldName("field"); field != nil && nodeText(field, src) == "route" {
					out = append(out, axumRouteFacts(node, src, relFile, dir)...)
				}
			}
		}
		for i := uint(0); i < uint(node.ChildCount()); i++ {
			walk(node.Child(i))
		}
	}
	walk(root)
	return out
}

// axumRouteFacts converts one `.route(path, chain)` call_expression into its
// route facts (one per HTTP verb in the chain), or nil if it doesn't match the
// expected two-argument (string literal, verb-builder chain) shape.
func axumRouteFacts(call *sitter.Node, src []byte, relFile, dir string) []facts.Fact {
	args := call.ChildByFieldName("arguments")
	if args == nil {
		return nil
	}
	var named []*sitter.Node
	for i := uint(0); i < uint(args.ChildCount()); i++ {
		if c := args.Child(i); c.IsNamed() {
			named = append(named, c)
		}
	}
	if len(named) != 2 {
		return nil
	}
	path, ok := stringLiteralValue(named[0], src)
	if !ok {
		return nil
	}
	entries := collectMethodRouterChain(named[1], src)
	if len(entries) == 0 {
		return nil
	}

	line := int(call.StartPosition().Row) + 1
	out := make([]facts.Fact, 0, len(entries))
	for _, e := range entries {
		props := map[string]any{
			"method":    e.method,
			"framework": "axum",
			"language":  "rust",
		}
		if e.handler != "" {
			props["handler"] = e.handler
		}
		out = append(out, facts.Fact{
			Kind:      facts.KindRoute,
			Name:      path,
			File:      relFile,
			Line:      line,
			Props:     props,
			Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: dir}},
		})
	}
	return out
}

// collectMethodRouterChain walks a MethodRouter-building expression like
// `get(handler1).post(handler2)` and returns one entry per HTTP verb in the
// chain, base call first. A bare `get(handler)` (no chaining) is the base
// case; a chained `.post(handler2)` is itself a call_expression whose function
// is a field_expression wrapping the inner chain, recursed into via `value`.
func collectMethodRouterChain(node *sitter.Node, src []byte) []axumRouteEntry {
	if node == nil || kindOf(node) != "call_expression" {
		return nil
	}
	fn := node.ChildByFieldName("function")
	if fn == nil {
		return nil
	}
	switch kindOf(fn) {
	case "identifier":
		name := nodeText(fn, src)
		if !axumHTTPMethods[name] {
			return nil
		}
		return []axumRouteEntry{{method: strings.ToUpper(name), handler: axumFirstArgName(node, src)}}
	case "field_expression":
		field := fn.ChildByFieldName("field")
		if field == nil {
			return nil
		}
		name := nodeText(field, src)
		if !axumHTTPMethods[name] {
			// A non-verb method in the chain is a DECORATOR, not a terminator:
			// `.route("/x", get(h).layer(mw))` attaches per-route middleware and is
			// idiomatic Axum, as are `.route_layer(…)` and `.with_state(…)`. Bailing
			// out here discarded the verbs underneath it, so the whole route vanished
			// even though `get` was sitting right there — a route silently absent from
			// the graph, which is the worst way to be wrong. Recurse past it instead
			// and keep whatever the chain below yields.
			//
			// Safe to be permissive: this only ever runs on the SECOND argument of a
			// `.route(path, …)` call, which is a MethodRouter by construction, so any
			// method on it is a wrapper around one.
			//
			// Found on a production Axum service added to the benchmark corpus.
			return collectMethodRouterChain(fn.ChildByFieldName("value"), src)
		}
		entries := collectMethodRouterChain(fn.ChildByFieldName("value"), src)
		return append(entries, axumRouteEntry{method: strings.ToUpper(name), handler: axumFirstArgName(node, src)})
	}
	return nil
}

// axumFirstArgName returns a verb-builder call's handler argument as source
// text — a bare identifier ("list_users") or a qualified path
// ("handlers::list_users") — or "" for anything else (a closure, a method
// value, etc.), which is a fine best-effort miss: the route fact is still
// emitted, just without a "handler" prop.
func axumFirstArgName(call *sitter.Node, src []byte) string {
	args := call.ChildByFieldName("arguments")
	if args == nil {
		return ""
	}
	for i := uint(0); i < uint(args.ChildCount()); i++ {
		c := args.Child(i)
		if !c.IsNamed() {
			continue
		}
		switch kindOf(c) {
		case "identifier", "scoped_identifier":
			return nodeText(c, src)
		}
		return ""
	}
	return ""
}

// stringLiteralValue returns a string_literal/raw_string_literal node's
// content (escape sequences included verbatim, not unescaped — fine for a
// route path).
func stringLiteralValue(node *sitter.Node, src []byte) (string, bool) {
	if node == nil {
		return "", false
	}
	switch kindOf(node) {
	case "string_literal", "raw_string_literal":
	default:
		return "", false
	}
	var b strings.Builder
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		if c := node.Child(i); kindOf(c) == "string_content" {
			b.WriteString(nodeText(c, src))
		}
	}
	return b.String(), true
}

// --- interprocedural nest-prefix composition ---
//
// Axum splits an API across router-builder functions: `pub fn router() -> Router`
// in routers/datasets.rs registers routes at bare in-router paths ("/status",
// "/{id}/data"), and a parent mounts it via
// `.nest("/api/v1/datasets", routers::datasets::router())` in router_builder.rs.
// extractAxumRoutes emits the bare path; composeAxumPrefixes prepends the mount
// prefix by resolving each nest to its callee builder and propagating prefixes to
// a fixpoint — the axum analog of the Go extractor's gorilla/mux+chi subrouter
// composition (goextractor/routeprefix.go). A builder mounted at several prefixes
// emits each route once per prefix; a root builder (mounted by nobody) or an
// unresolvable mount keeps the bare path, so composition never drops a route.

// axumNest is one `.nest(prefix, callee())` mount found in a builder function.
type axumNest struct {
	prefix   string // literal mount prefix, e.g. "/api/v1/datasets"
	fnName   string // callee builder fn name (last path segment), e.g. "router"
	modStem  string // callee module stem (2nd-to-last segment), e.g. "datasets"
	sameFile bool   // the call had no module qualifier -> resolve within the same file
}

// axumBuilder is one function that builds a router: its identity, source span
// (used to attribute already-emitted route facts to it), and its nest mounts.
type axumBuilder struct {
	relFile  string
	fnName   string
	startRow uint
	endRow   uint
	nests    []axumNest
}

func (b axumBuilder) key() string { return b.relFile + "\x00" + b.fnName }

// collectAxumBuilders records every function in a file with its span and the
// `.nest(...)` mounts it declares, so the crate-wide pass can attribute routes
// and resolve mounts. Every function is recorded (not just those with nests):
// a mount callee that only registers routes still needs a span+key entry.
func collectAxumBuilders(root *sitter.Node, src []byte, relFile string) []axumBuilder {
	var out []axumBuilder
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil {
			return
		}
		if kindOf(n) == "function_item" {
			if name := n.ChildByFieldName("name"); name != nil {
				b := axumBuilder{
					relFile:  relFile,
					fnName:   nodeText(name, src),
					startRow: n.StartPosition().Row,
					endRow:   n.EndPosition().Row,
				}
				collectNests(n, n, src, &b.nests)
				out = append(out, b)
			}
		}
		for i := uint(0); i < uint(n.ChildCount()); i++ {
			walk(n.Child(i))
		}
	}
	walk(root)
	return out
}

// collectNests appends every `.nest(literal, callee())` mount inside owner's body
// to out, without descending into nested function_items (their nests are theirs).
func collectNests(owner, n *sitter.Node, src []byte, out *[]axumNest) {
	if n == nil {
		return
	}
	if n != owner && kindOf(n) == "function_item" {
		return
	}
	if kindOf(n) == "call_expression" {
		if fn := n.ChildByFieldName("function"); fn != nil && kindOf(fn) == "field_expression" {
			if field := fn.ChildByFieldName("field"); field != nil && nodeText(field, src) == "nest" {
				if nst, ok := parseNest(n, src); ok {
					*out = append(*out, nst)
				}
			}
		}
	}
	for i := uint(0); i < uint(n.ChildCount()); i++ {
		collectNests(owner, n.Child(i), src, out)
	}
}

// parseNest matches a `.nest("/prefix", <callee>::router())` call: a string-literal
// first argument and a call-expression second argument whose function is a plain
// or scoped identifier. It returns the mount prefix and the callee's fn name +
// module stem for resolution, or ok=false for anything else (a dynamic prefix or a
// pre-built router variable, e.g. `.nest(mount, r)`).
func parseNest(call *sitter.Node, src []byte) (axumNest, bool) {
	args := call.ChildByFieldName("arguments")
	if args == nil {
		return axumNest{}, false
	}
	var named []*sitter.Node
	for i := uint(0); i < uint(args.ChildCount()); i++ {
		if c := args.Child(i); c.IsNamed() {
			named = append(named, c)
		}
	}
	if len(named) != 2 {
		return axumNest{}, false
	}
	prefix, ok := stringLiteralValue(named[0], src)
	if !ok || kindOf(named[1]) != "call_expression" {
		return axumNest{}, false
	}
	calleeFn := named[1].ChildByFieldName("function")
	if calleeFn == nil {
		return axumNest{}, false
	}
	switch kindOf(calleeFn) {
	case "identifier":
		return axumNest{prefix: prefix, fnName: nodeText(calleeFn, src), sameFile: true}, true
	case "scoped_identifier":
		var segs []string
		for _, s := range strings.Split(nodeText(calleeFn, src), "::") {
			if s = strings.TrimSpace(s); s != "" {
				segs = append(segs, s)
			}
		}
		if len(segs) == 0 {
			return axumNest{}, false
		}
		nst := axumNest{prefix: prefix, fnName: segs[len(segs)-1]}
		if len(segs) >= 2 {
			nst.modStem = segs[len(segs)-2]
		} else {
			nst.sameFile = true
		}
		return nst, true
	}
	return axumNest{}, false
}

// composeAxumPrefixes rewrites every Axum route fact with the full mount path of
// its builder, resolving `.nest(...)` mounts across the crate via a fixpoint.
// Deterministic: builders, edges, and emitted prefixes are all processed in
// sorted order.
func composeAxumPrefixes(allFacts []facts.Fact, builders []axumBuilder, crates []crateInfo) []facts.Fact {
	if len(builders) == 0 {
		return allFacts
	}
	byKey := make(map[string]*axumBuilder, len(builders))
	byFile := map[string][]*axumBuilder{}
	byName := map[string][]*axumBuilder{}
	for i := range builders {
		b := &builders[i]
		byKey[b.key()] = b
		byFile[b.relFile] = append(byFile[b.relFile], b)
		byName[b.fnName] = append(byName[b.fnName], b)
	}

	type edge struct{ callee, prefix string }
	edges := map[string][]edge{}
	nested := map[string]bool{}
	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b := byKey[k]
		crateDir := nearestCrateDir(factpath.Dir(b.relFile), crates)
		for _, nst := range b.nests {
			callee := resolveAxumCallee(nst, b.relFile, crateDir, byFile, byName, crates)
			if callee == "" {
				continue
			}
			edges[k] = append(edges[k], edge{callee: callee, prefix: nst.prefix})
			nested[callee] = true
		}
	}

	// Fixpoint: seed roots (mounted by nobody) at prefix "", propagate to callees.
	prefixes := map[string]map[string]bool{}
	type work struct{ key, prefix string }
	seen := map[work]bool{}
	var queue []work
	enqueue := func(w work) {
		if !seen[w] {
			seen[w] = true
			queue = append(queue, w)
		}
	}
	for _, k := range keys {
		if !nested[k] {
			enqueue(work{k, ""})
		}
	}
	for len(queue) > 0 {
		w := queue[0]
		queue = queue[1:]
		for _, e := range edges[w.key] {
			full := facts.JoinRoutePath(w.prefix, e.prefix)
			if prefixes[e.callee] == nil {
				prefixes[e.callee] = map[string]bool{}
			}
			if !prefixes[e.callee][full] {
				prefixes[e.callee][full] = true
				enqueue(work{e.callee, full})
			}
		}
	}

	out := make([]facts.Fact, 0, len(allFacts))
	for _, f := range allFacts {
		if f.Kind != facts.KindRoute || f.PropAny("framework") != "axum" {
			out = append(out, f)
			continue
		}
		b := builderForFact(f, byFile)
		if b == nil {
			out = append(out, f)
			continue
		}
		set := prefixes[b.key()]
		if len(set) == 0 {
			out = append(out, f) // root / unresolved mount -> keep bare path
			continue
		}
		ps := make([]string, 0, len(set))
		for p := range set {
			ps = append(ps, p)
		}
		sort.Strings(ps)
		for _, p := range ps {
			nf := f
			nf.Name = facts.JoinRoutePath(p, f.Name)
			// Each copy needs its OWN props map. `nf := f` copies the struct but not
			// the map behind Props, so without this every copy of a router nested at
			// several mount points shares one map — and the binders that run later
			// write per-route verdicts into it (unmatched_by_clients, unmatched_reason,
			// handler), so the last write would win for all of them.
			//
			// The failure was cache-dependent, which is worse than the bug: cached facts
			// round-trip through json.Unmarshal and come back with independent maps, so
			// it could only ever appear on a cache MISS. Same tree, different answer
			// depending on whether .enola was warm.
			nf.Props = f.CloneProps()
			out = append(out, nf)
		}
	}
	return out
}

// resolveAxumCallee maps a nest's callee to a builder key: a same-file bare call
// by fn name in the same file; a module-qualified call by fn name + file stem ==
// module stem, preferring the same crate when several match. "" (fall back to the
// bare path) when unresolved or ambiguous, so a wrong guess can never fabricate a
// prefix — at most it misses one.
func resolveAxumCallee(nst axumNest, fromFile, fromCrate string, byFile, byName map[string][]*axumBuilder, crates []crateInfo) string {
	if nst.sameFile {
		for _, b := range byFile[fromFile] {
			if b.fnName == nst.fnName {
				return b.key()
			}
		}
		return ""
	}
	var cands []*axumBuilder
	for _, b := range byName[nst.fnName] {
		if fileStem(b.relFile) == nst.modStem {
			cands = append(cands, b)
		}
	}
	if len(cands) == 1 {
		return cands[0].key()
	}
	if len(cands) > 1 {
		var same []*axumBuilder
		for _, b := range cands {
			if nearestCrateDir(factpath.Dir(b.relFile), crates) == fromCrate {
				same = append(same, b)
			}
		}
		if len(same) == 1 {
			return same[0].key()
		}
	}
	return ""
}

// builderForFact returns the tightest-spanning builder in the fact's file whose
// source range contains the fact's line (the enclosing router-builder function).
func builderForFact(f facts.Fact, byFile map[string][]*axumBuilder) *axumBuilder {
	row := uint(f.Line - 1)
	var best *axumBuilder
	for _, b := range byFile[f.File] {
		if row < b.startRow || row > b.endRow {
			continue
		}
		if best == nil || (b.endRow-b.startRow) < (best.endRow-best.startRow) {
			best = b
		}
	}
	return best
}

// fileStem is a Rust file's module name: its base name without the .rs suffix.
func fileStem(relFile string) string {
	return strings.TrimSuffix(filepath.Base(relFile), ".rs")
}
