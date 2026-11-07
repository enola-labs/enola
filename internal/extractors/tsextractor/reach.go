package tsextractor

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	sitter "github.com/enola-labs/enola/internal/treesitter"
	"github.com/enola-labs/enola/internal/workspace"

	"github.com/enola-labs/enola/internal/extractors/callsite"
	"github.com/enola-labs/enola/internal/extractors/tsutil"
	"github.com/enola-labs/enola/internal/facts"
)

// Which client calls the application can run.
//
// A frontend keeps client code no page reaches: an API method whose only caller is a
// facade method nothing calls, a method an interface declares and no one invokes.
// A request written there is in the source and is never sent, yet as a client route
// it made the endpoint it names look called. This pass marks each hand-written client
// route with PropClientReachable: whether its caller is reached from the
// application's entry points.
//
// Reachability is read by name, because the calls that matter are the ones a call
// graph cannot bind: `const api = useApi(); api.getItem(id)` reaches the client
// through a React context. A function or method is reached when its name is used in
// code already reached; everything an entry file declares is reached; and a file's
// top-level code runs once anything in it is reached. A function a symbol declares
// by name in its body, a handler or a closure a hook builds, is a scope of its own,
// reached the same way: by its name being used. Inside a symbol, binding a name is not
// using it: `const { save } = useShop()` and a hook's `return { save }` pass save on,
// and only a later read of save or shop.save uses it. A shorthand in any other object,
// `useQuery({ queryFn })`, still counts, since a library calls it by key. A name two
// symbols share
// reaches both, so the pass errs toward reached, which is the safe direction for a
// verdict that may drive an endpoint's removal.
//
// It gives a verdict only inside an application whose every use is in code this
// extractor parses: a Next.js app, or a React app with an entry file, at the root or
// as a workspace package (reachAppDirs). Vue, Svelte, Angular and Ember use components
// and handlers from templates, where a method called only from a template would read
// as unreached, and a library's calls are made by whoever consumes it, so those carry
// no verdict.

// fileReach is what one file contributes: the names each of its symbols uses, and the
// names its top-level code uses.
type fileReach struct {
	file    string
	symbols []string
	locals  map[string]string             // local scope → the name it is called by
	spans   []localSpan                   // where each local scope's function is written
	uses    map[string]map[string]useMode // symbol or local scope → names used in its body, and how
	top     map[string]useMode
	// The names a scope calls or hands over to be run (callsOrHandsOver), not bound as
	// a parameter: the uses a trigger travels along. top's are under "". lexical marks
	// the bare names among them (f(), not x.f()), which JavaScript resolves to what the
	// file declares or imports.
	calls   map[string]map[string]useMode
	lexical map[string]map[string]bool
	// plain marks, per scope ("" for top level), the names used as a plain identifier
	// (x, f(), <X/>) rather than as a member (o.x). JavaScript resolves a plain name in
	// its own module; only a member can name something no import brings in.
	plain map[string]map[string]bool
	// namedImports are the local names `import { a, b as c } from '…'` binds.
	namedImports map[string]bool
	// importedAs are the renamed ones: `import { b as c } from 'f'` binds c to f's b.
	importedAs map[string][]renamedExport
	modes      []modeSpan // regions an inline event handler or timer callback covers
	// The module edges that carry no name a use could match: a default import read
	// under a local name of its own, a default re-exported from another file, and a
	// module loaded by import(). Each names the file it reaches.
	defaults       []string            // the symbols this file default-exports
	defaultAliases map[string][]string // local name → files whose default it binds
	// reexportAliases are the names `export { default as Name } from '…'` publishes:
	// unlike an import's local name, other files can import them.
	reexportAliases map[string][]string
	// private are the file's top-level declarations it does not export: nothing
	// outside the file can name them.
	private map[string]bool
	// renamed are the names `export { Page as TagsPage } from './tags'` publishes:
	// the exported name → the file and the name it has there.
	renamed map[string][]renamedExport
	// exportsFrom are the files `export … from '…'` re-exports: a barrel's sources.
	exportsFrom []string
	// defaultFrom is `export { X as default } from '…'`: the file's default is that
	// file's X (a page re-exporting its component from a feature's barrel).
	defaultFrom []renamedExport
	// members are the file's symbols that belong to a class or object (a receiver).
	members map[string]bool
	// returned are, per function, the members of the object it returns (member_of):
	// `mutationFn` in a `() => ({ mutationFn })` options factory.
	returned  map[string][]string
	reexports []string            // files whose default this file re-exports
	dynamic   map[string][]string // symbol ("" for top level) → files it imports dynamically
	imports   []string            // every file this one imports, of any kind
}

// declaredNameOf maps a declaration's kind to the fields that name what it declares
// rather than use: their identifiers are not uses.
var declaredNameOf = map[string][]string{
	"function_declaration":           {"name"},
	"generator_function_declaration": {"name"},
	"class_declaration":              {"name"},
	"abstract_class_declaration":     {"name"},
	"interface_declaration":          {"name"},
	"type_alias_declaration":         {"name"},
	"method_definition":              {"name"},
	"method_signature":               {"name"},
	"abstract_method_signature":      {"name"},
	"public_field_definition":        {"name"},
	"property_signature":             {"name"},
	"variable_declarator":            {"name"},
	"pair":                           {"key"},
	// An enum's name and its members are declarations: `Draft = 'draft'` names a
	// member, and a member read as a use reached any symbol called Draft.
	"enum_declaration": {"name"},
	"enum_assignment":  {"name"},
}

// collectReach reads one parsed file. result must hold the file's symbol facts, and
// resolve maps an import specifier to the repository file it names.
func collectReach(kinds *tsutil.KindTable, root *sitter.Node, src []byte, relFile string, result []facts.Fact, resolve func(string) (string, bool)) *fileReach {
	g := callsite.Grammar{
		KindOf:    func(n *sitter.Node) string { return kindOf(kinds, n) },
		Functions: tsFunctionKinds,
		Wrappers:  tsDeclarationWrappers,
	}
	owner := map[uint]string{}
	for _, s := range callsite.Spans(g, root, result) {
		owner[s.StartByte] = s.Symbol
	}
	r := &fileReach{file: relFile, locals: map[string]string{}, uses: map[string]map[string]useMode{}, top: map[string]useMode{}, calls: map[string]map[string]useMode{}, lexical: map[string]map[string]bool{},
		plain: map[string]map[string]bool{}, namedImports: map[string]bool{}, importedAs: map[string][]renamedExport{}, reexportAliases: map[string][]string{}, private: map[string]bool{}, renamed: map[string][]renamedExport{}, members: map[string]bool{}, returned: map[string][]string{},
		defaultAliases: map[string][]string{}, dynamic: map[string][]string{}}
	consts := fileConsts(kinds, root, src)
	for _, f := range result {
		if f.Kind == facts.KindSymbol && f.File == relFile {
			// Types run nothing: an interface, a type alias and an interface's members
			// (abstract) are not code a page can reach. Kept as targets, a word like
			// `title` used anywhere reached a props interface's `title` member, ran the
			// file declaring it, and from there the rest of another feature.
			if isTypeOnlySymbol(f) || isDataMember(f) || facts.IsTestPath(relFile) {
				continue
			}
			r.symbols = append(r.symbols, f.Name)
			// A member of a class, or of the object a function returns (member_of), is
			// reached through that object wherever it travels; only a top-level
			// declaration the file does not export is private to it.
			member := f.PropString("receiver") != "" || f.PropString("member_of") != ""
			if exported, ok := f.PropAny("exported").(bool); ok && !exported && !member {
				r.private[f.Name] = true
			}
			if member {
				r.members[f.Name] = true
			}
			if owner := f.PropString("member_of"); owner != "" {
				r.returned[owner] = append(r.returned[owner], f.Name)
			}
			if f.PropAny(propDefaultExport) == true {
				r.defaults = append(r.defaults, f.Name)
			}
		}
	}
	importsOf(kinds, root, src, r, resolve)
	record := func(name, current string, mode useMode, calls, bare bool) {
		if bare {
			if r.plain[current] == nil {
				r.plain[current] = map[string]bool{}
			}
			r.plain[current][name] = true
		}
		if calls {
			if r.calls[current] == nil {
				r.calls[current] = map[string]useMode{}
			}
			if prev, ok := r.calls[current][name]; !ok || mode.stronger(prev) {
				r.calls[current][name] = mode
			}
			if bare {
				if r.lexical[current] == nil {
					r.lexical[current] = map[string]bool{}
				}
				r.lexical[current][name] = true
			}
		}
		uses := r.top
		if current != "" {
			if r.uses[current] == nil {
				r.uses[current] = map[string]useMode{}
			}
			uses = r.uses[current]
		}
		if prev, ok := uses[name]; !ok || mode.stronger(prev) {
			uses[name] = mode
		}
	}
	type nodeKey struct{ start, end uint }
	keyOf := func(n *sitter.Node) nodeKey { return nodeKey{n.StartByte(), n.EndByte()} }
	// Nodes that run under another use than the code around them: an event handler's
	// value, a timer's callback, a data hook's fetcher next to a refetch interval. A
	// call's arguments sit below its arguments node, so this is kept for the whole walk
	// rather than for one node's children.
	override := map[nodeKey]useMode{}
	// params is the parameter names of the functions around the node being walked,
	// innermost last: a lookup per identifier instead of a walk up the tree.
	var params []map[string]bool
	var walk func(n *sitter.Node, current string, mode useMode)
	walk = func(n *sitter.Node, current string, mode useMode) {
		k := kindOf(kinds, n)
		if tsFunctionKinds[k] {
			ps := n.ChildByFieldName("parameters")
			if ps == nil {
				ps = n.ChildByFieldName("parameter")
			}
			names := map[string]bool{}
			if ps != nil {
				collectParamNames(kinds, ps, src, names)
			}
			params = append(params, names)
			defer func() { params = params[:len(params)-1] }()
		}
		if tsFunctionKinds[k] {
			// The callsite index maps a closure inside a symbol to that symbol too: only
			// entering another symbol starts a new scope, with a plain use.
			if sym, ok := owner[n.StartByte()]; ok {
				if sym != current {
					current, mode = sym, plainUse
				}
			} else if name := localFunctionName(kinds, n, src); name != "" && (current != "" || isDefinedMethod(kinds, n, src)) {
				base := current
				if base == "" {
					base = relFile // a method an object at the top of the file defines
				}
				current = base + localScopeSep + name
				mode = plainUse
				r.locals[current] = name
				r.spans = append(r.spans, localSpan{current, int(n.StartPosition().Row) + 1, int(n.EndPosition().Row) + 1})
			}
		}
		switch k {
		case "call_expression":
			if fn := n.ChildByFieldName("function"); fn != nil && kindOf(kinds, fn) == "import" {
				if spec := importSpecifier(kinds, n, src); spec != "" {
					if file, ok := resolve(spec); ok {
						r.dynamic[current] = append(r.dynamic[current], file)
						r.imports = append(r.imports, file)
					}
				}
			}
			callee := ""
			if fn := n.ChildByFieldName("function"); fn != nil {
				callee = nodeText(fn, src)
			}
			args := callArguments(kinds, n)
			switch {
			case timerCallees[callee] && len(args) >= 1:
				ms := 0
				if len(args) >= 2 {
					ms = intervalMs(kinds, args[1], src, consts)
				}
				override[keyOf(args[0])] = useMode{kind: usePoll, ms: ms}
			case swrCallees[callee] && len(args) >= 3:
				if has, ms := refetchInterval(kinds, args[2], src, consts); has {
					override[keyOf(args[1])] = useMode{kind: usePoll, ms: ms}
				}
			case strings.HasSuffix(callee, "addEventListener") && len(args) >= 2:
				override[keyOf(args[1])] = useMode{kind: useAction}
			}
		case "jsx_attribute":
			if n.NamedChildCount() >= 2 && isEventAttribute(nodeText(n.NamedChild(0), src)) {
				for i := uint(1); i < n.NamedChildCount(); i++ {
					override[keyOf(n.NamedChild(i))] = useMode{kind: useAction}
				}
			}
		case "pair":
			if fetcherKeys[pairKey(kinds, n, src)] {
				if has, ms := refetchInterval(kinds, n.Parent(), src, consts); has {
					if v := n.ChildByFieldName("value"); v != nil {
						override[keyOf(v)] = useMode{kind: usePoll, ms: ms}
					}
				}
			}
		case "identifier", "property_identifier", "shorthand_property_identifier",
			"shorthand_property_identifier_pattern":
			// A type_identifier is a type annotation: it runs nothing. Recorded, it
			// read like a member name and reached any symbol called the same.
			if !isDeclaredName(kinds, n) && !bindsWithoutUse(kinds, n, src, current, owner) {
				// A parameter, destructured or not, is local to its function: reading
				// `renderItem` inside `({ renderItem }) => …` names the prop it was
				// handed, not a function of the same name another module declares.
				param := false
				if k == "identifier" {
					name := nodeText(n, src)
					for i := len(params) - 1; i >= 0 && !param; i-- {
						param = params[i][name]
					}
				}
				if !param {
					record(nodeText(n, src), current, mode, callsOrHandsOver(kinds, n), k == "identifier")
				}
			}
		}
		for i := range n.ChildCount() {
			c := n.Child(i)
			if m, ok := override[keyOf(c)]; ok {
				r.modes = append(r.modes, modeSpan{int(c.StartPosition().Row) + 1, int(c.EndPosition().Row) + 1, m})
				walk(c, current, m)
				continue
			}
			walk(c, current, mode)
		}
	}
	walk(root, "", plainUse)
	r.qualify(result)
	return r
}

// fileSep joins a symbol's name to its file in the pass's own keys. Symbol names are
// qualified by directory, so two files of one directory each exporting `Page` share
// one name; keyed by name alone, one page's Page ran another page's code. No symbol
// name contains it.
const fileSep = "\x01"

// keyOf is the pass's key for a symbol declared in file.
func keyOf(symbol, file string) string { return symbol + fileSep + file }

// symbolOf is the symbol name a key stands for.
func symbolOf(key string) string {
	if i := strings.Index(key, fileSep); i >= 0 {
		return key[:i]
	}
	return key
}

// qualify rewrites every key this file's symbols appear under to keyOf form: its
// symbols, defaults and private and member sets, and the scopes its uses are recorded
// under (a symbol, or a local scope inside one).
func (r *fileReach) qualify(result []facts.Fact) {
	declared := map[string]bool{}
	for _, f := range result {
		if f.Kind == facts.KindSymbol && f.File == r.file {
			declared[f.Name] = true
		}
	}
	q := func(scope string) string {
		if scope == "" {
			return ""
		}
		base, rest, _ := strings.Cut(scope, localScopeSep)
		if !declared[base] {
			return scope
		}
		if rest == "" && !strings.Contains(scope, localScopeSep) {
			return keyOf(base, r.file)
		}
		return keyOf(base, r.file) + localScopeSep + rest
	}
	list := func(xs []string) []string {
		for i, x := range xs {
			xs[i] = q(x)
		}
		return xs
	}
	r.symbols = list(r.symbols)
	r.defaults = list(r.defaults)
	set := func(m map[string]bool) map[string]bool {
		out := make(map[string]bool, len(m))
		for k, v := range m {
			out[q(k)] = v
		}
		return out
	}
	r.private, r.members = set(r.private), set(r.members)
	returned := make(map[string][]string, len(r.returned))
	for k, v := range r.returned {
		returned[q(k)] = list(v)
	}
	r.returned = returned
	nested := func(m map[string]map[string]bool) map[string]map[string]bool {
		out := make(map[string]map[string]bool, len(m))
		for k, v := range m {
			out[q(k)] = v
		}
		return out
	}
	r.lexical, r.plain = nested(r.lexical), nested(r.plain)
	modes := func(m map[string]map[string]useMode) map[string]map[string]useMode {
		out := make(map[string]map[string]useMode, len(m))
		for k, v := range m {
			out[q(k)] = v
		}
		return out
	}
	r.uses, r.calls = modes(r.uses), modes(r.calls)
	dyn := make(map[string][]string, len(r.dynamic))
	for k, v := range r.dynamic {
		dyn[q(k)] = v
	}
	r.dynamic = dyn
	locals := make(map[string]string, len(r.locals))
	for k, v := range r.locals {
		locals[q(k)] = v
	}
	r.locals = locals
	for i := range r.spans {
		r.spans[i].scope = q(r.spans[i].scope)
	}
}

// localSpan is the lines a local scope's function covers, 1-based and inclusive.
type localSpan struct {
	scope      string
	start, end int
}

// localScopeSep joins a local function's name to the scope that declares it. No
// symbol name contains it.
const localScopeSep = "\x00"

// localFunctionName is the name a function inside a symbol's body is declared under,
// when it is called only by that name: `function f() {}`, `const f = () => …` and
// `const f = useCallback(() => …, deps)`. A function handed to anything else, an
// effect, a timer, a library, runs without its name being used, so it stays part of
// the enclosing scope.
func localFunctionName(kinds *tsutil.KindTable, fn *sitter.Node, src []byte) string {
	k := kindOf(kinds, fn)
	if k == "function_declaration" || k == "generator_function_declaration" {
		if name := fn.ChildByFieldName("name"); name != nil {
			return nodeText(name, src)
		}
		return ""
	}
	if name := definedMethodName(kinds, fn, src); name != "" {
		return name
	}
	value := fn
	if p := fn.Parent(); p != nil && kindOf(kinds, p) == "arguments" {
		call := p.Parent()
		if call == nil || kindOf(kinds, call) != "call_expression" || p.NamedChildCount() == 0 || p.NamedChild(0).StartByte() != fn.StartByte() {
			return ""
		}
		callee := call.ChildByFieldName("function")
		if callee == nil || nodeText(callee, src) != "useCallback" && nodeText(callee, src) != "React.useCallback" {
			return ""
		}
		value = call
	}
	decl := value.Parent()
	if decl == nil || kindOf(kinds, decl) != "variable_declarator" {
		return ""
	}
	if v := decl.ChildByFieldName("value"); v == nil || v.StartByte() != value.StartByte() {
		return ""
	}
	if name := decl.ChildByFieldName("name"); name != nil && kindOf(kinds, name) == "identifier" {
		return nodeText(name, src)
	}
	return ""
}

// definedMethodName is the key a function is defined under in an object that is
// returned or assigned, not handed to a call: a store's methods
// (create((set, get) => ({ load: async () => … }))), an API object's. Such a function
// runs when its name is called, not when the object is built. A wrapper call around
// it (withAsyncWrapper('key', async () => …, set)) is part of the definition. An object passed to
// a call is a library's options, whose functions the library calls by key, and is left
// to the code around it.
func definedMethodName(kinds *tsutil.KindTable, fn *sitter.Node, src []byte) string {
	if kindOf(kinds, fn) == "method_definition" {
		if obj := fn.Parent(); obj != nil && kindOf(kinds, obj) == "object" && objectDefines(kinds, obj) {
			if name := fn.ChildByFieldName("name"); name != nil {
				return nodeText(name, src)
			}
		}
		return ""
	}
	value := fn
	if p := fn.Parent(); p != nil && kindOf(kinds, p) == "arguments" {
		if call := p.Parent(); call != nil && kindOf(kinds, call) == "call_expression" {
			value = call
		}
	}
	pair := value.Parent()
	if pair == nil || kindOf(kinds, pair) != "pair" {
		return ""
	}
	if v := pair.ChildByFieldName("value"); v == nil || v.StartByte() != value.StartByte() || v.EndByte() != value.EndByte() {
		return ""
	}
	obj := pair.Parent()
	if obj == nil || kindOf(kinds, obj) != "object" || !objectDefines(kinds, obj) {
		return ""
	}
	if k := pair.ChildByFieldName("key"); k != nil && kindOf(kinds, k) != "computed_property_name" {
		return strings.Trim(nodeText(k, src), `"'`)
	}
	return ""
}

// objectDefines reports an object that is returned from a function or assigned to a
// name, rather than handed to a call.
func objectDefines(kinds *tsutil.KindTable, obj *sitter.Node) bool {
	p := obj.Parent()
	if p == nil {
		return false
	}
	switch kindOf(kinds, p) {
	case "return_statement", "variable_declarator":
		return true
	case "parenthesized_expression":
		a := p.Parent()
		return a != nil && kindOf(kinds, a) == "arrow_function"
	}
	return false
}

// isDefinedMethod reports a function definedMethodName names.
func isDefinedMethod(kinds *tsutil.KindTable, fn *sitter.Node, src []byte) bool {
	return definedMethodName(kinds, fn, src) != ""
}

// bindsWithoutUse reports a name inside a symbol that is bound or passed on rather than
// read: a destructured name, `const { save } = …`, a shorthand in the object a hook
// returns, `return { save }`, directly or through `useMemo(() => ({ save }), deps)`, and
// a store selector's copy of a member under its own name, `useStore((s) => ({ save:
// s.save }))`, which hands save to whoever destructures it and runs nothing itself.
func bindsWithoutUse(kinds *tsutil.KindTable, n *sitter.Node, src []byte, current string, owner map[uint]string) bool {
	if current == "" {
		return false
	}
	switch kindOf(kinds, n) {
	case "shorthand_property_identifier_pattern":
		return true
	case "shorthand_property_identifier":
	case "property_identifier":
		return selectorCopy(kinds, n, src, current)
	default:
		return false
	}
	if strings.Contains(current, localScopeSep) || !isHookName(shortName(current)) {
		return false
	}
	obj := n.Parent()
	if obj == nil || kindOf(kinds, obj) != "object" || obj.Parent() == nil {
		return false
	}
	var fn *sitter.Node
	switch p := obj.Parent(); kindOf(kinds, p) {
	case "return_statement":
		fn = enclosingFunction(kinds, p)
	case "parenthesized_expression":
		if a := p.Parent(); a != nil && kindOf(kinds, a) == "arrow_function" {
			if body := a.ChildByFieldName("body"); body != nil && body.StartByte() == p.StartByte() {
				fn = a
			}
		}
	}
	if fn == nil {
		return false
	}
	if owner[fn.StartByte()] == current {
		return true
	}
	args := fn.Parent()
	if args == nil || kindOf(kinds, args) != "arguments" {
		return false
	}
	call := args.Parent()
	if call == nil || kindOf(kinds, call) != "call_expression" {
		return false
	}
	callee := call.ChildByFieldName("function")
	if callee == nil || (nodeText(callee, src) != "useMemo" && nodeText(callee, src) != "React.useMemo") {
		return false
	}
	ret := call.Parent()
	if ret == nil || kindOf(kinds, ret) != "return_statement" {
		return false
	}
	outer := enclosingFunction(kinds, ret)
	return outer != nil && owner[outer.StartByte()] == current
}

// selectorCopy reports the member n of `key: x.key` in the object a hook's arrow
// function returns to a call, the shape of a store selector.
func selectorCopy(kinds *tsutil.KindTable, n *sitter.Node, src []byte, current string) bool {
	if strings.Contains(current, localScopeSep) || !isHookName(shortName(current)) {
		return false
	}
	member := n.Parent()
	if member == nil || kindOf(kinds, member) != "member_expression" {
		return false
	}
	if prop := member.ChildByFieldName("property"); prop == nil || prop.StartByte() != n.StartByte() {
		return false
	}
	pair := member.Parent()
	if pair == nil || kindOf(kinds, pair) != "pair" {
		return false
	}
	key, value := pair.ChildByFieldName("key"), pair.ChildByFieldName("value")
	if key == nil || value == nil || value.StartByte() != member.StartByte() || nodeText(key, src) != nodeText(n, src) {
		return false
	}
	obj := pair.Parent()
	if obj == nil || kindOf(kinds, obj) != "object" {
		return false
	}
	body := obj.Parent()
	if body == nil || kindOf(kinds, body) != "parenthesized_expression" {
		return false
	}
	arrow := body.Parent()
	if arrow == nil || kindOf(kinds, arrow) != "arrow_function" {
		return false
	}
	args := arrow.Parent()
	return args != nil && kindOf(kinds, args) == "arguments"
}

// enclosingFunction is the nearest function around n.
func enclosingFunction(kinds *tsutil.KindTable, n *sitter.Node) *sitter.Node {
	for p := n.Parent(); p != nil; p = p.Parent() {
		if tsFunctionKinds[kindOf(kinds, p)] {
			return p
		}
	}
	return nil
}

// importsOf records a file's static imports and re-exports: every file it imports, the
// local names default imports bind, and the files whose default it re-exports.
func importsOf(kinds *tsutil.KindTable, root *sitter.Node, src []byte, r *fileReach, resolve func(string) (string, bool)) {
	for i := range root.ChildCount() {
		stmt := root.Child(i)
		k := kindOf(kinds, stmt)
		if k != "import_statement" && k != "export_statement" {
			continue
		}
		source := stmt.ChildByFieldName("source")
		if source == nil {
			source = findChildByKind(kinds, stmt, "string")
		}
		if source == nil || (k == "export_statement" && !strings.Contains(nodeText(stmt, src), " from")) {
			continue
		}
		file, ok := resolve(strings.Trim(nodeText(source, src), `"'`))
		if !ok {
			continue
		}
		r.imports = append(r.imports, file)
		if k == "export_statement" {
			r.exportsFrom = append(r.exportsFrom, file)
		}
		if k == "import_statement" {
			if clause := findChildByKind(kinds, stmt, "import_clause"); clause != nil {
				if def := findChildByKind(kinds, clause, "identifier"); def != nil {
					local := nodeText(def, src)
					r.defaultAliases[local] = append(r.defaultAliases[local], file)
				}
				if named := findChildByKind(kinds, clause, "named_imports"); named != nil {
					for j := range named.NamedChildCount() {
						spec := named.NamedChild(j)
						local := spec.ChildByFieldName("alias")
						if local == nil {
							local = spec.ChildByFieldName("name")
						} else if orig := spec.ChildByFieldName("name"); orig != nil {
							r.importedAs[nodeText(local, src)] = append(r.importedAs[nodeText(local, src)], renamedExport{file, nodeText(orig, src)})
						}
						if local != nil {
							r.namedImports[nodeText(local, src)] = true
						}
					}
				}
			}
			continue
		}
		// export { default } from '…', export { default as Name } from '…'
		if clause := findChildByKind(kinds, stmt, "export_clause"); clause != nil {
			for j := range clause.NamedChildCount() {
				spec := clause.NamedChild(j)
				name, alias := spec.ChildByFieldName("name"), spec.ChildByFieldName("alias")
				if name != nil && alias != nil && nodeText(name, src) != "default" {
					if nodeText(alias, src) == "default" {
						r.defaultFrom = append(r.defaultFrom, renamedExport{file, nodeText(name, src)})
					} else {
						r.renamed[nodeText(alias, src)] = append(r.renamed[nodeText(alias, src)], renamedExport{file, nodeText(name, src)})
					}
					continue
				}
				if name == nil || nodeText(name, src) != "default" {
					continue
				}
				if alias != nil {
					r.defaultAliases[nodeText(alias, src)] = append(r.defaultAliases[nodeText(alias, src)], file)
					r.reexportAliases[nodeText(alias, src)] = append(r.reexportAliases[nodeText(alias, src)], file)
				} else {
					r.reexports = append(r.reexports, file)
				}
			}
		}
	}
}

// importSpecifier is an import() call's specifier when it is written out: a string
// literal, or a template literal with nothing substituted.
func importSpecifier(kinds *tsutil.KindTable, call *sitter.Node, src []byte) string {
	args := callArguments(kinds, call)
	if len(args) == 0 {
		return ""
	}
	switch kindOf(kinds, args[0]) {
	case "string":
		return strings.Trim(nodeText(args[0], src), `"'`)
	case "template_string":
		t := nodeText(args[0], src)
		if !strings.Contains(t, "${") {
			return strings.Trim(t, "`")
		}
	}
	return ""
}

// isDeclaredName reports an identifier that names what its parent declares.
func isDeclaredName(kinds *tsutil.KindTable, n *sitter.Node) bool {
	p := n.Parent()
	if p == nil {
		return false
	}
	if kindOf(kinds, p) == "enum_body" {
		return true // a member without an initializer: `enum E { A, B }`
	}
	// A JSX attribute's name is the prop being set (`renderItem={…}`), not a reference
	// to anything called renderItem.
	if kindOf(kinds, p) == "jsx_attribute" && p.NamedChildCount() > 0 && p.NamedChild(0).StartByte() == n.StartByte() {
		return true
	}
	for _, field := range declaredNameOf[kindOf(kinds, p)] {
		if c := p.ChildByFieldName(field); c != nil && c.StartByte() == n.StartByte() && c.EndByte() == n.EndByte() {
			return true
		}
	}
	return false
}

// reachAppDirs lists the application packages the pass gives a verdict in: the root
// when it is a Next.js app, each workspace package that is one, and each package with
// a React dependency, no Vue, Svelte or Angular one, and an entry file. A package
// without an entry is a library, whose calls are made by whoever consumes it.
func reachAppDirs(repoPath string, fw tsFrameworks) []string {
	var out []string
	if fw.isNextJS && !fw.isVue && !fw.isNuxt && !fw.isSvelteKit && !fw.isAngular && !fw.isEmber {
		out = append(out, "")
	}
	out = append(out, fw.nextDirs...)
	seen := map[string]bool{}
	for _, d := range out {
		seen[d] = true
	}
	_ = filepath.WalkDir(repoPath, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if p != repoPath && (strings.HasPrefix(name, ".") || tsSkipDirs[name] || name == "testdata" || isExampleDir(name) || workspace.IsLinkedWorktree(p)) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != "package.json" {
			return nil
		}
		dir := filepath.Dir(p) //factpath:host
		rel, err := filepath.Rel(repoPath, dir)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			rel = ""
		}
		if seen[rel] || !hasPkgDependency(dir, "react") {
			return nil
		}
		for _, other := range []string{"vue", "svelte", "@angular/core", "ember-source"} {
			if hasPkgDependency(dir, other) {
				return nil
			}
		}
		for _, e := range reactEntryFiles {
			if _, err := os.Stat(filepath.Join(dir, e)); err == nil {
				seen[rel] = true
				out = append(out, rel)
				break
			}
		}
		return nil
	})
	return out
}

// inAppDir reports the app package a file belongs to, if any.
func inAppDir(file string, dirs []string) (string, bool) {
	best, found := "", false
	for _, d := range dirs {
		if (d == "" || file == d || strings.HasPrefix(file, d+"/")) && (!found || len(d) > len(best)) {
			best, found = d, true
		}
	}
	return best, found
}

// reactEntryFiles are where a React application without a file router starts.
var reactEntryFiles = []string{
	"src/main.tsx", "src/main.ts", "src/main.jsx", "src/main.js",
	"src/index.tsx", "src/index.ts", "src/index.jsx", "src/index.js",
	"index.tsx", "index.ts", "index.jsx", "index.js", "App.tsx", "App.jsx",
}

// markClientReachability sets PropClientReachable on the hand-written client routes in
// ff, from what each file uses. It does nothing outside a Next.js or React application,
// or when no entry file is found.
func markClientReachability(ff []facts.Fact, files []*fileReach, fw tsFrameworks, knownFiles map[string]bool) {
	if !reachApplies(fw) {
		return
	}
	roots := map[string]bool{}
	// The pages client_pages names: the routes the framework serves. Every other root
	// (an entry file, a file nothing imports) runs code, but is not a page; a library's
	// own pages/ folder that nothing imports is the latter.
	pageRoots := map[string]bool{}
	for _, f := range ff {
		if f.Kind == facts.KindRoute && f.PropString("framework") == "nextjs" && f.PropAny(facts.PropRole) != facts.RoleClient {
			if _, ok := inAppDir(f.File, fw.appDirs); ok {
				roots[f.File] = true
				pageRoots[f.File] = true
			}
		}
	}
	for _, d := range fw.appDirs {
		prefix := ""
		if d != "" {
			prefix = d + "/"
		}
		for _, e := range append(append([]string{}, reactEntryFiles...), "middleware.ts", "src/middleware.ts", "instrumentation.ts", "src/instrumentation.ts") {
			if knownFiles[prefix+e] {
				roots[prefix+e] = true
			}
		}
	}
	if len(roots) == 0 {
		return
	}

	byName := map[string][]string{} // short name → symbols
	symbolFile := map[string]*fileReach{}
	byFile := map[string]*fileReach{}
	aliases := map[string][]string{} // a default import's local name → the files it binds
	importedBy := map[string]int{}
	for _, r := range files {
		if r == nil {
			continue
		}
		byFile[r.file] = r
		for _, s := range r.symbols {
			symbolFile[s] = r
			byName[shortName(s)] = append(byName[shortName(s)], s)
		}
		for scope, name := range r.locals {
			symbolFile[scope] = r
			byName[name] = append(byName[name], scope)
		}
		// Only a re-exported default is a name another file can import: an import's
		// own local name (`import Card from './card'`) binds in that file alone.
		for name, targets := range r.reexportAliases {
			aliases[name] = append(aliases[name], targets...)
		}
		for _, t := range r.imports {
			if t != r.file {
				importedBy[t]++
			}
		}
	}
	// A file of an application that nothing imports runs on its own if it runs at all:
	// a script, a worker, a function deployed apart. Taken as an entry, it can only
	// make more calls count as reached, which is the safe side.
	for _, r := range files {
		if r == nil || importedBy[r.file] > 0 {
			continue
		}
		if _, ok := inAppDir(r.file, fw.appDirs); ok {
			roots[r.file] = true
		}
	}

	// Every page or one page: the root layout, the pages router's _app, entry files
	// and middleware run whichever page is open.
	rootTrigger := func(file string) trigger {
		t := trigger{rank: rankMount}
		base := file[strings.LastIndex(file, "/")+1:]
		stem := strings.TrimSuffix(base, filepath.Ext(base))
		dir := strings.TrimSuffix(file, "/"+base)
		switch {
		case stem == "layout" && (dir == "app" || strings.HasSuffix(dir, "/app")),
			stem == "_app" && (dir == "pages" || strings.HasSuffix(dir, "/pages")),
			stem == "middleware" || stem == "instrumentation":
			t.everyPage = true
		default:
			for _, e := range reactEntryFiles {
				if file == e || strings.HasSuffix(file, "/"+e) {
					t.everyPage = true
				}
			}
		}
		return t
	}

	// The strongest trigger reaching each function, local scope and file, raised to a
	// fixpoint. A name carries the strongest trigger any use of it passed on, so a name
	// is followed again only when that improves.
	val := map[string]trigger{}
	fileVal := map[string]trigger{}
	nameVal := map[string]trigger{}
	var queue []string
	raise := func(s string, t trigger) {
		if t.better(val[s]) {
			val[s] = t
			queue = append(queue, s)
		}
	}
	var runFile func(r *fileReach, t trigger)
	// reachDefault reaches what a file default-exports, and runs it. A default import
	// or re-export does use the default, so a file whose default this pass did not
	// mark has every symbol reached instead (orAll). An import() may be for a named
	// export, `const { apiClient } = await import(...)`, which the name already
	// reaches, so it takes the default only.
	reachDefault := func(file string, orAll bool, t trigger) {
		r := byFile[file]
		if r == nil {
			return
		}
		runFile(r, t)
		targets := r.defaults
		if len(targets) == 0 && orAll {
			targets = r.symbols
		}
		for _, s := range targets {
			raise(s, t)
		}
	}
	// A name declared in more than two files cannot say which one runs: it reaches them
	// all and carries no trigger. Two is a facade method and the client method it wraps.
	visible := barrelVisibility(byFile)
	declaredIn := map[string]map[string]bool{}
	for n, syms := range byName {
		for _, s := range syms {
			if r := symbolFile[s]; r != nil {
				if declaredIn[n] == nil {
					declaredIn[n] = map[string]bool{}
				}
				declaredIn[n][r.file] = true
			}
		}
	}
	importsOf := map[string]map[string]bool{}
	for _, r := range files {
		if r == nil {
			continue
		}
		set := map[string]bool{r.file: true}
		for _, f := range r.imports {
			set[f] = true
		}
		importsOf[r.file] = set
	}
	// propagate passes t to every target of a name, and to the files whose default the
	// name binds. key memoizes per name, or per file and name for a bare name resolved
	// in its file.
	propagate := func(key string, targets []string, defaults []string, t trigger) {
		if !t.better(nameVal[key]) {
			return
		}
		nameVal[key] = t
		for _, s := range targets {
			raise(s, t)
		}
		for _, f := range defaults {
			reachDefault(f, true, t)
		}
	}
	// use follows a scope's names: every name reaches what has it (reachability), and
	// a name the scope calls or hands over carries the trigger. A bare call resolves in
	// its file, to what the file declares, its locals, or the files it imports; one
	// that names nothing there, a name re-exported through a barrel, falls back to the
	// name as a member call does.
	use := func(r *fileReach, scope string, from trigger) {
		names, calls, lex := r.top, r.calls[scope], r.lexical[scope]
		if scope != "" {
			names = r.uses[scope]
		}
		for n := range names {
			if own := inScopeOf(r, n, r.plain[scope][n], byName, symbolFile, importsOf, func(f string) *fileReach { return byFile[f] }); len(own) > 0 {
				propagate(r.file+"\x00"+n, own, nil, from.reachedOnly())
				continue
			}
			if unboundPlainName(r, scope, n) {
				continue
			}
			propagate(n, global(r, byName[n], symbolFile, visible), aliases[n], from.reachedOnly())
		}
		for n, m := range calls {
			t := from.through(m)
			if lex[n] {
				var local []string
				for _, s := range byName[n] {
					if sf := symbolFile[s]; sf != nil && (sf == r || importsOf[r.file][sf.file]) {
						local = append(local, s)
					}
				}
				if len(local) > 0 || len(r.defaultAliases[n]) > 0 {
					propagate(r.file+"\x00"+n, local, r.defaultAliases[n], t)
					continue
				}
			}
			if own := inScopeOf(r, n, r.plain[scope][n], byName, symbolFile, importsOf, func(f string) *fileReach { return byFile[f] }); len(own) > 0 {
				propagate(r.file+"\x00"+n, own, nil, t)
				continue
			}
			if len(declaredIn[n]) <= 2 && !unboundPlainName(r, scope, n) {
				propagate(n, global(r, byName[n], symbolFile, visible), aliases[n], t)
			}
		}
	}
	runFile = func(r *fileReach, t trigger) {
		// A module's top level runs once, as it loads: whatever reached something in it,
		// a poll or a click, it runs on load, on every page if that reached it there.
		if t.rank >= rankAction {
			t = trigger{rank: rankMount, everyPage: t.everyPage}
		}
		if r == nil || !t.better(fileVal[r.file]) {
			return
		}
		fileVal[r.file] = t
		use(r, "", t)
		for _, f := range r.reexports {
			reachDefault(f, true, t)
		}
		// `export { X as default } from '…'`: the file's default is that file's X.
		for _, e := range r.defaultFrom {
			for _, s := range resolveExport(e.file, e.name, func(f string) *fileReach { return byFile[f] }, map[string]bool{}) {
				raise(s, t)
			}
		}
		for _, f := range r.dynamic[""] {
			reachDefault(f, false, t)
		}
	}
	for file := range roots {
		r := byFile[file]
		if r == nil {
			continue
		}
		t := rootTrigger(file)
		runFile(r, t)
		for _, s := range r.symbols {
			raise(s, t)
		}
	}
	for len(queue) > 0 {
		s := queue[0]
		queue = queue[1:]
		t := val[s]
		r := symbolFile[s]
		runFile(r, t)
		if r != nil {
			use(r, s, t)
			// Calling a function that returns an object of functions hands those on:
			// whoever takes the object (useMutation(options())) calls them.
			for _, m := range r.returned[s] {
				raise(m, t)
			}
			for _, f := range r.dynamic[s] {
				reachDefault(f, false, t)
			}
		}
	}

	classMember := map[string]bool{}
	classes := map[string]bool{}
	for _, f := range ff {
		if f.Kind == facts.KindSymbol && f.PropString("symbol_kind") == facts.SymbolClass {
			classes[f.Name] = true
		}
	}
	for _, f := range ff {
		if i := strings.LastIndexByte(f.Name, '.'); f.Kind == facts.KindSymbol && i > 0 && classes[f.Name[:i]] {
			classMember[f.Name] = true
		}
	}
	pages, pageOf, pageFile := reachingPages(pageRoots, files, byFile, byName, symbolFile, aliases, declaredIn, importsOf, classMember, visible)

	for i := range ff {
		f := &ff[i]
		if f.Kind != facts.KindRoute || f.PropAny(facts.PropRole) != facts.RoleClient ||
			!facts.HandWrittenClientSources[f.PropString(facts.PropSource)] || facts.IsTestPath(f.File) {
			continue
		}
		r := byFile[f.File]
		if r == nil {
			continue // a file this pass did not read: no verdict
		}
		if _, ok := inAppDir(f.File, fw.appDirs); !ok {
			continue // library code: its calls are made by whoever consumes it
		}
		t := fileVal[f.File]
		if scope := innermostLocal(r, f.Line); scope != "" {
			t = val[scope]
		} else if caller := f.PropString(facts.PropCaller); caller != "" {
			t = val[keyOf(caller, f.File)]
			if _, ok := val[keyOf(caller, f.File)]; !ok {
				t = val[caller] // a caller that is no declared symbol keeps its own name
			}
		}
		f.SetProp(facts.PropClientReachable, t.rank > rankNone)
		if t.rank == rankNone {
			continue
		}
		set := pageFile[f.File]
		if scope := innermostLocal(r, f.Line); scope != "" {
			set = pageOf[scope]
		} else if caller := f.PropString(facts.PropCaller); caller != "" {
			set = pageOf[keyOf(caller, f.File)]
			if _, ok := pageOf[keyOf(caller, f.File)]; !ok {
				set = pageOf[caller]
			}
		}
		everyPage := false
		if names := pages.names(set); len(names) > everyPageListMax && len(names) == len(pages) {
			// Reached from every page of an app with more than a handful: that is
			// what client_every_page says, and a list of them all names none in
			// particular. A small app's full list stays, it is still the answer.
			everyPage = true
			f.SetProp(facts.PropClientEveryPage, true)
		} else if len(names) > 0 {
			f.SetProp(facts.PropClientPages, names)
		}
		// A request written inside an inline handler or timer callback runs under it.
		if m, ok := innermostMode(r.modes, f.Line); ok {
			t = t.through(m)
		}
		if t.name() == "" {
			continue // reached, but only by name: no trigger to state
		}
		f.SetProp(facts.PropClientTrigger, t.name())
		if t.everyPage || everyPage {
			f.SetProp(facts.PropClientEveryPage, true)
		}
		if t.rank == rankPoll && t.ms > 0 {
			f.SetProp(facts.PropClientPollMs, t.ms)
		}
	}
}

// pageSet is a set of pages, one bit per page in pageIndex order.
type pageSet []uint64

// with returns s joined with o, and whether that added anything.
func (s pageSet) with(o pageSet) (pageSet, bool) {
	grew := false
	for i, w := range o {
		if w == 0 {
			continue
		}
		for len(s) <= i {
			s = append(s, 0)
		}
		if s[i]|w != s[i] {
			s[i] |= w
			grew = true
		}
	}
	return s, grew
}

// pageIndex names the pages a pageSet's bits stand for.
type pageIndex []string

func (p pageIndex) names(s pageSet) []string {
	var out []string
	for i, w := range s {
		for b := 0; b < 64; b++ {
			if w&(1<<b) != 0 {
				out = append(out, p[i*64+b])
			}
		}
	}
	sort.Strings(out)
	return out
}

// isPageFile reports whether a root is one page rather than code every page runs: a
// Next.js page under app/, or a file under the pages router other than its _app,
// _document or an API route.
func isPageFile(file string) bool {
	base := file[strings.LastIndex(file, "/")+1:]
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	if stem == "page" && (strings.HasPrefix(file, "app/") || strings.Contains(file, "/app/")) {
		return true
	}
	i := strings.Index("/"+file, "/pages/")
	if i < 0 || strings.HasPrefix(stem, "_") {
		return false
	}
	return !strings.HasPrefix(file[i:], "pages/api/")
}

// reachingPages is the trigger pass again, carrying which pages reach each symbol,
// local scope and file instead of how. It follows the same uses, more strictly: a
// name declared in more than two files reaches none of them here, called or not, so
// that a page is named only where its code can be told apart; the trigger pass, which
// decides only whether a call runs at all, lets such a name reach them all.
func reachingPages(roots map[string]bool, files []*fileReach, byFile map[string]*fileReach, byName map[string][]string,
	symbolFile map[string]*fileReach, aliases map[string][]string, declaredIn map[string]map[string]bool,
	importsOf map[string]map[string]bool, classMember map[string]bool, visible func(*fileReach, string) bool) (pageIndex, map[string]pageSet, map[string]pageSet) {
	var index pageIndex
	for f := range roots {
		if isPageFile(f) {
			index = append(index, f)
		}
	}
	sort.Strings(index)
	of, fileOf, nameOf := map[string]pageSet{}, map[string]pageSet{}, map[string]pageSet{}
	var queue []string
	raise := func(s string, b pageSet) {
		var grew bool
		if of[s], grew = of[s].with(b); grew {
			queue = append(queue, s)
		}
	}
	var runFile func(r *fileReach, b pageSet)
	reachDefault := func(file string, orAll bool, b pageSet) {
		r := byFile[file]
		if r == nil {
			return
		}
		runFile(r, b)
		targets := r.defaults
		if len(targets) == 0 && orAll {
			targets = r.symbols
		}
		for _, s := range targets {
			raise(s, b)
		}
	}
	propagate := func(key string, targets, defaults []string, b pageSet) {
		var grew bool
		if nameOf[key], grew = nameOf[key].with(b); !grew {
			return
		}
		for _, s := range targets {
			raise(s, b)
		}
		for _, f := range defaults {
			reachDefault(f, true, b)
		}
	}
	use := func(r *fileReach, scope string, b pageSet) {
		names, lex := r.top, r.lexical[scope]
		if scope != "" {
			names = r.uses[scope]
		}
		for n := range names {
			if lex[n] {
				var local []string
				for _, s := range byName[n] {
					if sf := symbolFile[s]; sf != nil && (sf == r || importsOf[r.file][sf.file]) && !classMember[symbolOf(s)] {
						local = append(local, s)
					}
				}
				if len(local) > 0 || len(r.defaultAliases[n]) > 0 {
					propagate(r.file+"\x00"+n, local, r.defaultAliases[n], b)
					continue
				}
				// A bare name re-exported through a barrel is a function, a local scope
				// or an object's member handed out by a hook. Here and above, it is
				// never a member of a class: zustand's create(…) is not
				// OrdersApi.create, even in a file that imports OrdersApi.
				if len(declaredIn[n]) <= 2 && !unboundPlainName(r, scope, n) {
					var free []string
					for _, s := range global(r, byName[n], symbolFile, visible) {
						if !classMember[symbolOf(s)] {
							free = append(free, s)
						}
					}
					propagate("\x00bare\x00"+n, free, aliases[n], b)
				}
				continue
			}
			if own := inScopeOf(r, n, r.plain[scope][n], byName, symbolFile, importsOf, func(f string) *fileReach { return byFile[f] }); len(own) > 0 {
				propagate(r.file+"\x00"+n, own, nil, b)
				continue
			}
			if len(declaredIn[n]) <= 2 && !unboundPlainName(r, scope, n) {
				propagate(n, global(r, byName[n], symbolFile, visible), aliases[n], b)
			}
		}
	}
	runFile = func(r *fileReach, b pageSet) {
		if r == nil {
			return
		}
		var grew bool
		if fileOf[r.file], grew = fileOf[r.file].with(b); !grew {
			return
		}
		use(r, "", b)
		for _, f := range r.reexports {
			reachDefault(f, true, b)
		}
		// `export { X as default } from '…'`: the file's default is that file's X.
		for _, e := range r.defaultFrom {
			for _, s := range resolveExport(e.file, e.name, func(f string) *fileReach { return byFile[f] }, map[string]bool{}) {
				raise(s, b)
			}
		}
		for _, f := range r.dynamic[""] {
			reachDefault(f, false, b)
		}
	}
	for i, file := range index {
		b := make(pageSet, i/64+1)
		b[i/64] = 1 << (i % 64)
		r := byFile[file]
		runFile(r, b)
		if r != nil {
			for _, s := range r.symbols {
				raise(s, b)
			}
		}
	}
	for len(queue) > 0 {
		s := queue[0]
		queue = queue[1:]
		b := of[s]
		r := symbolFile[s]
		runFile(r, b)
		if r != nil {
			use(r, s, b)
			for _, m := range r.returned[s] {
				raise(m, b)
			}
			for _, f := range r.dynamic[s] {
				reachDefault(f, false, b)
			}
		}
	}
	return index, of, fileOf
}

// innermostLocal is the narrowest local scope written around a line, or "" when the
// line is in no local function. A call's caller names the enclosing symbol, which a
// closure inside it does not change.
func innermostLocal(r *fileReach, line int) string {
	best, width := "", 0
	for _, sp := range r.spans {
		if sp.start <= line && line <= sp.end && (best == "" || sp.end-sp.start < width) {
			best, width = sp.scope, sp.end-sp.start
		}
	}
	return best
}

// reachApplies reports a repository with an application whose every use is in code
// this extractor parses.
func reachApplies(fw tsFrameworks) bool {
	return len(fw.appDirs) > 0
}

// shortName is the name a symbol is used by: the last segment of its fact name.
func shortName(symbol string) string {
	symbol = symbolOf(symbol)
	if i := strings.LastIndexByte(symbol, '.'); i >= 0 {
		return symbol[i+1:]
	}
	return symbol
}

// renamedExport is one `export { name as alias } from 'file'`.
type renamedExport struct{ file, name string }

// inScopeOf returns the symbols a name used in r can be: the ones r itself declares,
// else the ones in files r imports. A name resolves in its own module first, as
// JavaScript resolves it; only a name neither declares (a member reached through an
// object, a name re-exported through a barrel) falls back to every symbol of that name.
// Falling back first let `COLUMNS` in one feature's table reach another feature's
// `COLUMNS`, and with it every page of the other feature.
//
// Only a plain name prefers its own module's symbol: a member (`api.createLabel`) is
// the imported module's even where this file declares a createLabel of its own, so
// for a member both stay candidates.
func inScopeOf(r *fileReach, n string, plain bool, byName map[string][]string, symbolFile map[string]*fileReach,
	importsOf map[string]map[string]bool, fileOf func(string) *fileReach) []string {
	var own, imported []string
	for _, s := range byName[n] {
		sf := symbolFile[s]
		switch {
		case sf == nil:
		case sf == r:
			own = append(own, s)
		case importsOf[r.file][sf.file]:
			imported = append(imported, s)
		}
	}
	if plain && len(own) > 0 {
		return own
	}
	// Imported under a name of its own: `import { getCards as getCardsApi }`.
	for _, e := range r.importedAs[n] {
		imported = append(imported, resolveExport(e.file, e.name, fileOf, map[string]bool{})...)
	}
	if len(imported) > 0 {
		return append(own, imported...)
	}
	// Imported through a barrel: the name a file r imports exports, as it renames
	// it or re-exports it from further on (`export { Page as TagsPage }`,
	// `export * from`).
	for f := range importsOf[r.file] {
		if f != r.file {
			imported = append(imported, resolveExport(f, n, fileOf, map[string]bool{})...)
		}
	}
	return append(own, imported...)
}

// isTypeOnlySymbol reports a declaration with no runtime presence: an interface, a type
// alias, or an abstract member (an interface's property or method signature).
func isTypeOnlySymbol(f facts.Fact) bool {
	switch f.PropString("symbol_kind") {
	case facts.SymbolInterface, facts.SymbolType:
		return true
	}
	return f.PropBool("abstract")
}

// unboundPlainName reports a name a scope uses only as a plain identifier that its
// file neither declares, imports by name nor default-imports: a global (window, fetch)
// or a binding this pass does not track. It names no symbol of another module, so it
// must not fall back to every symbol of that name: `<Notification/>` in one file is
// not a default import called Notification in some other file.
func unboundPlainName(r *fileReach, scope, n string) bool {
	return r.plain[scope][n] && !r.namedImports[n] && len(r.defaultAliases[n]) == 0
}

// global drops from a name's symbols the ones a name used in r cannot be when r
// neither declares nor imports it: a local scope (a function declared inside another,
// callable only where it is written), a file's private top-level declaration, and a
// top-level symbol of a module r cannot see, directly or through the barrels it
// imports. A member of a class or object stays: an instance's methods are reached
// through objects, wherever they were created.
func global(r *fileReach, syms []string, symbolFile map[string]*fileReach, visible func(*fileReach, string) bool) []string {
	var out []string
	for _, s := range syms {
		if strings.Contains(s, localScopeSep) {
			continue
		}
		sf := symbolFile[s]
		if sf != nil && sf.private[s] {
			continue
		}
		if sf != nil && !sf.members[s] && !visible(r, sf.file) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// barrelVisibility answers whether a file can name another's top-level symbols: the
// other is one it imports, or one those re-export, through any chain of barrels.
func barrelVisibility(byFile map[string]*fileReach) func(*fileReach, string) bool {
	closure := map[string]map[string]bool{}
	var through func(file string) map[string]bool
	through = func(file string) map[string]bool {
		if c, ok := closure[file]; ok {
			return c
		}
		c := map[string]bool{file: true}
		closure[file] = c // cycles between barrels end here
		if g := byFile[file]; g != nil {
			for _, f := range g.exportsFrom {
				for x := range through(f) {
					c[x] = true
				}
			}
		}
		return c
	}
	return func(r *fileReach, target string) bool {
		if r == nil {
			return true
		}
		if r.file == target {
			return true
		}
		for _, f := range r.imports {
			if through(f)[target] {
				return true
			}
		}
		return false
	}
}

// resolveExport returns the symbols a file exports under name: one it declares, one it
// renames on the way out (`export { Page as TagsPage } from`), or one a file it
// re-exports everything from (`export * from`) exports, through any chain of barrels.
func resolveExport(file, name string, fileOf func(string) *fileReach, seen map[string]bool) []string {
	if seen[file] {
		return nil
	}
	seen[file] = true
	g := fileOf(file)
	if g == nil {
		return nil
	}
	var out []string
	for _, s := range g.symbols {
		if shortName(s) == name && !g.private[s] {
			out = append(out, s)
		}
	}
	if len(out) > 0 {
		return out
	}
	for _, e := range g.renamed[name] {
		out = append(out, resolveExport(e.file, e.name, fileOf, seen)...)
	}
	if len(out) > 0 {
		return out
	}
	for _, f := range g.exportsFrom {
		out = append(out, resolveExport(f, name, fileOf, seen)...)
	}
	return out
}

// isDataMember reports a class's or object's data field: it holds a value and runs
// nothing, so a member read of the same name (`x.name`) reaching it reached nothing
// but the file that declares it, and from there everything that file names.
func isDataMember(f facts.Fact) bool {
	if f.PropString("receiver") == "" {
		return false
	}
	switch f.PropString("symbol_kind") {
	case facts.SymbolVariable, facts.SymbolConstant, "property", "field":
		return true
	}
	return false
}

// everyPageListMax is the most pages a call reached from all of them is listed by
// name; above it the call is marked client_every_page instead.
const everyPageListMax = 20

// collectParamNames adds the names a parameter list binds, directly or by
// destructuring. A default value is an expression, not a binding.
func collectParamNames(kinds *tsutil.KindTable, n *sitter.Node, src []byte, out map[string]bool) {
	switch kindOf(kinds, n) {
	case "identifier", "shorthand_property_identifier_pattern":
		out[nodeText(n, src)] = true
		return
	case "type_annotation", "type_identifier":
		return
	}
	for i := range n.NamedChildCount() {
		c := n.NamedChild(i)
		// A default value is an expression, not a binding.
		if p := n.ChildByFieldName("value"); p != nil && p.StartByte() == c.StartByte() && kindOf(kinds, n) != "pair_pattern" {
			continue
		}
		collectParamNames(kinds, c, src, out)
	}
}
