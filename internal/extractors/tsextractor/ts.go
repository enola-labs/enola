package tsextractor

import (
	"context"
	"encoding/json"
	"github.com/enola-labs/enola/internal/extractors/tsutil"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/clientspec"
	"github.com/enola-labs/enola/internal/extractors/detectnames"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/parallel"

	"github.com/enola-labs/enola/internal/factpath"
	sitter "github.com/tree-sitter/go-tree-sitter"
	typescript "github.com/tree-sitter/tree-sitter-typescript/bindings/go"
)

// TSExtractor extracts architectural facts from TypeScript/TSX source code using tree-sitter.
type TSExtractor struct {
	// clients are the in-house HTTP clients the config declares for TypeScript, handed
	// over by the engine at registration and read-only afterwards.
	clients []clientspec.Spec
}

// New creates a new TSExtractor.
func New() *TSExtractor {
	return &TSExtractor{}
}

func (e *TSExtractor) Name() string {
	return "typescript"
}

// SetClientSpecs implements clientspec.Consumer.
func (e *TSExtractor) SetClientSpecs(specs []clientspec.Spec) { e.clients = specs }

// ConfigKey implements plugin.ConfigKeyed: the declared clients decide which call sites
// become routes, so they are part of what a cached result was extracted under.
func (e *TSExtractor) ConfigKey() string { return clientspec.Fingerprint(e.clients) }

// Detect returns true if the repository (or one of its immediate subdirectories
// in the case of a monorepo) contains TypeScript markers.
func (e *TSExtractor) Detect(repoPath string) (bool, error) {
	return e.DetectFiles(repoPath, detectnames.Walk(repoPath))
}

// unambiguousTSExts are the extensions that name this extractor's languages and
// nothing else. They are a strict SUBSET of what isTypeScriptFile claims, and the
// gap is the whole point: .js, .jsx, .mjs, .hbs and .graphql are all files a
// repository in any language may carry — a build script, a docs asset, a schema
// shared with a Go server — so detecting on them would make almost every repository
// a TypeScript one. Ownership may over-claim safely; detection may not.
var unambiguousTSExts = map[string]bool{
	".ts": true, ".tsx": true, ".vue": true, ".svelte": true, ".gts": true,
}

// DetectFiles implements plugin.FileListDetector.
//
// The marker search runs first and is unchanged — it is what finds the tsconfig or
// package.json that also tells Extract WHERE the project root is. What is new is the
// fallback: a repository whose TypeScript lives past findTSRoot's adaptive 2/8-level
// search was previously undetectable, which cost 178 files in the dart-sdk and 65 in
// roslyn, both of them TypeScript tooling parked deep inside a repository written in
// something else.
//
// Last comes the repository written in plain JavaScript, which has none of the above;
// see javaScriptLeads for why that is decided by count and not by a file's presence.
//
// Detecting without a root is already a state this extractor handles: a GraphQL-docs
// repository has reached Extract with findTSRoot returning no root since that arm was
// added, and every consumer of it pairs tsRoot with a repoPath fallback.
func (e *TSExtractor) DetectFiles(repoPath string, files []string) (bool, error) {
	if _, found := findTSRoot(repoPath); found {
		return true, nil
	}
	if hasGraphQLDocs(files) {
		return true, nil
	}
	for _, rel := range files {
		if detectnames.HasAnySegment(rel, "node_modules", "dist", "build", "out", "coverage") {
			continue
		}
		if unambiguousTSExts[strings.ToLower(filepath.Ext(rel))] {
			return true, nil
		}
	}
	return javaScriptLeads(files), nil
}

// findTSRoot returns the directory that is the TypeScript project root, along
// with a boolean indicating whether one was found. Search depth adapts to
// repo structure: Java/Gradle projects nest UI code deep (src/main/resources/ui)
// so we search up to 8 levels; plain repos need at most 2.
func findTSRoot(repoPath string) (string, bool) {
	if hasTSMarkers(repoPath) {
		return repoPath, true
	}
	maxDepth := 2
	if isDeepNestedProject(repoPath) {
		maxDepth = 8
	}
	return searchTSRoot(repoPath, 0, maxDepth)
}

func isDeepNestedProject(repoPath string) bool {
	markers := []string{
		"pom.xml", "build.gradle", "build.gradle.kts",
		"pyproject.toml", "setup.py", "setup.cfg", "requirements.txt",
	}
	for _, marker := range markers {
		if _, err := os.Stat(filepath.Join(repoPath, marker)); err == nil {
			return true
		}
	}
	return false
}

var tsSkipDirs = map[string]bool{
	"node_modules": true, "dist": true, ".next": true,
	"build": true, "out": true, "target": true, "vendor": true,
}

func searchTSRoot(dir string, depth, maxDepth int) (string, bool) {
	if depth >= maxDepth {
		return "", false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") || tsSkipDirs[entry.Name()] {
			continue
		}
		sub := filepath.Join(dir, entry.Name())
		if hasTSMarkers(sub) {
			return sub, true
		}
		if found, ok := searchTSRoot(sub, depth+1, maxDepth); ok {
			return found, true
		}
	}
	return "", false
}

// hasTSMarkers returns true if the directory looks like a project root this
// extractor should handle (TypeScript, or a JS framework it also parses).
func hasTSMarkers(dir string) bool {
	// tsconfig.json (standard), tsconfig.base.json (Nx monorepo), or a Deno
	// project's config — Deno ships TypeScript with no package.json at all
	// (deno.json/deno.jsonc, import_map.json), so a Deno Slack app or service
	// was undetectable by every package.json rule below.
	for _, name := range []string{"tsconfig.json", "tsconfig.base.json",
		"deno.json", "deno.jsonc", "import_map.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}

	// @hotwired/stimulus marks the plain-JavaScript Rails frontend: a Hotwire
	// app has no tsconfig and none of the TS-ecosystem dependencies, yet ships
	// hundreds of production controllers this extractor parses natively — on one
	// Rails 8 app, 350+ files were invisible until this marker.
	for _, pkg := range []string{"typescript", "vue", "react", "svelte", "next", "nuxt", "ember-source",
		"@hotwired/stimulus", "@hotwired/turbo-rails"} {
		if hasPkgDependency(dir, pkg) {
			return true
		}
	}

	// config/importmap.rb marks the same Rails frontend when importmap-rails
	// manages it: pins live in Ruby, so the app ships no package.json at all and
	// every package.json rule above is blind to it — an importmap app's whole
	// app/javascript tree (Stimulus controllers included) was claimed by this
	// extractor and never parsed.
	if _, err := os.Stat(filepath.Join(dir, "config", "importmap.rb")); err == nil {
		return true
	}
	// A dependency-free plain-JavaScript package is still a JavaScript project:
	// a Node CLI with zero deps declares itself structurally (bin, main,
	// exports, type, workspaces, or any dependency map). Only a bare
	// name-holding stub — a marker file, not a package — stays undetected.
	return packageJSONDeclaresPackage(dir)
}

func packageJSONDeclaresPackage(dir string) bool {
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return false
	}
	var p map[string]any
	if err := json.Unmarshal(data, &p); err != nil {
		return false
	}
	for _, key := range []string{"bin", "main", "exports", "type", "workspaces"} {
		if _, ok := p[key]; ok {
			return true
		}
	}
	for _, key := range []string{"dependencies", "devDependencies"} {
		if deps, ok := p[key].(map[string]any); ok && len(deps) > 0 {
			return true
		}
	}
	return false
}

// extractCtx bundles the per-file state threaded through declaration extraction
// so symbols can be enriched with React/Next.js semantic classification.
type extractCtx struct {
	src     []byte
	relFile string
	dir     string
	isTSX   bool
	tsFrameworks
	importMap map[string]string
	// memberRoots are the names `<name>.<member>()` resolves through (see
	// memberCallRoots), for a component's script block as for a file.
	memberRoots map[string]string
	// returnTypes are the file's own functions with a declared class return type
	// (see fileReturnTypes).
	returnTypes map[string]string
	imports     emberImportBindings // the file's import table, read for the module a superclass identifier came from
	ioBindings  map[string]bool     // local names bound to imports from a network module (I/O sinks)
	knownFiles  map[string]bool     // repo-relative (slash) paths of all indexed TS/JS files
	files       *tsFileIndex        // the same files by stem, for a specifier no tsconfig maps
	aliases     map[string]tsAlias  // this directory's tsconfig path aliases, for resolving an import written as a bare specifier
}

func (e *TSExtractor) extractFile(src []byte, relFile string, rc *repoCtx, aliases map[string]tsAlias) tsFileResult {
	// The grammar is chosen here, so the kind table is too: TypeScript and TSX assign
	// different meanings to the same symbol ids, and everything below reads node kinds
	// through this table. See kinds.go.
	isTSX := strings.HasSuffix(relFile, ".tsx") || strings.HasSuffix(relFile, ".jsx")
	kinds := tsKindsFor(isTSX)

	if out, ok := e.extractNonScript(kinds, src, relFile, rc, aliases); ok {
		return tsFileResult{facts: out}
	}
	// A Glimmer template-tag file is TypeScript/JavaScript with embedded
	// <template> blocks: blank the blocks in place (newlines preserved, so every
	// Line below stays true to the original file) and parse the remainder with
	// the standard grammar. The sliced-out segments feed emberEnrich after the
	// declaration pass.
	var emberSegments []emberTemplateSegment
	isEmberFile := isEmberTemplateTagFile(relFile)
	if isEmberFile {
		src, emberSegments = blankEmberTemplates(src)
	}

	result := sourcePatternFacts(src, relFile, rc)

	lang := typescript.LanguageTypescript()
	if isTSX {
		lang = typescript.LanguageTSX()
	}
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(lang)); err != nil {
		return tsFileResult{facts: result}
	}
	tree := parser.Parse(src, nil)
	defer tree.Close()
	root := tree.RootNode()

	if !facts.IsTestPath(relFile) {
		result = append(result, astPatternFacts(kinds, root, src, relFile, isTSX, rc.graphqlServer)...)
	}
	result = append(result, e.extractImports(kinds, root, src, relFile, aliases, rc.isSvelteKit)...)

	ctx := newFileCtx(kinds, root, src, relFile, isTSX, rc, aliases)
	result = append(result, e.fileDeclarations(kinds, root, ctx)...)

	// Whole-file reference pass (JSX component rendering, imported-identifier values
	// like route configs, namespace member access, require()-bound names). Emitted as
	// a KindFileRef so file-scope references the per-function call walk cannot see do
	// not leave used code mis-reported as dead.
	result = append(result, e.collectTSFileRefs(kinds, root, ctx, aliases, facts.KindFileRef)...)

	return e.frameworkPasses(kinds, root, ctx, aliases, result, isEmberFile, emberSegments)
}

func (e *TSExtractor) extractNonScript(kinds *tsutil.KindTable, src []byte, relFile string, rc *repoCtx, aliases map[string]tsAlias) ([]facts.Fact, bool) {
	switch {
	case isVueFile(relFile):
		return e.extractVueSFC(kinds, src, relFile, rc.isNuxt, aliases, rc.files, rc.nuxtAutoComponents), true
	case isSvelteFile(relFile):
		return e.extractSvelteSFC(kinds, src, relFile, rc.isSvelteKit, aliases, rc.files), true
	case isGraphQLDocFile(relFile):
		if facts.IsTestPath(relFile) {
			return nil, true
		}
		if rc.graphqlServer.sdlDocuments[filepath.ToSlash(relFile)] {
			if routes := extractGraphQLServerSDLDocument(src, relFile); len(routes) > 0 {
				return routes, true
			}
		}
		return extractGraphQLClientOps(string(src), relFile, facts.RouteSourceGraphQLOperation), true
	case isHbsFile(relFile):
		// Handlebars is only modeled where Ember's resolver gives the names
		// deterministic meaning; a lone .hbs in a non-Ember repo stays out.
		if !rc.isEmber {
			return nil, true
		}
		return e.extractEmberHbs(src, relFile, rc.knownFiles), true
	}
	return nil, false
}

func sourcePatternFacts(src []byte, relFile string, rc *repoCtx) []facts.Fact {
	var result []facts.Fact
	// Parse openapi-typescript generated files for backend API route dependencies.
	// These are client-role route facts showing which backend routes the TS code calls.
	result = append(result, extractOpenAPITypescriptFacts(src, relFile)...)

	// Hand-written fetch / makeRequest / axios API calls are also client-role routes
	// — but only from PRODUCTION code. A test's HTTP traffic is not an architectural
	// dependency: an e2e suite driving supertest against its own service would
	// otherwise become hundreds of outbound client routes, inflate that service's
	// edge_coverage, and — because those paths really do match another repo's server
	// routes — fabricate a cross-repo dependency edge out of test traffic. Same
	// principle as GAP-XL-15, which keeps test_ref facts out of the coupling graph.
	// facts.IsTestPath is the single shared definition (internal/facts/testpath.go);
	// resolving test-ness here rather than with a local suffix list is deliberate —
	// the copies that predated it had drifted apart in both directions.
	if !facts.IsTestPath(relFile) {
		if rc.graphqlServer.enabled {
			result = append(result, extractGraphQLServerSDL(src, relFile)...)
		}
		result = append(result, extractHTTPClientFacts(src, relFile)...)
		// Call-registered server routes (Express/Fastify/Hono/Koa). Same test-path
		// gate: an e2e suite that spins up its own app would otherwise contribute
		// server routes no production client calls.
		result = append(result, extractServerRouteFacts(src, relFile)...)
	}

	// gRPC-web client call sites become client-role routes to "/pkg.Service/Method".
	return append(result, extractGRPCClientFacts(src, relFile, rc.grpcStubs)...)
}

func astPatternFacts(kinds *tsutil.KindTable, root *sitter.Node, src []byte, relFile string, isTSX bool, graphqlServer graphqlServerContext) []facts.Fact {
	result := extractGraphQLTagFactsAST(src, relFile, kinds, isTSX, root)
	text := string(src)
	fetchBody := hasGraphQLFetchBodyCandidate(text)
	clientCandidate := strings.Contains(text, "graphql-request") || fetchBody
	var graphqlBindings graphqlImportBindings
	if clientCandidate || graphqlServer.enabled {
		graphqlBindings = collectGraphQLImportBindings(kinds, root, src)
	}
	if clientCandidate {
		result = append(result, extractGraphQLClientCallFactsAST(src, relFile, kinds, root, fetchBody, graphqlBindings)...)
	}
	if graphqlServer.enabled {
		result = append(result, extractGraphQLCodeFirstAST(src, relFile, kinds, root, graphqlBindings)...)
	}
	return append(result, extractTSKafkaFacts(kinds, root, src, relFile)...)
}

func newFileCtx(kinds *tsutil.KindTable, root *sitter.Node, src []byte, relFile string, isTSX bool, rc *repoCtx, aliases map[string]tsAlias) *extractCtx {
	dir := factpath.Dir(relFile)
	importMap, internalImports := buildImportBindings(kinds, root, src, relFile, aliases, rc.files)
	fw := rc.tsFrameworks
	if !fw.isNextJS && inNextJSPackage(relFile, fw.nextDirs) {
		fw.isNextJS = true
	}
	return &extractCtx{
		tsFrameworks: fw,
		src:          src,
		relFile:      relFile,
		dir:          dir,
		isTSX:        isTSX,
		importMap:    importMap,
		memberRoots:  memberCallRoots(kinds, root, src, dir, internalImports),
		returnTypes:  fileReturnTypes(kinds, root, src, dir, importMap),
		imports:      buildEmberImportBindings(kinds, root, src, relFile, aliases),
		ioBindings:   buildIOImportBindings(kinds, root, src),
		knownFiles:   rc.knownFiles,
		files:        rc.files,
		aliases:      aliases,
	}
}

func (e *TSExtractor) fileDeclarations(kinds *tsutil.KindTable, root *sitter.Node, ctx *extractCtx) []facts.Fact {
	decls := e.extractDeclarations(kinds, root, ctx)
	// A declaration may be exported via a separate `export { A, B }` clause or
	// `export default Name` statement rather than an inline `export` keyword.
	// Mark the corresponding symbols as exported.
	markExported(decls, collectExportedLocalNames(kinds, root, ctx.src))
	markDefaultExportedName(kinds, root, ctx, decls)
	return applyMCPTools(kinds, root, ctx, decls)
}

func markExported(decls []facts.Fact, exported map[string]bool) {
	if len(exported) == 0 {
		return
	}
	for i := range decls {
		if decls[i].Kind != facts.KindSymbol {
			continue
		}
		local := decls[i].Name[strings.LastIndexByte(decls[i].Name, '.')+1:]
		if exported[local] {
			decls[i].SetProp("exported", true)
		}
	}
}

func (e *TSExtractor) frameworkPasses(kinds *tsutil.KindTable, root *sitter.Node, ctx *extractCtx, aliases map[string]tsAlias, result []facts.Fact, isEmberFile bool, emberSegments []emberTemplateSegment) tsFileResult {
	src, relFile := ctx.src, ctx.relFile
	if ctx.isNextJS {
		if routeFact := detectRoute(relFile); routeFact != nil {
			if file := reexportedDefaultFile(kinds, root, ctx); file != "" && routeFact.PropString("type") == "page" {
				routeFact.SetProp(propDefaultFrom, file)
			}
			result = append(result, *routeFact)
		}
	}
	if isEmberFile || ctx.isEmber {
		// Ember: classify component/service/model classes, attach strict-mode
		// template references, record @service injections for the ember-resolver
		// binder, and emit the router map's page routes.
		result = emberEnrich(kinds, result, root, src, relFile, aliases, emberSegments)
	}
	if ctx.isReactNav && !facts.IsTestPath(relFile) {
		result = append(result, extractReactNavScreens(kinds, root, src, relFile, aliases)...)
		result = attachReactNavLinks(kinds, result, root, src, relFile)
	}
	if ctx.isEmber {
		result = append(result, emberRouteFacts(kinds, root, src, relFile)...)
	}

	var out tsFileResult
	if ctx.isAngular {
		// Angular: classify the decorator-declared classes and attach the edges their
		// dependency injection declares. A post-pass over the declaration facts, as
		// emberEnrich is, because the decorator that decides a class's role sits above
		// the node the declaration walk stopped at.
		result, out.angular, out.angularInline, out.angularHTTP = angularEnrich(kinds, result, root, ctx, aliases)
		// The route arrays and router calls this file declares, for the repo-wide
		// walk in composeAngularRoutes. A test file's router configures a fixture,
		// not the application, the same gate the server-route passes apply.
		if !facts.IsTestPath(relFile) && declaresAngularRouting(src) {
			out.angularRouter = collectAngularRouterFile(kinds, root, ctx, aliases)
		}
	}

	// Requests through in-house clients the config declares. Not gated on a framework:
	// the declared receiver type is what makes a call a request, in a NestJS service as
	// much as anywhere. Test files excluded, as every other route pass excludes them.
	// See configuredclient.go.
	if len(e.clients) > 0 && !facts.IsTestPath(relFile) {
		var configured []facts.Fact
		configured, out.clients = configuredClientFacts(kinds, root, ctx, e.clients)
		result = append(result, configured...)
	}

	if ctx.isVue || ctx.isNuxt {
		result = append(result, vueRouterFacts(kinds, root, src, relFile, aliases)...)
	}
	// Nuxt accepts render-function pages in addition to .vue SFCs. Vue pages take
	// the early SFC return above and are emitted by extractVueSFC; emit the other
	// supported extensions here.
	if ctx.isNuxt && !isVueFile(relFile) {
		if route := detectNuxtRoute(relFile); route != nil {
			result = append(result, *route)
		}
	}

	attributeClientCallers(kinds, root, result)
	out.facts = result
	return out
}

func emberRouteFacts(kinds *tsutil.KindTable, root *sitter.Node, src []byte, relFile string) []facts.Fact {
	var result []facts.Fact
	if isEmberRouterFile(relFile) {
		result = append(result, extractEmberRoutes(kinds, root, src, relFile)...)
	}
	if engine, ok := isEmberEngineRoutesFile(relFile); ok {
		result = append(result, extractEmberEngineRoutes(kinds, root, src, relFile, engine)...)
	}
	return result
}

func vueRouterFacts(kinds *tsutil.KindTable, root *sitter.Node, src []byte, relFile string, aliases map[string]tsAlias) []facts.Fact {
	if !containsCreateRouterCall(kinds, root, src) {
		return nil
	}
	// Detect Vue Router configuration files
	config := facts.Fact{
		Kind: facts.KindRoute,
		Name: relFile,
		File: relFile,
		Line: 1,
		Props: map[string]any{
			"type":      "router_config",
			"language":  "typescript",
			"framework": "vue",
		},
	}
	return append([]facts.Fact{config}, extractVueRouterRoutes(kinds, root, src, relFile, aliases)...)
}

func (e *TSExtractor) extractImports(kinds *tsutil.KindTable, root *sitter.Node, src []byte, relFile string, aliases map[string]tsAlias, isSvelteKit bool) []facts.Fact {
	var result []facts.Fact
	dir := factpath.Dir(relFile)

	for i := range root.ChildCount() {
		child := root.Child(i)

		// export_statement only has a "source" field for re-exports
		// (export * from / export { X } from), not local declarations.
		var source *sitter.Node
		isReexport := false
		switch kindOf(kinds, child) {
		case "import_statement":
			source = findChildByKind(kinds, child, "string")
		case "export_statement":
			source = child.ChildByFieldName("source")
			isReexport = true
		default:
			continue
		}
		if source == nil {
			continue
		}

		importPath := strings.Trim(nodeText(source, src), `"'`)

		// Resolve path aliases and relative imports to filesystem-relative paths
		resolved, isExternal := resolveImportPath(importPath, dir, aliases)

		importSource := "internal"
		if isExternal {
			importSource = "external"
		}
		if isSvelteKit && isSvelteKitVirtualImport(importPath) {
			importSource = facts.DepSourceFramework
		}

		props := map[string]any{
			"language": "typescript",
			"source":   importSource,
		}
		if isReexport {
			props["reexport"] = true
		}

		result = append(result, facts.Fact{
			Kind:  facts.KindDependency,
			Name:  dir + " -> " + resolved,
			File:  relFile,
			Line:  int(child.StartPosition().Row) + 1,
			Props: props,
			Relations: []facts.Relation{
				{Kind: facts.RelImports, Target: resolved},
			},
		})
	}

	// CommonJS require() and dynamic import() calls are the only import mechanism in
	// server/build/task trees and code-split call sites; capture them as dependency
	// edges too so those graphs are not invisible. These calls can be nested anywhere,
	// so walk the whole tree; a dir-pair is deduped against the static imports above.
	seenDep := make(map[string]bool)
	for _, r := range result {
		seenDep[r.Name] = true
	}
	var walkDeps func(n *sitter.Node)
	walkDeps = func(n *sitter.Node) {
		if n == nil {
			return
		}
		if kindOf(kinds, n) == "call_expression" {
			if fn := n.ChildByFieldName("function"); fn != nil {
				isRequire := kindOf(kinds, fn) == "identifier" && nodeText(fn, src) == "require"
				isDynImport := kindOf(kinds, fn) == "import"
				if isRequire || isDynImport {
					if strArg := findChildByKind(kinds, n.ChildByFieldName("arguments"), "string"); strArg != nil {
						importPath := strings.Trim(nodeText(strArg, src), `"'`)
						resolved, isExternal := resolveImportPath(importPath, dir, aliases)
						name := dir + " -> " + resolved
						if !seenDep[name] {
							seenDep[name] = true
							source := "internal"
							if isExternal {
								source = "external"
							}
							if isSvelteKit && isSvelteKitVirtualImport(importPath) {
								source = facts.DepSourceFramework
							}
							result = append(result, facts.Fact{
								Kind:      facts.KindDependency,
								Name:      name,
								File:      relFile,
								Line:      int(n.StartPosition().Row) + 1,
								Props:     map[string]any{"language": "typescript", "source": source, "dynamic": true},
								Relations: []facts.Relation{{Kind: facts.RelImports, Target: resolved}},
							})
						}
					}
				}
			}
		}
		for i := range n.ChildCount() {
			walkDeps(n.Child(i))
		}
	}
	if mayCallRequireOrImport(src) {
		walkDeps(root)
	}

	return result
}

func (e *TSExtractor) extractDeclarations(kinds *tsutil.KindTable, root *sitter.Node, ctx *extractCtx) []facts.Fact {
	return e.extractStatements(kinds, root, ctx, wrapperDepth)
}

// extractStatements emits the declarations among a block's statements. A statement
// that is a wrapper function called at once contributes the declarations in its body
// as the file's own (see wrapperBody): exported when the wrapper hands them out (see
// wrapperExposes), and without the plain variables it keeps to itself (see
// withoutPlainVariables).
func (e *TSExtractor) extractStatements(kinds *tsutil.KindTable, block *sitter.Node, ctx *extractCtx, wrappers int) []facts.Fact {
	var result []facts.Fact
	for i := range block.ChildCount() {
		stmt := block.Child(i)
		result = append(result, e.extractNode(kinds, stmt, ctx, false, "")...)
		if wrappers == 0 {
			continue
		}
		if body := wrapperBody(kinds, stmt); body != nil {
			inner := e.extractStatements(kinds, body, ctx, wrappers-1)
			markExposed(inner, ctx.dir, wrapperExposes(kinds, stmt, body, ctx.src))
			result = append(result, withoutPlainVariables(inner)...)
		}
	}
	return result
}

// extractNode emits facts for a single declaration node. fallbackName supplies a
// name for anonymous default-exported declarations (e.g. `export default function
// () {}`), derived from the file name; it is ignored when the declaration has its
// own name.
func (e *TSExtractor) extractNode(kinds *tsutil.KindTable, node *sitter.Node, ctx *extractCtx, isExported bool, fallbackName string) []facts.Fact {
	switch kindOf(kinds, node) {
	case "export_statement":
		return e.exportFacts(kinds, node, ctx)
	case "function_declaration", "function_expression", "generator_function_declaration", "generator_function":
		symbolName := fallbackName
		if name := findChildByKind(kinds, node, "identifier"); name != nil {
			symbolName = nodeText(name, ctx.src)
		}
		if symbolName == "" {
			return nil
		}
		return []facts.Fact{e.funcSymbol(kinds, node, node, ctx, symbolName, isExported)}
	case "arrow_function", "call_expression":
		// A call_expression is reached for `export default memo(...)` / `forwardRef(...)`.
		if fallbackName == "" {
			return nil
		}
		return []facts.Fact{e.funcSymbol(kinds, node, node, ctx, fallbackName, isExported)}
	case "class_declaration", "abstract_class_declaration", "class":
		return e.classFacts(kinds, node, ctx, isExported, fallbackName)
	case "expression_statement":
		return e.commonJSExportFacts(kinds, node, ctx)
	case "interface_declaration", "type_alias_declaration", "enum_declaration", "internal_module", "module":
		return e.namedDeclarationFacts(kinds, node, ctx, isExported)
	case "lexical_declaration", "variable_declaration":
		var result []facts.Fact
		for j := range node.ChildCount() {
			if decl := node.Child(j); kindOf(kinds, decl) == "variable_declarator" {
				result = append(result, e.variableFacts(kinds, node, decl, ctx, isExported)...)
			}
		}
		return result
	}
	return nil
}

func (e *TSExtractor) exportFacts(kinds *tsutil.KindTable, node *sitter.Node, ctx *extractCtx) []facts.Fact {
	isDefault := hasChildKind(kinds, node, "default")
	fb := ""
	if isDefault {
		fb = fileSymbolName(ctx.relFile)
	}
	// Named/inline declaration inside the export.
	if decl := firstDeclChild(kinds, node); decl != nil {
		return markDefaultExport(e.extractNode(kinds, decl, ctx, true, fb), isDefault)
	}
	// Anonymous default export of a value: name it after the file.
	if isDefault {
		for _, k := range []string{"function_expression", "generator_function", "class", "arrow_function", "call_expression"} {
			if c := findChildByKind(kinds, node, k); c != nil {
				return markDefaultExport(e.extractNode(kinds, c, ctx, true, fb), true)
			}
		}
	}
	return nil
}

func (e *TSExtractor) classFacts(kinds *tsutil.KindTable, node *sitter.Node, ctx *extractCtx, isExported bool, fallbackName string) []facts.Fact {
	src, dir, relFile := ctx.src, ctx.dir, ctx.relFile
	symbolName := fallbackName
	if name := findChildByKind(kinds, node, "type_identifier"); name != nil {
		symbolName = nodeText(name, src)
	}
	if symbolName == "" {
		return nil
	}
	classBody := findChildByKind(kinds, node, "class_body")
	result := []facts.Fact{e.classSymbol(kinds, node, classBody, ctx, symbolName, isExported)}

	// TypeORM: a class decorated @Entity is a table. The storage fact is a COMPANION
	// to the symbol above, as in Kotlin/Room and Java/JPA — the class keeps its own
	// symbol fact.
	if ctx.orms.typeORM && !facts.IsTestPath(relFile) {
		if sf := typeORMEntityStorage(kinds, node, src, symbolName, relFile, dir,
			int(node.StartPosition().Row)+1); sf != nil {
			result = append(result, *sf)
		}
	}

	// NestJS/InversifyJS: a class decorated @Controller declares server routes.
	// Companion facts in the same sense as the @Entity storage above — the class
	// keeps its symbol, and each verb-decorated method gains a route. Skipped for
	// test files: an e2e fixture's controller would otherwise mint server routes
	// that no production client calls, i.e. false unused-route findings (the
	// counterpart to the v141 client-side gate).
	if !facts.IsTestPath(relFile) {
		result = append(result, decoratorRouteFacts(kinds, node, classBody, src, relFile, dir, symbolName)...)
	}
	return append(result, e.classMemberFacts(kinds, classBody, ctx, symbolName, isExported)...)
}

func (e *TSExtractor) classSymbol(kinds *tsutil.KindTable, node, classBody *sitter.Node, ctx *extractCtx, symbolName string, isExported bool) facts.Fact {
	src, dir := ctx.src, ctx.dir
	f := facts.Fact{
		Kind: facts.KindSymbol,
		Name: dir + "." + symbolName,
		File: ctx.relFile,
		Line: int(node.StartPosition().Row) + 1,
		Props: map[string]any{
			"symbol_kind": facts.SymbolClass,
			"exported":    isExported,
			"language":    "typescript",
		},
		Relations: []facts.Relation{
			{Kind: facts.RelDeclares, Target: dir},
		},
	}

	// A TS `abstract class` is an abstraction (has unimplemented members and
	// cannot be instantiated) — tag it so package-metrics counts it toward
	// abstractness, matching Java/Kotlin/Python. Plain classes stay concrete.
	if kindOf(kinds, node) == "abstract_class_declaration" {
		f.SetProp("abstract", true)
	}

	// The base class the source names, and the module the file imported that
	// name from. No relation accompanies them: the identifier alone is not a
	// symbol identity (409 classes in one frontend write the same `Controller`
	// against two unrelated base classes), and the local name a default or
	// aliased import binds is not the name the exporting file declares, so an
	// edge built from either would be a resolution nothing measured.
	if super := tsSuperclassName(kinds, node, src); super != "" {
		f.SetProp(superclassProp, super)
		if module := ctx.imports.modules[super]; module != "" {
			f.SetProp(superclassModuleProp, module)
		}
	}

	if doc := tsDoc(kinds, node, src); doc != "" {
		facts.SetDoc(&f, doc)
	}
	f.Relations = append(f.Relations, implementsRelations(kinds, node, src)...)
	classifySymbol(kinds, &f, symbolName, classBody, ctx, facts.SymbolClass)
	if names := classDecoratorNames(kinds, node, src); names != "" {
		f.SetProp("decorators", names)
	}
	return f
}

// implementsRelations reads the implements clause, nested under class_heritage.
func implementsRelations(kinds *tsutil.KindTable, node *sitter.Node, src []byte) []facts.Relation {
	var out []facts.Relation
	for _, heritage := range childrenOfKind(kinds, node, "class_heritage") {
		for _, clause := range childrenOfKind(kinds, heritage, "implements_clause") {
			for _, t := range childrenOfKind(kinds, clause, "type_identifier") {
				out = append(out, facts.Relation{Kind: facts.RelImplements, Target: nodeText(t, src)})
			}
		}
	}
	return out
}

func childrenOfKind(kinds *tsutil.KindTable, node *sitter.Node, kind string) []*sitter.Node {
	var out []*sitter.Node
	for i := range node.ChildCount() {
		if c := node.Child(i); kindOf(kinds, c) == kind {
			out = append(out, c)
		}
	}
	return out
}

func (e *TSExtractor) classMemberFacts(kinds *tsutil.KindTable, classBody *sitter.Node, ctx *extractCtx, symbolName string, isExported bool) []facts.Fact {
	if classBody == nil {
		return nil
	}
	src := ctx.src
	scope := ctx.callScope(kinds, symbolName, classFieldTypes(kinds, classBody, src, ctx.dir, ctx.importMap))
	var result []facts.Fact
	var pendingDecorators []string
	for j := range classBody.ChildCount() {
		member := classBody.Child(j)
		if kindOf(kinds, member) == "decorator" {
			if dn, _ := decoratorNameArgs(kinds, member, src); dn != "" {
				pendingDecorators = append(pendingDecorators, dn)
			}
			continue
		}
		if kindOf(kinds, member) == "comment" || !member.IsNamed() {
			continue
		}
		memberDecorators := append(pendingDecorators, ownDecoratorNames(kinds, member, src)...)
		pendingDecorators = nil
		if f, ok := e.memberFact(kinds, member, ctx, scope, symbolName, isExported, memberDecorators); ok {
			result = append(result, f)
		}
	}
	return result
}

func (e *TSExtractor) memberFact(kinds *tsutil.KindTable, member *sitter.Node, ctx *extractCtx, scope callScope, symbolName string, isExported bool, decorators []string) (facts.Fact, bool) {
	src, dir, relFile := ctx.src, ctx.dir, ctx.relFile
	if kindOf(kinds, member) != "method_definition" && kindOf(kinds, member) != "public_field_definition" {
		return facts.Fact{}, false
	}
	methodName := findChildByKind(kinds, member, "property_identifier")
	if methodName == nil {
		methodName = findChildByKind(kinds, member, "identifier")
	}
	if methodName == nil {
		return facts.Fact{}, false
	}
	mName := nodeText(methodName, src)
	// A `#`-prefixed member is private to the class in the language's
	// own sense and has no callers to measure. A constructor does: it
	// runs on every instantiation, and what it calls is the difference
	// between a class that builds itself and one that fetches. Skipping
	// both together left "nothing may be fetched from a constructor"
	// with no fact to stand on.
	if strings.HasPrefix(mName, "#") {
		return facts.Fact{}, false
	}
	isPrivate := false
	for _, c := range childrenOfKind(kinds, member, "accessibility_modifier") {
		if nodeText(c, src) == "private" {
			isPrivate = true
			break
		}
	}
	mRels := []facts.Relation{{Kind: facts.RelDeclares, Target: dir}}
	callRels, m := collectCallsWithMetrics(member, scope, dir+"."+symbolName+"."+mName, mName)
	mRels = append(mRels, callRels...)
	mProps := map[string]any{
		"symbol_kind": facts.SymbolMethod,
		"exported":    isExported && !isPrivate,
		"language":    "typescript",
		"receiver":    symbolName,
	}
	if doc := tsDoc(kinds, member, src); doc != "" {
		facts.SetDocProp(mProps, doc)
	}
	if names := decoratorSetProp(decorators); names != "" {
		mProps["decorators"] = names
	}
	if takes, ok := declaresParameters(kinds, member); ok {
		mProps["takes_parameters"] = takes
	}
	switch accessorKind(kinds, member) {
	case "get":
		mProps["symbol_kind"] = facts.SymbolGetter
		mProps["getter_calls"] = countCallRelations(callRels)
	case "set":
		mProps[propAccessor] = "set"
	}
	if stimulusStaticField(kinds, member, mName, relFile) {
		mProps["framework"] = "stimulus"
		mProps["stimulus_static"] = mName
	}
	applyTSMetrics(mProps, m)
	return facts.Fact{
		Kind:      facts.KindSymbol,
		Name:      dir + "." + symbolName + "." + mName,
		File:      relFile,
		Line:      int(member.StartPosition().Row) + 1,
		Props:     mProps,
		Relations: mRels,
	}, true
}

// CommonJS export assignment: `exports.name = function` /
// `module.exports.name = function`. None of the declaration-shaped cases
// fire in a classic Node file, so a CommonJS module's whole public
// surface was invisible — an Express controller written as
// `exports.index = function(req, res)` emitted nothing at all. Only the
// member-assignment-of-a-function shape emits a symbol; a plain value,
// a re-exported identifier, or a whole-object `module.exports = {…}`
// carries no declaration this pass can name without guessing.
func (e *TSExtractor) commonJSExportFacts(kinds *tsutil.KindTable, node *sitter.Node, ctx *extractCtx) []facts.Fact {
	assign := findChildByKind(kinds, node, "assignment_expression")
	if assign == nil {
		return nil
	}
	name := commonJSExportName(kinds, assign.ChildByFieldName("left"), ctx.src)
	right := assign.ChildByFieldName("right")
	if name == "" || right == nil {
		return nil
	}
	switch kindOf(kinds, right) {
	case "function_expression", "arrow_function", "generator_function":
		return []facts.Fact{e.funcSymbol(kinds, node, right, ctx, name, true)}
	}
	return nil
}

var namedDeclarations = map[string]struct {
	symbolKind string
	nameKinds  []string
}{
	"interface_declaration":  {facts.SymbolInterface, []string{"type_identifier"}},
	"type_alias_declaration": {facts.SymbolType, []string{"type_identifier"}},
	"enum_declaration":       {facts.SymbolEnum, []string{"identifier", "type_identifier"}},
	// TypeScript `namespace X {}` / `module X {}`.
	"internal_module": {"namespace", []string{"identifier", "nested_identifier"}},
	"module":          {"namespace", []string{"identifier", "nested_identifier"}},
}

func (e *TSExtractor) namedDeclarationFacts(kinds *tsutil.KindTable, node *sitter.Node, ctx *extractCtx, isExported bool) []facts.Fact {
	d := namedDeclarations[kindOf(kinds, node)]
	for _, k := range d.nameKinds {
		if name := findChildByKind(kinds, node, k); name != nil {
			return []facts.Fact{e.simpleSymbol(node, ctx, nodeText(name, ctx.src), d.symbolKind, isExported)}
		}
	}
	return nil
}

func (e *TSExtractor) variableFacts(kinds *tsutil.KindTable, node, decl *sitter.Node, ctx *extractCtx, isExported bool) []facts.Fact {
	src, dir, relFile := ctx.src, ctx.dir, ctx.relFile
	name := findChildByKind(kinds, decl, "identifier")
	if name == nil {
		return nil
	}
	symbolName := nodeText(name, src)

	// Determine the value node and the symbol kind. A function written as
	// the value (an arrow, or `var f = function () {}`, which is how a script
	// older than arrows declares most of its functions) and a
	// memo/forwardRef-wrapped value are functions/components; everything else
	// is a plain variable.
	symbolKind := facts.SymbolVariable
	var body *sitter.Node
	if v := functionValue(kinds, decl); v != nil {
		symbolKind = facts.SymbolFunc
		body = v
	} else if call := findChildByKind(kinds, decl, "call_expression"); call != nil && isComponentWrapper(kinds, call, src) {
		symbolKind = facts.SymbolFunc
		body = call
	}

	vRels := []facts.Relation{{Kind: facts.RelDeclares, Target: dir}}
	var vMetrics *tsBodyMetrics
	if body != nil {
		callRels, m := collectCallsWithMetrics(body, ctx.callScope(kinds, "", nil), dir+"."+symbolName, symbolName)
		vRels = append(vRels, callRels...)
		vMetrics = m
	}
	f := facts.Fact{
		Kind: facts.KindSymbol,
		Name: dir + "." + symbolName,
		File: relFile,
		Line: int(node.StartPosition().Row) + 1,
		Props: map[string]any{
			"symbol_kind": symbolKind,
			"exported":    isExported,
			"language":    "typescript",
		},
		Relations: vRels,
	}
	if doc := tsDoc(kinds, decl, src); doc != "" {
		facts.SetDoc(&f, doc)
	}
	if symbolKind == facts.SymbolFunc {
		if takes, ok := declaresParameters(kinds, decl); ok {
			f.SetProp("takes_parameters", takes)
		}
		applyTSMetrics(f.Props, vMetrics)
	}
	classifySymbol(kinds, &f, symbolName, body, ctx, symbolKind)
	result := []facts.Fact{f}
	if symbolKind == facts.SymbolVariable {
		result = append(result, objectMemberSymbols(kinds, decl.ChildByFieldName("value"), ctx, symbolName, isExported)...)
	}

	// Drizzle: `export const orders = pgTable("orders", {...})` is a table. The
	// call_expression is already in hand above — it simply fails isComponentWrapper.
	if ctx.orms.drizzle && !facts.IsTestPath(relFile) {
		if call := findChildByKind(kinds, decl, "call_expression"); call != nil {
			if sf := drizzleTableStorage(kinds, call, src, symbolName, relFile, dir,
				int(decl.StartPosition().Row)+1); sf != nil {
				result = append(result, *sf)
			}
		}
	}
	return result
}

// funcSymbol builds a function/component symbol fact. declNode supplies the source
// location; body is walked for outgoing calls and JSX-based classification.
// commonJSExportName returns the exported member name when left is the
// `exports.<name>` or `module.exports.<name>` shape, and "" for every other
// assignment target — including bare `module.exports`, whose assigned value
// has no member name to carry.
func commonJSExportName(kinds *tsutil.KindTable, left *sitter.Node, src []byte) string {
	if left == nil || kindOf(kinds, left) != "member_expression" {
		return ""
	}
	prop := left.ChildByFieldName("property")
	if prop == nil || kindOf(kinds, prop) != "property_identifier" {
		return ""
	}
	obj := left.ChildByFieldName("object")
	if obj == nil {
		return ""
	}
	if kindOf(kinds, obj) == "identifier" && nodeText(obj, src) == "exports" {
		return nodeText(prop, src)
	}
	if kindOf(kinds, obj) == "member_expression" {
		inner := obj.ChildByFieldName("object")
		innerProp := obj.ChildByFieldName("property")
		if inner != nil && kindOf(kinds, inner) == "identifier" && nodeText(inner, src) == "module" &&
			innerProp != nil && nodeText(innerProp, src) == "exports" {
			return nodeText(prop, src)
		}
	}
	return ""
}

func (e *TSExtractor) funcSymbol(kinds *tsutil.KindTable, declNode, body *sitter.Node, ctx *extractCtx, name string, exported bool) facts.Fact {
	rels := []facts.Relation{{Kind: facts.RelDeclares, Target: ctx.dir}}
	callRels, m := collectCallsWithMetrics(body, ctx.callScope(kinds, "", nil), ctx.dir+"."+name, name)
	rels = append(rels, callRels...)
	f := facts.Fact{
		Kind: facts.KindSymbol,
		Name: ctx.dir + "." + name,
		File: ctx.relFile,
		Line: int(declNode.StartPosition().Row) + 1,
		Props: map[string]any{
			"symbol_kind": facts.SymbolFunc,
			"exported":    exported,
			"language":    "typescript",
		},
		Relations: rels,
	}
	if doc := tsDoc(kinds, declNode, ctx.src); doc != "" {
		facts.SetDoc(&f, doc)
	}
	if takes, ok := declaresParameters(kinds, declNode); ok {
		f.SetProp("takes_parameters", takes)
	}
	applyTSMetrics(f.Props, m)
	classifySymbol(kinds, &f, name, body, ctx, facts.SymbolFunc)
	return f
}

// simpleSymbol builds a declaration-only symbol fact (interface, type, enum, namespace).
func (e *TSExtractor) simpleSymbol(node *sitter.Node, ctx *extractCtx, name, kind string, exported bool) facts.Fact {
	f := facts.Fact{
		Kind: facts.KindSymbol,
		Name: ctx.dir + "." + name,
		File: ctx.relFile,
		Line: int(node.StartPosition().Row) + 1,
		Props: map[string]any{
			"symbol_kind": kind,
			"exported":    exported,
			"language":    "typescript",
		},
		Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: ctx.dir}},
	}
	return f
}

// detectRoute checks if a file path corresponds to a Next.js route.
func detectRoute(relFile string) *facts.Fact {
	// Next.js App Router: app/**/page.tsx, app/**/route.tsx
	// Next.js Pages Router: pages/**/*.tsx

	parts := strings.Split(filepath.ToSlash(relFile), "/")

	// App Router
	for i, p := range parts {
		if p == "app" && i < len(parts)-1 {
			fileName := parts[len(parts)-1]
			baseName := strings.TrimSuffix(strings.TrimSuffix(fileName, ".tsx"), ".ts")

			if baseName == "page" || baseName == "route" || baseName == "layout" || baseName == "loading" || baseName == "error" {
				// Strip Next.js route groups — directory segments wrapped in ()
				// that act as layout organizers without affecting the URL.
				// e.g. (standard), (client-data), (header) → removed from path.
				segParts := parts[i+1 : len(parts)-1]
				urlParts := make([]string, 0, len(segParts))
				for _, seg := range segParts {
					if len(seg) >= 2 && seg[0] == '(' && seg[len(seg)-1] == ')' {
						continue // route group — not part of the URL
					}
					urlParts = append(urlParts, seg)
				}

				routePath := "/" + strings.Join(urlParts, "/")
				if routePath == "/" {
					routePath = "/"
				}

				method := "GET"
				if baseName == "route" {
					method = "ALL" // API route handler
				}

				return &facts.Fact{
					Kind: facts.KindRoute,
					Name: routePath,
					File: relFile,
					Line: 1,
					Props: map[string]any{
						"method":    method,
						"type":      baseName,
						"router":    "app",
						"language":  "typescript",
						"framework": "nextjs",
					},
				}
			}
		}
	}

	// Pages Router. A test's page objects (tests/e2e/pages/LoginPage.ts) are no pages.
	if facts.IsTestPath(relFile) {
		return nil
	}
	for i, p := range parts {
		if p == "pages" && i < len(parts)-1 {
			remaining := parts[i+1:]
			fileName := remaining[len(remaining)-1]
			baseName := strings.TrimSuffix(strings.TrimSuffix(fileName, ".tsx"), ".ts")

			// Skip _app, _document, _error
			if strings.HasPrefix(baseName, "_") {
				return nil
			}

			routeParts := make([]string, 0, len(remaining))
			for j, rp := range remaining {
				if j == len(remaining)-1 {
					if baseName != "index" {
						routeParts = append(routeParts, baseName)
					}
				} else {
					routeParts = append(routeParts, rp)
				}
			}

			routePath := "/" + strings.Join(routeParts, "/")

			// Detect API routes
			isAPI := len(remaining) > 0 && remaining[0] == "api"
			method := "GET"
			if isAPI {
				method = "ALL"
			}

			return &facts.Fact{
				Kind: facts.KindRoute,
				Name: routePath,
				File: relFile,
				Line: 1,
				Props: map[string]any{
					"method":    method,
					"type":      "page",
					"router":    "pages",
					"language":  "typescript",
					"framework": "nextjs",
				},
			}
		}
	}

	return nil
}

// detectNextJSRoot checks if the repository is a Next.js project.
// It searches the TypeScript root directory (which may be a subdirectory in a
// monorepo) for next.config.* files or a package.json with a "next" dependency.
func detectNextJSRoot(repoPath, tsRoot string) bool {
	return detectNextJSAt(tsRoot) || (tsRoot != repoPath && detectNextJSAt(repoPath))
}

// collectPackageNames maps each directory holding a package.json to the package
// name it declares. Read off-glob (package.json is not a TypeScript file), the
// same way tsconfig aliases and ORM flags already are.
// collectPackageNames maps each directory that declares an npm package to its name,
// read from the "name" field of the package.json it contains.
//
// It WALKS THE REPO ITSELF rather than consuming the engine's ignore-glob-filtered file
// list, following the precedent set by the OpenAPI and Symfony-config extractors: the
// globs exist to suppress config/data noise, not to hide architecturally meaningful
// files. That distinction is load-bearing here. A config that ignores "**/*.json" — a
// reasonable thing to write, and what the bundled mcp-arch.yaml does — removed every
// package.json from the file list, so this returned nothing, no module carried a
// package_name, and the cross-repo linker's own-@scope guard silently could not fire. A
// repo importing a sibling package it publishes itself was then reported as depending
// on whatever other repo happened to be labelled like the scope.
//
// The failure was invisible in two directions at once: nothing errored, and the golden
// fixtures kept passing because they build their engine from config.Default(), which has
// no such glob. Reading the file directly removes the config's ability to break the
// guard at all.
//
// Because the globs no longer apply, every directory that must not be descended into is
// named here instead — tsSkipDirs (shared with the alias-root walk) plus dot-directories
// and testdata. node_modules is the critical one: a dependency's package.json would
// otherwise be read as if the repo published it.
func collectPackageNames(repoPath string) map[string]string {
	out := map[string]string{}
	_ = filepath.WalkDir(repoPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable subtree: skip it rather than fail extraction
		}
		if d.IsDir() {
			name := d.Name()
			if path != repoPath && (strings.HasPrefix(name, ".") || tsSkipDirs[name] || name == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != "package.json" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		var pkg struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(data, &pkg); err != nil || pkg.Name == "" {
			return nil
		}
		rel, err := filepath.Rel(repoPath, filepath.Dir(path)) //factpath:host
		if err != nil {
			return nil
		}
		out[factpath.Slash(rel)] = pkg.Name
		return nil
	})
	return out
}

// nearestPackageName returns the package name declared by the closest ancestor
// (or self) of dir, or "" if none — the npm resolution rule, so a file under
// packages/api/src belongs to packages/api's package.
func nearestPackageName(pkgNames map[string]string, dir string) string {
	for d := filepath.ToSlash(dir); ; {
		if name, ok := pkgNames[d]; ok {
			return name
		}
		i := strings.LastIndexByte(d, '/')
		if i < 0 {
			if d == "." {
				return ""
			}
			d = "."
			continue
		}
		d = d[:i]
	}
}

// nextJSPackageDirs returns the repo-relative directories of the packages that are
// Next.js apps. A workspace whose root package declares no next still holds one
// (onyx's web/ beside its desktop/ and widget/), and the root-level detection,
// which reads one directory, cannot see it. The walk skips what
// collectPackageNames skips, and directories of examples (isExampleDir).
func nextJSPackageDirs(repoPath string) []string {
	var out []string
	_ = filepath.WalkDir(repoPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if path != repoPath && (strings.HasPrefix(name, ".") || tsSkipDirs[name] || name == "testdata" || isExampleDir(name)) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != "package.json" {
			return nil
		}
		dir := filepath.Dir(path) //factpath:host
		if dir != repoPath && detectNextJSAt(dir) {
			if rel, err := filepath.Rel(repoPath, dir); err == nil {
				out = append(out, filepath.ToSlash(rel))
			}
		}
		return nil
	})
	return out
}

// isExampleDir reports whether a directory holds examples, samples or demos of the
// repository, alone or in a compound name (example-apps, with_demos). A Next.js app
// there demonstrates the repository and is none of its apps: read as one, the demo
// excalidraw ships in examples/with-nextjs made the repository a Next.js app.
func isExampleDir(name string) bool {
	for _, tok := range strings.FieldsFunc(strings.ToLower(name), func(r rune) bool { return r == '-' || r == '_' }) {
		switch tok {
		case "example", "examples", "sample", "samples", "demo", "demos":
			return true
		}
	}
	return false
}

// inNextJSPackage reports whether a file belongs to one of the Next.js packages.
func inNextJSPackage(relFile string, dirs []string) bool {
	for _, d := range dirs {
		if strings.HasPrefix(relFile, d+"/") {
			return true
		}
	}
	return false
}

func detectNextJSAt(dir string) bool {
	// Check next.config.* at this directory level
	for _, name := range []string{"next.config.js", "next.config.mjs", "next.config.ts"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}

	// Check package.json for next dependency
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return false
	}
	var pkg map[string]any
	if err := json.Unmarshal(data, &pkg); err != nil {
		return false
	}
	for _, key := range []string{"dependencies", "devDependencies"} {
		if deps, ok := pkg[key].(map[string]any); ok {
			if _, ok := deps["next"]; ok {
				return true
			}
		}
	}
	return false
}

func isTypeScriptFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".ts" || ext == ".tsx" || ext == ".vue" || ext == ".js" || ext == ".jsx" || ext == ".mjs" ||
		ext == ".svelte" || ext == ".gts" || ext == ".gjs" || ext == ".hbs" || ext == ".graphql" || ext == ".gql"
}

// minifiedLineThreshold is the line length above which a file is treated as
// minified/generated. Hand-written source effectively never has a single line
// this long; bundlers, minifiers, and embedded data blobs routinely produce
// lines of tens of thousands of characters on one line.
const minifiedLineThreshold = 2000

// isMinifiedSource reports whether content looks like a minified or bundled
// artifact rather than hand-written source, by the presence of any line longer
// than minifiedLineThreshold. Parsing such files (e.g. a checked-in webpack /
// vendor bundle served statically) pollutes the fact graph with obfuscated
// symbols and drives spurious complexity/hotspot findings, so they are skipped.
// The scan is bounded: it stops at the first over-length line without buffering
// the whole file.
func isMinifiedSource(content []byte) bool {
	col := 0
	for _, b := range content {
		if b == '\n' {
			col = 0
			continue
		}
		col++
		if col > minifiedLineThreshold {
			return true
		}
	}
	return false
}

// OwnsFile implements plugin.FileOwner for incremental caching.
// OwnsFile includes .html, because an Angular component's references live in its
// template and a template edit must re-run this extractor. The contract permits a
// superset — over-inclusion only reduces cache reuse — and a .html file in a
// non-Angular repository is read by nothing, so the only cost is a cache key that
// notices a page changing.
func (e *TSExtractor) OwnsFile(relFile string) bool {
	return isTypeScriptFile(relFile) || isAngularTemplateFile(relFile)
}

// isAngularTemplateFile reports whether a path is a candidate component template.
func isAngularTemplateFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".html" || ext == ".htm"
}

// hasChildKind reports whether node has a direct child of the given kind.
func hasChildKind(kinds *tsutil.KindTable, node *sitter.Node, kind string) bool {
	return findChildByKind(kinds, node, kind) != nil
}

// firstDeclChild returns the first named declaration child of an export_statement,
// or nil if the export wraps something else (a value, re-export clause, etc.).
func firstDeclChild(kinds *tsutil.KindTable, node *sitter.Node) *sitter.Node {
	for _, k := range []string{
		"function_declaration", "generator_function_declaration",
		"class_declaration", "abstract_class_declaration",
		"interface_declaration", "type_alias_declaration",
		"lexical_declaration", "variable_declaration",
		"enum_declaration", "internal_module", "module",
	} {
		if c := findChildByKind(kinds, node, k); c != nil {
			return c
		}
	}
	return nil
}

// fileSymbolName derives a symbol name from a file path for anonymous default
// exports. Generic Next.js filenames (page, route, layout, …) are disambiguated
// with their parent directory segment, e.g. app/dashboard/page.tsx → "DashboardPage".
func fileSymbolName(relFile string) string {
	base := filepath.Base(relFile)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	switch base {
	case "index", "page", "route", "layout", "loading", "error", "not-found", "template", "default",
		"+page", "+layout", "+error", "+server":
		parent := filepath.Base(factpath.Dir(relFile))
		if parent != "" && parent != "." && parent != string(filepath.Separator) {
			return toPascal(parent) + toPascal(base)
		}
	}
	return toPascal(base)
}

// toPascal converts an arbitrary identifier-ish string into PascalCase, splitting
// on any non-alphanumeric characters (e.g. "my-component" → "MyComponent").
func toPascal(s string) string {
	var b strings.Builder
	upNext := true
	for _, r := range s {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			if upNext && r >= 'a' && r <= 'z' {
				r -= 'a' - 'A'
			}
			b.WriteRune(r)
			upNext = false
		default:
			upNext = true
		}
	}
	return b.String()
}

// collectExportedLocalNames returns the set of locally-declared names that are
// exported via a separate `export { A, B as C }` clause or `export default Name`
// statement (where the declaration itself carries no inline export keyword).
func collectExportedLocalNames(kinds *tsutil.KindTable, root *sitter.Node, src []byte) map[string]bool {
	out := make(map[string]bool)
	for i := range root.ChildCount() {
		child := root.Child(i)
		if kindOf(kinds, child) != "export_statement" {
			continue
		}
		// export { A, B as C }
		if clause := findChildByKind(kinds, child, "export_clause"); clause != nil {
			for j := range clause.ChildCount() {
				spec := clause.Child(j)
				if kindOf(kinds, spec) != "export_specifier" {
					continue
				}
				if n := spec.ChildByFieldName("name"); n != nil {
					out[nodeText(n, src)] = true
				}
			}
			continue
		}
		// export default Name
		if hasChildKind(kinds, child, "default") {
			if id := findChildByKind(kinds, child, "identifier"); id != nil {
				out[nodeText(id, src)] = true
			}
		}
	}
	return out
}

// reactHTTPMethods are the App Router route-handler export names.
var reactHTTPMethods = map[string]bool{
	"GET": true, "POST": true, "PUT": true, "DELETE": true,
	"PATCH": true, "HEAD": true, "OPTIONS": true,
}

// classifySymbol enriches a symbol fact with React/Next.js semantic props
// (web_component, framework, and for route handlers method), mirroring the
// ios_component/framework classification used by the Swift extractor. body, when
// non-nil, is scanned for JSX to confirm component-ness in non-TSX files.
func classifySymbol(kinds *tsutil.KindTable, f *facts.Fact, name string, body *sitter.Node, ctx *extractCtx, symbolKind string) {
	// Next.js App Router route handler: GET/POST/... in a route.{ts,tsx} file.
	if symbolKind == facts.SymbolFunc && reactHTTPMethods[name] && isAppRouteFile(ctx.relFile) {
		f.SetProp("web_component", "route_handler")
		f.SetProp("method", name)
		f.SetProp("framework", "nextjs")
		return
	}
	// SvelteKit `load` export: +page.ts/+layout.ts/+page.server.ts/+layout.server.ts
	// under routes/. Invoked by SvelteKit's router for every navigation/render,
	// never by an in-repo call — same shape as the Next.js case above.
	if symbolKind == facts.SymbolFunc && name == "load" && ctx.isSvelteKit &&
		isUnderRoutesDir(ctx.relFile) && svelteKitLoadFileBasenames[svelteKitFileBasename(ctx.relFile)] {
		f.SetProp("web_component", "route_handler")
		f.SetProp("framework", "sveltekit")
		return
	}
	// SvelteKit +server.ts HTTP-method export (GET/POST/...) under routes/.
	if symbolKind == facts.SymbolFunc && reactHTTPMethods[name] && ctx.isSvelteKit &&
		isUnderRoutesDir(ctx.relFile) && svelteKitFileBasename(ctx.relFile) == "+server" {
		f.SetProp("web_component", "route_handler")
		f.SetProp("method", name)
		f.SetProp("framework", "sveltekit")
		return
	}
	// SvelteKit hooks.server.ts hooks (handle/handleError/handleFetch) — invoked by
	// SvelteKit by file/export-name convention, never by an in-repo call. Same
	// precedent as Python's framework-hook-name exclusion (gunicorn/ASGI lifespan).
	if symbolKind == facts.SymbolFunc && svelteKitHookNames[name] && ctx.isSvelteKit &&
		svelteKitFileBasename(ctx.relFile) == "hooks.server" {
		f.SetProp("web_component", "route_handler")
		f.SetProp("framework", "sveltekit")
		return
	}
	// Composable (Vue/Nuxt) or hook (React): a useXxx function.
	if symbolKind == facts.SymbolFunc && isHookName(name) {
		if ctx.isVue || ctx.isNuxt {
			f.SetProp("web_component", "composable")
			if ctx.isNuxt {
				f.SetProp("framework", "nuxt")
			} else {
				f.SetProp("framework", "vue")
			}
		} else {
			f.SetProp("web_component", "hook")
			f.SetProp("framework", "react")
		}
		return
	}
	// React component: a PascalCase function/class that renders JSX. In .tsx/.jsx
	// files a PascalCase function/class is treated as a component; elsewhere we
	// require literal JSX in the body to avoid misclassifying plain classes.
	if isComponentName(name) && (symbolKind == facts.SymbolFunc || symbolKind == facts.SymbolClass) {
		if ctx.isTSX || (body != nil && containsJSX(kinds, body)) {
			f.SetProp("web_component", "component")
			if ctx.isNextJS {
				f.SetProp("framework", "nextjs")
			} else {
				f.SetProp("framework", "react")
			}
		}
	}
}

// isHookName reports whether name follows the React hook convention useXxx.
func isHookName(name string) bool {
	if !strings.HasPrefix(name, "use") || len(name) < 4 {
		return false
	}
	c := name[3]
	return c >= 'A' && c <= 'Z'
}

// isComponentName reports whether name is PascalCase (a React component convention).
func isComponentName(name string) bool {
	return name != "" && name[0] >= 'A' && name[0] <= 'Z'
}

// isAppRouteFile reports whether relFile is a Next.js App Router route handler
// file (a route.{ts,tsx} under an "app" directory segment).
func isAppRouteFile(relFile string) bool {
	base := filepath.Base(relFile)
	base = strings.TrimSuffix(strings.TrimSuffix(base, ".tsx"), ".ts")
	if base != "route" {
		return false
	}
	for _, seg := range strings.Split(filepath.ToSlash(relFile), "/") {
		if seg == "app" {
			return true
		}
	}
	return false
}

// svelteKitLoadFileBasenames are +page/+layout (client or server) file basenames
// (extension stripped) whose exported `load` function is invoked by SvelteKit's
// router for every navigation/render — never by an in-repo call.
var svelteKitLoadFileBasenames = map[string]bool{
	"+page": true, "+layout": true,
	"+page.server": true, "+layout.server": true,
}

// svelteKitHookNames are the hooks.server.ts exports SvelteKit invokes by
// file/export-name convention (request handling, error reporting, outbound
// fetch interception) — never by an in-repo call.
var svelteKitHookNames = map[string]bool{
	"handle": true, "handleError": true, "handleFetch": true,
}

// isUnderRoutesDir reports whether relFile has a "routes" path segment, mirroring
// detectSvelteKitRoute's own directory check (svelte.go).
func isUnderRoutesDir(relFile string) bool {
	for _, seg := range strings.Split(filepath.ToSlash(relFile), "/") {
		if seg == "routes" {
			return true
		}
	}
	return false
}

// svelteKitFileBasename returns relFile's basename with its .ts/.js extension
// stripped, e.g. "+page.server.ts" -> "+page.server", "hooks.server.ts" -> "hooks.server".
func svelteKitFileBasename(relFile string) string {
	base := filepath.Base(filepath.ToSlash(relFile))
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// containsJSX reports whether the subtree rooted at node contains a JSX element.
func containsJSX(kinds *tsutil.KindTable, node *sitter.Node) bool {
	if node == nil {
		return false
	}
	switch kindOf(kinds, node) {
	case "jsx_element", "jsx_self_closing_element", "jsx_fragment":
		return true
	}
	for i := range node.ChildCount() {
		if containsJSX(kinds, node.Child(i)) {
			return true
		}
	}
	return false
}

// isComponentWrapper reports whether a call expression wraps a component, i.e. it
// calls memo / forwardRef (optionally as React.memo / React.forwardRef).
func isComponentWrapper(kinds *tsutil.KindTable, call *sitter.Node, src []byte) bool {
	fn := call.ChildByFieldName("function")
	if fn == nil {
		return false
	}
	name := ""
	switch kindOf(kinds, fn) {
	case "identifier":
		name = nodeText(fn, src)
	case "member_expression":
		if prop := fn.ChildByFieldName("property"); prop != nil {
			name = nodeText(prop, src)
		}
	}
	return name == "memo" || name == "forwardRef"
}

func findChildByKind(kinds *tsutil.KindTable, node *sitter.Node, kind string) *sitter.Node {
	if node == nil {
		return nil
	}
	for i := range node.ChildCount() {
		child := node.Child(i)
		if kindOf(kinds, child) == kind {
			return child
		}
	}
	return nil
}

func nodeText(node *sitter.Node, src []byte) string { return tsutil.Text(node, src) }

// tsAliasRoot is a directory (repoPath-relative, "" = root) and the alias
// map its tsconfig declares, already qualified with dir as a prefix.
// tsAlias is one tsconfig `paths` entry. The match mode is carried explicitly because
// the two forms cannot share prefix semantics: an exact entry `@acme/common` stored as a
// bare prefix would also match `@acme/common-utils` and draw edges into the wrong
// package. tsconfig itself distinguishes them — a pattern without `*` matches the whole
// specifier and nothing else.
type tsAlias struct {
	replacement string
	// suffix is what follows the `*` in the target, for a mapping whose replacement
	// does not end at the wildcard: `"@acme/ui/*": ["./packages/ui/*/src/index.ts"]`
	// resolves `@acme/ui/forms` to `packages/ui/forms/src/index.ts`, not to
	// `packages/ui/forms`. Empty for the ordinary `["./src/*"]` shape.
	suffix string
	exact  bool
}

type tsAliasRoot struct {
	dir     string
	aliases map[string]tsAlias
}

// collectTSAliasRoots finds every directory whose tsconfig.json (or
// tsconfig.base.json) declares path aliases — unlike findTSRoot, which stops
// at the first match, this covers monorepos with one tsconfig per package.
func collectTSAliasRoots(repoPath string) []tsAliasRoot {
	maxDepth := 2
	if isDeepNestedProject(repoPath) {
		maxDepth = 8
	}
	var roots []tsAliasRoot
	walkTSAliasRoots(repoPath, repoPath, 0, maxDepth, &roots)
	return roots
}

func walkTSAliasRoots(repoPath, dir string, depth, maxDepth int, out *[]tsAliasRoot) {
	if aliases, ok := aliasesAtDir(dir); ok {
		rel, err := filepath.Rel(repoPath, dir)
		if err != nil || rel == "." {
			rel = ""
		}
		rel = factpath.Slash(rel)

		// Concatenation, not filepath.Join, to preserve the trailing slash
		// resolveImportPath's `replacement + rest` depends on.
		//
		// Exact entries are qualified the same way: packages/tsconfig.base.json declares
		// "@excalidraw/common": ["./common/src/index.ts"], which means
		// packages/common/src/index.ts. Skipping this resolved one directory too high.
		qualified := make(map[string]tsAlias, len(aliases))
		for prefix, a := range aliases {
			if rel != "" {
				a.replacement = rel + "/" + a.replacement
			}
			qualified[prefix] = a
		}
		*out = append(*out, tsAliasRoot{dir: rel, aliases: qualified})
	}
	if depth >= maxDepth {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") || tsSkipDirs[entry.Name()] {
			continue
		}
		walkTSAliasRoots(repoPath, filepath.Join(dir, entry.Name()), depth+1, maxDepth, out)
	}
}

// aliasesAtDir tries tsconfig.json then tsconfig.base.json at dir, returning
// the first one that declares a non-empty paths map.
func aliasesAtDir(dir string) (map[string]tsAlias, bool) {
	for _, name := range []string{"tsconfig.json", "tsconfig.base.json"} {
		if aliases, ok := tryParseTSConfigAliases(filepath.Join(dir, name)); ok {
			return aliases, true
		}
	}
	return nil, false
}

// aliasesForDir returns the alias map of the root whose dir is the longest
// matching ancestor-or-equal prefix of dir, or nil if none match.
func aliasesForDir(roots []tsAliasRoot, dir string) map[string]tsAlias {
	dir = filepath.ToSlash(dir)
	var best *tsAliasRoot
	bestLen := -1
	for i := range roots {
		r := &roots[i]
		if r.dir != "" && dir != r.dir && !strings.HasPrefix(dir, r.dir+"/") {
			continue
		}
		if len(r.dir) > bestLen {
			best = r
			bestLen = len(r.dir)
		}
	}
	if best == nil {
		return nil
	}
	return best.aliases
}

// tryParseTSConfigAliases reads path alias mappings from a tsconfig.json,
// e.g. "@/*": ["./src/*"] maps prefix "@/" to replacement "src/". ok is
// false if the file and its relative extends chain are missing/invalid or declare
// no usable paths. Targets are rebased to the child config's directory so the caller
// can qualify them exactly as it qualifies aliases declared directly in that child.

type tsConfigAliasFile struct {
	Extends         string `json:"extends"`
	CompilerOptions struct {
		// BaseUrl is what a non-relative `paths` target is resolved against.
		// TypeScript resolves `"@common/*": ["common/*"]` under a `baseUrl` of
		// "./src" to src/common/*, and ignoring it resolved one directory too
		// high — which in one workspace meant every aliased import in the
		// application resolved to nothing, and with it every module composition
		// edge those imports carry.
		BaseURL string              `json:"baseUrl"`
		Paths   map[string][]string `json:"paths"`
	} `json:"compilerOptions"`
}

// stripJSONC removes comments and trailing commas without touching comment-looking
// text inside strings. tsconfig.json is JSONC in practice; using encoding/json directly
// made a perfectly ordinary commented config indistinguishable from a missing one.
func stripJSONC(data []byte) []byte {
	out := make([]byte, 0, len(data))
	quote, escaped := byte(0), false
	for i := 0; i < len(data); i++ {
		c := data[i]
		if quote != 0 {
			out = append(out, c)
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == quote {
				quote = 0
			}
			continue
		}
		if c == '"' || c == '\'' || c == '`' {
			quote = c
			out = append(out, c)
			continue
		}
		if c == '/' && i+1 < len(data) && data[i+1] == '/' {
			i += 2
			for i < len(data) && data[i] != '\n' && data[i] != '\r' {
				i++
			}
			if i < len(data) {
				out = append(out, data[i])
			}
			continue
		}
		if c == '/' && i+1 < len(data) && data[i+1] == '*' {
			i += 2
			for i+1 < len(data) && (data[i] != '*' || data[i+1] != '/') {
				i++
			}
			i++
			continue
		}
		if c == ',' {
			j := i + 1
			for j < len(data) && (data[j] == ' ' || data[j] == '\t' || data[j] == '\n' || data[j] == '\r') {
				j++
			}
			if j < len(data) && (data[j] == '}' || data[j] == ']') {
				continue
			}
		}
		out = append(out, c)
	}
	return out
}

func readTSConfig(path string) (tsConfigAliasFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return tsConfigAliasFile{}, err
	}
	var config tsConfigAliasFile
	if err := json.Unmarshal(stripJSONC(data), &config); err != nil {
		return tsConfigAliasFile{}, err
	}
	return config, nil
}

func resolveTSConfigExtends(from, spec string) string {
	if spec == "" || (!strings.HasPrefix(spec, ".") && !filepath.IsAbs(spec)) {
		return ""
	}
	p := spec
	if !filepath.IsAbs(p) {
		p = filepath.Join(filepath.Dir(from), filepath.FromSlash(p)) //factpath:host
	}
	if filepath.Ext(p) == "" {
		p += ".json"
	}
	return filepath.Clean(p) //factpath:host
}

func tryParseTSConfigAliases(tsconfigPath string) (map[string]tsAlias, bool) {
	originDir := filepath.Dir(tsconfigPath) //factpath:host
	path := filepath.Clean(tsconfigPath)    //factpath:host
	seen := map[string]bool{}
	for depth := 0; depth < 32; depth++ {
		if seen[path] {
			return nil, false
		}
		seen[path] = true
		config, err := readTSConfig(path)
		if err != nil {
			return nil, false
		}
		// TypeScript replaces (rather than merges) an inherited paths map. The first
		// declaration while walking child to parent is therefore the effective one.
		if config.CompilerOptions.Paths != nil {
			return parseTSConfigAliases(config, path, originDir)
		}
		path = resolveTSConfigExtends(path, strings.TrimSpace(config.Extends))
		if path == "" {
			return nil, false
		}
	}
	return nil, false
}

func parseTSConfigAliases(config tsConfigAliasFile, declaringPath, originDir string) (map[string]tsAlias, bool) {
	declaringDir := filepath.Dir(declaringPath) //factpath:host
	base := strings.TrimSpace(config.CompilerOptions.BaseURL)
	if base == "" {
		base = "."
	}
	absoluteBase := filepath.Clean(filepath.Join(declaringDir, filepath.FromSlash(base))) //factpath:host
	toOrigin := func(target string) string {
		target = strings.TrimSpace(target)
		root := absoluteBase
		if strings.HasPrefix(target, "./") || strings.HasPrefix(target, "../") {
			root = declaringDir
		}
		abs := filepath.Clean(filepath.Join(root, filepath.FromSlash(target))) //factpath:host
		rel, err := filepath.Rel(originDir, abs)
		if err != nil {
			return filepath.ToSlash(abs)
		}
		return filepath.ToSlash(rel)
	}

	aliases := make(map[string]tsAlias)
	for pattern, targets := range config.CompilerOptions.Paths {
		if len(targets) == 0 {
			continue
		}
		target := toOrigin(targets[0])
		switch {
		case strings.HasSuffix(pattern, "*") && strings.Contains(target, "*"):
			prefix := strings.TrimSuffix(pattern, "*")
			head, tail, _ := strings.Cut(target, "*")
			aliases[prefix] = tsAlias{replacement: head, suffix: tail}
		case !strings.HasSuffix(pattern, "*") && !strings.HasSuffix(target, "*"):
			aliases[pattern] = tsAlias{replacement: target, exact: true}
		}
	}
	return aliases, len(aliases) > 0
}

// resolveImportPath normalizes a TypeScript import path to a filesystem-relative path.
// It handles path aliases (@/), relative imports (./), and identifies external packages.
//
// When several aliases match, the LONGEST prefix wins. Two rules in one:
//
//   - Correctness. tsconfig `paths` resolution is most-specific-first, so a project
//     mapping both "@acme/schema" and "@acme/" means the former for "@acme/schema/x".
//     Taking any match resolved such imports to the wrong module.
//   - Determinism. This used to `range` the alias map and return on first match. Go
//     randomizes map iteration, so on a monorepo that maps a package BOTH to its source
//     and to its built output, the same import resolved to a different module on
//     different runs — and the snapshot stopped being reproducible. Measured on a
//     15k-file monorepo: 2 facts of 163,582 flipped between runs, which was enough to
//     make `enola check` report `edges +6/-6` on a tree nobody had touched. A delta
//     tool that invents churn is worse than one that is merely incomplete, because the
//     churn is indistinguishable from a real change.
//
// Ties are broken on the prefix string so the result is a total order, not merely a
// less-arbitrary one.
func resolveImportPath(importPath, fileDir string, aliases map[string]tsAlias) (string, bool) {
	// An exact entry wins outright. tsconfig resolves most-specific-first and a pattern
	// with no `*` matches the whole specifier and nothing else, so it cannot be ranked
	// against prefixes by length — `@acme/common` (exact) and `@acme/` (prefix) both
	// match `@acme/common`, and the exact one is the answer.
	if a, ok := aliases[importPath]; ok && a.exact {
		return factpath.Clean(a.replacement), false
	}

	bestPrefix, bestAlias := "", tsAlias{}
	for prefix, a := range aliases {
		if a.exact || !strings.HasPrefix(importPath, prefix) {
			continue
		}
		if len(prefix) > len(bestPrefix) || (len(prefix) == len(bestPrefix) && prefix < bestPrefix) {
			bestPrefix, bestAlias = prefix, a
		}
	}
	if bestPrefix != "" {
		rest := strings.TrimPrefix(importPath, bestPrefix)
		return factpath.Clean(bestAlias.replacement + rest + bestAlias.suffix), false
	}

	// Relative imports
	if strings.HasPrefix(importPath, ".") {
		resolved := factpath.Clean(factpath.Join(fileDir, importPath))
		return resolved, false
	}

	// Everything else is external (react, next/image, @tanstack/react-query, etc.)
	return importPath, true
}

// tsModuleExts are the source extensions a bare import path may resolve to, tried in
// TS-before-JS order (a project with both prefers the typed file).
var tsModuleExts = []string{".ts", ".tsx", ".js", ".jsx", ".mjs", ".vue", ".svelte", ".gts", ".gjs"}

// resolveModuleFile resolves an extensionless internal import path to the actual
// source file backing it, using the set of known indexed files. It returns the
// resolved file path, the directory that owns its symbols, and whether a match was
// found. A file module (`./utils` → utils.ts) owns symbols under its PARENT dir (the
// symbol naming convention "<dir>.<sym>" uses filepath.Dir of the file); a folder
// module (`./feed_item` → feed_item/index.tsx) owns symbols under the folder itself.
// This is what lets a default import bind to the folder-index default export, whose
// name (fileSymbolName → "<Folder>Index") is otherwise unmatchable.
func resolveModuleFile(resolved string, knownFiles map[string]bool) (indexPath, dir string, ok bool) {
	resolved = filepath.ToSlash(resolved)
	// The path may already name a file. A tsconfig exact alias maps a bare package
	// specifier straight onto its entry point — `"@acme/ui": ["./packages/ui/src/
	// index.ts"]` — so the resolved path carries its extension, and appending another
	// one matched nothing. Every caller then read the import as unresolvable: in one
	// workspace that was every lazily-loaded feature module in the application.
	if knownFiles[resolved] {
		return resolved, factpath.Dir(resolved), true
	}
	for _, ext := range tsModuleExts {
		if knownFiles[resolved+ext] {
			return resolved + ext, factpath.Dir(resolved), true
		}
	}
	for _, ext := range tsModuleExts {
		if idx := resolved + "/index" + ext; knownFiles[idx] {
			return idx, resolved, true
		}
	}
	return "", "", false
}

// buildImportBindings returns a map of locally-bound import name → canonical symbol
// fact name for named imports. It lets bare calls to imported functions (e.g.
// `formatName()`) resolve to the callee's declaration fact. Symbols declared in an
// imported module are named "<moduleDir>.<exportName>", where moduleDir is the
// directory of the resolved module file — this matches the common file-module case
// (e.g. import "./utils" → utils.ts → "<dir>.foo").
//
// A name imported from a package is named "<specifier>.<exportName>"
// (`@acme/sdk.Client`), the way the source spells it. The package may be another
// loaded repository, whose facts carry their own names for it; the import-aliases
// binder resolves the one to the other. Left unmapped, the call fell through to the
// same-directory spelling, which named a symbol of the importing file's own module.
//
// The second map holds the local names bound from a module of this repository (a
// relative, aliased or baseUrl path) rather than from a package, each qualified:
// the ones whose members can name this repository's symbols. A default import is
// qualified by its local name, the name a default-exported declaration nearly
// always carries; it is left out of the import map, where it would change what a
// bare call to it resolves to.
func buildImportBindings(kinds *tsutil.KindTable, root *sitter.Node, src []byte, relFile string, aliases map[string]tsAlias, files *tsFileIndex) (map[string]string, map[string]string) {
	fileDir := factpath.Dir(relFile)
	m := make(map[string]string)
	internal := make(map[string]string)
	for i := range root.ChildCount() {
		child := root.Child(i)
		if kindOf(kinds, child) != "import_statement" {
			continue
		}
		source := findChildByKind(kinds, child, "string")
		if source == nil {
			continue
		}
		importPath := strings.Trim(nodeText(source, src), `"'`)
		resolved, isExternal := resolveImportPath(importPath, fileDir, aliases)
		qualifier := factpath.Dir(resolved)
		fromRepo := !isExternal
		if isExternal {
			// A framework alias or a baseUrl path is a file of this repository that
			// resolveImportPath could not see; anything else is a package.
			if file, ok := files.resolve(importPath, relFile); ok {
				qualifier = factpath.Dir(file)
				fromRepo = true
			} else {
				qualifier = importPath
			}
		}

		clause := findChildByKind(kinds, child, "import_clause")
		if clause == nil {
			continue
		}
		if def := findChildByKind(kinds, clause, "identifier"); def != nil && fromRepo {
			local := nodeText(def, src)
			internal[local] = qualifier + "." + local
			// A default import is bound under its local name in the module it comes
			// from. The exported declaration is not looked up, so this is right
			// where the import keeps the declaration's name (`import callApi from
			// './callApi'`), which is the convention, and names nothing otherwise.
			// Unbound, a call to it fell through to "a function of this directory",
			// which is right only when the two files are siblings: across
			// directories the edge named a symbol that does not exist.
			if _, bound := m[local]; !bound {
				m[local] = qualifier + "." + local
			}
		}
		// `import * as api from '$lib/api'`: a member of the namespace is the module's
		// export of that name, so `api.get()` is "<moduleDir>.get". Only the member
		// roots carry it; the namespace itself is no symbol to call.
		if ns := findChildByKind(kinds, clause, "namespace_import"); ns != nil && fromRepo {
			if id := findChildByKind(kinds, ns, "identifier"); id != nil {
				internal[nodeText(id, src)] = qualifier
			}
		}
		named := findChildByKind(kinds, clause, "named_imports")
		if named == nil {
			continue // a default import is bound above, a namespace import in internal
		}
		for j := range named.ChildCount() {
			spec := named.Child(j)
			if kindOf(kinds, spec) != "import_specifier" {
				continue
			}
			nameNode := spec.ChildByFieldName("name")
			if nameNode == nil {
				continue
			}
			exportName := nodeText(nameNode, src)
			local := exportName
			if aliasNode := spec.ChildByFieldName("alias"); aliasNode != nil {
				local = nodeText(aliasNode, src)
			}
			m[local] = qualifier + "." + exportName
			if fromRepo {
				internal[local] = m[local]
			}
		}
	}
	return m, internal
}

// memberCallRoots returns the names `<name>.<member>()` resolves through, each
// mapped to its qualified name: a name imported from this repository, and a
// top-level declaration of this file whose value holds object-member functions
// (see objectMemberSymbols). Anything else, a package import above all, is left out:
// its members name no fact, and resolving them would only add dangling edges.
func memberCallRoots(kinds *tsutil.KindTable, root *sitter.Node, src []byte, dir string, internal map[string]string) map[string]string {
	roots := map[string]string{}
	for local, qualified := range internal {
		roots[local] = qualified
	}
	for i := range root.ChildCount() {
		stmt := root.Child(i)
		if kindOf(kinds, stmt) == "export_statement" {
			if d := stmt.ChildByFieldName("declaration"); d != nil {
				stmt = d
			}
		}
		if k := kindOf(kinds, stmt); k != "lexical_declaration" && k != "variable_declaration" {
			continue
		}
		for j := range stmt.ChildCount() {
			decl := stmt.Child(j)
			if kindOf(kinds, decl) != "variable_declarator" {
				continue
			}
			name := findChildByKind(kinds, decl, "identifier")
			if name != nil && hasObjectMembers(kinds, decl.ChildByFieldName("value"), src) {
				roots[nodeText(name, src)] = dir + "." + nodeText(name, src)
			}
		}
	}
	return roots
}

// tsTestSuffixes are the co-located TypeScript test/spec suffixes that
// config.Default().TestGlobs matches. Kept in one place so isTSTestFile and any
// future glob check agree.
var tsTestSuffixes = []string{".test.ts", ".test.tsx", ".spec.ts", ".spec.tsx"}

// emberTestSuffixes are Ember's hyphenated test suffixes, valid ONLY under a
// tests/ directory segment — ember-cli generates and qunit discovers
// tests/**/*-test.*, but a bare hyphenated suffix outside tests/ can collide
// with production code (an experimentation util named ab-test.ts). Mirrors
// config.Default().TestGlobs' "**/tests/**/*-test.*" entries.
var emberTestSuffixes = []string{"-test.ts", "-test.js", "-test.gts", "-test.gjs"}

// isTSTestFile reports whether a repo-relative path is a TypeScript test/spec file.
// The dotted convention reserves its suffixes everywhere, so — like Go's
// *_test.go — no production file can legally collide; the Ember convention is
// reserved only inside tests/, so the directory is demanded there.
func isTSTestFile(relFile string) bool {
	for _, suffix := range tsTestSuffixes {
		if strings.HasSuffix(relFile, suffix) {
			return true
		}
	}
	for _, suffix := range emberTestSuffixes {
		if strings.HasSuffix(relFile, suffix) {
			for _, seg := range strings.Split(filepath.ToSlash(relFile), "/") {
				if seg == "tests" {
					return true
				}
			}
		}
	}
	return false
}

// ExtractTestRefs implements plugin.TestRefExtractor. It parses *.test.ts(x) /
// *.spec.ts(x) files for the SOLE purpose of capturing their outbound references
// into production code, emitting one facts.KindTestRef fact per file that carries
// only RelCalls edges — no symbols. A production symbol exercised only by its test
// therefore keeps an incoming edge and is no longer mis-reported as dead, while no
// symbol/module/route explainer is affected.
//
// tsextractor already implements plugin.FileOwner (for production caching), so
// runTestRefExtractors scopes the handoff to isTypeScriptFile — which owns non-test
// TS files too. We therefore still filter to test paths here, exactly as
// goextractor and rubyextractor do.
//
// Targets are resolved with the SAME production resolvers the file-ref pass uses
// (collectTSFileRefs → resolveImportPath / resolveLocalOrImport / resolveJSXTag), so
// every target is fully qualified ("<dir>.<name>") and orphans' lastSeg folding
// stays a safety net rather than the primary match — emitting bare names instead
// would rescue unrelated same-named symbols. knownFiles is left nil: the resolver
// then degrades to short-name matching (its documented conservative bias — only
// ever ADDS references), which is exactly how the dead-code detector matches. Path
// aliases ARE reconstructed so "@/…"-imported helpers still resolve.
// prodFiles is unused: knownFiles is deliberately left nil (see above), so this
// pass has nothing to check a production file set against.
func (e *TSExtractor) ExtractTestRefs(ctx context.Context, repoPath string, files, _ []string) ([]facts.Fact, error) {
	var testFiles []string
	for _, relFile := range files {
		if isTSTestFile(relFile) {
			testFiles = append(testFiles, relFile)
		}
	}
	if len(testFiles) == 0 {
		return nil, nil
	}

	aliasRoots := collectTSAliasRoots(repoPath)

	perFile := parallel.MapFiles(ctx, testFiles, func(relFile string) []facts.Fact {
		src, err := os.ReadFile(filepath.Join(repoPath, relFile))
		if err != nil {
			log.Printf("[ts-extractor] error reading test file %s: %v", relFile, err)
			return nil
		}
		if isMinifiedSource(src) {
			return nil
		}
		return e.testRefsFromFile(src, relFile, aliasesForDir(aliasRoots, factpath.Dir(relFile)))
	})

	var out []facts.Fact
	for _, ff := range perFile {
		out = append(out, ff...)
	}
	return out, nil
}

// testRefsFromFile parses one TS test file and returns its single KindTestRef fact
// (or nil when it references nothing), reusing collectTSFileRefs' walk. Only the
// fields collectTSFileRefs reads are populated on the ctx; knownFiles is left nil
// (see ExtractTestRefs).
func (e *TSExtractor) testRefsFromFile(src []byte, relFile string, aliases map[string]tsAlias) []facts.Fact {
	isTSX := strings.HasSuffix(relFile, ".tsx") || strings.HasSuffix(relFile, ".jsx")
	kinds := tsKindsFor(isTSX)

	lang := typescript.LanguageTypescript()
	if isTSX {
		lang = typescript.LanguageTSX()
	}

	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(lang)); err != nil {
		return nil
	}
	tree := parser.Parse(src, nil)
	defer tree.Close()

	ctx := &extractCtx{
		src:     src,
		relFile: relFile,
		dir:     factpath.Dir(relFile),
		isTSX:   isTSX,
	}
	return e.collectTSFileRefs(kinds, tree.RootNode(), ctx, aliases, facts.KindTestRef)
}

// resolveLocalOrImport resolves a bare name used in a value/call position to its
// canonical symbol target: the imported binding when it is one, otherwise the
// same-module declaration "<dir>.<name>". A name that matches neither yields a target
// no symbol fact will match (harmless), so this only ever adds real references.
func resolveLocalOrImport(name, dir string, internal map[string]string) string {
	if t, ok := internal[name]; ok {
		return t
	}
	return dir + "." + name
}

// resolveJSXTag resolves a JSX element's tag name to a canonical symbol target, or ""
// when it is a host element (`<div>`) or an unresolvable/external component. A bare
// PascalCase tag that is not an import is resolved same-module (`<dir>.<Name>`) so a
// component rendered only by a sibling in the same file is not flagged dead.
func resolveJSXTag(kinds *tsutil.KindTable, nameNode *sitter.Node, src []byte, dir string, internal, namespaces map[string]string) string {
	switch kindOf(kinds, nameNode) {
	case "identifier":
		name := nodeText(nameNode, src)
		if t, ok := internal[name]; ok {
			return t
		}
		if isComponentName(name) {
			return dir + "." + name
		}
	case "member_expression", "nested_identifier":
		// <Foo.Bar/> — resolve via the root object Foo.
		obj := nameNode.ChildByFieldName("object")
		if obj == nil && nameNode.ChildCount() > 0 {
			obj = nameNode.Child(0)
		}
		if obj != nil {
			root := nodeText(obj, src)
			if d, ok := namespaces[root]; ok {
				if prop := nameNode.ChildByFieldName("property"); prop != nil {
					return d + "." + nodeText(prop, src)
				}
			}
			if t, ok := internal[root]; ok {
				return t
			}
		}
	}
	return ""
}

// tsBodyMetrics accumulates per-function complexity signals during the single
// body walk — mirrors the Go/Python/Ruby/Swift/Kotlin extractors.
type tsBodyMetrics struct {
	loopDepth          int             // max loop nesting depth
	scalingLoopDepth   int             // max nesting counting only unbounded (input-scaling) loops
	loopCount          int             // number of loop constructs (syntactic + array-method callbacks)
	decisions          int             // decision points (cyclomatic = 1 + decisions)
	callsInLoop        []string        // distinct call targets invoked at loop depth >= 1
	inLoopSeen         map[string]bool // dedup set for callsInLoop
	callsInScalingLoop []string        // distinct call targets invoked at scaling (unbounded) depth >= 1
	inScalingSeen      map[string]bool // dedup set for callsInScalingLoop
	scalingCallDepth   map[string]int  // deepest scaling nesting each of those is called at (calldepth.go)
	// The cross-call hierarchical rule (hierarchy.go): the calls handed a loop
	// element with the argument position that carries it, and the parameters a
	// scaling loop of this body walks.
	callsOnLoopElement    []string
	callsOnLoopElementArg []int
	onElementSeen         map[string]bool
	loopsOverParam        map[int]bool
	recursive             bool            // body directly calls the enclosing function
	ioDirect              bool            // body directly invokes a network/file I/O primitive
	ioCalls               []string        // the resolved calls that are themselves the round trip
	ioNamedCalls          []string        // resolved calls that are I/O by method name alone, settled per repo
	fieldsWritten         []string        // distinct `this.<name>` targets the body assigns to
	writeSeen             map[string]bool // dedup set for fieldsWritten
}

// tsIterators are array/collection methods whose callback runs once per element —
// i.e. a loop. A function/arrow argument to a method NOT in this set (setTimeout,
// addEventListener, .then/.catch, JSX event handlers, useEffect) runs once or
// later and is not treated as a loop. Aggregate-or-iterate names (some/every/find)
// are safe to include because a callback must be present before any counts.
var tsIterators = map[string]bool{
	"map": true, "forEach": true, "filter": true, "flatMap": true,
	"reduce": true, "reduceRight": true, "some": true, "every": true,
	"find": true, "findIndex": true, "findLast": true, "findLastIndex": true,
	"sort": true, "flat": true, "group": true, "partition": true,
}

// tsCheapMethods are obviously-cheap methods that are not I/O. No-arg-ish method
// calls to these inside loops are not recorded in calls_in_loop, keeping it focused
// (the analyzer's keyword gate is the real precision filter).
var tsCheapMethods = map[string]bool{
	"toString": true, "push": true, "pop": true, "shift": true, "unshift": true,
	"slice": true, "splice": true, "join": true, "concat": true, "includes": true,
	"indexOf": true, "length": true, "trim": true, "split": true, "replace": true,
	"keys": true, "values": true, "entries": true, "then": true, "catch": true,
	"finally": true, "bind": true, "call": true, "apply": true, "has": true,
	"get": true, "set": true, "add": true, "delete": true, "toFixed": true,
	"map": true, "forEach": true, "filter": true, "reduce": true, "sort": true,
	"toLowerCase": true, "toUpperCase": true, "toLocaleLowerCase": true,
	"toLocaleUpperCase": true, "startsWith": true, "endsWith": true,
	"charAt": true, "padStart": true, "padEnd": true, "repeat": true,
	// Date / number formatters — cheap per-row work, not I/O.
	"toLocaleDateString": true, "toLocaleString": true, "toLocaleTimeString": true,
	"toISOString": true, "getTime": true, "getFullYear": true, "getMonth": true,
	"getDate": true, "getHours": true, "getMinutes": true, "getDay": true,
}

// --- Network/file I/O primitive detection (seeds the performs_io closure) ---
//
// A body is tagged io_direct when it directly invokes an I/O primitive. Detection
// is syntactic (no type inference), matching three shapes: a bare global callee
// (fetch), a member call on a known receiver (axios.get, fs.readFile), or a call to
// a local binding imported from a network module (e.g. a `request` helper default-
// exported from a `.../lib/network/request` module). The io_direct flag is then
// propagated transitively into performs_io by ioclosure.Propagate, mirroring Swift.

// tsIOCallNames are bare-identifier callees that are unambiguous network/file I/O.
var tsIOCallNames = map[string]bool{
	"fetch": true, "axios": true, "importScripts": true,
}

// tsIOReceivers are receiver root tokens whose method calls are I/O regardless of the
// method name (axios.get, http.request, https.get, fs.readFile). Matched against the
// first dotted segment of the receiver, so `fs.promises.readFile` still matches on `fs`.
var tsIOReceivers = map[string]bool{
	"axios": true, "http": true, "https": true, "fs": true,
}

// tsIOMemberMethods are method names that denote I/O regardless of receiver.
var tsIOMemberMethods = map[string]bool{
	"sendBeacon": true, // navigator.sendBeacon

	// ORM query methods (Prisma, TypeORM, Drizzle) — a real DB round-trip.
	//
	// These seed io_direct, which ioclosure.Propagate then propagates transitively into
	// performs_io. That is the whole point: a direct in-loop `prisma.post.findMany()` was
	// already caught by the analyzer's own name list, but a REPOSITORY WRAPPER
	// around it was not — the wrapper invokes no network primitive, so it was never
	// io_direct, so it was never performs_io, so a per-iteration call to it was invisible.
	//
	// The names mirror perf.tsExpensiveMethods exactly, and for the same reason: they are
	// distinctive multi-word ORM names. The generic single verbs (find/fetch/update/save)
	// are deliberately ABSENT — frontend TS reuses them for in-memory helpers
	// (updateState, findIndex, getFetchAllUpdate), and since almost everything in a
	// frontend module is exported, tagging those as I/O floods the high-severity bucket
	// with false N+1s. That false-positive class is exactly what the TS detector was
	// narrowed to avoid; do not widen this list to the generic verbs.
	"findMany": true, "findFirst": true, "findUnique": true, "findOne": true,
	"createMany": true, "updateMany": true, "deleteMany": true,
	"aggregate": true, "queryRaw": true, "executeRaw": true,
}

// tsIOConstructors are constructor names (new X()) that open a network/stream resource.
var tsIOConstructors = map[string]bool{
	"WebSocket": true, "XMLHttpRequest": true, "EventSource": true,
}

// tsIONetworkPackages are npm packages that are HTTP clients — a call to a binding
// imported from one of these is I/O. Matched against the first path segment (the
// package name), so `got`/`ky` cannot false-match `forgot`/`sky`.
var tsIONetworkPackages = map[string]bool{
	"axios": true, "node-fetch": true, "cross-fetch": true, "isomorphic-fetch": true,
	"ky": true, "got": true, "superagent": true, "undici": true, "phin": true,
}

// tsNonClientModuleBasenames are submodule leaf names that carry types/constants/pure
// helpers rather than a request function, so a `.../network/types` or `.../network/utils`
// path must NOT qualify as an I/O sink even though it has a `network` segment (e.g. a
// redux-tools `network/types` module exports pure action-status helpers like `resolved`).
var tsNonClientModuleBasenames = map[string]bool{
	"types": true, "type": true, "constants": true, "const": true,
	"errors": true, "error": true, "utils": true, "util": true,
	"helpers": true, "helper": true, "config": true, "schema": true, "middleware": true,
}

// tsIsNetworkModule reports whether an import path denotes a network client — either a
// known HTTP-client package, or a path with a `network` segment (which catches an
// internal `.../lib/network/request` request helper). Paths whose leaf is a types/utils/
// constants submodule are excluded — they carry pure helpers, not a request function.
// Segment/exact matching only, never substring, to avoid collisions.
func tsIsNetworkModule(importPath string) bool {
	segs := strings.Split(importPath, "/")
	if len(segs) == 0 {
		return false
	}
	if tsNonClientModuleBasenames[segs[len(segs)-1]] {
		return false
	}
	if tsIONetworkPackages[segs[0]] {
		return true
	}
	for _, s := range segs {
		if s == "network" {
			return true
		}
	}
	return false
}

// buildIOImportBindings returns the set of local names bound to the DEFAULT or NAMESPACE
// import of a network module — the request-function pattern `import request from
// '.../network/request'` or `import * as net from '.../network'`. A call to one of these
// names, or a member call on one, is treated as direct I/O. Named imports are
// deliberately NOT bound: a network barrel/types module exports pure helpers, error
// classes, and action-status utilities (`resolved`, `NetworkError`, `assignPaginationDefaults`)
// alongside any request function, and binding those mislabels every caller as doing I/O.
// Sibling to buildImportBindings, which drops external and default imports entirely.
func buildIOImportBindings(kinds *tsutil.KindTable, root *sitter.Node, src []byte) map[string]bool {
	bindings := make(map[string]bool)
	for i := range root.ChildCount() {
		child := root.Child(i)
		if kindOf(kinds, child) != "import_statement" {
			continue
		}
		source := findChildByKind(kinds, child, "string")
		if source == nil {
			continue
		}
		if !tsIsNetworkModule(strings.Trim(nodeText(source, src), `"'`)) {
			continue
		}
		clause := findChildByKind(kinds, child, "import_clause")
		if clause == nil {
			continue
		}
		for j := range clause.ChildCount() {
			spec := clause.Child(j)
			switch kindOf(kinds, spec) {
			case "identifier": // default import: import request from '...'
				bindings[nodeText(spec, src)] = true
			case "namespace_import": // import * as net from '...'
				if id := findChildByKind(kinds, spec, "identifier"); id != nil {
					bindings[nodeText(id, src)] = true
				}
			}
		}
	}
	return bindings
}

// tsCalleeRoot returns the first dotted segment of a member-expression receiver text
// (`fs.promises` → `fs`, `axios` → `axios`), for matching against tsIOReceivers / bindings.
func tsCalleeRoot(recv string) string {
	if i := strings.IndexByte(recv, '.'); i >= 0 {
		return recv[:i]
	}
	return recv
}

// tsIsIOCall reports whether a call_expression directly performs network/file I/O:
// a bare global primitive (fetch) or network-imported binding (request); or a member
// call on a known I/O receiver (axios.get, fs.readFile) or a network-imported binding.
// A call that is I/O by its method name alone is tsIsIONamedCall.
func tsIsIOCall(kinds *tsutil.KindTable, call *sitter.Node, src []byte, ioBindings map[string]bool) bool {
	fn := call.ChildByFieldName("function")
	if fn == nil {
		return false
	}
	if kindOf(kinds, fn) == "identifier" {
		name := nodeText(fn, src)
		return tsIOCallNames[name] || ioBindings[name]
	}
	if recv, prop := tsMemberCall(kinds, call, src); prop != "" {
		root := tsCalleeRoot(recv)
		return tsIOReceivers[root] || ioBindings[root]
	}
	return false
}

// tsIsIONamedCall reports whether a member call is I/O by its method name alone
// (navigator.sendBeacon, repo.findMany).
func tsIsIONamedCall(kinds *tsutil.KindTable, call *sitter.Node, src []byte) bool {
	_, prop := tsMemberCall(kinds, call, src)
	return tsIOMemberMethods[prop]
}

// tsIsFunctionLike reports whether a node introduces a function scope (a deferred
// body). Calls inside such a body are decoupled from the enclosing loops.
func tsIsFunctionLike(kind string) bool {
	switch kind {
	case "arrow_function", "function_expression", "function_declaration",
		"function", "generator_function", "generator_function_declaration",
		"method_definition":
		return true
	}
	return false
}

func tsBooleanOp(kinds *tsutil.KindTable, node *sitter.Node) bool {
	for i := range node.ChildCount() {
		switch kindOf(kinds, node.Child(i)) {
		case "&&", "||", "??":
			return true
		}
	}
	return false
}

func tsByteContains(outer, inner *sitter.Node) bool {
	return inner.StartByte() >= outer.StartByte() && inner.EndByte() <= outer.EndByte()
}

// tsIteratorCallback returns the function/arrow callback of an array-iterator call
// (items.map(cb), items.forEach(cb)) — or nil if the call is not an iterator with a
// closure argument.
func tsIteratorCallback(kinds *tsutil.KindTable, call *sitter.Node, src []byte) *sitter.Node {
	fn := call.ChildByFieldName("function")
	if fn == nil || kindOf(kinds, fn) != "member_expression" {
		return nil
	}
	prop := fn.ChildByFieldName("property")
	if prop == nil || !tsIterators[nodeText(prop, src)] {
		return nil
	}
	args := call.ChildByFieldName("arguments")
	if args == nil {
		return nil
	}
	for i := range args.ChildCount() {
		switch c := args.Child(i); kindOf(kinds, c) {
		case "arrow_function", "function_expression", "function":
			return c
		}
	}
	return nil
}

// tsMemberCall returns the receiver text and property name of a method call whose
// callee is a member_expression (`obj.method()` → "obj", "method"), for recording
// in-loop calls on unknown receivers. Returns "" property if not a member call.
func tsMemberCall(kinds *tsutil.KindTable, call *sitter.Node, src []byte) (recv, prop string) {
	fn := call.ChildByFieldName("function")
	if fn == nil || kindOf(kinds, fn) != "member_expression" {
		return "", ""
	}
	p := fn.ChildByFieldName("property")
	if p == nil {
		return "", ""
	}
	prop = nodeText(p, src)
	if o := fn.ChildByFieldName("object"); o != nil {
		recv = nodeText(o, src)
	}
	return recv, prop
}

// tsBodyWalker walks a function/method body once, collecting call-edge relations
// and (when metrics != nil) per-function complexity signals.
type tsBodyWalker struct {
	callScope
	selfName, selfShort string
	metrics             *tsBodyMetrics
	loopDepth           int
	scalingDepth        int // current nesting counting only input-scaling (unbounded) loops
	// repeatDepth counts the enclosing loops that run a non-constant number of times. It
	// differs from scalingDepth for `while (true)`, which adds no factor of n but whose
	// body still runs many times — so a query inside it is still an N+1 candidate.
	repeatDepth int
	// loopScopes are the enclosing loops' variables, for the hierarchical rule in
	// hierarchy.go.
	loopScopes []tsLoopScope
	// paramScope and paramIndex are the function's parameters, for the cross-call
	// rule in hierarchy.go.
	paramScope []tsLoopScope
	paramIndex map[string]int
	// constLocals are the locals proven to be literal collections (constbound.go).
	constLocals map[string]bool
	rels        []facts.Relation
	seen        map[string]bool
}

func (w *tsBodyWalker) recordCall(target string) {
	if w.metrics == nil || target == "" {
		return
	}
	if target == w.selfName || target == w.selfShort {
		w.metrics.recursive = true
	}
	w.recordInLoop(target)
}

// noteLoopOverParam records that a scaling loop of this body walks what the named
// parameter (or `this`) holds. root is "" when the loop walks something else.
func (w *tsBodyWalker) noteLoopOverParam(root string) {
	if w.metrics == nil || root == "" {
		return
	}
	i, ok := w.paramIndex[root]
	if !ok {
		return
	}
	if w.metrics.loopsOverParam == nil {
		w.metrics.loopsOverParam = make(map[int]bool)
	}
	w.metrics.loopsOverParam[i] = true
}

// noteCallOnLoopElement records a call, made inside a scaling loop, that hands its
// callee an element of an enclosing loop: as an argument, or as the receiver.
func (w *tsBodyWalker) noteCallOnLoopElement(call *sitter.Node, target string) {
	if w.metrics == nil || w.scalingDepth == 0 || len(w.loopScopes) == 0 || w.metrics.onElementSeen[target] {
		return
	}
	pos, found := 0, false
	if fn := call.ChildByFieldName("function"); fn != nil && kindOf(w.kinds, fn) == "member_expression" {
		if tsReachedThroughElement(w.kinds, fn.ChildByFieldName("object"), w.src, w.loopScopes) {
			pos, found = tsReceiverParam, true
		}
	}
	if args := call.ChildByFieldName("arguments"); !found && args != nil {
		for i := range args.NamedChildCount() {
			if tsReachedThroughElement(w.kinds, args.NamedChild(i), w.src, w.loopScopes) {
				pos, found = int(i), true
				break
			}
		}
	}
	if !found {
		return
	}
	if w.metrics.onElementSeen == nil {
		w.metrics.onElementSeen = make(map[string]bool)
	}
	w.metrics.onElementSeen[target] = true
	w.metrics.callsOnLoopElement = append(w.metrics.callsOnLoopElement, target)
	w.metrics.callsOnLoopElementArg = append(w.metrics.callsOnLoopElementArg, pos)
}

func (w *tsBodyWalker) recordInLoop(target string) {
	if w.metrics == nil || target == "" || w.loopDepth == 0 {
		return
	}
	if w.metrics.inLoopSeen == nil {
		w.metrics.inLoopSeen = make(map[string]bool)
	}
	if !w.metrics.inLoopSeen[target] {
		w.metrics.inLoopSeen[target] = true
		w.metrics.callsInLoop = append(w.metrics.callsInLoop, target)
	}
	// A call inside a loop that repeats a non-constant number of times is an N+1
	// candidate. Only a genuinely constant loop (a literal-receiver iterator, for..of over
	// an array literal) excludes its calls; `while (true)` repeats, so its calls stay
	// candidates even though its depth is discounted from the Big-O exponent.
	if w.repeatDepth > 0 {
		if w.metrics.inScalingSeen == nil {
			w.metrics.inScalingSeen = make(map[string]bool)
		}
		w.metrics.scalingCallDepth = noteCallDepth(w.metrics.scalingCallDepth, target, w.scalingDepth)
		if !w.metrics.inScalingSeen[target] {
			w.metrics.inScalingSeen[target] = true
			w.metrics.callsInScalingLoop = append(w.metrics.callsInScalingLoop, target)
		}
	}
}

func (w *tsBodyWalker) walk(n *sitter.Node) {
	if n == nil {
		return
	}
	kind := kindOf(w.kinds, n)

	// A nested function/arrow definition is a deferred scope: its body runs when the
	// function is called, NOT per-iteration of the enclosing loops — so reset the
	// loop depth for its subtree. This is what stops a React event handler defined
	// inside a `.map(...)` render callback (`onClick={() => handleDelete(x)}`) from
	// being mis-counted as a per-iteration call. The iterator's own callback is
	// handled separately in the call_expression branch (its body walks at +1).
	if w.metrics != nil && tsIsFunctionLike(kind) {
		w.walkDeferred(n)
		return
	}
	if kind == "variable_declarator" {
		w.bindDerivedLocal(n)
	}
	if w.metrics != nil {
		w.countDecision(kind, n)
	}

	switch kind {
	case "for_statement", "for_in_statement", "while_statement", "do_statement":
		w.walkLoop(n, kind)
		return
	case "call_expression":
		if w.walkCall(n) {
			return
		}
	case "new_expression":
		w.noteIOConstructor(n)
	case "member_expression":
		w.noteThisMember(n)
	}

	for i := range n.ChildCount() {
		w.walk(n.Child(i))
	}
}

func (w *tsBodyWalker) walkDeferred(n *sitter.Node) {
	saved, savedScaling, savedRepeat := w.loopDepth, w.scalingDepth, w.repeatDepth
	savedScopes := w.loopScopes
	w.loopDepth, w.scalingDepth, w.repeatDepth = 0, 0, 0
	w.loopScopes = nil
	for i := range n.ChildCount() {
		w.walk(n.Child(i))
	}
	w.loopDepth, w.scalingDepth, w.repeatDepth = saved, savedScaling, savedRepeat
	w.loopScopes = savedScopes
}

func (w *tsBodyWalker) bindDerivedLocal(n *sitter.Node) {
	// A local derived from the current element is still the current element:
	// `const needs = job.needs ?? []` puts `needs` one step from `job`.
	if len(w.loopScopes) > 0 && tsReachedThroughElement(w.kinds, n.ChildByFieldName("value"), w.src, w.loopScopes) {
		top := &w.loopScopes[len(w.loopScopes)-1]
		tsBindPattern(w.kinds, n.ChildByFieldName("name"), w.src, top.addElem)
	}
	// The same for a parameter: `const els = frame.elements ?? []` is still frame's.
	if len(w.paramScope) == 0 {
		return
	}
	if root := tsElementRoot(w.kinds, n.ChildByFieldName("value"), w.src, w.paramScope); root != "" {
		tsBindPattern(w.kinds, n.ChildByFieldName("name"), w.src, func(name string) {
			w.paramScope[0].addElem(name)
			w.paramIndex[name] = w.paramIndex[root]
		})
	}
}

// Complexity metrics: count decision points so the single body walk doubles as
// the cyclomatic pass.
func (w *tsBodyWalker) countDecision(kind string, n *sitter.Node) {
	switch kind {
	case "assignment_expression", "augmented_assignment_expression":
		w.metrics.noteFieldWrite(w.kinds, n, w.src)
	case "if_statement", "ternary_expression", "switch_case", "catch_clause":
		w.metrics.decisions++
	case "binary_expression":
		if tsBooleanOp(w.kinds, n) {
			w.metrics.decisions++
		}
	}
}

func (w *tsBodyWalker) noteLoop(scales bool) {
	if w.metrics == nil {
		return
	}
	w.metrics.loopCount++
	w.metrics.decisions++
	if w.loopDepth+1 > w.metrics.loopDepth {
		w.metrics.loopDepth = w.loopDepth + 1
	}
	if scales && w.scalingDepth+1 > w.metrics.scalingLoopDepth {
		w.metrics.scalingLoopDepth = w.scalingDepth + 1
	}
}

// Syntactic loops: everything in the body runs per iteration. A loop over a literal
// collection (for..of [a,b,c]) or an infinite while(true)/do..while(true) event/retry
// loop is bounded — it raises loop_depth but not scaling_loop_depth (the Big-O exponent).
func (w *tsBodyWalker) walkLoop(n *sitter.Node, kind string) {
	repeats := tsLoopRepeats(w.kinds, n, w.src, w.constLocals)
	bounded, scope := w.loopShape(n, kind, repeats)
	w.noteLoop(!bounded)
	// What the loop draws from is evaluated once, before the first iteration:
	// the collection of a for..of, the initializer of a C-style for. Walking it
	// with the body read `for (const s of loadSpecs())` as a call per spec, and
	// an iterator in the header as one more level of nesting.
	var once *sitter.Node
	switch kind {
	case "for_in_statement":
		once = n.ChildByFieldName("right")
	case "for_statement":
		once = n.ChildByFieldName("initializer")
	}
	if once != nil {
		w.walk(once)
	}
	w.enterLoop(!bounded, repeats, scope)
	for i := range n.ChildCount() {
		if c := n.Child(i); once == nil || c.StartByte() != once.StartByte() || c.EndByte() != once.EndByte() {
			w.walk(c)
		}
	}
	w.leaveLoop(!bounded, repeats)
}

// A loop over the children of the element an enclosing loop is on adds no
// factor of n (hierarchy.go). It still repeats.
func (w *tsBodyWalker) loopShape(n *sitter.Node, kind string, repeats bool) (bounded bool, scope tsLoopScope) {
	bounded = tsLoopBounded(w.kinds, n, w.src, w.constLocals)
	switch kind {
	case "for_in_statement":
		if !bounded {
			w.noteLoopOverParam(tsElementRoot(w.kinds, n.ChildByFieldName("right"), w.src, w.paramScope))
		}
		if tsReachedThroughElement(w.kinds, n.ChildByFieldName("right"), w.src, w.loopScopes) {
			bounded = true
		}
		scope = tsForInScope(w.kinds, n, w.src, repeats)
	case "for_statement":
		if !bounded {
			w.noteLoopOverParam(tsForBoundRoot(w.kinds, n, w.src, w.paramScope))
		}
		if tsForBoundThroughElement(w.kinds, n, w.src, w.loopScopes) {
			bounded = true
		}
		scope = tsForScope(w.kinds, n, w.src)
	}
	return bounded, scope
}

func (w *tsBodyWalker) enterLoop(scales, repeats bool, scope tsLoopScope) {
	w.loopDepth++
	if scales {
		w.scalingDepth++
	}
	if repeats {
		w.repeatDepth++
	}
	w.loopScopes = append(w.loopScopes, scope)
}

func (w *tsBodyWalker) leaveLoop(scales, repeats bool) {
	w.loopScopes = w.loopScopes[:len(w.loopScopes)-1]
	w.loopDepth--
	if scales {
		w.scalingDepth--
	}
	if repeats {
		w.repeatDepth--
	}
}

// walkCall records a call and reports whether it walked the call's children itself.
func (w *tsBodyWalker) walkCall(n *sitter.Node) bool {
	target := w.resolve(n)
	w.noteIOCall(n, target)
	if target != "" {
		w.recordResolvedCall(n, target)
	} else if w.metrics != nil && w.loopDepth > 0 {
		// Method call on an unknown receiver inside a loop (repo.findMany(),
		// prisma.user.create()). No graph edge today, but its name feeds the perf
		// metric so the performance analyzer can flag per-iteration ORM/fetch I/O.
		if recv, prop := tsMemberCall(w.kinds, n, w.src); prop != "" && !tsCheapMethods[prop] {
			tgt := prop
			if recv != "" {
				tgt = recv + "." + prop
			}
			w.recordInLoop(tgt)
		}
	}
	return w.metrics != nil && w.walkIteratorCall(n)
}

// Seed the performs_io closure: flag the enclosing body when it directly
// invokes a network/file I/O primitive. Independent of loop depth — a wrapper
// calls its I/O sink once, and the transitive pass carries the signal upward.
//
// A call that is I/O by its method name alone seeds only when it did not resolve.
// A resolved one is held for settleIONamedCalls (ionamed.go), which needs the whole
// repository to tell svg.js's findOne from TypeORM's.
func (w *tsBodyWalker) noteIOCall(n *sitter.Node, target string) {
	if w.metrics == nil || w.metrics.ioDirect {
		return
	}
	if tsIsIOCall(w.kinds, n, w.src, w.ioBindings) || tsPassesFetch(w.kinds, n, w.src, w.importMap) ||
		(w.drizzle && tsIsDrizzleQuery(w.kinds, n, w.src)) {
		w.metrics.ioDirect = true
		return
	}
	if !tsIsIONamedCall(w.kinds, n, w.src) {
		return
	}
	if target == "" {
		w.metrics.ioDirect = true
	} else if !slices.Contains(w.metrics.ioNamedCalls, target) {
		w.metrics.ioNamedCalls = append(w.metrics.ioNamedCalls, target)
	}
}

func (w *tsBodyWalker) recordResolvedCall(n *sitter.Node, target string) {
	// A resolved target that is itself the round trip (iotargets.go).
	if w.metrics != nil && tsIOTargets[target] {
		w.metrics.ioDirect = true
		if !slices.Contains(w.metrics.ioCalls, target) {
			w.metrics.ioCalls = append(w.metrics.ioCalls, target)
		}
	}
	w.addCallRel(target)
	w.recordCall(target)
	w.noteCallOnLoopElement(n, target)
}

func (w *tsBodyWalker) addCallRel(target string) {
	if !w.seen[target] {
		w.seen[target] = true
		w.rels = append(w.rels, facts.Relation{Kind: facts.RelCalls, Target: target})
	}
}

// An array-iterator method with a callback (items.map(cb)) is a loop: its
// callback body runs per element, but the receiver/other args run once.
func (w *tsBodyWalker) walkIteratorCall(n *sitter.Node) bool {
	cb := tsIteratorCallback(w.kinds, n, w.src)
	if cb == nil {
		return false
	}
	// A `[a,b,c].map(cb)` over a literal receiver iterates a fixed count — it
	// raises loop_depth but not the scaling depth. The actual w.loopDepth /
	// w.scalingDepth bump happens at the callback body inside walkCallbackSubtree;
	// here we only record the maxes.
	bounded := tsIteratorReceiverBounded(w.kinds, n, w.src, w.constLocals)
	// `job.needs.map(cb)` under a loop over jobs: the receiver is the
	// current element's own collection (hierarchy.go).
	hier := false
	if fn := n.ChildByFieldName("function"); fn != nil {
		if !bounded {
			w.noteLoopOverParam(tsElementRoot(w.kinds, fn.ChildByFieldName("object"), w.src, w.paramScope))
		}
		hier = tsReachedThroughElement(w.kinds, fn.ChildByFieldName("object"), w.src, w.loopScopes)
	}
	w.noteLoop(!bounded && !hier)
	for i := range n.ChildCount() {
		if c := n.Child(i); tsByteContains(c, cb) {
			w.walkCallbackSubtree(c, cb, bounded, hier, tsCallbackElementAt(w.kinds, n, w.src))
		} else {
			w.walk(c)
		}
	}
	return true
}

// `new WebSocket(...)` / `new XMLHttpRequest()` opens a network/stream resource —
// tag the enclosing body io_direct (constructors are new_expression, not call_expression).
func (w *tsBodyWalker) noteIOConstructor(n *sitter.Node) {
	if w.metrics == nil || w.metrics.ioDirect {
		return
	}
	if ctor := n.ChildByFieldName("constructor"); ctor != nil && tsIOConstructors[nodeText(ctor, w.src)] {
		w.metrics.ioDirect = true
	}
}

// A `this.<member>` reference inside a class method marks that member used, even
// when it is not called: React binds event handlers as prop VALUES
// (onClick={this.handleClick}), so a handler referenced only in JSX has no call
// edge and would otherwise be mis-reported dead. className is the exact class
// symbol name, so the target matches the method fact "<dir>.<Class>.<member>".
func (w *tsBodyWalker) noteThisMember(n *sitter.Node) {
	if w.className == "" {
		return
	}
	obj := n.ChildByFieldName("object")
	if obj == nil || kindOf(w.kinds, obj) != "this" {
		return
	}
	if prop := n.ChildByFieldName("property"); prop != nil {
		w.addCallRel(w.dir + "." + w.className + "." + nodeText(prop, w.src))
	}
}

// walkCallbackSubtree descends toward an iterator's callback, bumping the loop depth
// exactly at the callback (its body is per-iteration) while walking everything else
// (the receiver, sibling args) at the current depth. When the iterator's receiver is a
// literal (bounded), the scaling depth is NOT bumped — the callback runs a fixed number
// of times, so it does not add a factor of n to Big-O.
func (w *tsBodyWalker) walkCallbackSubtree(n, cb *sitter.Node, bounded, hier bool, elemAt int) {
	if n == nil {
		return
	}
	if n.StartByte() == cb.StartByte() && n.EndByte() == cb.EndByte() {
		// The iterator invokes this callback per element: walk its BODY at +1.
		// We descend into the callback's children directly rather than walk(cb),
		// because walk() would treat the callback as a function scope and reset
		// the depth — but THIS callback genuinely runs per iteration.
		// An iterator receiver is either a literal (constant) or data-derived (scaling);
		// it is never infinite, so repeating and scaling coincide here.
		// A hierarchical receiver repeats without scaling (hierarchy.go).
		scales := !bounded && !hier
		w.loopDepth++
		if scales {
			w.scalingDepth++
		}
		if !bounded {
			w.repeatDepth++
		}
		w.loopScopes = append(w.loopScopes, tsCallbackScope(w.kinds, cb, w.src, !bounded, elemAt))
		for i := range cb.ChildCount() {
			w.walk(cb.Child(i))
		}
		w.loopScopes = w.loopScopes[:len(w.loopScopes)-1]
		w.loopDepth--
		if scales {
			w.scalingDepth--
		}
		if !bounded {
			w.repeatDepth--
		}
		return
	}
	for i := range n.ChildCount() {
		if c := n.Child(i); tsByteContains(c, cb) {
			w.walkCallbackSubtree(c, cb, bounded, hier, elemAt)
		} else {
			w.walk(c)
		}
	}
}

// collectCallsWithMetrics walks a function/method body subtree and returns
// deduplicated RelCalls relations plus per-function complexity metrics,
// used for function/method/arrow facts. selfName/selfShort enable direct-recursion
// detection.
// declaresParameters reports whether a member's own function takes any
// parameter, as `yes` or `no`, and nothing when the member has no function at
// all. A callback that ignores what it is handed is a general smell and the
// specific one a framework convention names: a modifier is given the element it
// is attached to, and one that declares no parameter is being used as a bare
// side-effect trigger rather than as a modifier. The answer is a word rather
// than a count because a rule asks "does it take one", and the property forms
// compare a value rather than order it — a count would need a threshold the
// consequent has no way to write.
//
// Only the member's OWN function is read: the first parameter list under the
// member, not one belonging to a callback nested inside it, which would answer
// about somebody else's signature.
func declaresParameters(kinds *tsutil.KindTable, member *sitter.Node) (string, bool) {
	var params *sitter.Node
	var find func(n *sitter.Node, depth int)
	find = func(n *sitter.Node, depth int) {
		if params != nil || n == nil || depth > 3 {
			return
		}
		for i := range n.ChildCount() {
			c := n.Child(i)
			if c == nil {
				continue
			}
			switch kindOf(kinds, c) {
			case "formal_parameters":
				params = c
				return
			case "arrow_function", "function_expression", "call_expression", "arguments":
				find(c, depth+1)
			}
			if params != nil {
				return
			}
		}
	}
	find(member, 0)
	if params == nil {
		return "", false
	}
	for i := range params.ChildCount() {
		switch kindOf(kinds, params.Child(i)) {
		case "(", ")", ",":
		default:
			return "yes", true
		}
	}
	return "no", true
}

func collectCallsWithMetrics(node *sitter.Node, scope callScope, selfName, selfShort string) ([]facts.Relation, *tsBodyMetrics) {
	m := &tsBodyMetrics{}
	kinds, src := scope.kinds, scope.src
	w := &tsBodyWalker{callScope: scope, selfName: selfName, selfShort: selfShort, metrics: m, seen: make(map[string]bool)}
	fn := tsEnclosingFunction(node)
	paramScope, paramIndex := tsParamScope(kinds, fn, src)
	w.paramScope, w.paramIndex = []tsLoopScope{paramScope}, paramIndex
	w.memberRoots = withLocalRoots(scope.memberRoots, tsLocalTypes(kinds, fn, node, src, scope.dir, scope.importMap))
	w.constLocals = tsConstLiteralLocals(kinds, node, src)
	w.walk(node)
	return w.rels, m
}

// noteFieldWrite records an assignment whose target is rooted at `this`, under
// the outermost property that follows it: `this.args.user.name = x` records
// `args`, and `this.selected = y` records `selected`. The root is what a
// convention speaks about — a component must not write through its arguments,
// a tracked function must not set the state it is derived from — and the exact
// path beyond it varies per call site without changing the answer.
//
// Only `this` is followed. An assignment to a local, a parameter, or another
// object is not a claim about the member's own state, and recording it would
// make the prop a list of everything the body touches rather than of what it
// mutates on itself.
func (m *tsBodyMetrics) noteFieldWrite(kinds *tsutil.KindTable, n *sitter.Node, src []byte) {
	if m == nil || n.ChildCount() == 0 {
		return
	}
	target := n.Child(0)
	if target == nil || kindOf(kinds, target) != "member_expression" {
		return
	}
	root := target
	var field string
	for root != nil && kindOf(kinds, root) == "member_expression" {
		if prop := root.ChildByFieldName("property"); prop != nil {
			field = nodeText(prop, src)
		}
		root = root.ChildByFieldName("object")
	}
	if root == nil || kindOf(kinds, root) != "this" || field == "" {
		return
	}
	if m.writeSeen == nil {
		m.writeSeen = map[string]bool{}
	}
	if m.writeSeen[field] {
		return
	}
	m.writeSeen[field] = true
	m.fieldsWritten = append(m.fieldsWritten, field)
}

// applyTSMetrics writes the complexity props onto a function/method fact's Props.
func applyTSMetrics(props map[string]any, m *tsBodyMetrics) {
	if m == nil {
		return
	}
	props["cyclomatic"] = 1 + m.decisions
	if len(m.fieldsWritten) > 0 {
		sort.Strings(m.fieldsWritten)
		props["fields_written"] = m.fieldsWritten
	}
	if m.loopDepth > 0 {
		props["loop_depth"] = m.loopDepth
		// Emit the scaling depth (bounded loops discounted) alongside — even when 0 — so
		// the consumer distinguishes "all loops bounded" from "signal absent".
		props["scaling_loop_depth"] = m.scalingLoopDepth
	}
	if m.loopCount > 0 {
		props["loop_count"] = m.loopCount
	}
	if len(m.callsInLoop) > 0 {
		props["calls_in_loop"] = m.callsInLoop
		// Emit the N+1 subset alongside — even when EMPTY — so the consumer distinguishes
		// "no call repeats" from "signal absent". An omitted key makes the consumer fall
		// back to the unfiltered calls_in_loop, defeating the discount in exactly the case
		// it exists for (every in-loop call sitting inside a constant loop).
		if m.callsInScalingLoop == nil {
			m.callsInScalingLoop = []string{}
		}
		props["calls_in_scaling_loop"] = m.callsInScalingLoop
		props["calls_in_scaling_loop_depth"] = callDepths(m.callsInScalingLoop, m.scalingCallDepth)
	}
	if len(m.callsOnLoopElement) > 0 {
		props["calls_on_loop_element"] = m.callsOnLoopElement
		props["calls_on_loop_element_arg"] = m.callsOnLoopElementArg
	}
	if len(m.loopsOverParam) > 0 {
		props["loops_over_param"] = true
		idx := make([]int, 0, len(m.loopsOverParam))
		for i := range m.loopsOverParam {
			idx = append(idx, i)
		}
		sort.Ints(idx)
		props["loops_over_param_index"] = idx
	}
	if m.recursive {
		props["recursive_self"] = true
	}
	if m.ioDirect {
		props["io_direct"] = true
	}
	if len(m.ioCalls) > 0 {
		sort.Strings(m.ioCalls)
		props["io_calls"] = m.ioCalls
	}
	if len(m.ioNamedCalls) > 0 && !m.ioDirect {
		sort.Strings(m.ioNamedCalls)
		props[propIONamedCalls] = m.ioNamedCalls
	}
}

// tsLoopBounded reports whether a syntactic loop's trip count is independent of the input
// size, so it adds no factor of n: a for..of / for..in over an array/object literal, or an
// infinite while(true) / do..while(true) event/retry loop. A C-style for(;;) is treated as
// unbounded (its bound is not statically evident).
//
// It does NOT mean the loop runs a constant number of times — see tsLoopRepeats.
func tsLoopBounded(kinds *tsutil.KindTable, n *sitter.Node, src []byte, consts map[string]bool) bool {
	return tsLoopConstant(kinds, n, src, consts) || tsLoopInfinite(kinds, n, src)
}

// tsLoopRepeats reports whether the loop body runs a non-constant number of times, so a
// call inside it is an N+1 candidate. Only a genuinely constant loop is excluded: a
// `while (true) { row = await fetch(id) }` reconnect/retry loop queries once per
// iteration even though its depth is discounted. Scaling and repeating differ.
func tsLoopRepeats(kinds *tsutil.KindTable, n *sitter.Node, src []byte, consts map[string]bool) bool {
	return !tsLoopConstant(kinds, n, src, consts)
}

// tsLoopConstant reports whether a loop runs a number of times fixed where it is
// written: a for..of / for..in over a constant collection, or a C-style for against
// a constant bound (constbound.go). A while/do loop never qualifies.
func tsLoopConstant(kinds *tsutil.KindTable, n *sitter.Node, src []byte, consts map[string]bool) bool {
	switch kindOf(kinds, n) {
	case "for_in_statement":
		return tsConstantIterable(kinds, n.ChildByFieldName("right"), src, consts)
	case "for_statement":
		return tsForConstBounded(kinds, n, src)
	}
	return false
}

// tsLoopInfinite reports whether a loop is the `while (true)` / `do … while (true)` form,
// exited by break/return rather than by exhausting the input.
func tsLoopInfinite(kinds *tsutil.KindTable, n *sitter.Node, src []byte) bool {
	switch kindOf(kinds, n) {
	case "while_statement", "do_statement":
		if c := n.ChildByFieldName("condition"); c != nil {
			return tsIsTrueCondition(kinds, c, src)
		}
	}
	return false
}

// tsIteratorReceiverBounded reports whether an array-iterator call (`recv.map(cb)`) has a
// receiver with a fixed number of elements (`[a,b,c].map(...)`, `PACKAGES.forEach(...)`),
// so the callback runs a fixed number of times. See constbound.go.
func tsIteratorReceiverBounded(kinds *tsutil.KindTable, n *sitter.Node, src []byte, consts map[string]bool) bool {
	fn := n.ChildByFieldName("function")
	if fn == nil || kindOf(kinds, fn) != "member_expression" {
		return false
	}
	return tsConstantIterable(kinds, fn.ChildByFieldName("object"), src, consts)
}

// tsIsTrueCondition reports whether a loop condition is the constant `true` / a nonzero
// literal — the `while (true) { … }` reconnect/event loop whose iteration count is driven
// by external events, not input size.
func tsIsTrueCondition(kinds *tsutil.KindTable, c *sitter.Node, src []byte) bool {
	inner := c
	if kindOf(kinds, inner) == "parenthesized_expression" && inner.NamedChildCount() > 0 {
		inner = inner.NamedChild(0)
	}
	switch kindOf(kinds, inner) {
	case "true":
		return true
	case "number":
		return nodeText(inner, src) != "0"
	}
	return false
}

// resolveTSCall resolves a single call_expression to a canonical target fact name,
// or "" when the call cannot be resolved (e.g. a method call on a value of unknown
// type). It resolves:
//   - bare calls `foo()` → imported symbol via importMap, else same-module "<dir>.foo"
//   - `this.method()` inside a class → "<dir>.<className>.method"
//   - `this.field.method()` on a field of stated type T → "<qualified T>.method"
//   - `x.member()` where x is a member-call root (see memberCallRoots) → "<qualified x>.member"
//   - `make().method()` where make is a function of this file declared to return T → "<qualified T>.method"
func (s callScope) resolve(call *sitter.Node) string {
	kinds, src, dir, className := s.kinds, s.src, s.dir, s.className
	importMap, fieldTypes, memberRoots, returnTypes := s.importMap, s.fieldTypes, s.memberRoots, s.returnTypes
	fn := call.ChildByFieldName("function")
	if fn == nil {
		return ""
	}
	switch kindOf(kinds, fn) {
	case "identifier":
		name := nodeText(fn, src)
		if target, ok := importMap[name]; ok {
			return target
		}
		return dir + "." + name
	case "member_expression":
		object := fn.ChildByFieldName("object")
		property := fn.ChildByFieldName("property")
		if object == nil || property == nil {
			return ""
		}
		// `this.method()` resolves within the enclosing class; other receivers
		// have an unknown type and are left unresolved to avoid dangling edges.
		if kindOf(kinds, object) == "this" && className != "" {
			return dir + "." + className + "." + nodeText(property, src)
		}
		if kindOf(kinds, object) == "identifier" && kindOf(kinds, property) == "property_identifier" {
			if root, ok := memberRoots[nodeText(object, src)]; ok {
				return root + "." + nodeText(property, src)
			}
		}
		// `make().method()`: a call on the result of one of this file's functions,
		// whose declared return type names the method's class.
		if kindOf(kinds, object) == "call_expression" && kindOf(kinds, property) == "property_identifier" {
			if callee := object.ChildByFieldName("function"); callee != nil && kindOf(kinds, callee) == "identifier" {
				if typ, ok := returnTypes[nodeText(callee, src)]; ok {
					return typ + "." + nodeText(property, src)
				}
			}
		}
		// `this.field.method()`: the field's stated type names the method's class.
		if kindOf(kinds, object) == "member_expression" && kindOf(kinds, property) == "property_identifier" {
			recv, field := object.ChildByFieldName("object"), object.ChildByFieldName("property")
			if recv != nil && field != nil && kindOf(kinds, recv) == "this" {
				if typ, ok := fieldTypes[nodeText(field, src)]; ok {
					return typ + "." + nodeText(property, src)
				}
			}
		}
	}
	return ""
}

// tsGrammarKey and tsGrammarLanguage name the grammar a tree was parsed with, so a
// cached query is never reused across the two. TypeScript and TSX are separate
// grammars with separate symbol ids, and a query compiled for one cannot be run
// against a tree parsed by the other.
func tsGrammarKey(isTSX bool) string {
	if isTSX {
		return "tsx"
	}
	return "typescript"
}

func tsGrammarLanguage(isTSX bool) *sitter.Language {
	if isTSX {
		return sitter.NewLanguage(typescript.LanguageTSX())
	}
	return sitter.NewLanguage(typescript.LanguageTypescript())
}
