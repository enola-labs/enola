package tsextractor

import (
	"strconv"
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"

	"github.com/enola-labs/enola/internal/extractors/tsutil"
	"github.com/enola-labs/enola/internal/facts"
)

// Requests made through a helper declared in the same file.
//
// A file can route every request through a function of its own:
//
//	const apiRequest = async (url: string, options = {}) => {
//	  const fullUrl = `${API_BASE_URL}/api${url}`;
//	  return fetch(fullUrl, options);
//	};
//	apiRequest(`/settings/trip-events/${id}/participants`);
//
// No client pattern names apiRequest, so none of those calls was a route, and the
// routes they reach read as called by nobody. What makes the helper a request is
// visible in the file: its first parameter is the END of the URL it hands fetch or
// axios, after a literal prefix. Each call to it in the same file is then a request to
// that prefix followed by the call's first argument.
//
// It is held narrow on purpose. The parameter must be the last operand of the URL, so
// a helper that builds `${base}/${id}/details` is not one; a URL local must agree in
// every declaration and never be reassigned; only a helper declared at the top of the
// file is read, and only calls in that file. A helper imported from elsewhere is
// importedwrapper.go's.

// localWrapper is a request helper declared at the top of a file.
type localWrapper struct {
	prefix    string // literal text the helper puts before its argument ("/api")
	framework string // the library it calls: fetch or axios
	lines     []int  // the lines of the helper's own requests to its argument, 1-based
}

// localWrapperFacts emits a client route for every call in this file to a request
// helper the file declares, and returns the lines of the helpers' own requests to their
// argument: such a request goes to the caller's URL, not anywhere the body can name. A
// request in a helper that does not use the argument (a token refresh) is not among them.
func localWrapperFacts(kinds *tsutil.KindTable, root *sitter.Node, ctx *extractCtx) ([]facts.Fact, [][2]int) {
	module := singleAssignedLocals(kinds, root, ctx.src)
	wrappers := localRequestWrappers(kinds, root, ctx.src, module)
	if len(wrappers) == 0 {
		return nil, nil
	}
	// A helper is confirmed by a call that hands it a path. An endpoint function puts
	// its parameter last too, `.../verify-email/${confirmationId}`, but its callers pass
	// an id; until a call passes a path, a function is not a helper and its own request
	// is left alone.
	called := map[string]bool{}
	r := pathResolver{kinds: kinds, src: ctx.src, locals: module}
	var out []facts.Fact
	seen := map[string]bool{}
	walkCalls(kinds, root, func(call *sitter.Node) {
		fn := calleeOf(kinds, call)
		if fn == nil || kindOf(kinds, fn) != "identifier" {
			return
		}
		name := nodeText(fn, ctx.src)
		w, ok := wrappers[name]
		if !ok {
			return
		}
		if args := callArguments(kinds, call); len(args) > 0 {
			if raw, ok := r.text(args[0]); ok && strings.HasPrefix(raw, "/") {
				called[name] = true
			}
		}
		f, ok := wrapperCallFact(kinds, call, ctx, r, w, name)
		if !ok {
			return
		}
		key := f.PropString("method") + "\x00" + f.Name + "\x00" + strconv.Itoa(f.Line)
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, f)
	})
	var spans [][2]int
	for name, w := range wrappers {
		if !called[name] {
			continue
		}
		for _, l := range w.lines {
			spans = append(spans, [2]int{l, l})
		}
	}
	return out, spans
}

// wrapperCallFact is the client route a call to a request helper makes: the helper's
// prefix followed by the path the call hands it, with the verb its options name.
func wrapperCallFact(kinds *tsutil.KindTable, call *sitter.Node, ctx *extractCtx, r pathResolver, w localWrapper, name string) (facts.Fact, bool) {
	args := callArguments(kinds, call)
	if len(args) == 0 {
		return facts.Fact{}, false
	}
	raw, ok := r.text(args[0])
	if !ok {
		raw, ok = afterBase(r, args[0])
	}
	if !ok || !strings.HasPrefix(raw, "/") {
		return facts.Fact{}, false
	}
	path, ok := rootRequestPath(w.prefix + raw)
	if !ok {
		return facts.Fact{}, false
	}
	verb := "GET" // what fetch and axios do when nothing says otherwise
	if len(args) > 1 {
		if v := objectVerb(kinds, args[1], ctx.src, "method"); v != "" {
			verb = v
		}
	}
	props := map[string]any{
		facts.PropRole:   facts.RoleClient,
		"method":         verb,
		"framework":      w.framework,
		"language":       "typescript",
		facts.PropSource: facts.RouteSourceTSHTTPClient,
		"api":            tsAPIHint(ctx.relFile),
		"client_wrapper": name,
	}
	if testDoublePath(ctx.relFile) {
		props["test_double"] = true
	}
	return facts.Fact{
		Kind:      facts.KindRoute,
		Name:      path,
		File:      ctx.relFile,
		Line:      int(call.StartPosition().Row) + 1,
		Props:     props,
		Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: ctx.dir}},
	}, true
}

// dropWrapperBodyRequests removes the client routes another pass read off a helper's
// own request. A pattern that folds the helper's URL local sees `${base}/api${url}`
// and keeps "/api", a request to nowhere: the URL is the caller's.
func dropWrapperBodyRequests(ff []facts.Fact, relFile string, spans [][2]int) []facts.Fact {
	if len(spans) == 0 {
		return ff
	}
	out := ff[:0]
	for _, f := range ff {
		if f.Kind == facts.KindRoute && f.File == relFile && f.PropString(facts.PropRole) == facts.RoleClient &&
			f.PropString("client_wrapper") == "" {
			inside := false
			for _, s := range spans {
				inside = inside || (f.Line >= s[0] && f.Line <= s[1])
			}
			if inside {
				continue
			}
		}
		out = append(out, f)
	}
	return out
}

// localRequestWrappers finds the request helpers declared at the top of a file, by name.
// fetch and makeRequest are excluded: the regex client pass already reads them by name.
//
// module holds the file's single-assigned bindings, so a base the file binds to an
// absolute URL is recognised as a third party's address rather than dropped.
func localRequestWrappers(kinds *tsutil.KindTable, root *sitter.Node, src []byte, module map[string]*sitter.Node) map[string]localWrapper {
	out := map[string]localWrapper{}
	consider := func(name string, fn *sitter.Node) {
		if name == "" || name == "fetch" || name == "makeRequest" || fn == nil {
			return
		}
		if w, ok := wrapperOf(kinds, fn, src, module); ok {
			out[name] = w
		}
	}
	for i := range root.ChildCount() {
		stmt := root.Child(i)
		if kindOf(kinds, stmt) == "export_statement" {
			if d := stmt.ChildByFieldName("declaration"); d != nil {
				stmt = d
			}
		}
		switch kindOf(kinds, stmt) {
		case "function_declaration":
			if n := stmt.ChildByFieldName("name"); n != nil {
				consider(nodeText(n, src), stmt)
			}
		case "lexical_declaration", "variable_declaration":
			for j := range stmt.ChildCount() {
				d := stmt.Child(j)
				if kindOf(kinds, d) != "variable_declarator" {
					continue
				}
				n, v := d.ChildByFieldName("name"), d.ChildByFieldName("value")
				if n == nil || v == nil || kindOf(kinds, n) != "identifier" {
					continue
				}
				switch kindOf(kinds, v) {
				case "arrow_function", "function_expression", "function":
					consider(nodeText(n, src), v)
				}
			}
		}
	}
	return out
}

// wrapperOf reports whether a function is a request helper, and with what prefix: its
// body hands fetch or axios a URL that ends in its first parameter. Every such request
// in the body must give the same prefix; a request whose URL does not involve the
// parameter is an ordinary request and is left to the other passes.
func wrapperOf(kinds *tsutil.KindTable, fn *sitter.Node, src []byte, module map[string]*sitter.Node) (localWrapper, bool) {
	return wrapperThrough(kinds, fn, src, module, nil)
}

// innerHelper reports that a call is to another request helper, with the prefix it
// puts before what it is handed and the library it ends in.
type innerHelper func(call *sitter.Node) (prefix, framework string, ok bool)

// wrapperThrough is wrapperOf for a function that may make its request through
// another helper rather than fetch or axios itself: then its prefix is the inner
// helper's followed by its own.
func wrapperThrough(kinds *tsutil.KindTable, fn *sitter.Node, src []byte, module map[string]*sitter.Node, inner innerHelper) (localWrapper, bool) {
	param := firstParameter(kinds, fn, src)
	body := fn.ChildByFieldName("body")
	if param == "" || body == nil {
		return localWrapper{}, false
	}
	decls, reassigned := localDeclarations(kinds, body, src)
	var found *localWrapper
	agree := true
	walkCalls(kinds, body, func(call *sitter.Node) {
		framework, through := requestLibrary(kinds, call, src), ""
		if framework == "" && inner != nil {
			if p, fw, ok := inner(call); ok {
				framework, through = fw, p
			}
		}
		if framework == "" {
			return
		}
		args := callArguments(kinds, call)
		if len(args) == 0 {
			return
		}
		prefix, uses := urlPrefix(kinds, args[0], src, param, decls, reassigned, module)
		if !uses {
			return
		}
		if prefix == nil {
			agree = false // it does use the parameter, in a form this cannot read
			return
		}
		full := through + *prefix
		if found == nil {
			found = &localWrapper{prefix: full, framework: framework}
		} else if found.prefix != full {
			agree = false
		}
		found.lines = append(found.lines, int(call.StartPosition().Row)+1)
	})
	if found == nil || !agree {
		return localWrapper{}, false
	}
	// An absolute URL is a third party's address, and the prefix would be a host this
	// pass cannot see the meaning of; a relative prefix is a path.
	if found.prefix != "" && !strings.HasPrefix(found.prefix, "/") {
		return localWrapper{}, false
	}
	return *found, true
}

// firstParameter names a function's first parameter, when it is a plain identifier.
func firstParameter(kinds *tsutil.KindTable, fn *sitter.Node, src []byte) string {
	if p := fn.ChildByFieldName("parameter"); p != nil && kindOf(kinds, p) == "identifier" {
		return nodeText(p, src) // `url => …`
	}
	params := fn.ChildByFieldName("parameters")
	if params == nil {
		return ""
	}
	for i := range params.ChildCount() {
		p := params.Child(i)
		switch kindOf(kinds, p) {
		case "required_parameter", "optional_parameter":
			if pat := p.ChildByFieldName("pattern"); pat != nil && kindOf(kinds, pat) == "identifier" {
				return nodeText(pat, src)
			}
			return ""
		case "identifier":
			return nodeText(p, src)
		}
	}
	return ""
}

// requestLibrary names the library a call is a request through: fetch(…), axios(…) or
// axios.<verb>(…). "" for any other call.
func requestLibrary(kinds *tsutil.KindTable, call *sitter.Node, src []byte) string {
	fn := call.ChildByFieldName("function")
	if fn == nil {
		return ""
	}
	switch kindOf(kinds, fn) {
	case "identifier":
		switch nodeText(fn, src) {
		case "fetch":
			return "fetch"
		case "axios":
			return "axios"
		}
	case "member_expression":
		if obj := fn.ChildByFieldName("object"); obj != nil && kindOf(kinds, obj) == "identifier" && nodeText(obj, src) == "axios" {
			return "axios"
		}
	}
	return ""
}

// localDeclarations collects every declaration of each local in a body, and the locals
// that are ever reassigned.
func localDeclarations(kinds *tsutil.KindTable, body *sitter.Node, src []byte) (map[string][]*sitter.Node, map[string]bool) {
	decls := map[string][]*sitter.Node{}
	reassigned := map[string]bool{}
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		switch kindOf(kinds, n) {
		case "variable_declarator":
			if name := n.ChildByFieldName("name"); name != nil && kindOf(kinds, name) == "identifier" {
				id := nodeText(name, src)
				decls[id] = append(decls[id], n.ChildByFieldName("value"))
			}
		case "assignment_expression", "augmented_assignment_expression":
			if left := n.ChildByFieldName("left"); left != nil && kindOf(kinds, left) == "identifier" {
				reassigned[nodeText(left, src)] = true
			}
		}
		for i := range n.ChildCount() {
			walk(n.Child(i))
		}
	}
	walk(body)
	return decls, reassigned
}

// urlPrefix reads the URL a request is made to. uses reports whether the URL involves
// the parameter at all; prefix is the literal text before it when the parameter is the
// URL's last operand, and nil when the parameter is used in any other way.
//
// A leading run of operands that are not literals is the base (`${API_BASE_URL}`) and is
// dropped, as the client patterns drop it; after it only literal text may come before
// the parameter.
func urlPrefix(kinds *tsutil.KindTable, n *sitter.Node, src []byte, param string,
	decls map[string][]*sitter.Node, reassigned map[string]bool, module map[string]*sitter.Node) (*string, bool) {

	if n == nil {
		return nil, false
	}
	switch kindOf(kinds, n) {
	case "identifier":
		id := nodeText(n, src)
		if id == param {
			empty := ""
			return &empty, true
		}
		values := decls[id]
		if len(values) == 0 {
			return nil, false
		}
		var agreed *string
		uses := false
		for _, v := range values {
			p, u := urlPrefix(kinds, v, src, param, nil, nil, module) // one level: a local built from a local is not read
			uses = uses || u
			if !u {
				continue
			}
			if p == nil || reassigned[id] || (agreed != nil && *agreed != *p) {
				return nil, true
			}
			agreed = p
		}
		if uses && len(values) > 1 && agreed != nil {
			// Every declaration must use it: one that does not is a different URL.
			for _, v := range values {
				if _, u := urlPrefix(kinds, v, src, param, nil, nil, module); !u {
					return nil, true
				}
			}
		}
		return agreed, uses

	case "template_string", "binary_expression":
		ops := urlOperands(kinds, n, src)
		uses := false
		for _, op := range ops {
			uses = uses || op.param(param)
		}
		if !uses {
			return nil, false
		}
		last := len(ops) - 1
		if !ops[last].param(param) {
			return nil, true
		}
		i := 0
		for i < last && !ops[i].literal {
			// The base. A base the file binds to an absolute URL is a third party's
			// address, so this is a client of someone else's API and not a request to
			// a path any loaded service could serve.
			if v, ok := module[ops[i].text]; ok && absoluteURL(kinds, v, src) {
				return nil, true
			}
			i++
		}
		var b strings.Builder
		for ; i < last; i++ {
			if !ops[i].literal {
				return nil, true // something unread between the prefix and the parameter
			}
			b.WriteString(ops[i].text)
		}
		prefix := b.String()
		return &prefix, true
	}
	return nil, false
}

// absoluteURL reports whether an expression's leading literal text is an absolute URL.
func absoluteURL(kinds *tsutil.KindTable, n *sitter.Node, src []byte) bool {
	ops := urlOperands(kinds, n, src)
	if len(ops) == 0 || !ops[0].literal {
		return false
	}
	lead := strings.ToLower(strings.TrimSpace(ops[0].text))
	return strings.HasPrefix(lead, "http://") || strings.HasPrefix(lead, "https://") || strings.HasPrefix(lead, "//")
}

// urlOperand is one operand of a URL expression: literal text, or an expression,
// recorded by its text so the parameter can be recognised.
type urlOperand struct {
	text    string
	literal bool
}

func (o urlOperand) param(name string) bool { return !o.literal && o.text == name }

// urlOperands splits a template literal or a + concatenation into its operands.
func urlOperands(kinds *tsutil.KindTable, n *sitter.Node, src []byte) []urlOperand {
	switch kindOf(kinds, n) {
	case "string":
		return []urlOperand{{text: strings.Trim(nodeText(n, src), `"'`), literal: true}}
	case "template_string":
		var out []urlOperand
		pos := n.StartByte() + 1
		for i := range n.ChildCount() {
			c := n.Child(i)
			if kindOf(kinds, c) != "template_substitution" {
				continue
			}
			if c.StartByte() > pos {
				out = append(out, urlOperand{text: string(src[pos:c.StartByte()]), literal: true})
			}
			var expr *sitter.Node
			for j := range c.ChildCount() {
				if e := c.Child(j); e.IsNamed() {
					expr = e
					break
				}
			}
			if expr != nil {
				out = append(out, urlOperand{text: strings.TrimSpace(nodeText(expr, src))})
			}
			pos = c.EndByte()
		}
		if end := n.EndByte() - 1; end > pos {
			out = append(out, urlOperand{text: string(src[pos:end]), literal: true})
		}
		return out
	case "binary_expression":
		op := n.ChildByFieldName("operator")
		left, right := n.ChildByFieldName("left"), n.ChildByFieldName("right")
		if op == nil || nodeText(op, src) != "+" || left == nil || right == nil {
			return []urlOperand{{text: nodeText(n, src)}}
		}
		return append(urlOperands(kinds, left, src), urlOperands(kinds, right, src)...)
	}
	return []urlOperand{{text: strings.TrimSpace(nodeText(n, src))}}
}

// afterBase reads a URL whose leading operands are a base this pass cannot resolve,
// `${API_BASE_URL}/api/items/${id}`, as the path after them, the way a helper's own
// URL is read (urlPrefix). A base the file binds to an absolute URL resolves, and is
// then no path at all.
func afterBase(r pathResolver, n *sitter.Node) (string, bool) {
	parts := r.parts(n, 0)
	i := 0
	for i < len(parts) && !parts[i].known {
		i++
	}
	if i == 0 || i == len(parts) || !strings.HasPrefix(parts[i].text, "/") {
		return "", false
	}
	var b strings.Builder
	for _, p := range parts[i:] {
		if p.known {
			b.WriteString(p.text)
		} else {
			b.WriteString("{}")
		}
	}
	return b.String(), true
}
