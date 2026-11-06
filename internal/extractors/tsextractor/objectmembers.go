package tsextractor

import (
	"strings"

	"github.com/enola-labs/enola/internal/extractors/tsutil"
	sitter "github.com/enola-labs/enola/internal/treesitter"

	"github.com/enola-labs/enola/internal/facts"
)

// maxObjectMemberDepth bounds the key path of an object-member symbol. RTK Query's
// `endpoints: (build) => ({ getRoles: build.query({ query: () => … }) })` is three
// keys deep; nothing past a handful names anything a reader would look for.
const maxObjectMemberDepth = 6

// objectMemberSymbols emits a symbol for each function held by an object literal in
// a top-level declaration's value, named by the declaration and the keys leading to
// it: `export const ordersApi = { list: () => fetch(…) }` declares
// "<dir>.ordersApi.list". Without these, a function written as an object property
// had no symbol, so its calls were credited to nothing and a client call inside it
// named no caller: the API-client object, the Zustand store action and the RTK
// Query endpoint are all written that way.
//
// An object literal reached through a call's arguments was handed to that call, and
// whatever receives it invokes its functions (`create((set) => ({ … }))`,
// `api.injectEndpoints({ … })`), so those symbols are marked framework_registered:
// nothing calls them by name, which is no evidence they are dead.
//
// The walk goes through object literals, call arguments, parenthesized and type-
// assertion wrappers, and arrow functions with an expression body (an object-
// returning factory). It does not enter statement blocks, JSX or other expressions.
func objectMemberSymbols(kinds *tsutil.KindTable, value *sitter.Node, ctx *extractCtx, declName string, exported bool) []facts.Fact {
	var out []facts.Fact
	seen := map[string]bool{}
	emit := func(fn, at *sitter.Node, path []string, inCall bool) {
		name := ctx.dir + "." + declName + "." + strings.Join(path, ".")
		if seen[name] {
			return
		}
		seen[name] = true
		rels := []facts.Relation{{Kind: facts.RelDeclares, Target: ctx.dir}}
		callRels, m := collectCallsWithMetrics(fn, ctx.callScope(kinds, "", nil), name, path[len(path)-1])
		rels = append(rels, callRels...)
		props := map[string]any{
			"symbol_kind":   facts.SymbolFunc,
			"exported":      exported,
			"language":      "typescript",
			"object_member": true,
		}
		if inCall {
			props["framework_registered"] = true
		}
		applyTSMetrics(props, m)
		out = append(out, facts.Fact{
			Kind:      facts.KindSymbol,
			Name:      name,
			File:      ctx.relFile,
			Line:      int(at.StartPosition().Row) + 1,
			EndLine:   tsutil.EndLine(fn),
			Props:     props,
			Relations: rels,
		})
	}

	walkObjectMembers(kinds, value, ctx.src, emit)
	return out
}

// returnedMemberSymbols emits a symbol for each function held by an object literal that the
// function fn returns, from its own body or as its expression: `export function plugin() {
// return { transform() {} } }` declares "<dir>.plugin.transform". A factory like that can hold most of a module's
// logic, and without these a line anywhere in it is credited to the factory as one block.
//
// They say where a line is and nothing else. The calls in them stay the factory's, so they
// have no edges beyond the declaration and the call graph does not move. Whatever receives
// the object invokes them, so they are framework_registered.
func returnedMemberSymbols(kinds *tsutil.KindTable, fn *sitter.Node, ctx *extractCtx, declName string) []facts.Fact {
	body := fn.ChildByFieldName("body")
	if body == nil {
		return nil
	}
	var out []facts.Fact
	seen := map[string]bool{}
	parent := ctx.dir + "." + declName
	emit := func(member, at *sitter.Node, path []string, _ bool) {
		name := parent + "." + strings.Join(path, ".")
		if seen[name] {
			return
		}
		seen[name] = true
		out = append(out, facts.Fact{
			Kind:    facts.KindSymbol,
			Name:    name,
			File:    ctx.relFile,
			Line:    int(at.StartPosition().Row) + 1,
			EndLine: tsutil.EndLine(member),
			Props: map[string]any{
				"symbol_kind":          facts.SymbolFunc,
				"exported":             false,
				"language":             "typescript",
				"object_member":        true,
				"framework_registered": true,
				facts.PropMemberOf:     parent,
			},
			Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: ctx.dir}},
		})
	}
	if kindOf(kinds, body) != "statement_block" {
		walkMembers(kinds, body, ctx.src, true, emit)
		return out
	}
	var visit func(n *sitter.Node)
	visit = func(n *sitter.Node) {
		for i := range n.NamedChildCount() {
			c := n.NamedChild(i)
			switch k := kindOf(kinds, c); {
			case tsFunctionKinds[k], k == "class_declaration", k == "class":
				// What a nested function returns is that function's.
			case k == "return_statement":
				for j := range c.NamedChildCount() {
					walkMembers(kinds, c.NamedChild(j), ctx.src, true, emit)
				}
			default:
				visit(c)
			}
		}
	}
	visit(body)
	return out
}

// walkObjectMembers calls fn for each function an object literal under value holds,
// with the node to name it by, its key path, and whether the literal was reached
// through a call's arguments. The traversal objectMemberSymbols documents.
func walkObjectMembers(kinds *tsutil.KindTable, value *sitter.Node, src []byte, fn func(fn, at *sitter.Node, path []string, inCall bool)) {
	walkMembers(kinds, value, src, false, fn)
}

// walkMembers is walkObjectMembers, also following what is spread into the literal when
// spreads is set: `...(build ? { renderChunk() {} } : {})` adds members conditionally.
func walkMembers(kinds *tsutil.KindTable, value *sitter.Node, src []byte, spreads bool, fn func(fn, at *sitter.Node, path []string, inCall bool)) {
	var visit func(n *sitter.Node, path []string, inCall bool)
	visit = func(n *sitter.Node, path []string, inCall bool) {
		if n == nil || len(path) > maxObjectMemberDepth {
			return
		}
		switch kindOf(kinds, n) {
		case "object":
			for i := range n.ChildCount() {
				member := n.Child(i)
				switch kindOf(kinds, member) {
				case "pair":
					key := objectKey(kinds, member.ChildByFieldName("key"), src)
					val := member.ChildByFieldName("value")
					if key == "" || val == nil {
						continue
					}
					p := append(append([]string(nil), path...), key)
					if tsFunctionKinds[kindOf(kinds, val)] {
						fn(val, member, p, inCall)
					}
					visit(val, p, inCall)
				case "method_definition":
					key := objectKey(kinds, member.ChildByFieldName("name"), src)
					if key == "" {
						continue
					}
					fn(member, member, append(append([]string(nil), path...), key), inCall)
				case "spread_element":
					if spreads {
						for j := range member.NamedChildCount() {
							visit(member.NamedChild(j), path, inCall)
						}
					}
				}
			}
		case "ternary_expression":
			if spreads {
				visit(n.ChildByFieldName("consequence"), path, inCall)
				visit(n.ChildByFieldName("alternative"), path, inCall)
			}
		case "binary_expression":
			if spreads {
				visit(n.ChildByFieldName("right"), path, inCall)
			}
		case "call_expression":
			visit(n.ChildByFieldName("arguments"), path, true)
		case "arguments", "parenthesized_expression", "as_expression", "satisfies_expression", "non_null_expression":
			for i := range n.ChildCount() {
				visit(n.Child(i), path, inCall)
			}
		case "arrow_function":
			if body := n.ChildByFieldName("body"); body != nil && kindOf(kinds, body) != "statement_block" {
				visit(body, path, inCall)
			}
		}
	}
	visit(value, nil, false)
}

// hasObjectMembers reports whether a declaration's value holds any function
// objectMemberSymbols would name, so `<decl>.<key>()` can be resolved to it.
func hasObjectMembers(kinds *tsutil.KindTable, value *sitter.Node, src []byte) bool {
	found := false
	walkObjectMembers(kinds, value, src, func(_, _ *sitter.Node, _ []string, _ bool) { found = true })
	return found
}

// objectKey returns an object key usable in a symbol name: an identifier-like
// property name or a plain string key. A computed or numeric key names nothing.
func objectKey(kinds *tsutil.KindTable, key *sitter.Node, src []byte) string {
	if key == nil {
		return ""
	}
	var text string
	switch kindOf(kinds, key) {
	case "property_identifier", "identifier":
		text = nodeText(key, src)
	case "string":
		text = strings.Trim(nodeText(key, src), "\"'`")
	default:
		return ""
	}
	for _, r := range text {
		if r != '_' && r != '$' && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return ""
		}
	}
	return text
}
