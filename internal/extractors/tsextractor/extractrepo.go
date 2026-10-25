package tsextractor

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"unicode/utf8"

	"github.com/enola-labs/enola/internal/extractors/extcoverage"
	"github.com/enola-labs/enola/internal/extractors/ioclosure"
	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/parallel"
)

// Extract parses TypeScript/TSX files and emits architectural facts.
func (e *TSExtractor) Extract(ctx context.Context, repoPath string, files []string) ([]facts.Fact, error) {
	tsRoot, _ := findTSRoot(repoPath)
	fw, isPrisma := detectTSFrameworks(repoPath, tsRoot, files)

	// Parse tsconfig.json path aliases, one root per package for monorepos.
	aliasRoots := collectTSAliasRoots(repoPath)
	// SvelteKit maps $lib by convention and may declare literal aliases in its
	// config even before `svelte-kit sync` has generated a tsconfig.
	if fw.isSvelteKit {
		aliasRoots = withSvelteKitAliasFallbacks(repoPath, aliasRoots)
	}

	tsFiles, htmlFiles, knownFiles := partitionTSFiles(files, fw.isAngular)
	sources := readTSSources(ctx, repoPath, tsFiles)
	rc := newRepoCtx(fw, tsFiles, sources, knownFiles)
	perFile := e.extractFiles(ctx, tsFiles, sources, rc, aliasRoots)
	templates := scanAngularTemplates(ctx, repoPath, htmlFiles)

	a := assembleFileResults(perFile, tsFiles)
	a.compose(repoPath, rc, templates)
	a.facts = append(a.facts, a.coverageFacts(repoPath)...)

	// One account per declared client in this repository, including a client that found
	// nothing here; see clientCoverageFacts.
	if len(e.clients) > 0 {
		a.facts = append(a.facts, clientCoverageFacts(repoPath, e.clients, a.clients)...)
	}
	// Prisma models live in schema.prisma — a separate DSL, so tree-sitter never sees it.
	// Read it off-glob, the same way package.json and tsconfig.json already are.
	if isPrisma {
		a.facts = append(a.facts, extractPrismaStorage(repoPath, tsRoot)...)
	}
	a.facts = append(a.facts, moduleFacts(repoPath, a.modules, fw.isAngular, packageDocs(tsFiles, sources))...)
	return a.facts, nil
}

func detectTSFrameworks(repoPath, tsRoot string, files []string) (tsFrameworks, bool) {
	// ORM detection is gated on the package.json dependency, exactly as Vue/Nuxt are, so
	// a class coincidentally decorated @Entity models nothing in a repo without TypeORM.
	isTypeORM, isDrizzle, isPrisma := detectORMs(repoPath, tsRoot, files)
	fw := tsFrameworks{
		isNextJS:    detectNextJSRoot(repoPath, tsRoot),
		nextDirs:    nextJSPackageDirs(repoPath),
		isVue:       detectVueRoot(repoPath, tsRoot),
		isNuxt:      detectNuxtRoot(repoPath, tsRoot),
		isSvelteKit: detectSvelteKitRoot(repoPath, tsRoot),
		isEmber:     detectEmberRoot(repoPath, tsRoot),
		isReactNav:  detectReactNavigationRoot(repoPath, tsRoot),
		isAngular:   detectAngularRoot(repoPath, tsRoot),
		isReact:     hasPkgDependency(tsRoot, "react") || (tsRoot != repoPath && hasPkgDependency(repoPath, "react")),
		orms:        ormFlags{typeORM: isTypeORM, drizzle: isDrizzle},
	}
	fw.appDirs = reachAppDirs(repoPath, fw)
	return fw, isPrisma
}

// Restrict to TypeScript files once, then parse them in parallel. The
// framework flags and path aliases are read-only, and extractFile is a
// pure function of (src, relFile, …), so per-file work is independent. Results
// are merged in file order for deterministic output.
func partitionTSFiles(files []string, isAngular bool) (tsFiles, htmlFiles []string, knownFiles map[string]bool) {
	knownFiles = make(map[string]bool)
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
	return tsFiles, htmlFiles, knownFiles
}

// Read each source once, in parallel. GraphQL context needs a repository-wide
// view before extraction, but it consumes these bytes rather than performing a
// serial pre-pass that rereads every file from disk.
func readTSSources(ctx context.Context, repoPath string, tsFiles []string) map[string][]byte {
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
	return sources
}

func newRepoCtx(fw tsFrameworks, tsFiles []string, sources map[string][]byte, knownFiles map[string]bool) *repoCtx {
	rc := &repoCtx{
		tsFrameworks: fw,
		knownFiles:   knownFiles,
		files:        newTSFileIndex(knownFiles),
		// Repo-wide pre-pass: resolve generated gRPC-web client stubs (service FQN +
		// RPC methods + client class) so per-file call-site detection can map a
		// `client.method(...)` call to its "/pkg.Service/Method" route. Built before
		// the parallel pass because a client's stub and its call sites usually live
		// in different files.
		grpcStubs:     buildGRPCStubIndex(tsFiles, sources),
		graphqlServer: detectGraphQLServerUsage(tsFiles, sources),
		// Exported request helpers, read before the parallel pass for the same reason:
		// a helper and the calls to it live in different files.
		requestHelpers: buildRequestHelperIndex(tsFiles, sources),
	}
	if fw.isNuxt {
		rc.nuxtAutoComponents = nuxtAutoComponentIndex(knownFiles)
	}
	return rc
}

func (e *TSExtractor) extractFiles(ctx context.Context, tsFiles []string, sources map[string][]byte, rc *repoCtx, aliasRoots []tsAliasRoot) []tsFileResult {
	return parallel.MapFiles(ctx, tsFiles, func(relFile string) tsFileResult {
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
		if isBinarySource(src) {
			log.Printf("[ts-extractor] skipping binary file %s", relFile)
			return tsFileResult{}
		}
		aliases := aliasesForDir(aliasRoots, factpath.Dir(relFile))
		res := e.extractFile(src, relFile, rc, aliases)
		// Routers, mounts and held-back routes for the repo-wide mount pass below.
		// Collected here because resolving an import needs this file's path aliases,
		// which are in scope only during the per-file walk. Same test-path gate as
		// the routes themselves: an e2e suite that mounts its own app contributes
		// no production surface.
		if !facts.IsTestPath(relFile) {
			res.routers = collectRouterFile(src, relFile, aliases, rc.knownFiles)
			res.globalPrefixes = scanGlobalPrefixes(src)
		}
		return res
	})
}

// Templates are scanned in parallel with no parser: an Angular template is not
// HTML once its @if/@for blocks are in it, and both dialects are live in the
// same repositories.
func scanAngularTemplates(ctx context.Context, repoPath string, htmlFiles []string) map[string]*angularTemplate {
	templates := make(map[string]*angularTemplate, len(htmlFiles))
	if len(htmlFiles) == 0 {
		return templates
	}
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
	return templates
}

type tsAssembly struct {
	facts           []facts.Fact
	reach           []*fileReach // what each file uses; see reach.go
	modules         map[string]bool
	routerFiles     []*routerFile
	angularRouters  []*angularRouterFile
	angularHTTP     []*angularHTTPFile
	inlineTemplates map[string]*angularTemplate
	globalPrefixes  []string
	clients         clientCounts
	hosts           clientHosts

	angularDI, angularRoutes, angularRequests, angularTemplates angularCounts
}

// Group files by directory for module detection. Files that produced no
// facts (unreadable, or skipped as minified/bundled) do not register a
// module, so a directory containing only skipped bundles (e.g. a vendored
// scripts dir) stays out of the graph rather than surfacing as an empty module.
func assembleFileResults(perFile []tsFileResult, tsFiles []string) *tsAssembly {
	a := &tsAssembly{
		modules:         make(map[string]bool),
		routerFiles:     make([]*routerFile, 0, len(perFile)),
		inlineTemplates: map[string]*angularTemplate{},
		clients:         clientCounts{},
	}
	for i, res := range perFile {
		if res.routers != nil {
			a.routerFiles = append(a.routerFiles, res.routers)
		}
		a.globalPrefixes = append(a.globalPrefixes, res.globalPrefixes...)
		a.angularDI.merge(res.angular)
		for name, t := range res.angularInline {
			a.inlineTemplates[name] = t
		}
		if res.angularRouter != nil {
			a.angularRouters = append(a.angularRouters, res.angularRouter)
		}
		if res.angularHTTP != nil {
			a.angularHTTP = append(a.angularHTTP, res.angularHTTP)
		}
		a.clients.merge(res.clients)
		a.hosts.merge(res.hosts)
		if res.reach != nil {
			a.reach = append(a.reach, res.reach)
		}
		if len(res.facts) == 0 {
			continue
		}
		a.facts = append(a.facts, res.facts...)
		a.modules[factpath.Dir(tsFiles[i])] = true
	}
	return a
}

func (a *tsAssembly) addWithModules(fs []facts.Fact) {
	a.facts = append(a.facts, fs...)
	for _, f := range fs {
		a.modules[factpath.Dir(f.File)] = true
	}
}

func (a *tsAssembly) compose(repoPath string, rc *repoCtx, templates map[string]*angularTemplate) {
	// Express sub-routers mounted from another file. The per-file pass holds their
	// routes back rather than emitting a fragment path; this is where both halves
	// are visible and the true runtime path can be composed. See routermount.go.
	a.addWithModules(composeRouterMounts(a.routerFiles, stringConstReader(repoPath)))
	bindServerRouteHandlers(a.facts, a.routerFiles)
	a.facts = nextRouteVerbs(a.facts)
	bindFileRouteHandlers(a.facts)
	for i := range a.facts {
		a.facts[i].DelProp(propDefaultExport)
	}
	a.facts = append(a.facts, svelteKitActionRoutes(a.facts)...)

	if prefix := applyNestGlobalPrefix(a.facts, a.globalPrefixes); prefix != "" {
		log.Printf("[ts-extractor] NestJS routes served under global prefix /%s", prefix)
	}

	// Serial post-pass: propagate the per-body io_direct flag transitively across the
	// call graph into performs_io, so wrapper-hidden network/file I/O is visible to the
	// performance analyzer.
	if rc.isNuxt {
		resolveNuxtAutoComposableCalls(a.facts)
	}
	resolveMemberTypedCalls(a.facts)
	settleIONamedCalls(a.facts, a.modules)
	ioclosure.PropagateWith(a.facts, ioclosure.Options{
		Declared:        declaredImplementers(a.facts),
		BodilessMembers: true,
		Apart:           settersBesideGetters(a.facts),
	})

	// Engine-relative routes compose onto their mount point here, where every
	// mount in the repo is visible; a per-file pass cannot see both sides.
	if rc.isEmber {
		composeEngineMounts(a.facts)
	}

	// Angular's routes are composed here, where every route array and every lazy
	// mount in the repository is visible; a per-file pass can see only one end of a
	// loadChildren. See angularroutes.go.
	routes, c := composeAngularRoutes(a.angularRouters)
	a.addWithModules(routes)
	a.angularRoutes.merge(c)

	// Templates are joined to the components that own them here, where the members of
	// every class and the selector every component declared are both in hand.
	a.angularTemplates = attachAngularTemplates(a.facts, templates, a.inlineTemplates)

	// Requests through an injected HttpClient are composed here: a base URL is a
	// static of one service and named by many, so the constants are only all in hand
	// once the repository is read. See angularhttp.go.
	reqs, c := composeAngularRequests(a.angularHTTP)
	a.addWithModules(reqs)
	a.angularRequests.merge(c)

	// Every injects edge is made to name a symbol this snapshot holds, now that the
	// whole repository is assembled; see reconcileAngularInjects.
	if rc.isAngular {
		a.angularDI.merge(reconcileAngularInjects(a.facts))
		a.angularRoutes.merge(resolveAngularLazyComponents(a.facts))
	}

	// Which client calls the application can run, now that every file's uses and
	// every page route are in hand. See reach.go.
	markClientReachability(a.facts, a.reach, rc.tsFrameworks, rc.knownFiles)
}

// What each pass could name, and what it could not, by cause. Emitted only when
// there were sites at all — a zero from an extractor that never looked reads the
// same as a zero from one that did, which is the whole failure this fact exists
// to prevent.
func (a *tsAssembly) coverageFacts(repoPath string) []facts.Fact {
	var out []facts.Fact
	for _, c := range []struct {
		extractor, edge string
		counts          angularCounts
	}{
		{"typescript:angular-di", "angular_inject", a.angularDI},
		{"typescript:angular-templates", "angular_template_ref", a.angularTemplates},
		{"typescript:angular-requests", "angular_http_call", a.angularRequests},
		{"typescript:angular-routes", "angular_route", a.angularRoutes},
	} {
		if c.counts.total() == 0 {
			continue
		}
		if f, ok := extcoverage.Fact(repoPath, c.extractor, c.edge, c.counts.resolved, c.counts.unresolved); ok {
			out = append(out, f)
		}
	}
	if f, ok := extcoverage.Fact(repoPath, "typescript:http-client", "typescript_client_host",
		a.hosts.kept, map[string]int{clientHostInterpolated: a.hosts.interpolated}); ok {
		out = append(out, f)
	}
	return out
}

func moduleFacts(repoPath string, modules map[string]bool, isAngular bool, docs map[string]string) []facts.Fact {
	pkgNames, publishedPkgs := collectPackageNames(repoPath)
	// The workspace project each directory belongs to, where the repository states
	// one. A monorepo's unit of ownership is its project, not its directory, and
	// every reading that groups by unit was inferring the boundary from the path.
	var projects map[string]string
	if isAngular {
		projects = angularProjectNames(repoPath)
	}
	out := make([]facts.Fact, 0, len(modules))
	for dir := range modules {
		props := map[string]any{
			"language": "typescript",
		}
		// The npm package this directory belongs to. The cross-repo linker reads it
		// to recognize a repo's own @scope, so an import of a sibling package the
		// repo itself publishes is not mistaken for a dependency on another repo
		// that happens to be labeled like the scope.
		if pd, ok := nearestPackageDir(pkgNames, dir); ok {
			props["package_name"] = pkgNames[pd]
			if publishedPkgs[pd] {
				props[facts.PropPublished] = true
			}
		}
		if name := nearestProjectName(projects, dir); name != "" {
			props["workspace_project"] = name
		}
		if doc := docs[dir]; doc != "" {
			facts.SetDocProp(props, doc)
		}
		out = append(out, facts.Fact{
			Kind:  facts.KindModule,
			Name:  dir,
			File:  dir,
			Props: props,
		})
	}
	return out
}

// Binary data is a NUL byte plus bytes that are not UTF-8, in the first 8 KB. A
// video segment (MPEG-TS) is named .ts and is not TypeScript; a source file that
// keeps a NUL in a string as a separator is still valid UTF-8.
func isBinarySource(src []byte) bool {
	head := src[:min(len(src), 8000)]
	if bytes.IndexByte(head, 0) < 0 {
		return false
	}
	invalid := 0
	for len(head) > 0 {
		r, n := utf8.DecodeRune(head)
		if r == utf8.RuneError && n == 1 {
			if invalid++; invalid >= 8 {
				return true
			}
		}
		head = head[n:]
	}
	return false
}
