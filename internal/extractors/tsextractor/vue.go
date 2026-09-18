package tsextractor

import (
	"bytes"
	"encoding/json"
	"github.com/enola-labs/enola/internal/extractors/tsutil"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"

	"github.com/enola-labs/enola/internal/factpath"
	sitter "github.com/tree-sitter/go-tree-sitter"
	typescript "github.com/tree-sitter/tree-sitter-typescript/bindings/go"
)

// vueScriptBlock holds the extracted <script> content from a Vue SFC.
type vueScriptBlock struct {
	Content   []byte
	IsSetup   bool
	Lang      string // "ts", "tsx", "js", or ""
	StartLine int    // 0-based line number where the script content begins
}

// extractVueScriptBlocks extracts all <script> blocks from a Vue SFC file.
// A Vue SFC may contain both a <script> and a <script setup> block.
func extractVueScriptBlocks(src []byte) []*vueScriptBlock {
	var blocks []*vueScriptBlock
	pos := 0
	for {
		remaining := src[pos:]
		idx := indexCaseInsensitive(remaining, []byte("<script"))
		if idx < 0 {
			break
		}
		tagStart := pos + idx

		tagEnd := bytes.IndexByte(src[tagStart:], '>')
		if tagEnd < 0 {
			break
		}
		tagEnd += tagStart

		attrs := string(src[tagStart : tagEnd+1])

		closeIdx := indexCaseInsensitive(src[tagEnd+1:], []byte("</script>"))
		if closeIdx < 0 {
			break
		}
		closeIdx += tagEnd + 1

		content := src[tagEnd+1 : closeIdx]
		isSetup := strings.Contains(strings.ToLower(attrs), "setup")
		lang := extractAttr(attrs, "lang")
		startLine := bytes.Count(src[:tagEnd+1], []byte("\n"))

		blocks = append(blocks, &vueScriptBlock{
			Content:   content,
			IsSetup:   isSetup,
			Lang:      lang,
			StartLine: startLine,
		})

		pos = closeIdx + len("</script>")
	}
	return blocks
}

func extractAttr(tag, name string) string {
	for _, q := range []byte{'"', '\''} {
		needle := name + "=" + string(q)
		idx := strings.Index(strings.ToLower(tag), strings.ToLower(needle))
		if idx < 0 {
			continue
		}
		start := idx + len(needle)
		end := strings.IndexByte(tag[start:], q)
		if end < 0 {
			continue
		}
		return tag[start : start+end]
	}
	return ""
}

func indexCaseInsensitive(src, needle []byte) int {
	return bytes.Index(bytes.ToLower(src), bytes.ToLower(needle))
}

func detectVue(repoPath string) bool {
	tsRoot, _ := findTSRoot(repoPath)
	return detectVueAt(tsRoot) || (tsRoot != repoPath && detectVueAt(repoPath))
}

func detectVueAt(dir string) bool {
	return hasPkgDependency(dir, "vue")
}

func detectNuxt(repoPath string) bool {
	tsRoot, _ := findTSRoot(repoPath)
	return detectNuxtAt(tsRoot) || (tsRoot != repoPath && detectNuxtAt(repoPath))
}

func detectNuxtAt(dir string) bool {
	for _, name := range []string{"nuxt.config.js", "nuxt.config.ts", "nuxt.config.mjs"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	return hasPkgDependency(dir, "nuxt")
}

func hasPkgDependency(dir, pkg string) bool {
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return false
	}
	var p map[string]any
	if err := json.Unmarshal(data, &p); err != nil {
		return false
	}
	for _, key := range []string{"dependencies", "devDependencies"} {
		if deps, ok := p[key].(map[string]any); ok {
			if _, ok := deps[pkg]; ok {
				return true
			}
		}
	}
	return false
}

// detectNuxtRoute checks if a .vue file path corresponds to a Nuxt route.
func detectNuxtRoute(relFile string) *facts.Fact {
	ext := filepath.Ext(relFile)
	switch strings.ToLower(ext) {
	case ".vue", ".js", ".jsx", ".mjs", ".ts", ".tsx":
	default:
		return nil
	}

	parts := strings.Split(filepath.ToSlash(relFile), "/")

	for i, p := range parts {
		if p == "pages" && i < len(parts)-1 {
			remaining := parts[i+1:]
			fileName := remaining[len(remaining)-1]
			baseName := strings.TrimSuffix(fileName, ext)
			// Nuxt's client/server page suffixes affect rendering, not the URL.
			baseName = strings.TrimSuffix(strings.TrimSuffix(baseName, ".client"), ".server")
			// A named view (child@sidebar.vue) shares the default view's URL.
			if at := strings.IndexByte(baseName, '@'); at >= 0 {
				baseName = baseName[:at]
			}

			routeParts := make([]string, 0, len(remaining))
			for j, rp := range remaining {
				if j == len(remaining)-1 {
					if baseName != "index" {
						routeParts = append(routeParts, baseName)
					}
				} else if !strings.HasPrefix(rp, "(") || !strings.HasSuffix(rp, ")") {
					// Nuxt route groups organize files without contributing a URL segment.
					routeParts = append(routeParts, rp)
				}
			}

			routePath := "/" + strings.Join(routeParts, "/")

			return &facts.Fact{
				Kind: facts.KindRoute,
				Name: routePath,
				File: relFile,
				Line: 1,
				Props: map[string]any{
					"method":    "GET",
					"type":      "page",
					"router":    "pages",
					"language":  "typescript",
					"framework": "nuxt",
				},
			}
		}
	}

	return nil
}

var (
	vueInterpolationRe  = regexp.MustCompile(`(?s)\{\{(.*?)\}\}`)
	vueDirectiveValueRe = regexp.MustCompile(`(?is)(?:^|\s)(?:v-[\w-]+(?::[\w-]+)?(?:\.[\w-]+)*|[@:#][^\s=]+)\s*=\s*(?:"([^"]*)"|'([^']*)')`)
	vueComponentTagRe   = regexp.MustCompile(`(?i)<\s*([A-Z][A-Za-z0-9_.-]*|[a-z][a-z0-9]*-[a-z0-9_.-]+)\b`)
)

var vueCompilerMacroNames = map[string]bool{
	"defineProps": true, "defineEmits": true, "defineSlots": true,
	"defineModel": true, "defineExpose": true, "defineOptions": true,
}

// buildVueImportBindings maps a template's local import spelling to the symbol
// the imported file actually declares. Default imports may be freely renamed,
// while a named import may have a different local alias.
func buildVueImportBindings(kinds *tsutil.KindTable, root *sitter.Node, src []byte, relFile string, aliases map[string]tsAlias) emberImportBindings {
	b := emberImportBindings{internal: map[string]string{}, external: map[string]string{}, modules: map[string]string{}}
	fileDir := factpath.Dir(relFile)
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
		importPath := strings.Trim(nodeText(source, src), `"'`)
		resolved, external := resolveImportPath(importPath, fileDir, aliases)
		moduleDir := factpath.Dir(resolved)
		bind := func(local, target string) {
			if local == "" {
				return
			}
			b.modules[local] = resolved
			if external {
				b.external[local] = importPath
			} else {
				b.internal[local] = target
			}
		}
		for j := range clause.ChildCount() {
			child := clause.Child(j)
			switch kindOf(kinds, child) {
			case "identifier":
				bind(nodeText(child, src), moduleDir+"."+fileSymbolName(resolved))
			case "named_imports":
				for k := range child.ChildCount() {
					spec := child.Child(k)
					if kindOf(kinds, spec) != "import_specifier" {
						continue
					}
					name := spec.ChildByFieldName("name")
					if name == nil {
						continue
					}
					exported := nodeText(name, src)
					local := exported
					if alias := spec.ChildByFieldName("alias"); alias != nil {
						local = nodeText(alias, src)
					}
					bind(local, moduleDir+"."+exported)
				}
			}
		}
	}
	return b
}

// nuxtAutoComponentIndex returns only unambiguous convention-derived component
// names. Both basename and path-prefixed forms are indexed because Nuxt projects
// may configure pathPrefix off.
func nuxtAutoComponentIndex(knownFiles map[string]bool) map[string]string {
	candidates := make(map[string]map[string]bool)
	for file := range knownFiles {
		if !isVueFile(file) {
			continue
		}
		parts := strings.Split(filepath.ToSlash(file), "/")
		componentAt := -1
		for i, part := range parts {
			if part == "components" {
				componentAt = i
			}
		}
		if componentAt < 0 || componentAt == len(parts)-1 {
			continue
		}
		target := factpath.Dir(file) + "." + fileSymbolName(file)
		names := []string{fileSymbolName(file)}
		var prefixed strings.Builder
		for _, part := range parts[componentAt+1:] {
			prefixed.WriteString(toPascal(strings.TrimSuffix(part, filepath.Ext(part))))
		}
		if prefixed.Len() > 0 {
			names = append(names, prefixed.String())
		}
		for _, name := range names {
			if candidates[name] == nil {
				candidates[name] = make(map[string]bool)
			}
			candidates[name][target] = true
		}
	}
	index := make(map[string]string)
	for name, targets := range candidates {
		if len(targets) == 1 {
			for target := range targets {
				index[name] = target
			}
		}
	}
	return index
}

// vueTemplateContent returns the first SFC template body. Vue permits at most one
// top-level template block; malformed/unclosed blocks deliberately produce no refs.
func vueTemplateContent(src []byte) []byte {
	start := indexCaseInsensitive(src, []byte("<template"))
	if start < 0 {
		return nil
	}
	openEnd := bytes.IndexByte(src[start:], '>')
	if openEnd < 0 {
		return nil
	}
	openEnd += start
	closeStart := indexCaseInsensitive(src[openEnd+1:], []byte("</template>"))
	if closeStart < 0 {
		return nil
	}
	closeStart += openEnd + 1
	return src[openEnd+1 : closeStart]
}

// vueTemplateRefs resolves names used by a Vue template against declarations and
// imports visible to that SFC. Filtering through those two exact scopes avoids
// turning HTML text, property names, v-for locals, and native tags into graph edges.
func vueTemplateRefs(rawSrc []byte, relFile string, extracted []facts.Fact, bindings emberImportBindings, autoComponents map[string]string) []string {
	template := vueTemplateContent(rawSrc)
	if len(template) == 0 {
		return nil
	}
	dir := factpath.Dir(relFile)
	visible := make(map[string]string)
	for _, f := range extracted {
		if f.Kind != facts.KindSymbol || f.File != relFile {
			continue
		}
		if dot := strings.LastIndexByte(f.Name, '.'); dot >= 0 {
			visible[f.Name[dot+1:]] = f.Name
		}
	}
	for local, target := range bindings.internal {
		visible[local] = target
	}
	for local, target := range autoComponents {
		if visible[local] == "" {
			visible[local] = target
		}
	}

	seen := make(map[string]bool)
	addExpr := func(expr []byte) {
		for _, name := range identTokens(string(expr)) {
			if target := visible[name]; target != "" && target != dir+"."+fileSymbolName(relFile) {
				seen[target] = true
			}
		}
	}
	for _, m := range vueInterpolationRe.FindAllSubmatch(template, -1) {
		addExpr(m[1])
	}
	for _, m := range vueDirectiveValueRe.FindAllSubmatch(template, -1) {
		if len(m[1]) > 0 {
			addExpr(m[1])
		} else {
			addExpr(m[2])
		}
	}
	for _, m := range vueComponentTagRe.FindAllSubmatch(template, -1) {
		name := string(m[1])
		if dot := strings.IndexByte(name, '.'); dot >= 0 {
			name = name[:dot]
		}
		if target := visible[name]; target != "" {
			seen[target] = true
			continue
		}
		if target := visible[toPascal(name)]; target != "" {
			seen[target] = true
		}
	}
	targets := make([]string, 0, len(seen))
	for target := range seen {
		targets = append(targets, target)
	}
	sort.Strings(targets)
	return targets
}

// collectVueCompilerMacros reads actual call-expression nodes, so macro-looking
// text in a comment or string cannot manufacture component metadata.
func collectVueCompilerMacros(kinds *tsutil.KindTable, calls []*sitter.Node, src []byte) []string {
	seen := make(map[string]bool)
	for _, node := range calls {
		if fn := node.ChildByFieldName("function"); fn != nil && kindOf(kinds, fn) == "identifier" {
			if name := nodeText(fn, src); vueCompilerMacroNames[name] {
				seen[name] = true
			}
		}
	}
	macros := make([]string, 0, len(seen))
	for macro := range seen {
		macros = append(macros, macro)
	}
	sort.Strings(macros)
	return macros
}

func isVueFile(path string) bool {
	return strings.ToLower(filepath.Ext(path)) == ".vue"
}

// containsCreateRouterCall reports whether the AST subtree contains a createRouter() call.
func containsCreateRouterCall(kinds *tsutil.KindTable, node *sitter.Node, src []byte) bool {
	if node == nil {
		return false
	}
	if kindOf(kinds, node) == "call_expression" {
		fn := node.ChildByFieldName("function")
		if fn != nil && kindOf(kinds, fn) == "identifier" && nodeText(fn, src) == "createRouter" {
			return true
		}
	}
	for i := range node.ChildCount() {
		if containsCreateRouterCall(kinds, node.Child(i), src) {
			return true
		}
	}
	return false
}

// extractVueSFC extracts architectural facts from a Vue Single File Component.
func (e *TSExtractor) extractVueSFC(kinds *tsutil.KindTable, rawSrc []byte, relFile string, isNuxt bool, aliases map[string]tsAlias, nuxtAutoComponents map[string]string) []facts.Fact {
	var result []facts.Fact
	blocks := extractVueScriptBlocks(rawSrc)
	allBindings := emberImportBindings{internal: map[string]string{}, external: map[string]string{}, modules: map[string]string{}}
	macroSet := make(map[string]bool)
	macroContracts := make(map[string]map[string]bool)
	macroTypes := make(map[string]string)

	isSetup := false
	for _, block := range blocks {
		if block.IsSetup {
			isSetup = true
		}
		blockFacts, bindings, macros, contracts, declaredTypes := e.extractVueScriptBlock(kinds, block, relFile, isNuxt, aliases)
		result = append(result, blockFacts...)
		for name, target := range bindings.internal {
			allBindings.internal[name] = target
		}
		if block.IsSetup {
			for _, macro := range macros {
				macroSet[macro] = true
			}
			for contract, names := range contracts {
				if macroContracts[contract] == nil {
					macroContracts[contract] = make(map[string]bool)
				}
				for _, name := range names {
					macroContracts[contract][name] = true
				}
			}
			for contract, declared := range declaredTypes {
				macroTypes[contract] = declared
			}
		}
	}

	dir := factpath.Dir(relFile)
	componentName := fileSymbolName(relFile)
	factName := dir + "." + componentName
	fw := "vue"
	if isNuxt {
		fw = "nuxt"
	}

	// If the script block already emitted a symbol with the component name
	// (e.g. from `export default defineComponent(...)`), enrich it with Vue
	// classification instead of creating a duplicate.
	found := false
	for i := range result {
		if result[i].Kind == facts.KindSymbol && result[i].Name == factName {
			result[i].Props["web_component"] = "component"
			result[i].Props["framework"] = fw
			if isSetup {
				result[i].Props["vue_setup"] = true
			}
			found = true
			break
		}
	}
	if !found {
		compFact := facts.Fact{
			Kind: facts.KindSymbol,
			Name: factName,
			File: relFile,
			Line: 1,
			Props: map[string]any{
				"symbol_kind":   facts.SymbolFunc,
				"exported":      true,
				"language":      "typescript",
				"web_component": "component",
				"framework":     fw,
			},
			Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: dir}},
		}
		if isSetup {
			compFact.Props["vue_setup"] = true
		}
		result = append(result, compFact)
	}
	if len(macroSet) > 0 {
		macros := make([]string, 0, len(macroSet))
		for macro := range macroSet {
			macros = append(macros, macro)
		}
		sort.Strings(macros)
		for i := range result {
			if result[i].Kind != facts.KindSymbol || result[i].Name != factName {
				continue
			}
			result[i].Props["vue_macros"] = macros
			for _, macro := range macros {
				result[i].Props["vue_"+strings.TrimPrefix(strings.ToLower(macro), "define")] = true
			}
			for contract, names := range macroContracts {
				var ordered []string
				for name := range names {
					ordered = append(ordered, name)
				}
				sort.Strings(ordered)
				result[i].Props[contract] = ordered
			}
			if len(macroTypes) > 0 {
				declared := make([]string, 0, len(macroTypes))
				for contract, typeText := range macroTypes {
					declared = append(declared, contract+"="+typeText)
				}
				sort.Strings(declared)
				result[i].Props["vue_contract_types"] = declared
			}
			break
		}
	}

	// A binding used only from the template is still a real architectural use.
	// Attach those exact, scope-resolved references to the component itself.
	autoComponents := map[string]string(nil)
	if isNuxt {
		autoComponents = nuxtAutoComponents
	}
	templateTargets := vueTemplateRefs(rawSrc, relFile, result, allBindings, autoComponents)
	if len(templateTargets) > 0 {
		for i := range result {
			if result[i].Kind != facts.KindSymbol || result[i].Name != factName {
				continue
			}
			for _, target := range templateTargets {
				if target != factName && !result[i].HasRelation(facts.RelCalls, target) {
					result[i].Relations = append(result[i].Relations, facts.Relation{Kind: facts.RelCalls, Target: target})
				}
			}
			break
		}
	}

	if isNuxt {
		if routeFact := detectNuxtRoute(relFile); routeFact != nil {
			result = append(result, *routeFact)
		}
	}

	return result
}

// extractVueScriptBlock parses a single <script> block from a Vue SFC and
// returns the extracted facts with line numbers adjusted to the original file.
func (e *TSExtractor) extractVueScriptBlock(kinds *tsutil.KindTable, block *vueScriptBlock, relFile string, isNuxt bool, aliases map[string]tsAlias) ([]facts.Fact, emberImportBindings, []string, map[string][]string, map[string]string) {
	isTSX := block.Lang == "tsx"
	lang := typescript.LanguageTypescript()
	if isTSX {
		lang = typescript.LanguageTSX()
	}

	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(lang)); err != nil {
		return nil, emberImportBindings{}, nil, nil, nil
	}

	tree := parser.Parse(block.Content, nil)
	defer tree.Close()

	root := tree.RootNode()
	bindings := buildVueImportBindings(kinds, root, block.Content, relFile, aliases)
	// All three passes below look for the same thing — the compiler-macro call sites
	// — so the tree is matched ONCE and the results shared. Each used to walk the
	// whole script block in Go, which costs a heap allocation per node visited
	// regardless of traversal idiom (see tsutil.QueryNodes).
	calls := tsutil.QueryNodes(tsGrammarKey(isTSX), tsGrammarLanguage(isTSX), "(call_expression) @c", root)
	macros := collectVueCompilerMacros(kinds, calls, block.Content)
	contracts := vueMacroContracts(kinds, root, calls, block.Content)
	declaredTypes := vueMacroDeclaredTypes(kinds, calls, block.Content)

	var result []facts.Fact
	result = append(result, e.extractImports(kinds, root, block.Content, relFile, aliases, false)...)
	// Vue/Nuxt script blocks are parsed independently from ordinary .ts files,
	// so run the shared GraphQL tag extractor over their AST as well. This covers
	// Nuxt Apollo composables such as useAsyncQuery(gql`...`) and useMutation,
	// while retaining the tag extractor's comment/string false-positive guards.
	if !facts.IsTestPath(relFile) {
		result = append(result, extractGraphQLTagFactsAST(block.Content, relFile, kinds, isTSX, root)...)
	}

	ctx := &extractCtx{
		src:       block.Content,
		relFile:   relFile,
		dir:       factpath.Dir(relFile),
		isTSX:     isTSX,
		isVue:     true,
		isNuxt:    isNuxt,
		importMap: buildImportSymbols(kinds, root, block.Content, relFile, aliases),
		imports:   buildEmberImportBindings(kinds, root, block.Content, relFile, aliases),
	}
	decls := e.extractDeclarations(kinds, root, ctx)

	if exported := collectExportedLocalNames(kinds, root, block.Content); len(exported) > 0 {
		for i := range decls {
			if decls[i].Kind != facts.KindSymbol {
				continue
			}
			local := decls[i].Name[strings.LastIndexByte(decls[i].Name, '.')+1:]
			if exported[local] {
				decls[i].Props["exported"] = true
			}
		}
	}
	result = append(result, decls...)
	// Script setup executes at component scope, so its top-level calls and values
	// are real uses even though they are not inside a function declaration.
	result = append(result, e.collectTSFileRefs(kinds, root, ctx, aliases, facts.KindFileRef)...)

	if block.StartLine > 0 {
		for i := range result {
			if result[i].Line > 0 {
				result[i].Line += block.StartLine
			}
		}
	}

	return result, bindings, macros, contracts, declaredTypes
}
