package tsextractor

import (
	"bytes"
	"strconv"
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"
	typescript "github.com/tree-sitter/tree-sitter-typescript/bindings/go"

	"github.com/enola-labs/enola/internal/extractors/tsutil"
	"github.com/enola-labs/enola/internal/facts"
)

// Requests made through a helper another file exports.
//
// localwrapper.go reads a helper declared in the file that calls it, and leaves an
// imported one alone. A client that routes every request through one shared client
// is the other shape, and the commoner one:
//
//	// lib/api/auth.ts
//	export class AuthApiClient {
//	  async handleAuthenticatedRequest(url: string, options: RequestInit = {}) {
//	    const fullUrl = `${API_BASE_URL}${url}`;
//	    return fetch(fullUrl, options);
//	  }
//	}
//	export const authApiClient = new AuthApiClient();
//
//	// lib/api/proshops.ts
//	import { authApiClient } from '@/lib/api/auth';
//	authApiClient.handleAuthenticatedRequest('/api/settings/proshops', { method: 'GET' });
//
// None of those calls was a route, so every endpoint reached through the shared
// client read as called by nobody. A repository-wide pass finds the exported helpers
// first: an exported function, or a method of a class whose instance the file
// exports, that is a helper by the same test localwrapper.go applies (its first
// parameter ends the URL it hands fetch or axios, after a literal prefix). A call is
// then a request when its callee resolves, through this file's named imports, to one
// of them. A default import, a namespace import and a re-export are not followed.

// requestHelperIndex holds the exported request helpers of a repository, by the file
// that exports them.
type requestHelperIndex struct {
	funcs     map[string]map[string]localWrapper            // file → exported name → helper
	instances map[string]map[string]map[string]localWrapper // file → exported instance → method → helper
	bodies    map[string][][2]int                           // file → lines of its helpers' own requests
}

func (ix *requestHelperIndex) empty() bool {
	return ix == nil || (len(ix.funcs) == 0 && len(ix.instances) == 0)
}

// buildRequestHelperIndex parses only the files that could export a helper: ones that
// make a request and export something.
func buildRequestHelperIndex(tsFiles []string, sources map[string][]byte) *requestHelperIndex {
	ix := &requestHelperIndex{
		funcs:     map[string]map[string]localWrapper{},
		instances: map[string]map[string]map[string]localWrapper{},
		bodies:    map[string][][2]int{},
	}
	for _, rel := range tsFiles {
		src := sources[rel]
		if src == nil || facts.IsTestPath(rel) || !bytes.Contains(src, []byte("export")) ||
			(!bytes.Contains(src, []byte("fetch(")) && !bytes.Contains(src, []byte("axios"))) {
			continue
		}
		ix.addFile(rel, src)
	}
	return ix
}

func (ix *requestHelperIndex) addFile(rel string, src []byte) {
	isTSX := strings.HasSuffix(rel, ".tsx") || strings.HasSuffix(rel, ".jsx")
	lang := typescript.LanguageTypescript()
	if isTSX {
		lang = typescript.LanguageTSX()
	}
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(lang)); err != nil {
		return
	}
	tree := parser.Parse(src, nil)
	defer tree.Close()
	kinds := tsKindsFor(isTSX)
	root := tree.RootNode()
	module := singleAssignedLocals(kinds, root, src)

	classes := map[string]map[string]localWrapper{}
	exportedFuncs := map[string]localWrapper{}
	type instance struct{ name, class string }
	var instances []instance
	type pendingFunc struct {
		name string
		node *sitter.Node
	}
	var pending []pendingFunc
	note := func(w localWrapper) {
		for _, l := range w.lines {
			ix.bodies[rel] = append(ix.bodies[rel], [2]int{l, l})
		}
	}

	for i := range root.ChildCount() {
		stmt := root.Child(i)
		exported := false
		if kindOf(kinds, stmt) == "export_statement" {
			if d := stmt.ChildByFieldName("declaration"); d != nil {
				stmt, exported = d, true
			}
		}
		switch kindOf(kinds, stmt) {
		case "class_declaration":
			name := stmt.ChildByFieldName("name")
			body := stmt.ChildByFieldName("body")
			if name == nil || body == nil {
				continue
			}
			methods := classHelpers(kinds, body, src, module)
			// The regex client pass reads makeRequest by name; it is a helper here only
			// for the methods of its own class that go through it.
			delete(methods, "makeRequest")
			if len(methods) > 0 {
				classes[nodeText(name, src)] = methods
			}
		case "function_declaration":
			if n := stmt.ChildByFieldName("name"); n != nil && exported && nodeText(n, src) != "fetch" && nodeText(n, src) != "makeRequest" {
				if w, ok := wrapperOf(kinds, stmt, src, module); ok {
					exportedFuncs[nodeText(n, src)] = w
				} else {
					pending = append(pending, pendingFunc{nodeText(n, src), stmt})
				}
			}
		case "lexical_declaration", "variable_declaration":
			if !exported {
				continue
			}
			for j := range stmt.ChildCount() {
				d := stmt.Child(j)
				if kindOf(kinds, d) != "variable_declarator" {
					continue
				}
				n, v := d.ChildByFieldName("name"), d.ChildByFieldName("value")
				if n == nil || v == nil || kindOf(kinds, n) != "identifier" {
					continue
				}
				name := nodeText(n, src)
				switch kindOf(kinds, v) {
				case "arrow_function", "function_expression", "function":
					if name == "fetch" || name == "makeRequest" {
						continue
					}
					if w, ok := wrapperOf(kinds, v, src, module); ok {
						exportedFuncs[name] = w
					} else {
						pending = append(pending, pendingFunc{name, v})
					}
				case "new_expression":
					if c := v.ChildByFieldName("constructor"); c != nil && kindOf(kinds, c) == "identifier" {
						instances = append(instances, instance{name, nodeText(c, src)})
					}
				}
			}
		}
	}
	// An exported function that makes its request through a helper method of an
	// instance this file declares: `deduplicatedFetch(url)` handing url to
	// `requestCache.fetch(url)`, whose body calls fetch.
	inner := func(call *sitter.Node) (localWrapper, bool) {
		fn := calleeOf(kinds, call)
		if fn == nil || kindOf(kinds, fn) != "member_expression" {
			return localWrapper{}, false
		}
		obj, prop := fn.ChildByFieldName("object"), fn.ChildByFieldName("property")
		if obj == nil || prop == nil || kindOf(kinds, obj) != "identifier" {
			return localWrapper{}, false
		}
		for _, in := range instances {
			if in.name == nodeText(obj, src) {
				if w, ok := classes[in.class][nodeText(prop, src)]; ok {
					return w, true
				}
			}
		}
		return localWrapper{}, false
	}
	for _, p := range pending {
		if w, ok := wrapperThrough(kinds, p.node, src, module, inner); ok {
			exportedFuncs[p.name] = w
		}
	}
	// A helper is handed a path; see takesPath. Nothing here sees a call that would
	// confirm one, so the URL's shape decides.
	for name, w := range exportedFuncs {
		if !takesPath(w) {
			delete(exportedFuncs, name)
		}
	}
	for _, methods := range classes {
		for name, w := range methods {
			if !takesPath(w) {
				delete(methods, name)
			}
		}
	}
	for name, w := range exportedFuncs {
		if ix.funcs[rel] == nil {
			ix.funcs[rel] = map[string]localWrapper{}
		}
		ix.funcs[rel][name] = w
		note(w)
	}
	for _, in := range instances {
		methods, ok := classes[in.class]
		if !ok {
			continue
		}
		if ix.instances[rel] == nil {
			ix.instances[rel] = map[string]map[string]localWrapper{}
		}
		ix.instances[rel][in.name] = methods
		for _, w := range methods {
			note(w)
		}
	}
}

// importedWrapperFacts emits a client route for every call in this file to a request
// helper it imports by name, or to a method of a class in this file that hands its
// URL to one (`this.makeRequest(url)` over `authApiClient.handleAuthenticatedRequest`).
// It returns the lines of those calls too: another pass may have read the same call
// by the helper's name, and less well, so its route on that line gives way.
func importedWrapperFacts(kinds *tsutil.KindTable, root *sitter.Node, ctx *extractCtx, ix *requestHelperIndex) ([]facts.Fact, [][2]int) {
	if ix.empty() {
		return nil, nil
	}
	funcs := map[string]localWrapper{}                // local name → helper
	instances := map[string]map[string]localWrapper{} // local name → method → helper
	for i := range root.ChildCount() {
		stmt := root.Child(i)
		if kindOf(kinds, stmt) != "import_statement" {
			continue
		}
		source := findChildByKind(kinds, stmt, "string")
		clause := findChildByKind(kinds, stmt, "import_clause")
		if source == nil || clause == nil {
			continue
		}
		file, ok := importedFile(strings.Trim(nodeText(source, ctx.src), `"'`), ctx)
		if !ok || (ix.funcs[file] == nil && ix.instances[file] == nil) {
			continue
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
			n := spec.ChildByFieldName("name")
			if n == nil {
				continue
			}
			exported, local := nodeText(n, ctx.src), nodeText(n, ctx.src)
			if a := spec.ChildByFieldName("alias"); a != nil {
				local = nodeText(a, ctx.src)
			}
			if w, ok := ix.funcs[file][exported]; ok {
				funcs[local] = w
			}
			if m, ok := ix.instances[file][exported]; ok {
				instances[local] = m
			}
		}
	}
	if len(funcs) == 0 && len(instances) == 0 {
		return nil, nil
	}

	module := singleAssignedLocals(kinds, root, ctx.src)
	imported := func(fn *sitter.Node) (localWrapper, string, bool) {
		switch kindOf(kinds, fn) {
		case "identifier":
			name := nodeText(fn, ctx.src)
			w, ok := funcs[name]
			return w, name, ok
		case "member_expression":
			obj, prop := fn.ChildByFieldName("object"), fn.ChildByFieldName("property")
			if obj == nil || prop == nil || kindOf(kinds, obj) != "identifier" {
				return localWrapper{}, "", false
			}
			w, ok := instances[nodeText(obj, ctx.src)][nodeText(prop, ctx.src)]
			return w, nodeText(fn, ctx.src), ok
		}
		return localWrapper{}, "", false
	}
	// This file's class methods that make their requests through an imported helper.
	// Keyed by where the class starts: a tree-sitter node is a value, and two walks
	// reaching one class hand back different pointers to it.
	own := map[uint]map[string]localWrapper{} // class start byte → method → helper
	inner := func(call *sitter.Node) (localWrapper, bool) {
		fn := calleeOf(kinds, call)
		if fn == nil {
			return localWrapper{}, false
		}
		w, _, ok := imported(fn)
		return w, ok
	}
	walkClasses(kinds, root, func(class *sitter.Node) {
		body := class.ChildByFieldName("body")
		if body == nil {
			return
		}
		for j := range body.ChildCount() {
			m := body.Child(j)
			mn := m.ChildByFieldName("name")
			if kindOf(kinds, m) != "method_definition" || mn == nil {
				continue
			}
			if w, ok := wrapperThrough(kinds, m, ctx.src, module, inner); ok {
				if own[class.StartByte()] == nil {
					own[class.StartByte()] = map[string]localWrapper{}
				}
				own[class.StartByte()][nodeText(mn, ctx.src)] = w
			}
		}
	})
	resolvers := map[uint]pathResolver{} // by the member's start byte, as own is
	resolverFor := func(call *sitter.Node) pathResolver {
		member, class := enclosingMember(kinds, call)
		if member == nil {
			return pathResolver{kinds: kinds, src: ctx.src, locals: module}
		}
		if r, ok := resolvers[member.StartByte()]; ok {
			return r
		}
		className := ""
		if n := class.ChildByFieldName("name"); n != nil {
			className = nodeText(n, ctx.src)
		}
		r := pathResolver{
			kinds:     kinds,
			src:       ctx.src,
			className: className,
			constants: angularClassConstants(kinds, class.ChildByFieldName("body"), ctx.src, className),
			locals:    singleAssignedLocals(kinds, member, ctx.src),
		}
		resolvers[member.StartByte()] = r
		return r
	}

	var out []facts.Fact
	var lines [][2]int
	seen := map[string]bool{}
	walkCalls(kinds, root, func(call *sitter.Node) {
		fn := calleeOf(kinds, call)
		if fn == nil {
			return
		}
		w, name, ok := imported(fn)
		if !ok && kindOf(kinds, fn) == "member_expression" {
			// this.<method>(…) on a method of the enclosing class.
			obj, prop := fn.ChildByFieldName("object"), fn.ChildByFieldName("property")
			if obj != nil && prop != nil && kindOf(kinds, obj) == "this" {
				if _, class := enclosingMember(kinds, call); class != nil {
					w, ok = own[class.StartByte()][nodeText(prop, ctx.src)]
					name = nodeText(fn, ctx.src)
				}
			}
		}
		if !ok || forwardsParameter(kinds, call, ctx.src) {
			return
		}
		f, ok := wrapperCallFact(kinds, call, ctx, resolverFor(call), w, name)
		if !ok {
			return
		}
		lines = append(lines, [2]int{f.Line, f.Line})
		key := f.PropString("method") + "\x00" + f.Name + "\x00" + strconv.Itoa(f.Line)
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, f)
	})
	return out, lines
}

// calleeOf is what a call calls. With type arguments, tree-sitter reads
// `await this.makeRequest<T>(url)` as a call of `await this.makeRequest`, so an await
// in the callee's place is looked through.
func calleeOf(kinds *tsutil.KindTable, call *sitter.Node) *sitter.Node {
	fn := call.ChildByFieldName("function")
	if fn != nil && kindOf(kinds, fn) == "await_expression" {
		for i := range fn.ChildCount() {
			if c := fn.Child(i); c.IsNamed() {
				return c
			}
		}
	}
	return fn
}

// forwardsParameter reports a call that passes on a path its function was handed:
// `makeRequest(endpoint) { return client.get(`/api${endpoint}`) }`. The request is
// made where makeRequest is called, not here. A forwarded path brings its own leading
// slash, so the parameter is the whole URL or follows text that does not end in one;
// an id fills a segment after a slash, `/proshops/${id}`, and that call is a request.
func forwardsParameter(kinds *tsutil.KindTable, call *sitter.Node, src []byte) bool {
	args := callArguments(kinds, call)
	if len(args) == 0 {
		return false
	}
	ops := urlOperands(kinds, args[0], src)
	for p := call.Parent(); p != nil; p = p.Parent() {
		switch kindOf(kinds, p) {
		case "method_definition", "function_declaration", "function_expression", "arrow_function", "function":
			param := firstParameter(kinds, p, src)
			if param == "" {
				continue
			}
			for i, op := range ops {
				if op.param(param) && (i == 0 || !ops[i-1].literal || !strings.HasSuffix(ops[i-1].text, "/")) {
					return true
				}
			}
		}
	}
	return false
}

// walkClasses visits every class declared in a file, exported or not.
func walkClasses(kinds *tsutil.KindTable, root *sitter.Node, visit func(*sitter.Node)) {
	for i := range root.ChildCount() {
		stmt := root.Child(i)
		if kindOf(kinds, stmt) == "export_statement" {
			if d := stmt.ChildByFieldName("declaration"); d != nil {
				stmt = d
			}
		}
		if kindOf(kinds, stmt) == "class_declaration" {
			visit(stmt)
		}
	}
}

// enclosingMember returns the class member a node is written in, and its class.
func enclosingMember(kinds *tsutil.KindTable, n *sitter.Node) (member, class *sitter.Node) {
	for p := n.Parent(); p != nil; p = p.Parent() {
		switch kindOf(kinds, p) {
		case "method_definition", "public_field_definition":
			if member == nil {
				member = p
			}
		case "class_declaration", "class":
			if member != nil {
				return member, p
			}
			return nil, nil
		}
	}
	return nil, nil
}

// importedFile resolves an import specifier to the repository file it names.
func importedFile(spec string, ctx *extractCtx) (string, bool) {
	resolved, external := resolveImportPath(spec, ctx.dir, ctx.aliases)
	if !external {
		file, _, ok := resolveModuleFile(resolved, ctx.knownFiles)
		return file, ok
	}
	return ctx.files.resolve(spec, ctx.relFile)
}

// classHelpers finds a class's request helper methods: the ones whose body hands
// their first parameter to fetch or axios, and then, until nothing changes, the ones
// that hand it to such a method of the same class through this: `fetch(url)` that
// calls `this.makeRequest(url)`, which calls fetch.
func classHelpers(kinds *tsutil.KindTable, body *sitter.Node, src []byte, module map[string]*sitter.Node) map[string]localWrapper {
	nodes := map[string]*sitter.Node{}
	for j := range body.ChildCount() {
		m := body.Child(j)
		if kindOf(kinds, m) != "method_definition" {
			continue
		}
		if mn := m.ChildByFieldName("name"); mn != nil {
			nodes[nodeText(mn, src)] = m
		}
	}
	methods := map[string]localWrapper{}
	for name, m := range nodes {
		if w, ok := wrapperOf(kinds, m, src, module); ok && takesPath(w) {
			methods[name] = w
		}
	}
	inner := func(call *sitter.Node) (localWrapper, bool) {
		fn := calleeOf(kinds, call)
		if fn == nil || kindOf(kinds, fn) != "member_expression" {
			return localWrapper{}, false
		}
		obj, prop := fn.ChildByFieldName("object"), fn.ChildByFieldName("property")
		if obj == nil || prop == nil || kindOf(kinds, obj) != "this" {
			return localWrapper{}, false
		}
		w, ok := methods[nodeText(prop, src)]
		return w, ok
	}
	for changed := true; changed; {
		changed = false
		for name, m := range nodes {
			if _, done := methods[name]; done {
				continue
			}
			if w, ok := wrapperThrough(kinds, m, src, module, inner); ok && takesPath(w) {
				methods[name] = w
				changed = true
			}
		}
	}
	return methods
}

// takesPath reports a helper whose parameter is a path: it brings its own leading
// slash, so the prefix before it is "" or "/api", never "/items/", and it is never a
// query value, "/range?date=". A function whose parameter fills a segment or a query
// value is an endpoint function taking an id or a filter, and its request is its own.
func takesPath(w localWrapper) bool {
	return !strings.HasSuffix(w.prefix, "/") && !strings.ContainsAny(w.prefix, "?=&#")
}
