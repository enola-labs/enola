package goextractor

import (
	"go/ast"
	"go/token"
	"strconv"

	"github.com/enola-labs/enola/internal/facts"
)

// MCP tools.
//
// An MCP server is entered through its tools, and a tool's handler is nearly always a
// function literal passed where the tool is registered:
//
//	mcp.AddTool(srv, &mcp.Tool{Name: "explore", ...}, func(ctx, req, in) (...) { ... })
//	s.AddTool(mcp.NewTool("explore", ...), handler)
//
// A literal has no symbol, so the tool had no entry and its calls were the
// registering function's. Each literal handler is given a symbol of its own, named
// "<package>.tool:<name>", carrying the calls of its body. A named handler keeps its
// symbol. Either way the function carries a `tool` prop, which the flow analysis
// names the flow by. The tool is registered by a framework call, so the symbol is
// marked framework_registered as well, which keeps it out of the dead-code findings.

// toolProp names the MCP tool a function serves.
const toolProp = "tool"

// toolsProp carries a function's tool registrations with named handlers from
// extractFunc to bindTools, which removes it: each entry is "<tool>\x00<function>".
const toolsProp = "_tools"

// mcpTools reads the tool registrations in a function body. It returns a symbol for
// each literal handler, and the registrations whose handler is a named function.
func mcpTools(fset *token.FileSet, body *ast.BlockStmt, ctx resolveCtx, relFile, pkgDir string) ([]facts.Fact, []string) {
	var out []facts.Fact
	var named []string
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "AddTool" || len(call.Args) < 2 {
			return true
		}
		name := toolName(call.Args[len(call.Args)-2])
		if name == "" {
			return true
		}
		switch h := call.Args[len(call.Args)-1].(type) {
		case *ast.FuncLit:
			symbol := pkgDir + ".tool:" + name
			m := analyzeBody(h.Body, ctx, symbol, funcLitParamNames(h))
			f := facts.Fact{
				Kind:    facts.KindSymbol,
				Name:    symbol,
				File:    relFile,
				Line:    fset.Position(h.Pos()).Line,
				EndLine: fset.Position(h.End()).Line,
				Props: map[string]any{
					"symbol_kind":          facts.SymbolFunc,
					"language":             "go",
					"exported":             false,
					"function_literal":     true,
					"framework_registered": true,
					toolProp:               name,
					"cyclomatic":           m.cyclomatic,
				},
				Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: pkgDir}},
			}
			for _, c := range m.calls {
				f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelCalls, Target: c})
			}
			out = append(out, f)
		default:
			if key := resolveChain(flattenSelector(h), ctx); key != "" {
				named = append(named, name+"\x00"+key)
			}
		}
		return true
	})
	return out, named
}

// toolName reads a tool's name where it is declared: `&mcp.Tool{Name: "x"}` or
// `mcp.NewTool("x", ...)`. "" for anything else.
func toolName(e ast.Expr) string {
	if u, ok := e.(*ast.UnaryExpr); ok && u.Op == token.AND {
		e = u.X
	}
	switch v := e.(type) {
	case *ast.CompositeLit:
		sel, ok := v.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Tool" {
			return ""
		}
		for _, elt := range v.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Name" {
				return stringLiteral(kv.Value)
			}
		}
	case *ast.CallExpr:
		if sel, ok := v.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "NewTool" && len(v.Args) > 0 {
			return stringLiteral(v.Args[0])
		}
	}
	return ""
}

func stringLiteral(e ast.Expr) string {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return ""
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		return ""
	}
	return s
}

func funcLitParamNames(lit *ast.FuncLit) map[string]bool {
	out := map[string]bool{}
	if lit.Type.Params != nil {
		for _, p := range lit.Type.Params.List {
			for _, n := range p.Names {
				out[n.Name] = true
			}
		}
	}
	return out
}

// bindTools gives each named function a tool registration hands its tool the tool
// prop, and removes the registrations from the functions that carry them.
func bindTools(allFacts []facts.Fact) {
	tools := map[string]string{}
	for i := range allFacts {
		f := &allFacts[i]
		if f.Kind != facts.KindSymbol {
			continue
		}
		if entries, ok := f.Props[toolsProp].([]string); ok {
			for _, e := range entries {
				name, key, _ := cutNUL(e)
				if _, taken := tools[key]; !taken {
					tools[key] = name
				}
			}
			delete(f.Props, toolsProp)
		}
	}
	if len(tools) == 0 {
		return
	}
	for i := range allFacts {
		f := &allFacts[i]
		if name, ok := tools[f.Name]; ok && f.Kind == facts.KindSymbol {
			f.SetProp(toolProp, name)
			f.SetProp("framework_registered", true)
		}
	}
}

func cutNUL(s string) (string, string, bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == 0 {
			return s[:i], s[i+1:], true
		}
	}
	return s, "", false
}
