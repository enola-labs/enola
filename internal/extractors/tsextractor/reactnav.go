package tsextractor

import (
	"github.com/enola-labs/enola/internal/extractors/tsutil"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"

	"github.com/enola-labs/enola/internal/facts"
)

// React Navigation — screen registrations become page routes and literal
// navigate() calls become navigation edges, the Ember router mechanism applied
// to React Native.
//
// Extraction is text-scan, not AST: React Native writes JSX in plain .js files,
// which the TypeScript grammar (deliberately selected for .ts/.js) does not
// parse as JSX — the same reason serverroutes and the mount scanner are
// regex-based. Both scans are literal-only; a computed screen name draws
// nothing.

// NavRouteNameProp names a route fact for the navigation join (the
// framework-neutral sibling of ember_route_name); NavRouteLinksProp carries the
// literal navigate targets recorded on the enclosing symbol.
const (
	NavRouteNameProp  = "nav_route_name"
	NavRouteLinksProp = "nav_route_links"
)

func detectReactNavigation(repoPath string) bool {
	tsRoot, _ := findTSRoot(repoPath)
	if hasPkgDependency(tsRoot, "@react-navigation/native") ||
		(tsRoot != repoPath && hasPkgDependency(repoPath, "@react-navigation/native")) {
		return true
	}
	// A monorepo's example/demo app declares the dependency in its own
	// package.json one level down (the framework's own repository is the
	// extreme case: the root IS the dependency and depends on nothing).
	entries, err := os.ReadDir(repoPath)
	if err != nil {
		return false
	}
	for _, ent := range entries {
		if ent.IsDir() && !strings.HasPrefix(ent.Name(), ".") && !tsSkipDirs[ent.Name()] &&
			hasPkgDependency(filepath.Join(repoPath, ent.Name()), "@react-navigation/native") {
			return true
		}
	}
	return false
}

// screenTag matches `<Stack.Screen … >` openings; name/component are pulled
// from the attribute span so their order does not matter.
var (
	screenTag       = regexp.MustCompile(`<[A-Za-z_$][\w$]*\.(?:Screen|Group)\b([^>]*)`)
	screenNameAttr  = regexp.MustCompile(`\bname=["']([^"']+)["']`)
	screenCompAttr  = regexp.MustCompile(`\bcomponent=\{\s*([A-Za-z_$][\w$]*)\s*\}`)
	navigateLiteral = regexp.MustCompile(`\.(?:navigate|push|replace)\(\s*["']([^"']+)["']`)
)

// extractReactNavScreens emits one page route per literal Screen registration,
// bound handled_by to the component the registration names when that component
// is an import the file states (the Ember handled_by contract, resolved
// in-extractor because the import binding is file-local and exact).
func extractReactNavScreens(kinds *tsutil.KindTable, root *sitter.Node, src []byte, relFile string, aliases map[string]tsAlias) []facts.Fact {
	text := string(src)
	matches := screenTag.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return nil
	}
	bindings := buildEmberImportBindings(kinds, root, src, relFile, aliases)
	var out []facts.Fact
	seen := map[string]bool{}
	for _, m := range matches {
		attrs := text[m[2]:m[3]]
		nm := screenNameAttr.FindStringSubmatch(attrs)
		if nm == nil {
			continue
		}
		name := nm[1]
		if seen[name] {
			continue
		}
		seen[name] = true
		f := facts.Fact{
			Kind: facts.KindRoute,
			Name: name,
			File: relFile,
			Line: 1 + strings.Count(text[:m[0]], "\n"),
			Props: map[string]any{
				"method":         "GET",
				"type":           "page",
				"router":         "screens",
				"language":       "typescript",
				"framework":      "react-navigation",
				NavRouteNameProp: name,
			},
		}
		if cm := screenCompAttr.FindStringSubmatch(attrs); cm != nil {
			if target, ok := bindings.internal[cm[1]]; ok {
				f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelHandledBy, Target: target})
			}
		}
		out = append(out, f)
	}
	return out
}

// attachReactNavLinks records each literal navigate target on the enclosing
// top-level declaration's symbol, so "what navigates to this screen" is a
// symbol-level edge (the binder joins the names to route facts).
func attachReactNavLinks(kinds *tsutil.KindTable, result []facts.Fact, root *sitter.Node, src []byte, relFile string) []facts.Fact {
	text := string(src)
	navs := navigateLiteral.FindAllStringSubmatchIndex(text, -1)
	if len(navs) == 0 {
		return result
	}
	type declRange struct {
		name       string
		start, end int
	}
	var decls []declRange
	dir := ""
	for i := range root.ChildCount() {
		node := root.Child(i)
		if kindOf(kinds, node) == "export_statement" {
			if d := firstDeclChild(kinds, node); d != nil {
				node = d
			}
		}
		var nameNode *sitter.Node
		switch kindOf(kinds, node) {
		case "class_declaration", "abstract_class_declaration", "class":
			nameNode = findChildByKind(kinds, node, "type_identifier")
		case "function_declaration", "generator_function_declaration":
			nameNode = findChildByKind(kinds, node, "identifier")
		case "lexical_declaration", "variable_declaration":
			for j := range node.ChildCount() {
				d := node.Child(j)
				if kindOf(kinds, d) == "variable_declarator" {
					if id := findChildByKind(kinds, d, "identifier"); id != nil {
						decls = append(decls, declRange{name: nodeText(id, src),
							start: int(node.StartByte()), end: int(node.EndByte())})
					}
				}
			}
			continue
		default:
			continue
		}
		if nameNode != nil {
			decls = append(decls, declRange{name: nodeText(nameNode, src),
				start: int(node.StartByte()), end: int(node.EndByte())})
		}
	}
	if len(decls) == 0 {
		return result
	}
	byDecl := map[string]map[string]bool{}
	for _, m := range navs {
		target := text[m[2]:m[3]]
		if target == "" || strings.HasPrefix(target, "/") {
			continue
		}
		for _, d := range decls {
			if m[0] >= d.start && m[0] < d.end {
				if byDecl[d.name] == nil {
					byDecl[d.name] = map[string]bool{}
				}
				byDecl[d.name][target] = true
				break
			}
		}
	}
	if len(byDecl) == 0 {
		return result
	}
	if idx := strings.LastIndexByte(relFile, '/'); idx >= 0 {
		dir = relFile[:idx]
	}
	for declName, targets := range byDecl {
		factName := dir + "." + declName
		links := make([]string, 0, len(targets))
		for t := range targets {
			links = append(links, t)
		}
		sort.Strings(links)
		for i := range result {
			if result[i].Kind == facts.KindSymbol && result[i].Name == factName {
				result[i].SetProp(NavRouteLinksProp, links)
				break
			}
		}
	}
	return result
}
