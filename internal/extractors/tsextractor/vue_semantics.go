package tsextractor

import (
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/extractors/tsutil"
	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/tree-sitter/go-tree-sitter"
)

var (
	vueIdentifierRe  = regexp.MustCompile(`[A-Za-z_$][A-Za-z0-9_$-]*`)
	vueEmitLiteralRe = regexp.MustCompile(`\b(?:e|event)\s*:\s*['"]([^'"]+)['"]`)
	vueStringValueRe = regexp.MustCompile(`['"]([^'"]+)['"]`)
)

// vueMacroContracts extracts statically named parts of script-setup contracts.
// The values are names, not inferred TypeScript types: keeping those separate
// avoids claiming type resolution where the extractor only read declarations.
func vueMacroContracts(kinds *tsutil.KindTable, root *sitter.Node, calls []*sitter.Node, src []byte) map[string][]string {
	types := make(map[string]string)
	for i := range root.ChildCount() {
		n := root.Child(i)
		if kind := kindOf(kinds, n); kind == "interface_declaration" || kind == "type_alias_declaration" {
			if name := n.ChildByFieldName("name"); name != nil {
				types[nodeText(name, src)] = nodeText(n, src)
			}
		}
	}
	sets := make(map[string]map[string]bool)
	add := func(contract, name string) {
		if name == "" {
			return
		}
		if sets[contract] == nil {
			sets[contract] = make(map[string]bool)
		}
		sets[contract][name] = true
	}
	for _, n := range calls {
		{
			fn := n.ChildByFieldName("function")
			if fn != nil && kindOf(kinds, fn) == "identifier" {
				macro := nodeText(fn, src)
				if vueCompilerMacroNames[macro] {
					text := nodeText(n, src)
					shape := vueMacroShape(text, types)
					switch macro {
					case "defineProps":
						for _, name := range vueTopLevelKeys(shape) {
							add("vue_prop_names", name)
						}
					case "defineSlots":
						for _, name := range vueTopLevelKeys(shape) {
							add("vue_slot_names", name)
						}
					case "defineExpose":
						for _, name := range vueTopLevelKeys(shape) {
							add("vue_exposed_names", name)
						}
					case "defineEmits":
						for _, name := range vueTopLevelKeys(shape) {
							add("vue_emit_names", name)
						}
						for _, match := range vueEmitLiteralRe.FindAllStringSubmatch(shape, -1) {
							add("vue_emit_names", match[1])
						}
						if strings.HasPrefix(strings.TrimSpace(shape), "[") {
							for _, match := range vueStringValueRe.FindAllStringSubmatch(shape, -1) {
								add("vue_emit_names", match[1])
							}
						}
					case "defineModel":
						name := "modelValue"
						if args := n.ChildByFieldName("arguments"); args != nil {
							if str := findChildByKind(kinds, args, "string"); str != nil {
								name = strings.Trim(nodeText(str, src), `"'`)
							}
						}
						add("vue_model_names", name)
					}
				}
			}
		}
	}
	out := make(map[string][]string)
	for contract, names := range sets {
		for name := range names {
			out[contract] = append(out[contract], name)
		}
		sort.Strings(out[contract])
	}
	return out
}

// vueMacroDeclaredTypes preserves the source-declared generic payload. This is
// deliberately declaration text rather than claimed fully-resolved TS types.
func vueMacroDeclaredTypes(kinds *tsutil.KindTable, calls []*sitter.Node, src []byte) map[string]string {
	out := make(map[string]string)
	for _, n := range calls {
		{
			fn := n.ChildByFieldName("function")
			if fn != nil && kindOf(kinds, fn) == "identifier" {
				macro := nodeText(fn, src)
				contract := map[string]string{
					"defineProps": "props", "defineEmits": "emits",
					"defineSlots": "slots", "defineModel": "model:modelValue",
				}[macro]
				text := nodeText(n, src)
				if contract != "" {
					if macro == "defineModel" {
						if args := n.ChildByFieldName("arguments"); args != nil {
							if str := findChildByKind(kinds, args, "string"); str != nil {
								contract = "model:" + strings.Trim(nodeText(str, src), `"'`)
							}
						}
					}
					if lt := strings.IndexByte(text, '<'); lt >= 0 {
						if end := balancedEnd(text, lt, '<', '>'); end > lt {
							out[contract] = strings.TrimSpace(text[lt+1 : end])
						}
					}
				}
			}
		}
	}
	return out
}

// vueMacroShape returns the inline type/runtime argument, or the declaration
// text for a simple referenced interface/type alias.
func vueMacroShape(call string, types map[string]string) string {
	if lt := strings.IndexByte(call, '<'); lt >= 0 {
		if end := balancedEnd(call, lt, '<', '>'); end > lt {
			shape := strings.TrimSpace(call[lt+1 : end])
			if vueIdentifierRe.MatchString(shape) && vueIdentifierRe.FindString(shape) == shape {
				if declared := types[shape]; declared != "" {
					return declared
				}
			}
			return shape
		}
	}
	if open := strings.IndexByte(call, '('); open >= 0 {
		if end := balancedEnd(call, open, '(', ')'); end > open {
			return strings.TrimSpace(call[open+1 : end])
		}
	}
	return ""
}

func balancedEnd(s string, start int, open, close byte) int {
	depth := 0
	var quote byte
	for i := start; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			switch c {
			case '\\':
				i++
			case quote:
				quote = 0
			}
			continue
		}
		if c == '\'' || c == '"' || c == '`' {
			quote = c
			continue
		}
		switch c {
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// vueTopLevelKeys extracts keys from the outermost object/type literal only.
func vueTopLevelKeys(shape string) []string {
	start := strings.IndexByte(shape, '{')
	if start < 0 {
		return nil
	}
	seen := make(map[string]bool)
	depth := 0
	memberStart := false
	var quote byte
	for i := start; i < len(shape); i++ {
		c := shape[i]
		if quote != 0 {
			switch c {
			case '\\':
				i++
			case quote:
				quote = 0
			}
			continue
		}
		if c == '\'' || c == '"' || c == '`' {
			quote = c
			continue
		}
		if c == '{' || c == '[' || c == '(' {
			depth++
			if depth == 1 {
				memberStart = true
			}
			continue
		}
		if c == '}' || c == ']' || c == ')' {
			depth--
			continue
		}
		if depth == 1 && (c == ',' || c == ';') {
			memberStart = true
			continue
		}
		if depth != 1 || !memberStart || c != '_' && c != '$' && (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') {
			continue
		}
		match := vueIdentifierRe.FindString(shape[i:])
		if match == "readonly" {
			i += len(match) - 1
			continue
		}
		memberStart = false
		j := i + len(match)
		for j < len(shape) && (shape[j] == ' ' || shape[j] == '\t' || shape[j] == '\r' || shape[j] == '\n' || shape[j] == '?') {
			j++
		}
		if j < len(shape) && (shape[j] == ':' || shape[j] == '(' || shape[j] == ',' || shape[j] == '}') {
			seen[match] = true
		}
		i += len(match) - 1
	}
	var out []string
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// extractVueRouterRoutes reads literal Vue Router route records from createRouter.
func extractVueRouterRoutes(kinds *tsutil.KindTable, root *sitter.Node, src []byte, relFile string, aliases map[string]tsAlias) []facts.Fact {
	arrays := make(map[string]*sitter.Node)
	var indexArrays func(*sitter.Node)
	indexArrays = func(n *sitter.Node) {
		if n == nil {
			return
		}
		if kindOf(kinds, n) == "variable_declarator" {
			name, value := n.ChildByFieldName("name"), n.ChildByFieldName("value")
			if name != nil && value != nil && kindOf(kinds, value) == "array" {
				arrays[nodeText(name, src)] = value
			}
		}
		for i := range n.ChildCount() {
			indexArrays(n.Child(i))
		}
	}
	indexArrays(root)
	bindings := buildVueImportBindings(kinds, root, src, relFile, aliases)
	var out []facts.Fact
	seen := make(map[string]bool)
	var walkArray func(*sitter.Node, string)
	walkArray = func(array *sitter.Node, parent string) {
		if array == nil {
			return
		}
		for i := range array.ChildCount() {
			obj := array.Child(i)
			if kindOf(kinds, obj) != "object" {
				continue
			}
			pathNode := vueObjectValue(kinds, obj, src, "path")
			if pathNode == nil || kindOf(kinds, pathNode) != "string" {
				continue
			}
			fragment := strings.Trim(nodeText(pathNode, src), `"'`)
			full := facts.JoinRoutePath(parent, fragment)
			component := vueRouterComponentTarget(kinds, vueObjectValue(kinds, obj, src, "component"), src, relFile, aliases, bindings)
			children := vueObjectValue(kinds, obj, src, "children")
			if children != nil && (kindOf(kinds, children) == "identifier" || kindOf(kinds, children) == "shorthand_property_identifier") {
				children = arrays[nodeText(children, src)]
			}
			if component != "" {
				key := full + "\x00" + component
				if !seen[key] {
					seen[key] = true
					out = append(out, facts.Fact{Kind: facts.KindRoute, Name: full, File: relFile,
						Line:      int(obj.StartPosition().Row) + 1,
						Props:     map[string]any{"method": "GET", "type": "page", "router": "vue-router", "language": "typescript", "framework": "vue", "handler": component},
						Relations: []facts.Relation{{Kind: facts.RelHandledBy, Target: component}}})
				}
			}
			if children != nil && kindOf(kinds, children) == "array" {
				walkArray(children, full)
			}
		}
	}
	var walk func(*sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil {
			return
		}
		if kindOf(kinds, n) == "call_expression" {
			fn := n.ChildByFieldName("function")
			if fn != nil && kindOf(kinds, fn) == "identifier" && nodeText(fn, src) == "createRouter" {
				args := n.ChildByFieldName("arguments")
				obj := findChildByKind(kinds, args, "object")
				if routes := vueObjectValue(kinds, obj, src, "routes"); routes != nil {
					if kindOf(kinds, routes) == "identifier" || kindOf(kinds, routes) == "shorthand_property_identifier" {
						routes = arrays[nodeText(routes, src)]
					}
					walkArray(routes, "")
				}
			}
		}
		for i := range n.ChildCount() {
			walk(n.Child(i))
		}
	}
	walk(root)
	return out
}

func vueObjectValue(kinds *tsutil.KindTable, obj *sitter.Node, src []byte, key string) *sitter.Node {
	if obj == nil {
		return nil
	}
	if value := objectPropValue(kinds, obj, src, key); value != nil {
		return value
	}
	for i := range obj.ChildCount() {
		child := obj.Child(i)
		if kindOf(kinds, child) == "shorthand_property_identifier" && nodeText(child, src) == key {
			return child
		}
	}
	return nil
}

func vueRouterComponentTarget(kinds *tsutil.KindTable, value *sitter.Node, src []byte, relFile string, aliases map[string]tsAlias, bindings emberImportBindings) string {
	if value == nil {
		return ""
	}
	if kindOf(kinds, value) == "identifier" {
		return bindings.internal[nodeText(value, src)]
	}
	var importPath string
	var walk func(*sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil || importPath != "" {
			return
		}
		if kindOf(kinds, n) == "call_expression" {
			fn := n.ChildByFieldName("function")
			if fn != nil && kindOf(kinds, fn) == "import" {
				if str := findChildByKind(kinds, n.ChildByFieldName("arguments"), "string"); str != nil {
					importPath = strings.Trim(nodeText(str, src), `"'`)
					return
				}
			}
		}
		for i := range n.ChildCount() {
			walk(n.Child(i))
		}
	}
	walk(value)
	if importPath == "" {
		return ""
	}
	resolved, external := resolveImportPath(importPath, factpath.Dir(relFile), aliases)
	if external {
		return ""
	}
	return factpath.Dir(resolved) + "." + fileSymbolName(resolved)
}

// resolveNuxtAutoComposableCalls rewrites only dangling useXxx targets to one
// unambiguous exported declaration under a composables directory, or under a
// stores directory, which @pinia/nuxt auto-imports the same way.
func resolveNuxtAutoComposableCalls(all []facts.Fact) {
	exists := make(map[string]bool)
	candidates := make(map[string]map[string]bool)
	for _, f := range all {
		if f.Kind != facts.KindSymbol {
			continue
		}
		exists[f.Name] = true
		parts := strings.Split(filepath.ToSlash(f.File), "/")
		underComposables := false
		for _, part := range parts[:max(0, len(parts)-1)] {
			if part == "composables" || part == "stores" {
				underComposables = true
				break
			}
		}
		name := f.Name[strings.LastIndexByte(f.Name, '.')+1:]
		if !underComposables || !isHookName(name) {
			continue
		}
		if candidates[name] == nil {
			candidates[name] = make(map[string]bool)
		}
		candidates[name][f.Name] = true
	}
	unique := make(map[string]string)
	for name, targets := range candidates {
		if len(targets) == 1 {
			for target := range targets {
				unique[name] = target
			}
		}
	}
	for i := range all {
		for j := range all[i].Relations {
			r := &all[i].Relations[j]
			if r.Kind != facts.RelCalls || exists[r.Target] {
				continue
			}
			short := r.Target[strings.LastIndexByte(r.Target, '.')+1:]
			if target := unique[short]; target != "" {
				r.Target = target
			}
		}
	}
}
