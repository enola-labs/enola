package rustextractor

import (
	"github.com/enola-labs/enola/internal/extractors/tsutil"
	sitter "github.com/enola-labs/enola/internal/treesitter"

	"github.com/enola-labs/enola/internal/facts"
)

// nestedFunctions returns the named functions declared inside another function: a fn item
// in its body, and a closure a let binds to a plain name. An unbound closure is left out,
// since nothing can refer to it by name. Which of them already are symbols is
// facts.AttachNested's to tell.
func nestedFunctions(root *sitter.Node, src []byte) []facts.Nested {
	var out []facts.Nested
	var walk func(n *sitter.Node, inFunction bool)
	walk = func(n *sitter.Node, inFunction bool) {
		for i := range n.NamedChildCount() {
			c := n.NamedChild(i)
			k := kindOf(c)
			if inFunction {
				name := ""
				switch k {
				case "function_item":
					name = nodeText(c.ChildByFieldName("name"), src)
				case "let_declaration":
					if p, v := c.ChildByFieldName("pattern"), c.ChildByFieldName("value"); p != nil && v != nil && kindOf(p) == "identifier" && kindOf(v) == "closure_expression" {
						name = nodeText(p, src)
					}
				}
				if name != "" {
					out = append(out, facts.Nested{Name: name, Line: int(c.StartPosition().Row) + 1, EndLine: tsutil.EndLine(c)})
				}
			}
			walk(c, inFunction || k == "function_item" || k == "closure_expression")
		}
	}
	walk(root, false)
	return out
}
