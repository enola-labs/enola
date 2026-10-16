package tsextractor

import (
	"regexp"
	"strings"

	"github.com/enola-labs/enola/internal/extractors/tsutil"
	sitter "github.com/tree-sitter/go-tree-sitter"

	"github.com/enola-labs/enola/internal/facts"
)

// MCP tools.
//
// An MCP server is entered through its tools, registered with a name and a handler
// that is nearly always an inline function:
//
//	server.tool('get_tasks', 'List tasks', schema, async (args) => { ... })
//	server.registerTool('get_tasks', { description }, handler)
//	registerTool(server, 'get_tasks', 'List tasks', schema, async (args) => { ... })
//
// The last is a project's own wrapper around the SDK, which is how a server with
// hundreds of tools keeps the SDK's type inference affordable. A registration is a
// `.tool(...)` or `.registerTool(...)` call, or a call to a `register…Tool`
// function, in a file that imports an MCP library; its name is the first string
// argument and its handler the last argument.
//
// As in Go, an inline handler is given a symbol of its own, "<dir>.tool:<name>",
// carrying the calls of its body, and a named handler keeps its symbol. Either way
// the function carries a `tool` prop, which the flow analysis names the flow by,
// and is framework_registered: nothing calls it by name.

var (
	mcpImportRe     = regexp.MustCompile(`from\s+['"](?:@modelcontextprotocol/|fastmcp|mcp-framework)`)
	toolWrapperRe   = regexp.MustCompile(`^register\w*Tool$`)
	toolMethodNames = map[string]bool{"tool": true, "registerTool": true}
)

// mcpToolSymbols returns a symbol for each inline tool handler in the file, and the
// tool each named handler serves, keyed by the handler's local name.
func mcpToolSymbols(kinds *tsutil.KindTable, root *sitter.Node, ctx *extractCtx) ([]facts.Fact, map[string]string) {
	if !mcpImportRe.Match(ctx.src) {
		return nil, nil
	}
	var out []facts.Fact
	named := map[string]string{}
	seen := map[string]bool{}
	var visit func(n *sitter.Node)
	visit = func(n *sitter.Node) {
		for i := range n.NamedChildCount() {
			visit(n.NamedChild(i))
		}
		if kindOf(kinds, n) != "call_expression" || !isToolRegistration(kinds, n.ChildByFieldName("function"), ctx.src) {
			return
		}
		args := n.ChildByFieldName("arguments")
		if args == nil || args.NamedChildCount() < 2 {
			return
		}
		name := ""
		for i := range args.NamedChildCount() {
			if a := args.NamedChild(i); kindOf(kinds, a) == "string" {
				name = strings.Trim(nodeText(a, ctx.src), "'\"")
				break
			}
		}
		handler := args.NamedChild(args.NamedChildCount() - 1)
		if name == "" || handler == nil {
			return
		}
		switch {
		case tsFunctionKinds[kindOf(kinds, handler)]:
			symbol := ctx.dir + ".tool:" + name
			if seen[symbol] {
				return
			}
			seen[symbol] = true
			rels := []facts.Relation{{Kind: facts.RelDeclares, Target: ctx.dir}}
			callRels, m := collectCallsWithMetrics(handler, ctx.callScope(kinds, "", nil), symbol, "tool:"+name)
			props := map[string]any{
				"symbol_kind":          facts.SymbolFunc,
				"language":             "typescript",
				"tool":                 name,
				"framework_registered": true,
			}
			applyTSMetrics(props, m)
			out = append(out, facts.Fact{
				Kind:      facts.KindSymbol,
				Name:      symbol,
				File:      ctx.relFile,
				Line:      int(handler.StartPosition().Row) + 1,
				Props:     props,
				Relations: append(rels, callRels...),
			})
		case kindOf(kinds, handler) == "identifier":
			named[nodeText(handler, ctx.src)] = name
		}
	}
	visit(root)
	return out, named
}

// isToolRegistration reports whether a call's callee registers an MCP tool.
func isToolRegistration(kinds *tsutil.KindTable, callee *sitter.Node, src []byte) bool {
	if callee == nil {
		return false
	}
	switch kindOf(kinds, callee) {
	case "member_expression":
		p := callee.ChildByFieldName("property")
		return p != nil && toolMethodNames[nodeText(p, src)]
	case "identifier":
		return toolWrapperRe.MatchString(nodeText(callee, src))
	}
	return false
}

// applyMCPTools adds the file's inline tool handlers to its declarations and marks
// each named handler with the tool it serves.
func applyMCPTools(kinds *tsutil.KindTable, root *sitter.Node, ctx *extractCtx, decls []facts.Fact) []facts.Fact {
	symbols, named := mcpToolSymbols(kinds, root, ctx)
	for i := range decls {
		if decls[i].Kind != facts.KindSymbol {
			continue
		}
		if tool, ok := named[strings.TrimPrefix(decls[i].Name, ctx.dir+".")]; ok {
			decls[i].SetProp("tool", tool)
			decls[i].SetProp("framework_registered", true)
		}
	}
	return append(decls, symbols...)
}
