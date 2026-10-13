package tsextractor

import (
	sitter "github.com/tree-sitter/go-tree-sitter"

	"github.com/enola-labs/enola/internal/extractors/tsutil"
)

var drizzleChains = map[string]map[string]bool{
	"from":      {"select": true, "selectDistinct": true, "selectDistinctOn": true},
	"values":    {"insert": true},
	"set":       {"update": true},
	"where":     {"delete": true},
	"returning": {"delete": true},
}

func tsIsDrizzleQuery(kinds *tsutil.KindTable, call *sitter.Node, src []byte) bool {
	fn := call.ChildByFieldName("function")
	if fn == nil || kindOf(kinds, fn) != "member_expression" {
		return false
	}
	prop := fn.ChildByFieldName("property")
	if prop == nil {
		return false
	}
	name := nodeText(prop, src)
	if name == "execute" {
		return tsFirstArgIsSQLTemplate(kinds, call, src)
	}
	builders, ok := drizzleChains[name]
	if !ok {
		return false
	}
	inner := fn.ChildByFieldName("object")
	if inner == nil || kindOf(kinds, inner) != "call_expression" {
		return false
	}
	innerFn := inner.ChildByFieldName("function")
	if innerFn == nil || kindOf(kinds, innerFn) != "member_expression" {
		return false
	}
	innerProp := innerFn.ChildByFieldName("property")
	return innerProp != nil && builders[nodeText(innerProp, src)]
}

func tsFirstArgIsSQLTemplate(kinds *tsutil.KindTable, call *sitter.Node, src []byte) bool {
	args := call.ChildByFieldName("arguments")
	if args == nil || args.NamedChildCount() == 0 {
		return false
	}
	first := args.NamedChild(0)
	if kindOf(kinds, first) != "call_expression" {
		return false
	}
	tag := first.ChildByFieldName("function")
	if tag == nil || kindOf(kinds, tag) != "identifier" || nodeText(tag, src) != "sql" {
		return false
	}
	tmpl := first.ChildByFieldName("arguments")
	return tmpl != nil && kindOf(kinds, tmpl) == "template_string"
}
