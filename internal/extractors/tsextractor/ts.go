package tsextractor

import (
	"context"
	"encoding/json"
	"github.com/enola-labs/enola/internal/extractors/extcoverage"
	"github.com/enola-labs/enola/internal/extractors/tsutil"
	"io/fs"
	"log"
	"os"
	"path/filepath"
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
	return false, nil
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

// Extract parses TypeScript/TSX files and emits architectural facts.
func (e *TSExtractor) Extract(ctx context.Context, repoPath string, files []string) ([]facts.Fact, error) {
	var allFacts []facts.Fact

	// Detect frameworks
	isNextJS := detectNextJS(repoPath)
	isVue := detectVue(repoPath)
	isNuxt := detectNuxt(repoPath)
	isSvelteKit := detectSvelteKit(repoPath)
	isEmber := detectEmber(repoPath)
	isReactNav := detectReactNavigation(repoPath)
	isAngular := detectAngular(repoPath)
	// ORM detection is gated on the package.json dependency, exactly as Vue/Nuxt are, so
	// a class coincidentally decorated @Entity models nothing in a repo without TypeORM.
	isTypeORM, isDrizzle, isPrisma := detectORMs(repoPath)
	orms := ormFlags{typeORM: isTypeORM, drizzle: isDrizzle}

	// Parse tsconfig.json path aliases, one root per package for monorepos.
	aliasRoots := collectTSAliasRoots(repoPath)

	// SvelteKit maps $lib by convention and may declare literal aliases in its
	// config even before `svelte-kit sync` has generated a tsconfig.
	if isSvelteKit {
		aliasRoots = withSvelteKitAliasFallbacks(repoPath, aliasRoots)
	}

	// Restrict to TypeScript files once, then parse them in parallel. The
	// framework flags and path aliases above are read-only, and extractFile is a
	// pure function of (src, relFile, …), so per-file work is independent. Results
	// are merged in file order for deterministic output.
	var tsFiles, htmlFiles []string
	knownFiles := make(map[string]bool)
	for _, relFile := range files {
		if isTypeScriptFile(relFile) {
			tsFiles = append(tsFiles, relFile)
			knownFiles[filepath.ToSlash(relFile)] = true
			continue
		}
		// An Angular component's template is a separate .html file, and the members
		// and child components it names are frequently referenced nowhere else. It is
		// read only in an Angular repository: everywhere else a .html file is a page,
		// a fixture or documentation, and scanning it would model nothing.
		if isAngular && isAngularTemplateFile(relFile) && !facts.IsTestPath(relFile) {
			htmlFiles = append(htmlFiles, relFile)
		}
	}
	var nuxtAutoComponents map[string]string
	if isNuxt {
		nuxtAutoComponents = nuxtAutoComponentIndex(knownFiles)
	}

	// Repo-wide pre-pass: resolve generated gRPC-web client stubs (service FQN +
	// RPC methods + client class) so per-file call-site detection can map a
	// `client.method(...)` call to its "/pkg.Service/Method" route. Built before
	// the parallel pass because a client's stub and its call sites usually live
	// in different files.
	// Read each source once, in parallel. GraphQL context needs a repository-wide
	// view before extraction, but it consumes these bytes rather than performing a
	// serial pre-pass that rereads every file from disk.
	type sourceResult struct {
		src []byte
		err error
	}
	readSources := parallel.MapFiles(ctx, tsFiles, func(relFile string) sourceResult {
		src, err := os.ReadFile(filepath.Join(repoPath, relFile))
		return sourceResult{src: src, err: err}
	})
	sources := make(map[string][]byte, len(tsFiles))
	for i, read := range readSources {
		if read.err != nil {
			log.Printf("[ts-extractor] error reading %s: %v", tsFiles[i], read.err)
			continue
		}
		sources[tsFiles[i]] = read.src
	}
	grpcStubs := buildGRPCStubIndex(tsFiles, sources)
	graphqlServer := detectGraphQLServerUsage(tsFiles, sources)

	perFile := parallel.MapFiles(ctx, tsFiles, func(relFile string) tsFileResult {
		src := sources[relFile]
		if src == nil {
			return tsFileResult{}
		}
		if isMinifiedSource(src) {
			// Minified/bundled artifact (e.g. a checked-in webpack/vendor bundle
			// served statically). Emitting facts for its obfuscated symbols
			// pollutes complexity/hotspot/performance analysis, so skip it.
			log.Printf("[ts-extractor] skipping minified/bundled file %s", relFile)
			return tsFileResult{}
		}
		aliases := aliasesForDir(aliasRoots, factpath.Dir(relFile))
		var res tsFileResult
		res.facts, res.angular, res.angularRouter, res.angularInline, res.angularHTTP, res.clients = e.extractFile(src, relFile, isNextJS, isVue, isNuxt, isSvelteKit, isEmber, isReactNav, isAngular, graphqlServer, orms, aliases, knownFiles, nuxtAutoComponents, grpcStubs)
		// Routers, mounts and held-back routes for the repo-wide mount pass below.
		// Collected here because resolving an import needs this file's path aliases,
		// which are in scope only during the per-file walk. Same test-path gate as
		// the routes themselves: an e2e suite that mounts its own app contributes
		// no production surface.
		if !facts.IsTestPath(relFile) {
			res.routers = collectRouterFile(src, relFile, aliases, knownFiles)
		}
		return res
	})
	sources = nil

	// Templates are scanned in parallel with no parser: an Angular template is not
	// HTML once its @if/@for blocks are in it, and both dialects are live in the
	// same repositories.
	templates := make(map[string]*angularTemplate, len(htmlFiles))
	if len(htmlFiles) > 0 {
		scans := parallel.MapFiles(ctx, htmlFiles, func(relFile string) *angularTemplate {
			src, err := os.ReadFile(filepath.Join(repoPath, relFile))
			if err != nil {
				log.Printf("[ts-extractor] error reading %s: %v", relFile, err)
				return nil
			}
			return scanAngularTemplate(src, relFile)
		})
		for i, t := range scans {
			if t != nil {
				templates[htmlFiles[i]] = t
			}
		}
	}

	// Group files by directory for module detection. Files that produced no
	// facts (unreadable, or skipped as minified/bundled) do not register a
	// module, so a directory containing only skipped bundles (e.g. a vendored
	// scripts dir) stays out of the graph rather than surfacing as an empty module.
	modules := make(map[string]bool)
	routerFiles := make([]*routerFile, 0, len(perFile))
	var angular angularCounts
	var angularRouters []*angularRouterFile
	var angularHTTPFiles []*angularHTTPFile
	clientTotals := clientCounts{}
	var angularRoutes, angularRequests angularCounts
	inlineTemplates := map[string]*angularTemplate{}
	for i, res := range perFile {
		if res.routers != nil {
			routerFiles = append(routerFiles, res.routers)
		}
		angular.merge(res.angular)
		for name, t := range res.angularInline {
			inlineTemplates[name] = t
		}
		if res.angularRouter != nil {
			angularRouters = append(angularRouters, res.angularRouter)
		}
		if res.angularHTTP != nil {
			angularHTTPFiles = append(angularHTTPFiles, res.angularHTTP)
		}
		clientTotals.merge(res.clients)
		if len(res.facts) == 0 {
			continue
		}
		allFacts = append(allFacts, res.facts...)
		modules[factpath.Dir(tsFiles[i])] = true
	}

	// Express sub-routers mounted from another file. The per-file pass holds their
	// routes back rather than emitting a fragment path; this is where both halves
	// are visible and the true runtime path can be composed. See routermount.go.
	if mounted := composeRouterMounts(routerFiles); len(mounted) > 0 {
		allFacts = append(allFacts, mounted...)
		for _, f := range mounted {
			modules[factpath.Dir(f.File)] = true
		}
	}

	// Serial post-pass: propagate the per-body io_direct flag transitively across the
	// call graph into performs_io, so wrapper-hidden network/file I/O is visible to the
	// performance analyzer. Mirrors the Swift extractor's computePerformsIO.
	if isNuxt {
		resolveNuxtAutoComposableCalls(allFacts)
	}
	computeTSPerformsIO(allFacts)

	// Engine-relative routes compose onto their mount point here, where every
	// mount in the repo is visible; a per-file pass cannot see both sides.
	if isEmber {
		composeEngineMounts(allFacts)
	}

	// Angular's routes are composed here, where every route array and every lazy
	// mount in the repository is visible; a per-file pass can see only one end of a
	// loadChildren. See angularroutes.go.
	if routes, c := composeAngularRoutes(angularRouters); len(routes) > 0 || c.total() > 0 {
		allFacts = append(allFacts, routes...)
		for _, f := range routes {
			modules[factpath.Dir(f.File)] = true
		}
		angularRoutes.merge(c)
	}

	// Templates are joined to the components that own them here, where the members of
	// every class and the selector every component declared are both in hand.
	angularTemplateCounts := attachAngularTemplates(allFacts, templates, inlineTemplates)

	// Requests through an injected HttpClient are composed here: a base URL is a
	// static of one service and named by many, so the constants are only all in hand
	// once the repository is read. See angularhttp.go.
	if reqs, c := composeAngularRequests(angularHTTPFiles); len(reqs) > 0 || c.total() > 0 {
		allFacts = append(allFacts, reqs...)
		for _, f := range reqs {
			modules[factpath.Dir(f.File)] = true
		}
		angularRequests.merge(c)
	}

	// Every injects edge is made to name a symbol this snapshot holds, now that the
	// whole repository is assembled; see reconcileAngularInjects.
	if isAngular {
		angular.merge(reconcileAngularInjects(allFacts))
		angularRoutes.merge(resolveAngularLazyComponents(allFacts))
	}

	// What the injection pass could name, and what it could not, by cause. Emitted
	// only when there were injection sites at all — a zero from an extractor that
	// never looked reads the same as a zero from one that did, which is the whole
	// failure this fact exists to prevent.
	if angular.total() > 0 {
		if f, ok := extcoverage.Fact(repoPath, "typescript:angular-di", "angular_inject",
			angular.resolved, angular.unresolved); ok {
			allFacts = append(allFacts, f)
		}
	}
	if angularTemplateCounts.total() > 0 {
		if f, ok := extcoverage.Fact(repoPath, "typescript:angular-templates", "angular_template_ref",
			angularTemplateCounts.resolved, angularTemplateCounts.unresolved); ok {
			allFacts = append(allFacts, f)
		}
	}
	if angularRequests.total() > 0 {
		if f, ok := extcoverage.Fact(repoPath, "typescript:angular-requests", "angular_http_call",
			angularRequests.resolved, angularRequests.unresolved); ok {
			allFacts = append(allFacts, f)
		}
	}
	if angularRoutes.total() > 0 {
		if f, ok := extcoverage.Fact(repoPath, "typescript:angular-routes", "angular_route",
			angularRoutes.resolved, angularRoutes.unresolved); ok {
			allFacts = append(allFacts, f)
		}
	}

	// One account per declared client in this repository, including a client that found
	// nothing here; see clientCoverageFacts.
	if len(e.clients) > 0 {
		allFacts = append(allFacts, clientCoverageFacts(repoPath, e.clients, clientTotals)...)
	}

	// Prisma models live in schema.prisma — a separate DSL, so tree-sitter never sees it.
	// Read it off-glob, the same way package.json and tsconfig.json already are.
	if isPrisma {
		allFacts = append(allFacts, extractPrismaStorage(repoPath)...)
	}

	// Emit module facts for each directory
	pkgNames := collectPackageNames(repoPath)
	// The workspace project each directory belongs to, where the repository states
	// one. A monorepo's unit of ownership is its project, not its directory, and
	// every reading that groups by unit was inferring the boundary from the path.
	var projects map[string]string
	if isAngular {
		projects = angularProjectNames(repoPath)
	}
	for dir := range modules {
		props := map[string]any{
			"language": "typescript",
		}
		// The npm package this directory belongs to. The cross-repo linker reads it
		// to recognize a repo's own @scope, so an import of a sibling package the
		// repo itself publishes is not mistaken for a dependency on another repo
		// that happens to be labeled like the scope.
		if name := nearestPackageName(pkgNames, dir); name != "" {
			props["package_name"] = name
		}
		if name := nearestProjectName(projects, dir); name != "" {
			props["workspace_project"] = name
		}
		allFacts = append(allFacts, facts.Fact{
			Kind:  facts.KindModule,
			Name:  dir,
			File:  dir,
			Props: props,
		})
	}

	return allFacts, nil
}

// extractCtx bundles the per-file state threaded through declaration extraction
// so symbols can be enriched with React/Next.js semantic classification.
type extractCtx struct {
	src         []byte
	relFile     string
	dir         string
	isTSX       bool
	isNextJS    bool
	isVue       bool
	isNuxt      bool
	isSvelteKit bool
	orms        ormFlags
	importMap   map[string]string
	imports     emberImportBindings // the file's import table, read for the module a superclass identifier came from
	ioBindings  map[string]bool     // local names bound to imports from a network module (I/O sinks)
	knownFiles  map[string]bool     // repo-relative (slash) paths of all indexed TS/JS files
	aliases     map[string]tsAlias  // this directory's tsconfig path aliases, for resolving an import written as a bare specifier
}

func (e *TSExtractor) extractFile(src []byte, relFile string, isNextJS, isVue, isNuxt, isSvelteKit, isEmber, isReactNav, isAngular bool, graphqlServer graphqlServerContext, orms ormFlags, aliases map[string]tsAlias, knownFiles map[string]bool, nuxtAutoComponents map[string]string, grpcStubs *grpcStubIndex) ([]facts.Fact, angularCounts, *angularRouterFile, map[string]*angularTemplate, *angularHTTPFile, clientCounts) {
	// The grammar is chosen here, so the kind table is too: TypeScript and TSX assign
	// different meanings to the same symbol ids, and everything below reads node kinds
	// through this table. See kinds.go.
	isTSX := strings.HasSuffix(relFile, ".tsx") || strings.HasSuffix(relFile, ".jsx")
	kinds := tsKindsFor(isTSX)

	if isVueFile(relFile) {
		return e.extractVueSFC(kinds, src, relFile, isNuxt, aliases, nuxtAutoComponents), angularCounts{}, nil, nil, nil, nil
	}
	if isSvelteFile(relFile) {
		return e.extractSvelteSFC(kinds, src, relFile, isSvelteKit, aliases), angularCounts{}, nil, nil, nil, nil
	}
	if isGraphQLDocFile(relFile) {
		if facts.IsTestPath(relFile) {
			return nil, angularCounts{}, nil, nil, nil, nil
		}
		if graphqlServer.sdlDocuments[filepath.ToSlash(relFile)] {
			if routes := extractGraphQLServerSDLDocument(src, relFile); len(routes) > 0 {
				return routes, angularCounts{}, nil, nil, nil, nil
			}
		}
		return extractGraphQLClientOps(string(src), relFile, facts.RouteSourceGraphQLOperation), angularCounts{}, nil, nil, nil, nil
	}
	if isHbsFile(relFile) {
		// Handlebars is only modeled where Ember's resolver gives the names
		// deterministic meaning; a lone .hbs in a non-Ember repo stays out.
		if !isEmber {
			return nil, angularCounts{}, nil, nil, nil, nil
		}
		return e.extractEmberHbs(src, relFile, knownFiles), angularCounts{}, nil, nil, nil, nil
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

	var result []facts.Fact

	// Parse openapi-typescript generated files for backend API route dependencies.
	// These are client-role route facts showing which backend routes the TS code calls.
	if openapiRoutes := extractOpenAPITypescriptFacts(src, relFile); len(openapiRoutes) > 0 {
		result = append(result, openapiRoutes...)
	}

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
		if graphqlServer.enabled {
			result = append(result, extractGraphQLServerSDL(src, relFile)...)
		}
		result = append(result, extractHTTPClientFacts(src, relFile)...)
		// Call-registered server routes (Express/Fastify/Hono/Koa). Same test-path
		// gate: an e2e suite that spins up its own app would otherwise contribute
		// server routes no production client calls.
		result = append(result, extractServerRouteFacts(src, relFile)...)
	}

	// gRPC-web client call sites become client-role routes to "/pkg.Service/Method".
	result = append(result, extractGRPCClientFacts(src, relFile, grpcStubs)...)

	lang := typescript.LanguageTypescript()
	if isTSX {
		lang = typescript.LanguageTSX()
	}

	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(lang)); err != nil {
		return result, angularCounts{}, nil, nil, nil, nil
	}

	tree := parser.Parse(src, nil)
	defer tree.Close()

	root := tree.RootNode()
	if !facts.IsTestPath(relFile) {
		result = append(result, extractGraphQLTagFactsAST(src, relFile, kinds, isTSX, root)...)
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
		result = append(result, extractTSKafkaFacts(kinds, root, src, relFile)...)
	}

	// Extract from the tree
	result = append(result, e.extractImports(kinds, root, src, relFile, aliases, isSvelteKit)...)

	ctx := &extractCtx{
		src:         src,
		relFile:     relFile,
		dir:         factpath.Dir(relFile),
		isTSX:       isTSX,
		isNextJS:    isNextJS,
		isVue:       isVue,
		isNuxt:      isNuxt,
		isSvelteKit: isSvelteKit,
		orms:        orms,
		importMap:   buildImportSymbols(kinds, root, src, relFile, aliases),
		imports:     buildEmberImportBindings(kinds, root, src, relFile, aliases),
		ioBindings:  buildIOImportBindings(kinds, root, src),
		knownFiles:  knownFiles,
		aliases:     aliases,
	}
	decls := e.extractDeclarations(kinds, root, ctx)

	// A declaration may be exported via a separate `export { A, B }` clause or
	// `export default Name` statement rather than an inline `export` keyword.
	// Mark the corresponding symbols as exported.
	if exported := collectExportedLocalNames(kinds, root, src); len(exported) > 0 {
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
	result = append(result, decls...)

	// Whole-file reference pass (JSX component rendering, imported-identifier values
	// like route configs, namespace member access, require()-bound names). Emitted as
	// a KindFileRef so file-scope references the per-function call walk cannot see do
	// not leave used code mis-reported as dead.
	result = append(result, e.collectTSFileRefs(kinds, root, ctx, aliases, facts.KindFileRef)...)

	// Detect Next.js routes
	if isNextJS {
		if routeFact := detectRoute(relFile); routeFact != nil {
			result = append(result, *routeFact)
		}
	}

	// Ember: classify component/service/model classes, attach strict-mode
	// template references, record @service injections for the ember-resolver
	// binder, and emit the router map's page routes.
	if isEmberFile || isEmber {
		result = emberEnrich(kinds, result, root, src, relFile, aliases, emberSegments)
	}
	if isReactNav && !facts.IsTestPath(relFile) {
		result = append(result, extractReactNavScreens(kinds, root, src, relFile, aliases)...)
		result = attachReactNavLinks(kinds, result, root, src, relFile)
	}
	if isEmber && isEmberRouterFile(relFile) {
		result = append(result, extractEmberRoutes(kinds, root, src, relFile)...)
	}
	if isEmber {
		if engine, ok := isEmberEngineRoutesFile(relFile); ok {
			result = append(result, extractEmberEngineRoutes(kinds, root, src, relFile, engine)...)
		}
	}

	// Angular: classify the decorator-declared classes and attach the edges their
	// dependency injection declares. A post-pass over the declaration facts, as
	// emberEnrich is, because the decorator that decides a class's role sits above
	// the node the declaration walk stopped at.
	var angular angularCounts
	var router *angularRouterFile
	var inlineTemplates map[string]*angularTemplate
	var httpFile *angularHTTPFile
	if isAngular {
		result, angular, inlineTemplates, httpFile = angularEnrich(kinds, result, root, ctx, aliases)
		// The route arrays and router calls this file declares, for the repo-wide
		// walk in composeAngularRoutes. A test file's router configures a fixture,
		// not the application, the same gate the server-route passes apply.
		if !facts.IsTestPath(relFile) && declaresAngularRouting(src) {
			router = collectAngularRouterFile(kinds, root, ctx, aliases)
		}
	}

	// Requests through in-house clients the config declares. Not gated on a framework:
	// the declared receiver type is what makes a call a request, in a NestJS service as
	// much as anywhere. Test files excluded, as every other route pass excludes them.
	// See configuredclient.go.
	var clients clientCounts
	if len(e.clients) > 0 && !facts.IsTestPath(relFile) {
		var configured []facts.Fact
		configured, clients = configuredClientFacts(kinds, root, ctx, e.clients)
		result = append(result, configured...)
	}

	// Detect Vue Router configuration files
	if (isVue || isNuxt) && containsCreateRouterCall(kinds, root, src) {
		result = append(result, facts.Fact{
			Kind: facts.KindRoute,
			Name: relFile,
			File: relFile,
			Line: 1,
			Props: map[string]any{
				"type":      "router_config",
				"language":  "typescript",
				"framework": "vue",
			},
		})
		result = append(result, extractVueRouterRoutes(kinds, root, src, relFile, aliases)...)
	}
	// Nuxt accepts render-function pages in addition to .vue SFCs. Vue pages take
	// the early SFC return above and are emitted by extractVueSFC; emit the other
	// supported extensions here.
	if isNuxt && !isVueFile(relFile) {
		if route := detectNuxtRoute(relFile); route != nil {
			result = append(result, *route)
		}
	}

	return result, angular, router, inlineTemplates, httpFile, clients
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
	walkDeps(root)

	return result
}

func (e *TSExtractor) extractDeclarations(kinds *tsutil.KindTable, root *sitter.Node, ctx *extractCtx) []facts.Fact {
	var result []facts.Fact
	for i := range root.ChildCount() {
		result = append(result, e.extractNode(kinds, root.Child(i), ctx, false, "")...)
	}
	return result
}

// extractNode emits facts for a single declaration node. fallbackName supplies a
// name for anonymous default-exported declarations (e.g. `export default function
// () {}`), derived from the file name; it is ignored when the declaration has its
// own name.
func (e *TSExtractor) extractNode(kinds *tsutil.KindTable, node *sitter.Node, ctx *extractCtx, isExported bool, fallbackName string) []facts.Fact {
	var result []facts.Fact
	src, dir, relFile := ctx.src, ctx.dir, ctx.relFile

	switch kindOf(kinds, node) {
	case "export_statement":
		isDefault := hasChildKind(kinds, node, "default")
		fb := ""
		if isDefault {
			fb = fileSymbolName(relFile)
		}
		// Named/inline declaration inside the export.
		if decl := firstDeclChild(kinds, node); decl != nil {
			return e.extractNode(kinds, decl, ctx, true, fb)
		}
		// Anonymous default export of a value: name it after the file.
		if isDefault {
			for _, k := range []string{"function_expression", "generator_function", "class", "arrow_function", "call_expression"} {
				if c := findChildByKind(kinds, node, k); c != nil {
					return e.extractNode(kinds, c, ctx, true, fb)
				}
			}
		}

	case "function_declaration", "function_expression", "generator_function_declaration", "generator_function":
		name := findChildByKind(kinds, node, "identifier")
		symbolName := fallbackName
		if name != nil {
			symbolName = nodeText(name, src)
		}
		if symbolName == "" {
			break
		}
		result = append(result, e.funcSymbol(kinds, node, node, ctx, symbolName, isExported))

	case "arrow_function":
		if fallbackName != "" {
			result = append(result, e.funcSymbol(kinds, node, node, ctx, fallbackName, isExported))
		}

	case "call_expression":
		// Reached for `export default memo(...)` / `forwardRef(...)`.
		if fallbackName != "" {
			result = append(result, e.funcSymbol(kinds, node, node, ctx, fallbackName, isExported))
		}

	case "class_declaration", "abstract_class_declaration", "class":
		name := findChildByKind(kinds, node, "type_identifier")
		symbolName := fallbackName
		if name != nil {
			symbolName = nodeText(name, src)
		}
		if symbolName == "" {
			break
		}
		f := facts.Fact{
			Kind: facts.KindSymbol,
			Name: dir + "." + symbolName,
			File: relFile,
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

		// Check for implements clause (nested under class_heritage)
		for j := range node.ChildCount() {
			c := node.Child(j)
			if kindOf(kinds, c) == "class_heritage" {
				for k := range c.ChildCount() {
					heritage := c.Child(k)
					if kindOf(kinds, heritage) == "implements_clause" {
						for l := range heritage.ChildCount() {
							t := heritage.Child(l)
							if kindOf(kinds, t) == "type_identifier" {
								f.Relations = append(f.Relations, facts.Relation{
									Kind:   facts.RelImplements,
									Target: nodeText(t, src),
								})
							}
						}
					}
				}
			}
		}

		classBody := findChildByKind(kinds, node, "class_body")
		classifySymbol(kinds, &f, symbolName, classBody, ctx, facts.SymbolClass)
		if names := classDecoratorNames(kinds, node, src); names != "" {
			f.SetProp("decorators", names)
		}
		result = append(result, f)

		// TypeORM: a class decorated @Entity is a table. The storage fact is a COMPANION
		// to the symbol above, as in Kotlin/Room and Java/JPA — the class keeps its own
		// symbol fact.
		if ctx.orms.typeORM {
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
			result = append(result, decoratorRouteFacts(kinds, node, classBody, src, relFile, dir)...)
		}

		// Extract class methods
		if classBody != nil {
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
				if kindOf(kinds, member) != "method_definition" && kindOf(kinds, member) != "public_field_definition" {
					continue
				}
				methodName := findChildByKind(kinds, member, "property_identifier")
				if methodName == nil {
					methodName = findChildByKind(kinds, member, "identifier")
				}
				if methodName == nil {
					continue
				}
				mName := nodeText(methodName, src)
				// A `#`-prefixed member is private to the class in the language's
				// own sense and has no callers to measure. A constructor does: it
				// runs on every instantiation, and what it calls is the difference
				// between a class that builds itself and one that fetches. Skipping
				// both together left "nothing may be fetched from a constructor"
				// with no fact to stand on.
				if strings.HasPrefix(mName, "#") {
					continue
				}
				isPrivate := false
				for k := range member.ChildCount() {
					c := member.Child(k)
					if kindOf(kinds, c) == "accessibility_modifier" && nodeText(c, src) == "private" {
						isPrivate = true
						break
					}
				}
				mRels := []facts.Relation{{Kind: facts.RelDeclares, Target: dir}}
				callRels, m := collectCallsWithMetrics(kinds, member, src, dir, symbolName, ctx.importMap, ctx.ioBindings, dir+"."+symbolName+"."+mName, mName)
				mRels = append(mRels, callRels...)
				mProps := map[string]any{
					"symbol_kind": facts.SymbolMethod,
					"exported":    isExported && !isPrivate,
					"language":    "typescript",
					"receiver":    symbolName,
				}
				if names := decoratorSetProp(memberDecorators); names != "" {
					mProps["decorators"] = names
				}
				if takes, ok := declaresParameters(kinds, member); ok {
					mProps["takes_parameters"] = takes
				}
				if isGetterDefinition(kinds, member) {
					mProps["symbol_kind"] = facts.SymbolGetter
					mProps["getter_calls"] = countCallRelations(callRels)
				}
				if stimulusStaticField(kinds, member, mName, relFile) {
					mProps["framework"] = "stimulus"
					mProps["stimulus_static"] = mName
				}
				applyTSMetrics(mProps, m)
				result = append(result, facts.Fact{
					Kind:      facts.KindSymbol,
					Name:      dir + "." + symbolName + "." + mName,
					File:      relFile,
					Line:      int(member.StartPosition().Row) + 1,
					Props:     mProps,
					Relations: mRels,
				})
			}
		}

	case "expression_statement":
		// CommonJS export assignment: `exports.name = function` /
		// `module.exports.name = function`. None of the declaration-shaped cases
		// fire in a classic Node file, so a CommonJS module's whole public
		// surface was invisible — an Express controller written as
		// `exports.index = function(req, res)` emitted nothing at all. Only the
		// member-assignment-of-a-function shape emits a symbol; a plain value,
		// a re-exported identifier, or a whole-object `module.exports = {…}`
		// carries no declaration this pass can name without guessing.
		assign := findChildByKind(kinds, node, "assignment_expression")
		if assign == nil {
			break
		}
		name := commonJSExportName(kinds, assign.ChildByFieldName("left"), src)
		if name == "" {
			break
		}
		if right := assign.ChildByFieldName("right"); right != nil {
			switch kindOf(kinds, right) {
			case "function_expression", "arrow_function", "generator_function":
				result = append(result, e.funcSymbol(kinds, node, right, ctx, name, true))
			}
		}

	case "interface_declaration":
		if name := findChildByKind(kinds, node, "type_identifier"); name != nil {
			result = append(result, e.simpleSymbol(node, ctx, nodeText(name, src), facts.SymbolInterface, isExported))
		}

	case "type_alias_declaration":
		if name := findChildByKind(kinds, node, "type_identifier"); name != nil {
			result = append(result, e.simpleSymbol(node, ctx, nodeText(name, src), facts.SymbolType, isExported))
		}

	case "enum_declaration":
		name := findChildByKind(kinds, node, "identifier")
		if name == nil {
			name = findChildByKind(kinds, node, "type_identifier")
		}
		if name != nil {
			result = append(result, e.simpleSymbol(node, ctx, nodeText(name, src), facts.SymbolEnum, isExported))
		}

	case "internal_module", "module":
		// TypeScript `namespace X {}` / `module X {}`.
		name := findChildByKind(kinds, node, "identifier")
		if name == nil {
			name = findChildByKind(kinds, node, "nested_identifier")
		}
		if name != nil {
			result = append(result, e.simpleSymbol(node, ctx, nodeText(name, src), "namespace", isExported))
		}

	case "lexical_declaration", "variable_declaration":
		for j := range node.ChildCount() {
			decl := node.Child(j)
			if kindOf(kinds, decl) != "variable_declarator" {
				continue
			}
			name := findChildByKind(kinds, decl, "identifier")
			if name == nil {
				continue
			}
			symbolName := nodeText(name, src)

			// Determine the value node and the symbol kind. Arrow functions and
			// memo/forwardRef-wrapped values are functions/components; everything
			// else is a plain variable.
			symbolKind := facts.SymbolVariable
			var body *sitter.Node
			if v := findChildByKind(kinds, decl, "arrow_function"); v != nil {
				symbolKind = facts.SymbolFunc
				body = v
			} else if call := findChildByKind(kinds, decl, "call_expression"); call != nil && isComponentWrapper(kinds, call, src) {
				symbolKind = facts.SymbolFunc
				body = call
			}

			vRels := []facts.Relation{{Kind: facts.RelDeclares, Target: dir}}
			var vMetrics *tsBodyMetrics
			if body != nil {
				callRels, m := collectCallsWithMetrics(kinds, body, src, dir, "", ctx.importMap, ctx.ioBindings, dir+"."+symbolName, symbolName)
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
			if symbolKind == facts.SymbolFunc {
				if takes, ok := declaresParameters(kinds, decl); ok {
					f.SetProp("takes_parameters", takes)
				}
				applyTSMetrics(f.Props, vMetrics)
			}
			classifySymbol(kinds, &f, symbolName, body, ctx, symbolKind)
			result = append(result, f)

			// Drizzle: `export const orders = pgTable("orders", {...})` is a table. The
			// call_expression is already in hand above — it simply fails isComponentWrapper.
			if ctx.orms.drizzle {
				if call := findChildByKind(kinds, decl, "call_expression"); call != nil {
					if sf := drizzleTableStorage(kinds, call, src, symbolName, relFile, dir,
						int(decl.StartPosition().Row)+1); sf != nil {
						result = append(result, *sf)
					}
				}
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
	callRels, m := collectCallsWithMetrics(kinds, body, ctx.src, ctx.dir, "", ctx.importMap, ctx.ioBindings, ctx.dir+"."+name, name)
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

	// Pages Router
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

// detectNextJS checks if the repository is a Next.js project.
// It searches the TypeScript root directory (which may be a subdirectory in a
// monorepo) for next.config.* files or a package.json with a "next" dependency.
func detectNextJS(repoPath string) bool {
	tsRoot, _ := findTSRoot(repoPath)
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

func nodeText(node *sitter.Node, src []byte) string {
	return string(src[node.StartByte():node.EndByte()])
}

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

// buildImportSymbols returns a map of locally-bound import name → canonical symbol
// fact name for named imports from internal modules. It lets bare calls to
// imported functions (e.g. `formatName()`) resolve to the callee's declaration
// fact. Symbols declared in an imported module are named "<moduleDir>.<exportName>",
// where moduleDir is the directory of the resolved module file — this matches the
// common file-module case (e.g. import "./utils" → utils.ts → "<dir>.foo").
func buildImportSymbols(kinds *tsutil.KindTable, root *sitter.Node, src []byte, relFile string, aliases map[string]tsAlias) map[string]string {
	fileDir := factpath.Dir(relFile)
	m := make(map[string]string)
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
		if isExternal {
			continue // external modules have no local declaration facts
		}
		moduleDir := factpath.Dir(resolved)

		clause := findChildByKind(kinds, child, "import_clause")
		if clause == nil {
			continue
		}
		named := findChildByKind(kinds, clause, "named_imports")
		if named == nil {
			continue // default/namespace imports are not resolved
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
			m[local] = moduleDir + "." + exportName
		}
	}
	return m
}

// collectTSFileRefs performs a whole-file reference pass for the dead-code detector.
//
// The per-function call walk (collectCallsWithMetrics) only records call_expression
// edges inside function bodies, so it misses the ways a React/CommonJS codebase
// actually uses a symbol: rendering a component in JSX (<Foo/>), passing an imported
// identifier as a value (route configs — `{ component: Foo }`), namespace member
// access (`ns.foo`), and require()-bound names. Many of these live at module scope
// (top-level route arrays), which has no enclosing symbol fact to hang an edge on.
//
// We fold every such reference into a single KindFileRef fact — the reference-only
// carrier the dead-code detector already consumes for top-level references — so
// genuinely-used code is not mis-reported as dead. References are matched downstream
// by short name, so binding a local import name to "<moduleDir>.<name>" is enough
// even when the canonical declaration lives behind a folder index (the last segment
// still matches). This only ever ADDS references, so it can hide a real orphan but
// never invent a false one — the detector's deliberate conservative bias.
//
// kind selects the emitted fact kind: facts.KindFileRef for the production
// file-scope pass, facts.KindTestRef when the caller is ExtractTestRefs walking a
// test/spec file. The walk and resolvers are identical either way — a reference
// from a test is spelled exactly as the same reference from production code.
func (e *TSExtractor) collectTSFileRefs(kinds *tsutil.KindTable, root *sitter.Node, ctx *extractCtx, aliases map[string]tsAlias, kind string) []facts.Fact {
	src := ctx.src
	fileDir := factpath.Dir(ctx.relFile)
	internal := make(map[string]string)   // local name -> canonical target (internal modules only)
	namespaces := make(map[string]string) // `import * as ns` local -> module dir
	var reexports []string                // canonical targets re-exported via `export { x } from './y'`
	var defaultRefs []string              // default-export targets of default-imported modules

	bind := func(local, moduleDir, exportName string) {
		if local != "" {
			internal[local] = moduleDir + "." + exportName
		}
	}
	// resolveModule resolves an import specifier to its owning module directory and,
	// when the backing file is known, that file's path (for computing the module's
	// default-export name). ok is false for external modules. It prefers the exact
	// file/folder-index from the known-files set — so a folder module resolves to the
	// folder itself, not its parent — falling back to filepath.Dir when the target
	// isn't indexed (still lets short-name matching link named imports).
	resolveModule := func(node *sitter.Node) (moduleDir, indexPath string, ok bool) {
		if node == nil {
			return "", "", false
		}
		importPath := strings.Trim(nodeText(node, src), `"'`)
		resolved, isExternal := resolveImportPath(importPath, fileDir, aliases)
		if isExternal {
			return "", "", false
		}
		if idx, dir, found := resolveModuleFile(resolved, ctx.knownFiles); found {
			return dir, idx, true
		}
		return factpath.Dir(resolved), "", true
	}

	// Pass 1: parse the bindings — static imports, `export … from` re-exports, and
	// require()/dynamic-import assignments — into name → target maps.
	for i := range root.ChildCount() {
		child := root.Child(i)
		switch kindOf(kinds, child) {
		case "import_statement":
			moduleDir, indexPath, ok := resolveModule(findChildByKind(kinds, child, "string"))
			if !ok {
				continue
			}
			clause := findChildByKind(kinds, child, "import_clause")
			if clause == nil {
				continue
			}
			for j := range clause.ChildCount() {
				c := clause.Child(j)
				switch kindOf(kinds, c) {
				case "identifier": // default import: `import Foo from './x'`
					local := nodeText(c, src)
					bind(local, moduleDir, local)
					// A default import IS a use of the module's default export, whose
					// symbol is named by fileSymbolName (an anonymous
					// `export default connect(...)(X)` in a folder index becomes
					// "<Folder>Index" — unmatchable by the local name). Record it so the
					// wrapper symbol is not falsely reported dead.
					if indexPath != "" {
						defaultRefs = append(defaultRefs, moduleDir+"."+fileSymbolName(indexPath))
					}
				case "namespace_import": // `import * as ns from './x'`
					if id := findChildByKind(kinds, c, "identifier"); id != nil {
						namespaces[nodeText(id, src)] = moduleDir
					}
				case "named_imports":
					for k := range c.ChildCount() {
						spec := c.Child(k)
						if kindOf(kinds, spec) != "import_specifier" {
							continue
						}
						nameNode := spec.ChildByFieldName("name")
						if nameNode == nil {
							continue
						}
						exportName := nodeText(nameNode, src)
						local := exportName
						if a := spec.ChildByFieldName("alias"); a != nil {
							local = nodeText(a, src)
						}
						bind(local, moduleDir, exportName)
					}
				}
			}
		case "export_statement":
			// `export { a, default as b } from './y'` re-exports y's symbols; record a
			// reference to each so a symbol consumed only through a barrel is not
			// mis-reported as dead (matched by short name downstream).
			moduleDir, indexPath, ok := resolveModule(child.ChildByFieldName("source"))
			if !ok {
				continue
			}
			if clause := findChildByKind(kinds, child, "export_clause"); clause != nil {
				for k := range clause.ChildCount() {
					spec := clause.Child(k)
					if kindOf(kinds, spec) != "export_specifier" {
						continue
					}
					nameNode := spec.ChildByFieldName("name")
					if nameNode == nil {
						continue
					}
					// `export { default as X } from './y'` re-exports y's default; the
					// literal name "default" matches no symbol, so resolve it to y's
					// default-export name (fileSymbolName) instead.
					if name := nodeText(nameNode, src); name == "default" && indexPath != "" {
						reexports = append(reexports, moduleDir+"."+fileSymbolName(indexPath))
					} else {
						reexports = append(reexports, moduleDir+"."+name)
					}
				}
			}
		case "lexical_declaration", "variable_declaration":
			// CommonJS: `const x = require('./y')` / `const { a } = require('./y')`.
			for j := range child.ChildCount() {
				d := child.Child(j)
				if kindOf(kinds, d) != "variable_declarator" {
					continue
				}
				val := d.ChildByFieldName("value")
				if val == nil || kindOf(kinds, val) != "call_expression" {
					continue
				}
				fn := val.ChildByFieldName("function")
				if fn == nil || nodeText(fn, src) != "require" {
					continue
				}
				moduleDir, _, ok := resolveModule(findChildByKind(kinds, val.ChildByFieldName("arguments"), "string"))
				if !ok {
					continue
				}
				nameNode := d.ChildByFieldName("name")
				if nameNode == nil {
					continue
				}
				switch kindOf(kinds, nameNode) {
				case "identifier":
					local := nodeText(nameNode, src)
					bind(local, moduleDir, local)
				case "object_pattern":
					for k := range nameNode.ChildCount() {
						if p := nameNode.Child(k); kindOf(kinds, p) == "shorthand_property_identifier_pattern" {
							nm := nodeText(p, src)
							bind(nm, moduleDir, nm)
						}
					}
				}
			}
		}
	}

	// Pass 2: walk the whole tree collecting references — to the imported/required
	// bindings above and, in call positions, to same-module declarations.
	var targets []string
	seen := make(map[string]bool)
	add := func(t string) {
		if t != "" && !seen[t] {
			seen[t] = true
			targets = append(targets, t)
		}
	}
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil {
			return
		}
		switch kindOf(kinds, n) {
		case "import_statement":
			return // binding sites, not uses
		case "identifier", "type_identifier":
			// type_identifier covers an imported type/interface used only as an
			// annotation (`repo: Repo`), which is otherwise never an edge.
			if t, ok := internal[nodeText(n, src)]; ok {
				add(t)
			}
			return
		case "member_expression":
			obj := n.ChildByFieldName("object")
			prop := n.ChildByFieldName("property")
			if obj != nil && kindOf(kinds, obj) == "identifier" {
				name := nodeText(obj, src)
				if dir, ok := namespaces[name]; ok && prop != nil {
					add(dir + "." + nodeText(prop, src)) // ns.foo -> <dir>.foo
					return
				}
				if t, ok := internal[name]; ok {
					add(t) // Foo.bar on imported Foo marks Foo used
				}
			}
			for i := range n.ChildCount() {
				walk(n.Child(i))
			}
			return
		case "jsx_opening_element", "jsx_self_closing_element":
			if nameNode := n.ChildByFieldName("name"); nameNode != nil {
				add(resolveJSXTag(kinds, nameNode, src, ctx.dir, internal, namespaces))
			}
			for i := range n.ChildCount() {
				walk(n.Child(i))
			}
			return
		case "call_expression":
			// A bare callee and identifier arguments are USE positions (never
			// declarations), so it is safe to resolve them same-module as well as via
			// imports. This catches module-scope calls the per-function walk cannot see
			// (`startSession()` at file top level) and functions passed as values
			// (`connect(mapStateToProps, actions)`, HOC/callback wiring) — otherwise a
			// symbol used only that way is falsely reported dead.
			if fn := n.ChildByFieldName("function"); fn != nil && kindOf(kinds, fn) == "identifier" {
				add(resolveLocalOrImport(nodeText(fn, src), ctx.dir, internal))
			}
			if args := n.ChildByFieldName("arguments"); args != nil {
				for i := range args.ChildCount() {
					if a := args.Child(i); kindOf(kinds, a) == "identifier" {
						add(resolveLocalOrImport(nodeText(a, src), ctx.dir, internal))
					}
				}
			}
			for i := range n.ChildCount() {
				walk(n.Child(i))
			}
			return
		}
		for i := range n.ChildCount() {
			walk(n.Child(i))
		}
	}
	walk(root)

	for _, t := range reexports {
		add(t)
	}
	for _, t := range defaultRefs {
		add(t)
	}
	if len(targets) == 0 {
		return nil
	}

	rels := make([]facts.Relation, 0, len(targets))
	for _, t := range targets {
		rels = append(rels, facts.Relation{Kind: facts.RelCalls, Target: t})
	}
	return []facts.Fact{{
		Kind:      kind,
		Name:      ctx.relFile,
		File:      ctx.relFile,
		Line:      1,
		Props:     map[string]any{"language": "typescript"},
		Relations: rels,
	}}
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
	recursive          bool            // body directly calls the enclosing function
	ioDirect           bool            // body directly invokes a network/file I/O primitive
	fieldsWritten      []string        // distinct `this.<name>` targets the body assigns to
	writeSeen          map[string]bool // dedup set for fieldsWritten
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
// propagated transitively into performs_io by computeTSPerformsIO, mirroring Swift.

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
	// These seed io_direct, which computeTSPerformsIO then propagates transitively into
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
// Sibling to buildImportSymbols, which drops external and default imports entirely.
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
// call on a known I/O receiver (axios.get, fs.readFile), a network-imported binding, or
// an I/O method name (navigator.sendBeacon).
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
		return tsIOReceivers[root] || ioBindings[root] || tsIOMemberMethods[prop]
	}
	return false
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
	src []byte
	// kinds names node types for the grammar this file was parsed with. A field is
	// safe here, unlike on TSExtractor, because a walker belongs to one parse;
	// TSExtractor is shared across the goroutines parallel.MapFiles runs.
	kinds               *tsutil.KindTable
	dir, className      string
	importMap           map[string]string
	ioBindings          map[string]bool
	selfName, selfShort string
	metrics             *tsBodyMetrics
	loopDepth           int
	scalingDepth        int // current nesting counting only input-scaling (unbounded) loops
	// repeatDepth counts the enclosing loops that run a non-constant number of times. It
	// differs from scalingDepth for `while (true)`, which adds no factor of n but whose
	// body still runs many times — so a query inside it is still an N+1 candidate.
	repeatDepth int
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
		saved, savedScaling, savedRepeat := w.loopDepth, w.scalingDepth, w.repeatDepth
		w.loopDepth, w.scalingDepth, w.repeatDepth = 0, 0, 0
		for i := range n.ChildCount() {
			w.walk(n.Child(i))
		}
		w.loopDepth, w.scalingDepth, w.repeatDepth = saved, savedScaling, savedRepeat
		return
	}

	// Complexity metrics: count decision points so the single body walk doubles as
	// the cyclomatic pass.
	if w.metrics != nil {
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

	// Syntactic loops: everything in the body runs per iteration. A loop over a literal
	// collection (for..of [a,b,c]) or an infinite while(true)/do..while(true) event/retry
	// loop is bounded — it raises loop_depth but not scaling_loop_depth (the Big-O exponent).
	switch kind {
	case "for_statement", "for_in_statement", "while_statement", "do_statement":
		bounded := tsLoopBounded(w.kinds, n, w.src)
		repeats := tsLoopRepeats(w.kinds, n)
		if w.metrics != nil {
			w.metrics.loopCount++
			w.metrics.decisions++
			if w.loopDepth+1 > w.metrics.loopDepth {
				w.metrics.loopDepth = w.loopDepth + 1
			}
			if !bounded && w.scalingDepth+1 > w.metrics.scalingLoopDepth {
				w.metrics.scalingLoopDepth = w.scalingDepth + 1
			}
		}
		w.loopDepth++
		if !bounded {
			w.scalingDepth++
		}
		if repeats {
			w.repeatDepth++
		}
		for i := range n.ChildCount() {
			w.walk(n.Child(i))
		}
		w.loopDepth--
		if !bounded {
			w.scalingDepth--
		}
		if repeats {
			w.repeatDepth--
		}
		return
	}

	if kind == "call_expression" {
		// Seed the performs_io closure: flag the enclosing body when it directly
		// invokes a network/file I/O primitive. Independent of loop depth — a wrapper
		// calls its I/O sink once, and the transitive pass carries the signal upward.
		if w.metrics != nil && !w.metrics.ioDirect && tsIsIOCall(w.kinds, n, w.src, w.ioBindings) {
			w.metrics.ioDirect = true
		}
		if target := resolveTSCall(w.kinds, n, w.src, w.dir, w.className, w.importMap); target != "" {
			if !w.seen[target] {
				w.seen[target] = true
				w.rels = append(w.rels, facts.Relation{Kind: facts.RelCalls, Target: target})
			}
			w.recordCall(target)
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
		// An array-iterator method with a callback (items.map(cb)) is a loop: its
		// callback body runs per element, but the receiver/other args run once.
		if w.metrics != nil {
			if cb := tsIteratorCallback(w.kinds, n, w.src); cb != nil {
				// A `[a,b,c].map(cb)` over a literal receiver iterates a fixed count — it
				// raises loop_depth but not the scaling depth. The actual w.loopDepth /
				// w.scalingDepth bump happens at the callback body inside walkCallbackSubtree;
				// here we only record the maxes.
				bounded := tsIteratorReceiverBounded(w.kinds, n, w.src)
				w.metrics.loopCount++
				w.metrics.decisions++
				if w.loopDepth+1 > w.metrics.loopDepth {
					w.metrics.loopDepth = w.loopDepth + 1
				}
				if !bounded && w.scalingDepth+1 > w.metrics.scalingLoopDepth {
					w.metrics.scalingLoopDepth = w.scalingDepth + 1
				}
				for i := range n.ChildCount() {
					if c := n.Child(i); tsByteContains(c, cb) {
						w.walkCallbackSubtree(c, cb, bounded)
					} else {
						w.walk(c)
					}
				}
				return
			}
		}
	}

	// `new WebSocket(...)` / `new XMLHttpRequest()` opens a network/stream resource —
	// tag the enclosing body io_direct (constructors are new_expression, not call_expression).
	if kind == "new_expression" && w.metrics != nil && !w.metrics.ioDirect {
		if ctor := n.ChildByFieldName("constructor"); ctor != nil && tsIOConstructors[nodeText(ctor, w.src)] {
			w.metrics.ioDirect = true
		}
	}

	// A `this.<member>` reference inside a class method marks that member used, even
	// when it is not called: React binds event handlers as prop VALUES
	// (onClick={this.handleClick}), so a handler referenced only in JSX has no call
	// edge and would otherwise be mis-reported dead. className is the exact class
	// symbol name, so the target matches the method fact "<dir>.<Class>.<member>".
	if kind == "member_expression" && w.className != "" {
		if obj := n.ChildByFieldName("object"); obj != nil && kindOf(w.kinds, obj) == "this" {
			if prop := n.ChildByFieldName("property"); prop != nil {
				target := w.dir + "." + w.className + "." + nodeText(prop, w.src)
				if !w.seen[target] {
					w.seen[target] = true
					w.rels = append(w.rels, facts.Relation{Kind: facts.RelCalls, Target: target})
				}
			}
		}
	}

	for i := range n.ChildCount() {
		w.walk(n.Child(i))
	}
}

// walkCallbackSubtree descends toward an iterator's callback, bumping the loop depth
// exactly at the callback (its body is per-iteration) while walking everything else
// (the receiver, sibling args) at the current depth. When the iterator's receiver is a
// literal (bounded), the scaling depth is NOT bumped — the callback runs a fixed number
// of times, so it does not add a factor of n to Big-O.
func (w *tsBodyWalker) walkCallbackSubtree(n, cb *sitter.Node, bounded bool) {
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
		w.loopDepth++
		if !bounded {
			w.scalingDepth++
			w.repeatDepth++
		}
		for i := range cb.ChildCount() {
			w.walk(cb.Child(i))
		}
		w.loopDepth--
		if !bounded {
			w.scalingDepth--
			w.repeatDepth--
		}
		return
	}
	for i := range n.ChildCount() {
		if c := n.Child(i); tsByteContains(c, cb) {
			w.walkCallbackSubtree(c, cb, bounded)
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

func collectCallsWithMetrics(kinds *tsutil.KindTable, node *sitter.Node, src []byte, dir, className string, importMap map[string]string, ioBindings map[string]bool, selfName, selfShort string) ([]facts.Relation, *tsBodyMetrics) {
	m := &tsBodyMetrics{}
	w := &tsBodyWalker{src: src, kinds: kinds, dir: dir, className: className, importMap: importMap, ioBindings: ioBindings, selfName: selfName, selfShort: selfShort, metrics: m, seen: make(map[string]bool)}
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
	}
	if m.recursive {
		props["recursive_self"] = true
	}
	if m.ioDirect {
		props["io_direct"] = true
	}
}

// tsLoopBounded reports whether a syntactic loop's trip count is independent of the input
// size, so it adds no factor of n: a for..of / for..in over an array/object literal, or an
// infinite while(true) / do..while(true) event/retry loop. A C-style for(;;) is treated as
// unbounded (its bound is not statically evident).
//
// It does NOT mean the loop runs a constant number of times — see tsLoopRepeats.
func tsLoopBounded(kinds *tsutil.KindTable, n *sitter.Node, src []byte) bool {
	return tsLoopConstant(kinds, n) || tsLoopInfinite(kinds, n, src)
}

// tsLoopRepeats reports whether the loop body runs a non-constant number of times, so a
// call inside it is an N+1 candidate. Only a genuinely constant loop is excluded: a
// `while (true) { row = await fetch(id) }` reconnect/retry loop queries once per
// iteration even though its depth is discounted. Scaling and repeating differ.
func tsLoopRepeats(kinds *tsutil.KindTable, n *sitter.Node) bool {
	return !tsLoopConstant(kinds, n)
}

// tsLoopConstant reports whether a for..of / for..in iterates an array/object literal —
// a compile-time-fixed element count. A while/do loop never qualifies.
func tsLoopConstant(kinds *tsutil.KindTable, n *sitter.Node) bool {
	if kindOf(kinds, n) != "for_in_statement" {
		return false
	}
	r := n.ChildByFieldName("right")
	return r != nil && tsIterableLiteral(kinds, r)
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
// literal receiver (`[a,b,c].map(...)`), so the callback runs a fixed number of times.
func tsIteratorReceiverBounded(kinds *tsutil.KindTable, n *sitter.Node, src []byte) bool {
	fn := n.ChildByFieldName("function")
	if fn == nil || kindOf(kinds, fn) != "member_expression" {
		return false
	}
	obj := fn.ChildByFieldName("object")
	return obj != nil && tsIterableLiteral(kinds, obj)
}

// tsIterableLiteral reports whether a node is an array/object literal (a fixed-size iterable).
func tsIterableLiteral(kinds *tsutil.KindTable, n *sitter.Node) bool {
	switch kindOf(kinds, n) {
	case "array", "object":
		return true
	}
	return false
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

// computeTSPerformsIO propagates the walk-time io_direct flag transitively across the
// call graph into a performs_io prop, so a function that reaches network/file I/O only
// through helpers is still flagged — the signal the performance analyzer reads to catch a
// per-iteration network call behind a wrapper. Mirrors the Swift computePerformsIO, but
// simpler: TS call targets are already canonical fact names, so no bare-name fan-out is
// needed. A monotone fixpoint (only ever flips false→true) makes it cycle-safe.
func computeTSPerformsIO(allFacts []facts.Fact) {
	// Index symbol facts by name (a name may map to >1 fact) and record which names exist.
	exists := make(map[string]bool)
	for i := range allFacts {
		if allFacts[i].Kind == facts.KindSymbol {
			exists[allFacts[i].Name] = true
		}
	}

	io := make(map[string]bool)      // name → performs I/O (directly or transitively)
	adj := make(map[string][]string) // name → called names that are known symbols
	for i := range allFacts {
		f := &allFacts[i]
		if f.Kind != facts.KindSymbol {
			continue
		}
		if b, _ := f.PropAny("io_direct").(bool); b {
			io[f.Name] = true
		}
		seen := make(map[string]bool)
		for _, r := range f.Relations {
			if r.Kind != facts.RelCalls || r.Target == f.Name || seen[r.Target] || !exists[r.Target] {
				continue
			}
			seen[r.Target] = true
			adj[f.Name] = append(adj[f.Name], r.Target)
		}
	}

	// Fixpoint: a name performs I/O if any callee does. Monotone, so it terminates
	// even with call cycles (a no-I/O cycle simply stays false).
	for changed := true; changed; {
		changed = false
		for name, callees := range adj {
			if io[name] {
				continue
			}
			for _, c := range callees {
				if io[c] {
					io[name] = true
					changed = true
					break
				}
			}
		}
	}

	for i := range allFacts {
		f := &allFacts[i]
		if f.Kind == facts.KindSymbol && io[f.Name] {
			if f.Props == nil {
				f.Props = map[string]any{}
			}
			f.SetProp("performs_io", true)
		}
	}
}

// resolveTSCall resolves a single call_expression to a canonical target fact name,
// or "" when the call cannot be resolved (e.g. a method call on a value of unknown
// type). It resolves:
//   - bare calls `foo()` → imported symbol via importMap, else same-module "<dir>.foo"
//   - `this.method()` inside a class → "<dir>.<className>.method"
func resolveTSCall(kinds *tsutil.KindTable, call *sitter.Node, src []byte, dir, className string, importMap map[string]string) string {
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
