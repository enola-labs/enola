package goextractor

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"unicode"

	"github.com/enola-labs/enola/internal/extractors/detectnames"
	"github.com/enola-labs/enola/internal/extractors/ioclosure"
	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
)

// goBuiltins are Go's predeclared functions and type-conversion identifiers.
// Bare calls to these (e.g. len(x), make(...), string(b)) are not calls to a
// symbol, so resolving them would produce dangling phantom call edges.
var goBuiltins = map[string]bool{
	// Builtin functions.
	"append": true, "cap": true, "clear": true, "close": true, "complex": true,
	"copy": true, "delete": true, "imag": true, "len": true, "make": true,
	"max": true, "min": true, "new": true, "panic": true, "print": true,
	"println": true, "real": true, "recover": true,
	// Predeclared types used as conversions.
	"string": true, "bool": true, "byte": true, "rune": true, "error": true,
	"any": true, "int": true, "int8": true, "int16": true, "int32": true,
	"int64": true, "uint": true, "uint8": true, "uint16": true, "uint32": true,
	"uint64": true, "uintptr": true, "float32": true, "float64": true,
	"complex64": true, "complex128": true,
}

// GoExtractor extracts architectural facts from Go source code using go/ast.
type GoExtractor struct{}

// New creates a new GoExtractor.
func New() *GoExtractor {
	return &GoExtractor{}
}

func (e *GoExtractor) Name() string {
	return "go"
}

// Detect returns true if the repository contains a go.mod file.
func (e *GoExtractor) Detect(repoPath string) (bool, error) {
	return e.DetectFiles(repoPath, detectnames.Walk(repoPath))
}

// DetectFiles implements plugin.FileListDetector.
//
// The rule this replaces was a go.mod at the REPOSITORY ROOT, which is not a depth
// bound but has the same effect and is easier to miss for it: ente keeps its server
// in server/go.mod and its CLI in cli/go.mod, so 493 Go files went unindexed in a
// repository enola was being asked to read as a cross-repo cluster — the Flutter
// client resolved, the Go backend it calls did not exist.
//
// A go.mod anywhere detects, and .go is accepted alongside it for a tree vendored
// or split without its own module file. Neither spelling belongs to another
// language.
func (e *GoExtractor) DetectFiles(_ string, files []string) (bool, error) {
	for _, rel := range files {
		if detectnames.Base(rel) == "go.mod" || strings.HasSuffix(rel, ".go") {
			return true, nil
		}
	}
	return false, nil
}

// OwnsFile declares the paths this extractor reads: Go source, plus the module
// manifests whose contents steer resolution (readModules reads go.mod). It
// makes the extractor cacheable like the other language extractors and lets
// the file census attribute unparsed .go files to it rather than reporting Go
// as an extension nothing claims.
func (e *GoExtractor) OwnsFile(relFile string) bool {
	base := filepath.Base(relFile)
	return strings.HasSuffix(relFile, ".go") || base == "go.mod" || base == "go.sum"
}

// parsedPkg holds parsing results for a single Go package directory.
type parsedPkg struct {
	pkgName     string
	relFiles    []string
	parsedFiles []*ast.File
	fileMap     map[string]*ast.File // relFile → *ast.File
}

// Extract parses Go files and emits architectural facts.
// It uses three global passes so that struct field types are visible across
// package boundaries within the same module — necessary for resolving
// multi-hop call chains like h.authLib.Service.Register.
func (e *GoExtractor) Extract(ctx context.Context, repoPath string, files []string) ([]facts.Fact, error) {
	var allFacts []facts.Fact
	fset := token.NewFileSet()
	mods := readModules(repoPath, files)

	// Pass 1: parse all Go files, grouping by package directory.
	parsedPkgs := make(map[string]*parsedPkg)
	for _, f := range files {
		if !strings.HasSuffix(f, ".go") {
			continue
		}
		pkgDir := factpath.Dir(f)
		pp := parsedPkgs[pkgDir]
		if pp == nil {
			pp = &parsedPkg{fileMap: make(map[string]*ast.File)}
			parsedPkgs[pkgDir] = pp
		}
		pp.relFiles = append(pp.relFiles, f)

		absFile := filepath.Join(repoPath, f)
		src, err := os.ReadFile(absFile)
		if err != nil {
			log.Printf("[go-extractor] error reading %s: %v", f, err)
			continue
		}
		parsed, err := parser.ParseFile(fset, absFile, src, parser.ParseComments)
		if err != nil {
			log.Printf("[go-extractor] error parsing %s: %v", f, err)
			continue
		}
		if pp.pkgName == "" {
			pp.pkgName = parsed.Name.Name
		}
		pp.parsedFiles = append(pp.parsedFiles, parsed)
		pp.fileMap[f] = parsed
	}

	pkgDirs := make([]string, 0, len(parsedPkgs))
	for pkgDir := range parsedPkgs {
		pkgDirs = append(pkgDirs, pkgDir)
	}
	sort.Strings(pkgDirs)

	// Build declared package-name map so buildFileImports can resolve implicit
	// aliases correctly (e.g. "go-auth" path base → "auth" package name).
	pkgNames := make(map[string]string)
	for _, pkgDir := range pkgDirs {
		if pp := parsedPkgs[pkgDir]; pp.pkgName != "" {
			pkgNames[pkgDir] = pp.pkgName
		}
	}

	// Pass 2: build a global field-type map from ALL packages in the module.
	// This allows cross-package field-chain resolution (e.g., a subpackage can
	// look up fields of a root-package struct).
	// Aliases first: a field or a result declared as an alias is a value of the type
	// the alias names, and that is where its methods are.
	aliases := make(map[string]string)
	for _, pkgDir := range pkgDirs {
		for k, v := range collectAliases(parsedPkgs[pkgDir].parsedFiles, pkgDir, mods, pkgNames) {
			aliases[k] = v
		}
	}
	globalFieldTypes := make(map[string]string)
	for _, pkgDir := range pkgDirs {
		select {
		case <-ctx.Done():
			return allFacts, ctx.Err()
		default:
		}
		for k, v := range collectFieldTypes(parsedPkgs[pkgDir].parsedFiles, pkgDir, mods, pkgNames, aliases) {
			globalFieldTypes[k] = v
		}
	}

	// Pass 2a: what every function in the module is declared to return, so a call
	// on a call's result (`eng.Resolver().NodeName(x)`) and a variable assigned from
	// one (`r := eng.Resolver()`) resolve to the method they reach.
	callTypes := callTypeTables{
		returns: make(map[string][]string), aliases: aliases, funcs: make(map[string]bool),
		elems: elemTypes{byType: make(map[string]string), byField: make(map[string]string)},
	}
	for _, pkgDir := range pkgDirs {
		collectElemTypes(parsedPkgs[pkgDir].parsedFiles, pkgDir, mods, pkgNames, aliases, callTypes.elems)
		for k, v := range collectReturnTypes(parsedPkgs[pkgDir].parsedFiles, pkgDir, mods, pkgNames, aliases) {
			callTypes.returns[k] = v
		}
		for name := range collectPackageFuncs(parsedPkgs[pkgDir].parsedFiles) {
			callTypes.funcs[pkgDir+"."+name] = true
		}
	}

	// Pass 2a': the interface assertions of every package, which are the only
	// statement of what implements what. They let a call on an interface reach the
	// I/O behind it: see markGoDirectIO and the closure at the end.
	callTypes.implementers = make(map[string][]string)
	callTypes.embeds = make(map[string][]string)
	for _, pkgDir := range pkgDirs {
		for iface, impls := range collectInterfaceAssertions(parsedPkgs[pkgDir].parsedFiles, pkgDir, mods, pkgNames, aliases) {
			callTypes.implementers[iface] = append(callTypes.implementers[iface], impls...)
		}
		for typ, embedded := range collectEmbeddedTypes(parsedPkgs[pkgDir].parsedFiles, pkgDir, mods, pkgNames, aliases) {
			callTypes.embeds[typ] = append(callTypes.embeds[typ], embedded...)
		}
	}

	// Pass 2a'': the implementers the source states without an assertion — a
	// constructor returning the interface — and the ones the method sets infer. See
	// ifaceimpl.go. Both are fed to the closure and recorded on the interface facts;
	// the recorded stated set includes the assertions and, like the inferred one, no
	// mock.
	decls := newTypeDecls()
	for _, pkgDir := range pkgDirs {
		collectTypeDecls(decls, parsedPkgs[pkgDir], pkgDir, mods, pkgNames, aliases)
	}
	stated, inferred := implementersFrom(decls, callTypes.embeds, callTypes.implementers)
	for _, found := range []map[string][]string{stated, inferred} {
		for iface, impls := range found {
			for _, t := range impls {
				if !contains(callTypes.implementers[iface], t) {
					callTypes.implementers[iface] = append(callTypes.implementers[iface], t)
				}
			}
		}
	}

	promoteReturnTypes(callTypes.returns, callTypes.embeds)

	// Pass 2a, registries: the registry fields of every package (registry.go), which
	// need the module's interfaces.
	ifaces := make(map[string]bool, len(decls.ifaceMethods))
	for name := range decls.ifaceMethods {
		ifaces[name] = true
	}
	callTypes.registries = make(map[string]bool)
	for _, pkgDir := range pkgDirs {
		for k := range collectRegistryFields(parsedPkgs[pkgDir].parsedFiles, pkgDir, mods, pkgNames, aliases, ifaces) {
			callTypes.registries[k] = true
		}
	}

	// Pass 2b: build the gRPC client stub index (generated concrete clients →
	// method wire paths) so consumer call sites in any package resolve to the
	// "/pkg.Service/Method" they invoke.
	var allParsed []*ast.File
	for _, pkgDir := range pkgDirs {
		allParsed = append(allParsed, parsedPkgs[pkgDir].parsedFiles...)
	}
	grpcStubs := buildGoGRPCStubIndex(allParsed)

	// Pass 2d: module-wide interprocedural route-prefix index. Resolves the mount
	// prefix a router parameter carries at its call sites, so routes registered on
	// a subrouter passed into a per-file/per-package function are stored at their
	// true runtime path (e.g. "/api/settings/courses", not the bare "/courses").
	routerWrappers := routerWrapperPackages(parsedPkgs)
	stringConstants := buildStringConsts(parsedPkgs)
	routerFields := ownRouterFields(globalFieldTypes, routerWrappers)
	routePrefixes := routeIndex{
		prefixes:     buildRoutePrefixIndex(parsedPkgs, mods, pkgNames, globalFieldTypes, routerWrappers, stringConstants, routerFields),
		wrappers:     routerWrappers,
		consts:       stringConstants,
		routerFields: routerFields,
	}

	// Pass 3: extract facts per package using the global field types.
	for _, pkgDir := range pkgDirs {
		pp := parsedPkgs[pkgDir]
		select {
		case <-ctx.Done():
			return allFacts, ctx.Err()
		default:
		}
		if pp.pkgName == "" {
			continue
		}
		pkgFacts := e.extractPackage(fset, pkgDir, pp, mods, globalFieldTypes, callTypes, pkgNames, grpcStubs, routePrefixes)
		allFacts = append(allFacts, pkgFacts...)
	}

	resolvePromotedCalls(allFacts, callTypes.embeds)

	// The commands a program declares, settled once every function is known.
	var commandLits []string
	for _, pkgDir := range pkgDirs {
		pp := parsedPkgs[pkgDir]
		for _, f := range pp.parsedFiles {
			commandLits = append(commandLits, commandLiterals(f, pkgDir, buildFileImports(f, mods, pkgNames))...)
		}
	}
	bindCommands(allFacts, commandLits, mods)
	bindTools(allFacts)
	annotateImplementers(allFacts, stated, inferred)

	// Carry io_direct to every function that reaches one, across packages, and
	// through an interface to the types asserted to implement it.
	ioclosure.PropagateWith(allFacts, ioclosure.Options{Declared: callTypes.implementers, Opaque: goObservability})

	return allFacts, nil
}

func (e *GoExtractor) extractPackage(fset *token.FileSet, pkgDir string, pp *parsedPkg, mods goModules, fieldTypes map[string]string, callTypes callTypeTables, pkgNames map[string]string, grpcStubs *goGRPCStubIndex, routePrefixes routeIndex) []facts.Fact {
	var result []facts.Fact

	// Package-scoped map of top-level `var x = NewXxxClient(...)` bindings, so a
	// gRPC client held in a package var (declared in any file of the package)
	// resolves at its call sites.
	pkgVarClients := collectPackageVarClients(pp.parsedFiles, grpcStubs)

	// Package-scoped string-literal bindings (const/var/assign/field) so a
	// `baseURL + "/path"` client call can recover the host the concat discards,
	// even when the base URL is declared in a sibling file of the same package.
	baseURLLits := collectBaseURLLiterals(pp.parsedFiles)

	// The package's own top-level function names. It exists to tell a
	// same-package generic call from an index into a map of funcs: `doRequest[T]
	// (...)` and `handlers[name]()` are the same syntax, and only the first is a
	// call to a function. Without it the conservative rule in flattenSelector
	// drops both, which cost a Go CLI in this estate the FetchPagedList half of
	// its API seam — api.Request never recorded calling api.doRequest, so the
	// reachability walk had to find the seam by another route and found only
	// part of it.
	pkgFuncs := collectPackageFuncs(pp.parsedFiles)
	pkgScope := collectPackageScope(pp.parsedFiles)
	pkgConsts := collectPackageConsts(pp.parsedFiles)

	for _, relFile := range pp.relFiles {
		f, ok := pp.fileMap[relFile]
		if !ok {
			continue
		}
		result = append(result, e.extractFile(fset, f, relFile, pkgDir, mods, fieldTypes, callTypes, pkgNames, grpcStubs, pkgVarClients, baseURLLits, routePrefixes, pkgFuncs, pkgScope, pkgConsts)...)
	}

	// Kafka topics passed through a package constructor, whose declaration and
	// callers are usually in different files of the package.
	result = append(result, extractKafkaConstructorFacts(fset, pp, pkgDir)...)

	moduleFact := facts.Fact{
		Kind: facts.KindModule,
		Name: pkgDir,
		File: pkgDir,
		Props: map[string]any{
			"package":  pp.pkgName,
			"language": "go",
		},
	}
	// Functions referenced by the package's own var and const initializers. They hang
	// on the package rather than on a symbol because the table that holds them is
	// usually unexported and so has no symbol of its own — `var builtIns = map[string]
	// providerFunc{"rubydex": runRubydex}` is the shape, and runRubydex read as dead
	// with it unrecorded. Package-level initialization IS the package's work, so the
	// package is the honest source for the edge.
	if refs := packageLevelInitRefs(pp, pkgDir, mods, pkgNames, pkgFuncs, callTypes.funcs); len(refs) > 0 {
		for _, ref := range refs {
			moduleFact.Relations = append(moduleFact.Relations, facts.Relation{
				Kind:   facts.RelCalls,
				Target: ref,
			})
		}
	}
	// Store the full Go module path on the root package fact so that the graph
	// layer can normalise cross-repo call targets (Bug 2).
	//
	// The root module only. The readers map a stripped target to the repository
	// root, so a nested module's path here would send its targets to the wrong
	// directory; resolving into a nested module from another repository is not
	// done yet.
	if mp := mods.rootPath(); pkgDir == "." && mp != "" {
		moduleFact.SetProp("modulePath", mp)
	}
	if doc := packageDoc(pp); doc != "" {
		facts.SetDoc(&moduleFact, doc)
	}
	result = append(result, moduleFact)

	return result
}

// packageDoc returns the doc prop for a package: its package comment, from doc.go
// when that file has one, else from the first file by name that does. Test files
// are skipped, and so is a licence written directly above the package clause.
func packageDoc(pp *parsedPkg) string {
	names := make([]string, 0, len(pp.relFiles))
	for _, rel := range pp.relFiles {
		if !strings.HasSuffix(rel, "_test.go") {
			names = append(names, rel)
		}
	}
	sort.Slice(names, func(i, j int) bool {
		di, dj := isDocGo(names[i]), isDocGo(names[j])
		if di != dj {
			return di
		}
		return names[i] < names[j]
	})
	for _, rel := range names {
		f := pp.fileMap[rel]
		if f == nil || f.Doc == nil {
			continue
		}
		text := f.Doc.Text()
		if first := facts.DocSummary(text); strings.HasPrefix(first, "Copyright") || strings.Contains(first, "SPDX-License-Identifier") {
			continue
		}
		if doc := facts.DocText(text); doc != "" {
			return doc
		}
	}
	return ""
}

func isDocGo(rel string) bool {
	return rel == "doc.go" || strings.HasSuffix(rel, "/doc.go")
}

func (e *GoExtractor) extractFile(fset *token.FileSet, f *ast.File, relFile, pkgDir string, mods goModules, fieldTypes map[string]string, callTypes callTypeTables, pkgNames map[string]string, grpcStubs *goGRPCStubIndex, pkgVarClients map[string]string, baseURLLits map[string][]string, routePrefixes routeIndex, pkgFuncs, pkgScope, pkgConsts map[string]bool) []facts.Fact {
	var result []facts.Fact

	// Build per-file import alias map for call resolution.
	fileImports := buildFileImports(f, mods, pkgNames)

	// Extract imports
	for _, imp := range f.Imports {
		importPath := strings.Trim(imp.Path.Value, `"`)

		// Normalize internal import targets to short paths (e.g.
		// "github.com/foo/bar/internal/pkg" → "internal/pkg") so they
		// match the module fact names used elsewhere in the store.
		relTarget := importPath
		if dir, ok := mods.resolve(importPath); ok {
			relTarget = dir
		}

		result = append(result, facts.Fact{
			Kind: facts.KindDependency,
			Name: pkgDir + " -> " + importPath,
			File: relFile,
			Line: fset.Position(imp.Pos()).Line,
			Props: map[string]any{
				"language": "go",
				"source":   classifyImport(importPath, mods),
			},
			Relations: []facts.Relation{
				{Kind: facts.RelImports, Target: relTarget},
			},
		})
	}

	// Walk declarations
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			result = append(result, e.extractFunc(fset, d, relFile, pkgDir, mods, fileImports, fieldTypes, callTypes, pkgFuncs, pkgScope, pkgConsts, pkgNames)...)
		case *ast.GenDecl:
			result = append(result, e.extractGenDecl(fset, d, relFile, pkgDir, mods, fileImports, pkgScope, pkgNames)...)
		}
	}

	// Extract route registrations
	routes := extractRoutes(fset, f, relFile, pkgDir, routePrefixes, fileImports)
	for i := range routes {
		if t := handlerTarget(routes[i].PropString("handler"), pkgDir, fileImports); t != "" {
			routes[i].SetProp(facts.PropHandlerTarget, t)
		}
	}
	result = append(result, routes...)

	// Extract outbound HTTP-client calls
	result = append(result, extractHTTPClientFacts(fset, f, relFile, pkgDir, baseURLLits)...)

	// Extract outbound gRPC-client calls
	result = append(result, extractGRPCClientFacts(fset, f, relFile, pkgDir, mods, fileImports, fieldTypes, pkgVarClients, grpcStubs)...)

	// Extract storage patterns
	result = append(result, extractStorage(fset, f, relFile, pkgDir)...)

	// Extract Kafka topic references (produced/consumed), for async cross-repo links
	result = append(result, extractKafkaFacts(fset, f, relFile, pkgDir)...)

	return result
}

func (e *GoExtractor) extractFunc(fset *token.FileSet, fn *ast.FuncDecl, relFile, pkgDir string, mods goModules, fileImports map[string]string, fieldTypes map[string]string, callTypes callTypeTables, pkgFuncs, pkgScope, pkgConsts map[string]bool, pkgNames map[string]string) []facts.Fact {
	var result []facts.Fact

	name := fn.Name.Name
	exported := fn.Name.IsExported()
	kind := facts.SymbolFunc

	var receiver string
	var recvVar string
	if fn.Recv != nil && len(fn.Recv.List) > 0 {
		kind = facts.SymbolMethod
		field := fn.Recv.List[0]
		receiver = typeExprToString(field.Type)
		name = receiver + "." + name
		if len(field.Names) > 0 {
			recvVar = field.Names[0].Name
		}
	}

	qualifiedName := pkgDir + "." + name

	symbolFact := facts.Fact{
		Kind: facts.KindSymbol,
		Name: qualifiedName,
		File: relFile,
		Line: fset.Position(fn.Pos()).Line,
		Props: map[string]any{
			"symbol_kind": kind,
			"exported":    exported,
			"language":    "go",
		},
		Relations: []facts.Relation{
			{Kind: facts.RelDeclares, Target: pkgDir},
		},
	}

	if receiver != "" {
		symbolFact.SetProp("receiver", receiver)
	}
	facts.SetDoc(&symbolFact, fn.Doc.Text())

	// An HTTP handler is exactly func(http.ResponseWriter, *http.Request). Tag it, so a
	// route can be bound to the symbol that actually serves it.
	//
	// The route's `handler` prop is rendered from the REGISTRATION site, so it names the
	// receiver VARIABLE ("h.aiCoachHandler.GetInsight") while the symbol is named by its
	// receiver TYPE (".../aicoach.HandlerV2.GetInsight"). The two key spaces are disjoint
	// — on fairwayhub/golf, 1397 handler props intersect 13482 symbol names exactly twice
	// — so any binder must resolve by method name, and a method name alone is ambiguous:
	// the wiring package's NullWeatherService.GetDailyWeatherRange shares one with the
	// real handler, and a name-only rule binds the route to the stub. A wrong handled_by
	// edge feeds impact_analysis and find_path, which is worse than no edge.
	//
	// The signature is the structural discriminator, and it is free here: goextractor
	// parses with go/ast, so fn.Type.Params is exact. Set only when true — a positive
	// marker, so the goldens do not gain a line per function.
	if isHTTPHandlerSignature(fn.Type) {
		symbolFact.SetProp("http_handler", true)
	}

	// Extract function calls and per-function complexity metrics in a single
	// body walk. The metrics ride on Props (map[string]any) and feed the
	// performance analyzer; they are parser-derived, never inferred.
	if fn.Body != nil {
		ctx := resolveCtx{
			pkgDir:     pkgDir,
			mods:       mods,
			imports:    fileImports,
			recvVar:    recvVar,
			recvType:   receiver,
			fieldTypes: fieldTypes,
			pkgFuncs:   pkgFuncs,
			pkgScope:   pkgScope,
			pkgConsts:  pkgConsts,
			localPkgs:  pkgNames,

			returnTypes: callTypes.returns,
			aliases:     callTypes.aliases,
			moduleFuncs: callTypes.funcs,
			elemTypes:   callTypes.elems,

			registryFields: callTypes.registries,
		}
		typeParams := typeParamNames(fn.Recv, fn.Type)
		ctx.localTypes = collectLocalTypesFrom(fn.Body, ctx, signatureTypes(fn.Type, ctx, typeParams), typeParams)
		if dispatches := stringDispatches(fn.Body, ctx); len(dispatches) > 0 {
			symbolFact.SetProp(dispatchProp, dispatches)
		}
		toolFacts, namedTools := mcpTools(fset, fn.Body, ctx, relFile, pkgDir)
		result = append(result, toolFacts...)
		if len(namedTools) > 0 {
			symbolFact.SetProp(toolsProp, namedTools)
		}
		m := analyzeBody(fn.Body, ctx, qualifiedName, funcParamNames(fn))
		for _, call := range m.calls {
			symbolFact.Relations = append(symbolFact.Relations, facts.Relation{
				Kind:   facts.RelCalls,
				Target: call,
			})
		}
		// A function handed somewhere as a value rather than called: a dispatch-table
		// entry, a callback argument, a struct field. analyzeBody only looks for
		// CallExpr, so without this the callee has no incoming edge at all.
		called := make(map[string]bool, len(m.calls))
		for _, call := range m.calls {
			called[call] = true
		}
		for _, ref := range funcValueRefs(fn.Body, ctx) {
			if called[ref] {
				continue // already recorded as a call from this same body
			}
			symbolFact.Relations = append(symbolFact.Relations, facts.Relation{
				Kind:   facts.RelCalls,
				Target: ref,
			})
		}
		for _, inst := range m.instantiates {
			symbolFact.Relations = append(symbolFact.Relations, facts.Relation{
				Kind:   facts.RelInstantiates,
				Target: inst,
			})
		}
		// Only emit non-trivial metrics so existing snapshots and facts from
		// other extractors (which don't compute these) stay clean.
		symbolFact.SetProp("cyclomatic", m.cyclomatic)
		if m.loopDepth > 0 {
			symbolFact.SetProp("loop_depth", m.loopDepth)
			// Emit the scaling depth (bounded loops discounted) alongside — even when 0 —
			// so the consumer distinguishes "all loops bounded" from "signal absent".
			symbolFact.SetProp("scaling_loop_depth", m.scalingLoopDepth)
		}
		if m.loopCount > 0 {
			symbolFact.SetProp("loop_count", m.loopCount)
		}
		if len(m.clientPathCalls) > 0 {
			symbolFact.SetProp("client_path_calls", m.clientPathCalls)
		}
		if len(m.callsInLoop) > 0 {
			symbolFact.SetProp("calls_in_loop", m.callsInLoop)
			// Emit the N+1 subset alongside — even when EMPTY — so the consumer
			// distinguishes "no call repeats" from "signal absent". An omitted key makes
			// perf.scalingLoopCalls() fall back to the unfiltered calls_in_loop, which
			// silently defeats the discount in exactly the case it exists for: every
			// in-loop call sitting inside a constant loop.
			if m.callsInScalingLoop == nil {
				m.callsInScalingLoop = []string{}
			}
			symbolFact.SetProp("calls_in_scaling_loop", m.callsInScalingLoop)
			symbolFact.SetProp("calls_in_scaling_loop_depth", callDepths(m.callsInScalingLoop, m.scalingCallDepth))
			// Emitted even when empty, for the same reason as calls_in_scaling_loop:
			// an absent key cannot be told from "no such call".
			if m.callsOnLoopElement == nil {
				m.callsOnLoopElement = []string{}
			}
			symbolFact.SetProp("calls_on_loop_element", m.callsOnLoopElement)
		}
		if m.loopsOverParam {
			symbolFact.SetProp("loops_over_param", true)
		}
		if m.recursiveSelf {
			symbolFact.SetProp("recursive_self", true)
		}
		markGoDirectIO(&symbolFact, callTypes, m.callsPerCall)
		// What the body calls only from inside a once.Do. The edge is real and
		// stays; the closure reads this to not carry I/O across it.
		if once := onceOnly(m.calls, m.callsPerCall); len(once) > 0 {
			symbolFact.SetProp("calls_once", once)
		}
	}

	result = append(result, symbolFact)
	return result
}

func (e *GoExtractor) extractGenDecl(fset *token.FileSet, gd *ast.GenDecl, relFile, pkgDir string, mods goModules, fileImports map[string]string, pkgScope map[string]bool, pkgNames map[string]string) []facts.Fact {
	var result []facts.Fact

	for _, spec := range gd.Specs {
		switch s := spec.(type) {
		case *ast.TypeSpec:
			result = append(result, e.extractTypeSpec(fset, gd, s, relFile, pkgDir, mods, fileImports, pkgScope, pkgNames)...)
		case *ast.ValueSpec:
			result = append(result, e.extractValueSpec(fset, gd, s, relFile, pkgDir)...)
		}
	}

	return result
}

// extractValueSpec emits exported package-level consts and vars as symbols.
// They are part of a package's declared surface — a shared vocabulary
// constant or a sentinel error is exactly the kind of declaration other
// packages branch on — and without them a const-only file is invisible to
// the graph: parsed, yet contributing nothing an anchor or a coverage
// question could join against. Unexported names stay out; they are
// implementation detail, and emitting every private binding would drown
// the symbol set in noise.
func (e *GoExtractor) extractValueSpec(fset *token.FileSet, gd *ast.GenDecl, vs *ast.ValueSpec, relFile, pkgDir string) []facts.Fact {
	kind := facts.SymbolVariable
	if gd.Tok == token.CONST {
		kind = facts.SymbolConstant
	}
	var result []facts.Fact
	for _, ident := range vs.Names {
		if !ident.IsExported() {
			continue
		}
		result = append(result, facts.Fact{
			Kind: facts.KindSymbol,
			Name: pkgDir + "." + ident.Name,
			File: relFile,
			Line: fset.Position(ident.Pos()).Line,
			Props: map[string]any{
				"symbol_kind": kind,
				"exported":    true,
				"language":    "go",
			},
		})
	}
	return result
}

func (e *GoExtractor) extractTypeSpec(fset *token.FileSet, gd *ast.GenDecl, ts *ast.TypeSpec, relFile, pkgDir string, mods goModules, fileImports map[string]string, pkgScope map[string]bool, pkgNames map[string]string) []facts.Fact {
	var result []facts.Fact

	name := ts.Name.Name
	exported := ts.Name.IsExported()
	qualifiedName := pkgDir + "." + name
	ctx := resolveCtx{pkgDir: pkgDir, mods: mods, imports: fileImports, localPkgs: pkgNames}

	var kind string
	var implements []string

	var instantiates []string
	switch t := ts.Type.(type) {
	case *ast.StructType:
		kind = facts.SymbolStruct
		typeParams := map[string]bool{}
		if ts.TypeParams != nil {
			for _, field := range ts.TypeParams.List {
				for _, n := range field.Names {
					typeParams[n.Name] = true
				}
			}
		}
		if t.Fields != nil {
			for _, field := range t.Fields.List {
				if len(field.Names) == 0 {
					// Embedded type — a potential interface implementation.
					if embeddedName := typeExprToString(field.Type); embeddedName != "" {
						implements = append(implements, embeddedName)
					}
					continue
				}
				// A named field USES every internal type its type mentions, however
				// it is wrapped: `[]Layer` and `map[Key]*Value` use Layer, Key and
				// Value. Emit a usage edge so a struct referenced only as a field
				// type is not a dead-code false positive (type usage is otherwise
				// not edge-tracked).
				for _, typeStr := range namedTypesIn(field.Type, nil) {
					if !strings.Contains(typeStr, ".") && (!pkgScope[typeStr] || typeParams[typeStr]) {
						continue // predeclared, or a type parameter: nothing declared
					}
					target := resolveTypeName(typeStr, ctx)
					if isTrackedTypeTarget(target, ctx) && !slices.Contains(instantiates, target) {
						instantiates = append(instantiates, target)
					}
				}
			}
		}
	case *ast.InterfaceType:
		kind = facts.SymbolInterface
		result = append(result, interfaceMethodSymbols(fset, t, relFile, pkgDir, name)...)
	default:
		kind = facts.SymbolType
	}

	symbolFact := facts.Fact{
		Kind: facts.KindSymbol,
		Name: qualifiedName,
		File: relFile,
		Line: fset.Position(ts.Pos()).Line,
		Props: map[string]any{
			"symbol_kind": kind,
			"exported":    exported,
			"language":    "go",
		},
		Relations: []facts.Relation{
			{Kind: facts.RelDeclares, Target: pkgDir},
		},
	}

	for _, impl := range implements {
		symbolFact.Relations = append(symbolFact.Relations, facts.Relation{
			Kind:   facts.RelImplements,
			Target: impl,
		})
	}

	// A type declared alone carries its comment on the declaration, a type in a
	// group on its own spec.
	typeDoc := ts.Doc
	if typeDoc == nil && !gd.Lparen.IsValid() {
		typeDoc = gd.Doc
	}
	facts.SetDoc(&symbolFact, typeDoc.Text())

	for _, inst := range instantiates {
		symbolFact.Relations = append(symbolFact.Relations, facts.Relation{
			Kind:   facts.RelInstantiates,
			Target: inst,
		})
	}

	result = append(result, symbolFact)
	return result
}

// interfaceMethodSymbols emits one symbol fact per named method an interface
// declaration carries. The declaration is as measurable as the interface type
// fact beside it, and it is the member the constraints evaluator needs: a call
// edge through an interface value targets pkgDir.Iface.Method, and exact-name,
// fail-closed resolution grounds only on a fact by that name (finding 0009).
// Embedded interfaces have no method name of their own here and emit nothing —
// expanding them would guess at another declaration's contents.
func interfaceMethodSymbols(fset *token.FileSet, iface *ast.InterfaceType, relFile, pkgDir, ifaceName string) []facts.Fact {
	var result []facts.Fact
	if iface.Methods == nil {
		return result
	}
	for _, field := range iface.Methods.List {
		for _, methodIdent := range field.Names {
			result = append(result, facts.Fact{
				Kind: facts.KindSymbol,
				Name: pkgDir + "." + ifaceName + "." + methodIdent.Name,
				File: relFile,
				Line: fset.Position(methodIdent.Pos()).Line,
				Props: map[string]any{
					"symbol_kind": facts.SymbolMethod,
					"exported":    methodIdent.IsExported(),
					"language":    "go",
					"receiver":    ifaceName,
				},
				Relations: []facts.Relation{
					{Kind: facts.RelDeclares, Target: pkgDir},
				},
			})
		}
	}
	return result
}

// resolveCtx holds the context needed to resolve call targets within a function body.
type resolveCtx struct {
	pkgDir     string
	mods       goModules
	imports    map[string]string // alias → relative package path
	recvVar    string            // receiver variable name, e.g. "h"
	recvType   string            // receiver type (star stripped), e.g. "AuthHandler"
	fieldTypes map[string]string // "pkgDir.TypeName.fieldName" → pre-qualified typeString
	localTypes map[string]string // local variable name → qualified type, e.g. "svc" → "internal/auth.Service"
	// "pkgDir.Func" / "pkgDir.Type.Method" → declared result types, module-wide. See calltypes.go.
	returnTypes map[string][]string
	// "pkgDir.Alias" → the type `type Alias = T` names, module-wide. See calltypes.go.
	aliases map[string]string
	// What indexing a declared collection yields, module-wide. See calltypes.go.
	elemTypes elemTypes
	// "pkgDir.Type.field" for the registry fields, module-wide. See registry.go.
	registryFields map[string]bool
	pkgFuncs       map[string]bool // this package's top-level function names
	// "pkgDir.Func" for every top-level function in the module. nil where a pass
	// does not resolve function values across packages.
	moduleFuncs map[string]bool
	// Every name this package declares at top level: functions, types, variables
	// and constants. nil where a pass does not collect it.
	pkgScope map[string]bool
	// The constants this package declares, which bound a retry loop where a variable
	// holding a collection's size would not. nil where a pass does not collect it.
	pkgConsts map[string]bool
	// This module's package directories (pkgDir → package name), which tell its own
	// packages from the standard library's: both are written as plain paths
	// ("internal/app", "net/http"). nil where a pass does not need it.
	localPkgs map[string]string
}

// bodyMetrics holds the call list and the per-function complexity signals
// derived from a single walk of a function body.
type bodyMetrics struct {
	// callsPerCall are the calls made each time the function runs: calls less
	// those that sit only inside a `once.Do(func() { … })`.
	callsPerCall []string
	calls        []string // resolved call targets, deduped, in source order
	instantiates []string // resolved internal struct types constructed as composite literals, deduped
	callsInLoop  []string // subset of calls invoked at loop nesting depth >= 1
	// clientPathCalls records "callee\x00path\x00METHOD" for every call passing a
	// path-shaped string literal. It is a CANDIDATE list and nothing more: whether
	// the callee is an HTTP seam cannot be known here, because the seam usually
	// lives in another package and this pass sees one. The seam binder decides.
	clientPathCalls    []string
	callsInScalingLoop []string // subset of calls invoked at scaling (unbounded) nesting depth >= 1
	// scalingCallDepth is the deepest scaling nesting each of those is called at
	// (calldepth.go).
	scalingCallDepth map[string]int
	// callsOnLoopElement is the subset of callsInScalingLoop handed an element of an
	// enclosing loop — as an argument or as the receiver. Paired with the callee's
	// own loopsOverParam it is the cross-call form of the hierarchical rule: the
	// callee is continuing this walk, not starting one per element.
	callsOnLoopElement []string
	// loopsOverParam records that a scaling loop in this body walks something reached
	// through a parameter or the receiver, which is what makes it a continuation of
	// the caller's walk rather than an independent traversal.
	loopsOverParam   bool
	loopDepth        int  // max nesting depth of for/range loops
	scalingLoopDepth int  // max nesting counting only unbounded (input-scaling) loops
	loopCount        int  // total number of for/range loops
	cyclomatic       int  // McCabe complexity (1 + decision points)
	recursiveSelf    bool // body directly calls the enclosing function
}

// analyzeBody walks a function body once and extracts both the call edges
// (identical resolution to the previous extractCalls) and complexity metrics.
//
// Loop nesting depth is tracked without an explicit recursive walker by keeping
// a stack of the end positions of the loops currently enclosing the node being
// visited: ast.Inspect is pre-order, and the AST is properly nested, so a node
// is inside every loop on the stack whose body it lexically falls within. Calls
// inside func literals are attributed by lexical nesting; interface-dispatch
// targets remain unresolved exactly as before.
func analyzeBody(body ast.Node, ctx resolveCtx, selfName string, params map[string]bool) bodyMetrics {
	var m bodyMetrics
	decisions := 0
	seen := make(map[string]bool)
	instSeen := make(map[string]bool)
	inLoopSeen := make(map[string]bool)
	inScalingSeen := make(map[string]bool)
	onElementSeen := make(map[string]bool)
	var loopEnds []token.Pos // end positions of enclosing loops
	// scalingEnds tracks only the enclosing loops that scale with input (a `for {}` event
	// loop and a `range` over a composite literal are excluded), so len(scalingEnds) is
	// the current scaling nesting depth used for Big-O.
	var scalingEnds []token.Pos
	// repeatEnds tracks the enclosing loops that run a NON-CONSTANT number of times.
	// It differs from scalingEnds for `for {}`: an infinite loop adds no factor of n, but
	// its body still runs many times, so a per-iteration query inside it is an N+1
	// candidate (a parent-chain walk doing one SELECT per level). Only a range over a
	// composite literal is excluded here. Scaling and repeating are not the same property.
	var repeatEnds []token.Pos
	// loopScopes tracks the loop variables introduced by every enclosing loop, so an
	// inner loop whose ranged collection is reached THROUGH an outer loop variable
	// (a hierarchical walk like `range pkg.Files`) can be told from one over an
	// independent/same collection (all-pairs). A hierarchical loop visits each element
	// once across the whole nest, so it adds no factor of n to the Big-O exponent.
	var loopScopes []loopScope
	// loopHeads are, per enclosing loop, the part of its statement evaluated once
	// before the first iteration: a range's operand, a for's init clause.
	var loopHeads []loopHead
	// Locals whose contents cannot grow with the input, resolved once for the body
	// so the range case below is a lookup rather than a second walk per loop.
	boundedVars := boundedLocals(body)
	// onceEnds are the ends of the enclosing `once.Do(func() { … })` calls. What
	// runs in one runs once per process, not once per call of this function.
	var onceEnds []token.Pos
	repeatedSeen := make(map[string]bool)

	ast.Inspect(body, func(n ast.Node) bool {
		if n == nil {
			return false
		}
		for len(onceEnds) > 0 && n.Pos() >= onceEnds[len(onceEnds)-1] {
			onceEnds = onceEnds[:len(onceEnds)-1]
		}
		// Pop loops whose extent we have now left.
		for len(loopEnds) > 0 && n.Pos() >= loopEnds[len(loopEnds)-1] {
			loopEnds = loopEnds[:len(loopEnds)-1]
			loopHeads = loopHeads[:len(loopHeads)-1]
		}
		for len(scalingEnds) > 0 && n.Pos() >= scalingEnds[len(scalingEnds)-1] {
			scalingEnds = scalingEnds[:len(scalingEnds)-1]
		}
		for len(repeatEnds) > 0 && n.Pos() >= repeatEnds[len(repeatEnds)-1] {
			repeatEnds = repeatEnds[:len(repeatEnds)-1]
		}
		for len(loopScopes) > 0 && n.Pos() >= loopScopes[len(loopScopes)-1].end {
			loopScopes = loopScopes[:len(loopScopes)-1]
		}
		switch x := n.(type) {
		case *ast.ForStmt:
			m.loopCount++
			decisions++
			loopEnds = append(loopEnds, x.End())
			if len(loopEnds) > m.loopDepth {
				m.loopDepth = len(loopEnds)
			}
			// A loop whose trip count is fixed at compile time, and one that merely
			// advances a cursor an enclosing loop already advances, both leave the
			// exponent alone. They differ in whether the BODY repeats: a constant
			// loop runs a constant number of times, so a query inside it is not an
			// N+1 candidate, while a shared-cursor loop runs as many times as the
			// scan it is part of and stays one.
			constBounded := goForConstBounded(x) || goForRetryLoop(x, ctx.pkgConsts, ctx.imports)
			sharesCursor := goForSharesOuterCursor(x, loopScopes)
			// A loop draining what an enclosing batch loop fetched this round is
			// that loop's hierarchical inner loop (batchloop.go).
			forScales := !goForBounded(x) && !constBounded && !sharesCursor && !drainsBatch(x, loopScopes)
			if forScales && referencesParam(x.Cond, params) {
				// `for i := 0; i < len(s); i++` walks s just as `range s` does; the
				// parameter is named in the bound rather than in a range expression.
				m.loopsOverParam = true
			}
			if forScales {
				scalingEnds = append(scalingEnds, x.End())
				if len(scalingEnds) > m.scalingLoopDepth {
					m.scalingLoopDepth = len(scalingEnds)
				}
			}
			// A `for {}` is infinite, not constant: it repeats. An event loop does
			// not, in the sense that matters here: it runs once per tick or message,
			// so a query in it is not one per element.
			repeats := !constBounded && !goForEventLoop(x)
			if repeats {
				repeatEnds = append(repeatEnds, x.End())
			}
			var init ast.Node
			if x.Init != nil {
				init = x.Init
			}
			head := newLoopHead(init, forScales, repeats)
			head.once = terminalBlocks(x.Body)
			loopHeads = append(loopHeads, head)
			scope := loopScope{end: x.End(), vars: forLoopVars(x), cursor: forCursor(x), amortizes: forScales}
			if pageVars, isBatch, isWalk := goBatchLoop(x); isBatch || isWalk {
				// What a round binds is the round's element, and a `for {}` that
				// pages amortizes its drain loop though it adds no depth itself. A
				// cursor walk binds per row and amortizes the same way, but its
				// fetch is one per row and not the batching.
				scope.batch, scope.fetches, scope.amortizes = isBatch, true, true
				scope.vars = append(scope.vars, pageVars...)
			}
			loopScopes = append(loopScopes, scope)
		case *ast.RangeStmt:
			m.loopCount++
			decisions++
			loopEnds = append(loopEnds, x.End())
			if len(loopEnds) > m.loopDepth {
				m.loopDepth = len(loopEnds)
			}
			// A range whose trip count is not constant amortizes inner hierarchical
			// loops whether or not it scales itself; see loopScope.amortizes.
			rangeAmortizes := false
			rangeScales := false
			if !goRangeBounded(x) && !goRangeBoundedLocal(x, boundedVars) && !goRangeOverRegistry(x, ctx) {
				rangeAmortizes = true
				// A hierarchical loop — one whose ranged collection is reached THROUGH
				// an enclosing loop variable (`range pkg.Files`, `range pkgs[pkgDir]…`)
				// — visits each element once across the whole nest, so it adds no factor
				// of n to the scaling exponent. Only a loop over an independent or same
				// collection (all-pairs) multiplies. Derived loops still repeat, so they
				// stay N+1 candidates (repeatEnds) — only the scaling depth is spared.
				if referencesParam(x.X, params) {
					m.loopsOverParam = true
				}
				if !referencesLoopVar(x.X, loopScopes) {
					rangeScales = true
					scalingEnds = append(scalingEnds, x.End())
					if len(scalingEnds) > m.scalingLoopDepth {
						m.scalingLoopDepth = len(scalingEnds)
					}
				}
				repeatEnds = append(repeatEnds, x.End())
			}
			head := newLoopHead(x.X, rangeScales, rangeAmortizes)
			head.once = terminalBlocks(x.Body)
			loopHeads = append(loopHeads, head)
			loopScopes = append(loopScopes, loopScope{end: x.End(), vars: rangeLoopVars(x), amortizes: rangeAmortizes})
		case *ast.AssignStmt:
			// A value computed from the current element is still the current element
			// as far as the traversal is concerned: `rel := norm(f); use(rel)` hands
			// `use` what the loop is walking, one indirection later. Without this the
			// cross-call hierarchical rule only ever fires on a bare loop variable,
			// which is the minority of call sites.
			if len(loopScopes) > 0 && referencesLoopVar(rhsExpr(x), loopScopes) {
				top := &loopScopes[len(loopScopes)-1]
				for _, lhs := range x.Lhs {
					if id, ok := lhs.(*ast.Ident); ok && id.Name != "_" {
						top.vars = append(top.vars, id.Name)
					}
				}
			}
			decisions += 0
		case *ast.IfStmt:
			decisions++
		case *ast.CaseClause:
			if len(x.List) > 0 { // ignore default
				decisions++
			}
		case *ast.CommClause:
			if x.Comm != nil { // ignore default in select
				decisions++
			}
		case *ast.BinaryExpr:
			if x.Op == token.LAND || x.Op == token.LOR {
				decisions++
			}
		case *ast.CallExpr:
			callee := x.Fun
			// `doRequest[T](…)` — a same-package generic call. flattenSelector
			// refuses a bare identifier under an index because `handlers[name]()`
			// is written identically and would resolve to the map. Knowing the
			// package's function names settles which one this is, so the safe
			// default can be narrowed exactly where it costs something.
			if idx, isIndex := callee.(*ast.IndexExpr); isIndex {
				if id, isIdent := idx.X.(*ast.Ident); isIdent && ctx.pkgFuncs[id.Name] {
					callee = idx.X
				}
			}
			// callTarget is resolveChain for a plain selector chain, and reads through
			// one that passes over a call or an assertion where the type is declared.
			resolved := callTarget(callee, ctx)
			if resolved == "" {
				return true
			}
			if !seen[resolved] {
				seen[resolved] = true
				m.calls = append(m.calls, resolved)
			}
			if len(onceEnds) == 0 && !repeatedSeen[resolved] {
				repeatedSeen[resolved] = true
				m.callsPerCall = append(m.callsPerCall, resolved)
			}
			if isOnceDo(x, resolved) {
				onceEnds = append(onceEnds, x.End())
			}
			// A call in the innermost loop's head runs once per iteration of the
			// loops AROUND that one, not of it: `for _, x := range load()` calls
			// load once.
			inLoops, inRepeating, inScaling := len(loopEnds), len(repeatEnds), len(scalingEnds)
			if len(loopHeads) > 0 {
				if h := loopHeads[len(loopHeads)-1]; h.contains(x.Pos()) {
					inLoops--
					if h.repeats {
						inRepeating--
					}
					if h.scales {
						inScaling--
					}
				}
			}
			if inLoops > 0 && !inLoopSeen[resolved] {
				inLoopSeen[resolved] = true
				m.callsInLoop = append(m.callsInLoop, resolved)
			}
			// A call inside a loop that repeats a non-constant number of times is an N+1
			// candidate. A call only ever inside a constant loop (range over a composite
			// literal) runs a fixed number of times and is not — but a `for {}` DOES
			// repeat, so its calls stay candidates even though its depth is discounted.
			// A call made once per round of a batch loop is the batched call, not an
			// N+1 (batchloop.go). One inside its drain loop has a different
			// innermost loop and is unaffected.
			if len(loopScopes) > 0 && loopScopes[len(loopScopes)-1].batch {
				inRepeating = 0
			}
			if inRepeating > 0 {
				m.scalingCallDepth = noteCallDepth(m.scalingCallDepth, resolved, inScaling)
			}
			if inRepeating > 0 && !inScalingSeen[resolved] {
				inScalingSeen[resolved] = true
				m.callsInScalingLoop = append(m.callsInScalingLoop, resolved)
			}
			if inScaling > 0 && !onElementSeen[resolved] && passesLoopElement(x, loopScopes) {
				onElementSeen[resolved] = true
				m.callsOnLoopElement = append(m.callsOnLoopElement, resolved)
			}
			if resolved == selfName {
				m.recursiveSelf = true
			}
			// Third-party callees are dropped here rather than in the binder,
			// because a router registration is the commonest call in Go that
			// passes a path literal — `mux.Router.HandleFunc("/api/orders", h)`
			// and gin's `api.GET("/ping", h)` are SERVER routes, and recording
			// them as candidates put a noisy provisional list on every symbol
			// that wires a router.
			if isModuleLocalCallee(resolved) {
				if path, verb, ok := pathLiteralArg(x); ok {
					m.clientPathCalls = append(m.clientPathCalls, resolved+"\x00"+path+"\x00"+verb)
				}
			}
		case *ast.CompositeLit:
			// A composite literal `T{...}` / `&T{...}` uses (instantiates) type T.
			// Type usage is otherwise not edge-tracked, so an internal struct used
			// only as a literal reads as a dead-code false positive. Emit a usage
			// edge to a module-internal type, or to another module's type under its
			// import path, which is how a library loaded beside it is reached; never
			// to a stdlib type.
			if t := compositeLitType(x, ctx); t != "" && isTrackedTypeTarget(t, ctx) && !instSeen[t] {
				instSeen[t] = true
				m.instantiates = append(m.instantiates, t)
			}
		}
		return true
	})
	m.cyclomatic = 1 + decisions
	return m
}

// loopHead is the span of a loop statement evaluated once, before the first
// iteration, with which of the nesting counters that loop raised.
type loopHead struct {
	pos, end        token.Pos
	scales, repeats bool
	// once are the runs of the body that end by leaving the loop (terminal.go). A
	// call in one is made at most once per entry, as a call in the head is.
	once []posSpan
}

// newLoopHead takes nil for a loop with no such part (`for cond {}`, `for {}`).
func newLoopHead(n ast.Node, scales, repeats bool) loopHead {
	h := loopHead{scales: scales, repeats: repeats}
	if n != nil {
		h.pos, h.end = n.Pos(), n.End()
	}
	return h
}

func (h loopHead) contains(p token.Pos) bool {
	if h.pos != token.NoPos && p >= h.pos && p < h.end {
		return true
	}
	for _, s := range h.once {
		if p >= s.pos && p < s.end {
			return true
		}
	}
	return false
}

// loopScope records the variables an enclosing loop introduces, with the loop's
// end position so it can be popped by the position-based nesting walk.
type loopScope struct {
	end  token.Pos
	vars []string
	// cursor is the index variable a three-clause for loop declares and steps, or ""
	// for a range loop and for a for loop that declares none. An inner loop testing
	// this variable is advancing the same cursor; see goForSharesOuterCursor.
	cursor string
	// amortizes records whether an inner hierarchical loop may be discounted against
	// this one. Reaching a collection through an outer loop's variable cancels a
	// factor only when the outer loop contributed one: under a CONSTANT outer there
	// is nothing to cancel, and discounting anyway reported a plain walk as O(1).
	//
	// A hierarchical loop amortizes even though it does not scale itself, because it
	// passes the hierarchy on — the elements it yields still belong to the
	// collection the scaling loop at the root of the chain is walking.
	amortizes bool
	// batch marks a loop that takes its input a batch at a time; see batchloop.go.
	batch bool
	// fetches marks a loop whose round binds what it then drains: a batch loop,
	// or a cursor walk that fetches per row. The drain is hierarchical in both.
	fetches bool
}

// rangeLoopVars returns the key/value variable names a range loop introduces.
func rangeLoopVars(x *ast.RangeStmt) []string {
	var vs []string
	if id, ok := x.Key.(*ast.Ident); ok {
		vs = append(vs, id.Name)
	}
	if id, ok := x.Value.(*ast.Ident); ok {
		vs = append(vs, id.Name)
	}
	return vs
}

// forLoopVars returns the variable names declared in a for-loop init clause
// (`for i := 0; …`).
func forLoopVars(x *ast.ForStmt) []string {
	var vs []string
	if as, ok := x.Init.(*ast.AssignStmt); ok && as.Tok == token.DEFINE {
		for _, l := range as.Lhs {
			if id, ok := l.(*ast.Ident); ok {
				vs = append(vs, id.Name)
			}
		}
	}
	return vs
}

// referencesLoopVar reports whether expr references any variable introduced by an
// enclosing loop — i.e. the collection is reached through an outer loop element
// (a hierarchical walk) rather than being independent of the outer loops.
func referencesLoopVar(expr ast.Expr, scopes []loopScope) bool {
	if expr == nil {
		return false
	}
	vars := map[string]bool{}
	for _, s := range scopes {
		if !s.amortizes {
			continue
		}
		for _, v := range s.vars {
			if v != "" && v != "_" {
				vars[v] = true
			}
		}
	}
	if len(vars) == 0 {
		return false
	}
	found := false
	ast.Inspect(expr, func(n ast.Node) bool {
		if found {
			return false
		}
		if id, ok := n.(*ast.Ident); ok && vars[id.Name] {
			found = true
		}
		return true
	})
	return found
}

// isTrackedTypeTarget reports a type a usage edge may point at: one of this module's,
// or one under another module's import path ("github.com/acme/auth.Config"), which a
// snapshot holding that module as another repository resolves to its declaration. A
// standard library type gets none.
func isTrackedTypeTarget(resolved string, ctx resolveCtx) bool {
	return isInternalTypeTarget(resolved, ctx) || isOtherModuleType(resolved)
}

// isOtherModuleType reports a type named under another Go module's import path: its
// package path starts with a host ("github.com/…").
func isOtherModuleType(resolved string) bool {
	i := strings.LastIndex(resolved, ".")
	if i < 0 {
		return false
	}
	pkg := resolved[:i]
	host, _, ok := strings.Cut(pkg, "/")
	return ok && strings.Contains(host, ".")
}

// isInternalTypeTarget reports whether a resolved type target (pkg.Type) names a type
// of this module: the same package, or a package the module declares. A module's own
// package and a standard library one are both written as plain paths ("internal/app",
// "net/http"), so the module's package list decides. Without it, a path with a "/" and
// no domain "." is taken as the module's, which misreads "net/http" and misses a
// single-segment package at the repository root.
func isInternalTypeTarget(resolved string, ctx resolveCtx) bool {
	i := strings.LastIndex(resolved, ".")
	if i < 0 {
		return false
	}
	pkg := resolved[:i]
	if pkg == ctx.pkgDir {
		return true
	}
	if ctx.localPkgs != nil {
		_, ok := ctx.localPkgs[pkg]
		return ok
	}
	return strings.Contains(pkg, "/") && !strings.Contains(pkg, ".")
}

// goForBounded reports whether a for-statement's trip count is independent of the input
// size: a bare `for { }` is driven by break/return/events, not data size, so it adds no
// factor of n to Big-O. (A `for i := 0; i < n; i++` with a data-derived bound is treated
// as unbounded — its static bound is not evident here.)
//
// It does NOT mean the loop runs a constant number of times: a `for { id = parent(id) }`
// chain walk iterates once per level. Such a loop is discounted from scaling_loop_depth
// but still contributes calls_in_scaling_loop, the N+1 candidate set.
func goForBounded(x *ast.ForStmt) bool {
	return x.Cond == nil && x.Init == nil && x.Post == nil
}

// goForEventLoop reports a `for { select { … } }`: a bare loop whose whole body is
// one select, waiting on a ticker, a context or a channel. Each round handles one
// event, so its trip count is time or traffic, never the size of a collection. A
// bare loop with anything else in its body (a paging loop's fetch) still repeats.
func goForEventLoop(x *ast.ForStmt) bool {
	if !goForBounded(x) || x.Body == nil || len(x.Body.List) != 1 {
		return false
	}
	_, ok := x.Body.List[0].(*ast.SelectStmt)
	return ok
}

// goRangeBounded reports whether a range loop iterates a fixed-size composite literal
// (`for _, x := range []T{a, b, c}` / a map literal) — a genuinely constant count. Unlike
// goForBounded, this bounds the trip count itself, so calls inside such a loop are not
// N+1 candidates either.
func goRangeBounded(x *ast.RangeStmt) bool {
	_, ok := x.X.(*ast.CompositeLit)
	return ok
}

// flattenSelector converts a (potentially deep) selector chain to a left-to-right
// slice of name segments. Returns nil for non-identifier/non-selector expressions
// (e.g. function-result calls, type assertions, index expressions).
func flattenSelector(expr ast.Expr) []string {
	switch e := expr.(type) {
	case *ast.Ident:
		return []string{e.Name}
	case *ast.SelectorExpr:
		prefix := flattenSelector(e.X)
		if prefix == nil {
			return nil
		}
		return append(prefix, e.Sel.Name)
	case *ast.IndexListExpr:
		// A generic instantiation with several type arguments, `pkg.Map[K, V](…)`.
		// Unambiguous: Go has no multi-index expression other than instantiation.
		return flattenSelector(e.X)
	case *ast.IndexExpr:
		// An index in the callee chain, which is two different things wearing one
		// syntax. `api.Request[Task](…)` is a generic instantiation — 30 of
		// that same Go CLI's 35 such sites, the whole of its API seam, invisible
		// while `fmt.Sprintf` two lines below in the same closure came through
		// fine.
		// `g.facts[i].PropString(…)` is a method on a slice element, and on the
		// tree of this very repository that is the commoner shape: two symbols
		// gained loop-call props from it on the first run after this change.
		//
		// Both were being dropped, which is what returning nil here means.
		//
		// Only a qualified callee is unwrapped, and that is the fail-closed
		// choice rather than an oversight. `a[i](…)` is equally valid Go for
		// calling out of a map or slice of funcs, syntax cannot tell the two
		// apart without type information, and resolveChain turns a bare
		// identifier into `<pkg>.<name>` — so unwrapping `handlers[key]()` would
		// not dangle, it would draw a confident edge to a real package-level
		// variable. A same-package generic call stays missing instead. A missing
		// edge beats a wrong one.
		//
		// An element call resolves to its field chain — `g.facts[i]` becomes
		// `g.facts` — so `g.facts.PropString` reads as a method on the slice
		// rather than on its element. That is the convention this extractor
		// already applies to every other field chain, and it is an imprecision in
		// the name rather than a wrong target: the call is real and it is made at
		// that path.
		if _, qualified := e.X.(*ast.SelectorExpr); !qualified {
			return nil
		}
		return flattenSelector(e.X)
	}
	return nil
}

// buildFileImports returns a map of import alias → relative package path for all
// imports in f. The relative path is the package's directory in the repository,
// resolved through the module that owns the import, to match fact naming. An
// import of a module root maps to that module's directory ("." at the root).
// Blank ("_") and dot (".") imports are excluded.
// pkgNames maps pkgDir → declared package name (from parsing); it is used to
// resolve the implicit alias for packages whose path base is not a valid identifier
// (e.g. "github.com/x/go-auth" has base "go-auth" but package name "auth").
func buildFileImports(f *ast.File, mods goModules, pkgNames map[string]string) map[string]string {
	m := make(map[string]string)
	for _, imp := range f.Imports {
		importPath := strings.Trim(imp.Path.Value, `"`)

		if imp.Name != nil {
			if imp.Name.Name == "_" || imp.Name.Name == "." {
				continue
			}
		}

		// A package importing a module root maps to that module's directory
		// ("." at the repository root) so that call targets resolve to the
		// root package's fact names.
		relTarget := importPath
		if dir, ok := mods.resolve(importPath); ok {
			relTarget = dir
		}

		alias := ""
		if imp.Name != nil {
			alias = imp.Name.Name
		} else if pkgNames != nil {
			// Use the declared package name as alias when available — this is
			// correct when the last path segment isn't a valid identifier
			// (e.g. "go-auth" → package name "auth").
			if name, ok := pkgNames[relTarget]; ok {
				alias = name
			} else {
				alias = filepath.Base(importPath)
			}
		} else {
			alias = filepath.Base(importPath)
		}

		if alias != "" {
			m[alias] = relTarget
		}
	}
	return m
}

// collectFieldTypes pre-scans all struct declarations in the given parsed files
// and returns a map of "pkgDir.TypeName.fieldName" → pre-qualified typeString for
// named fields. Types are pre-qualified at collection time using each struct's
// source-package context so they remain correct when looked up from a different
// package (e.g. an adapters package looking up root-package struct fields).
func collectFieldTypes(files []*ast.File, pkgDir string, mods goModules, pkgNames map[string]string, aliases map[string]string) map[string]string {
	m := make(map[string]string)
	for _, f := range files {
		fileImports := buildFileImports(f, mods, pkgNames)
		ctx := resolveCtx{pkgDir: pkgDir, imports: fileImports, aliases: aliases}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				st, ok := ts.Type.(*ast.StructType)
				if !ok || st.Fields == nil {
					continue
				}
				typeName := ts.Name.Name
				for _, field := range st.Fields.List {
					typeStr := typeExprToString(field.Type)
					if typeStr == "" {
						continue
					}
					// Pre-qualify so cross-package lookups return the correct
					// fact-name prefix rather than the local alias or bare type name.
					qualifiedType := resolveTypeName(typeStr, ctx)
					for _, fname := range field.Names {
						key := pkgDir + "." + typeName + "." + fname.Name
						m[key] = qualifiedType
					}
					// An embedded field is named by its type: `db.BaseRepository`
					// is the field BaseRepository, so `r.BaseRepository.QueryContext()`
					// walks through it as a promoted `r.QueryContext()` already does.
					if len(field.Names) == 0 {
						if name := embeddedFieldName(typeStr); name != "" {
							m[pkgDir+"."+typeName+"."+name] = qualifiedType
						}
					}
				}
			}
		}
	}
	return m
}

// embeddedFieldName is the name Go gives an embedded field: its type's own
// identifier, without the pointer, the package qualifier or type arguments.
func embeddedFieldName(typeStr string) string {
	name := strings.TrimPrefix(typeStr, "*")
	if i := strings.IndexByte(name, '['); i >= 0 {
		name = name[:i]
	}
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		name = name[i+1:]
	}
	return name
}

// resolveChain resolves a flattened call chain to a graph fact name.
//
// Resolution rules:
//   - 1 element (bare call): same-package function → pkgDir.name
//   - 2 elements [alias, func]: import alias → relPath.func; receiver var → pkgDir.ReceiverType.func; fallback → raw join
//   - 3+ elements: resolve root to a qualified "pkg.Type", walk intermediate fields via fieldTypes, produce qualifiedType.method
//
// Falls back to the raw joined string when resolution is not possible, so no call is dropped.
//
// Known limitation: calls through an interface value (e.g. iface.Method()) cannot be
// statically bound to a concrete implementation without type-flow analysis. The
// resolved target names the interface method, which interfaceMethodSymbols backs with
// a declared symbol fact — the edge grounds on the declaration, never on a guessed
// implementation.
func resolveChain(chain []string, ctx resolveCtx) string {
	switch len(chain) {
	case 0:
		return ""
	case 1:
		// Builtins and predeclared type conversions (len, make, string(...), etc.)
		// are not symbols — emitting them produces dangling phantom nodes.
		//
		// Unless this package declares a function of that name, which SHADOWS the
		// predeclared identifier: Go resolves `min(a, b)` to the package's own min,
		// and so must this. Repositories written before Go 1.21 carry their own min
		// and max helpers by the hundred, and dropping those calls left the helper
		// with no incoming edge at all — reported as dead code at HIGH confidence,
		// the tier documented as the safest to delete, while being called seventeen
		// times. Measured on one Go service: 4 of 41 high-confidence findings.
		if goBuiltins[chain[0]] && !ctx.pkgFuncs[chain[0]] {
			return ""
		}
		// A bare name the package does not declare is a parameter, a closure
		// bound to a local, or a type parameter. Calling it calls no function
		// this name could identify, and pkgDir.name would be a node nothing
		// declares.
		if ctx.pkgScope != nil && !ctx.pkgScope[chain[0]] {
			return ""
		}
		return ctx.pkgDir + "." + chain[0]
	case 2:
		root, sel := chain[0], chain[1]
		if importPath, ok := ctx.imports[root]; ok {
			return importPath + "." + sel
		}
		if root == ctx.recvVar && ctx.recvType != "" {
			return ctx.pkgDir + "." + ctx.recvType + "." + sel
		}
		if qualType, ok := ctx.localTypes[root]; ok && qualType != "" {
			// root is a local variable of a known type; sel is a method on it.
			return qualType + "." + sel
		}
		return root + "." + sel
	default:
		// 3+ elements: attempt field-chain resolution.
		root := chain[0]
		var qualType string // "pkgDir.TypeName" or "importedPkg.TypeName"
		var fieldStart int  // index of the first intermediate field in chain

		if root == ctx.recvVar && ctx.recvType != "" {
			qualType = ctx.pkgDir + "." + ctx.recvType
			fieldStart = 1
		} else if lt, ok := ctx.localTypes[root]; ok && lt != "" {
			// root is a local variable; its type is already fully qualified.
			qualType = lt
			fieldStart = 1
		} else if importPath, ok := ctx.imports[root]; ok {
			// root is an import alias; chain[1] is a type in that package.
			qualType = importPath + "." + chain[1]
			fieldStart = 2
		} else {
			return strings.Join(chain, ".")
		}

		for _, fieldName := range chain[fieldStart : len(chain)-1] {
			nextType, ok := fieldType(qualType, fieldName, ctx)
			if !ok {
				return strings.Join(chain, ".")
			}
			qualType = nextType
		}

		return qualType + "." + chain[len(chain)-1]
	}
}

// collectLocalTypes scans a function body for local variable declarations whose
// type is statically knowable and returns a map of variable name → qualified type
// name (e.g. "svc" → "internal/auth.Service"). It recognises:
//   - `var x T` / `var x *T` declarations
//   - `x := &Foo{}` / `x := Foo{}` composite literals
//   - `x := NewFoo(...)` / `x := pkg.NewFoo(...)` constructor conventions
//   - `x := f(...)` and `x, err := f(...)` where f is declared in this module
//
// Variable names are recorded so that calls like `svc.Do()` resolve to the
// canonical method fact name instead of dangling on a raw join.
func collectLocalTypes(body ast.Node, ctx resolveCtx) map[string]string {
	return collectLocalTypesFrom(body, ctx, nil, nil)
}

// collectLocalTypesFrom is collectLocalTypes for a body whose enclosing signature is
// known: declared holds the types of its parameters and named results.
//
// The map is one per function and knows nothing of scope, so a name is only as
// good as its being used for one thing. A declared name that the body declares
// AGAIN, as another type or as one that cannot be read off the source, is dropped
// for the whole function: `func f(r Resolver) { for _, r := range rows { r.Scan() } }`
// must not send Scan to Resolver. The same holds for a closure's own parameters.
func collectLocalTypesFrom(body ast.Node, ctx resolveCtx, declared map[string]string, typeParams map[string]bool) map[string]string {
	locals := make(map[string]string, len(declared))
	fromSignature := make(map[string]bool, len(declared))
	dropped := make(map[string]bool)
	for name, t := range declared {
		locals[name] = t
		fromSignature[name] = true
	}
	// A later declaration resolves against the earlier ones: `r := eng.Resolver()`
	// needs eng.
	ctx.localTypes = locals

	// set records a declaration of name in the body. qual is "" when its type is
	// not knowable.
	set := func(name, qual string) {
		if name == "_" || dropped[name] {
			return
		}
		if fromSignature[name] {
			if qual != locals[name] {
				delete(locals, name)
				dropped[name] = true
			}
			return
		}
		if qual != "" {
			locals[name] = qual
		}
	}
	// declare is set for a parameter list met inside the body, a closure's.
	declare := func(ft *ast.FuncType) {
		for name, t := range signatureTypes(ft, ctx, typeParams) {
			if dropped[name] {
				continue
			}
			if have, known := locals[name]; known && have != t {
				delete(locals, name)
				dropped[name] = true
				continue
			}
			locals[name] = t
			fromSignature[name] = true
		}
		// A closure parameter of no nameable type still redeclares the name.
		if ft != nil && ft.Params != nil {
			for _, field := range ft.Params.List {
				if declaredType(field.Type, ctx, typeParams) != "" {
					if _, variadic := field.Type.(*ast.Ellipsis); !variadic {
						continue
					}
				}
				for _, n := range field.Names {
					set(n.Name, "")
				}
			}
		}
	}

	ast.Inspect(body, func(n ast.Node) bool {
		switch stmt := n.(type) {
		case *ast.FuncLit:
			declare(stmt.Type)
		case *ast.RangeStmt:
			// A range's key is never typed here. Its value is the element of a
			// collection whose element type is declared, and unknown otherwise.
			if stmt.Tok == token.DEFINE {
				if id, ok := stmt.Key.(*ast.Ident); ok {
					set(id.Name, "")
				}
				if id, ok := stmt.Value.(*ast.Ident); ok {
					set(id.Name, elemOf(stmt.X, ctx))
				}
			}
		case *ast.DeclStmt:
			gd, ok := stmt.Decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				return true
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				if vs.Type != nil {
					if typeStr := typeExprToString(vs.Type); typeStr != "" {
						qual := resolveTypeName(typeStr, ctx)
						for _, name := range vs.Names {
							set(name.Name, qual)
						}
					} else {
						for _, name := range vs.Names {
							set(name.Name, "")
						}
					}
					continue
				}
				// `var x = <rhs>` — infer from the initializer.
				if len(vs.Names) == len(vs.Values) {
					for i, name := range vs.Names {
						set(name.Name, inferRHSType(vs.Values[i], ctx))
					}
				}
			}
		case *ast.AssignStmt:
			if stmt.Tok != token.DEFINE {
				return true
			}
			if len(stmt.Lhs) != len(stmt.Rhs) {
				// `x, err := f()`: one call, its declared results in order.
				var results []string
				if call, ok := stmt.Rhs[0].(*ast.CallExpr); ok && len(stmt.Rhs) == 1 {
					if rt := callResultTypes(call, ctx); len(rt) == len(stmt.Lhs) {
						results = rt
					}
				}
				// `v, ok := x.(*T)` and `v, ok := m[k]`: the value, then a bool.
				if results == nil && len(stmt.Rhs) == 1 && len(stmt.Lhs) == 2 {
					switch stmt.Rhs[0].(type) {
					case *ast.TypeAssertExpr, *ast.IndexExpr:
						if t := exprType(stmt.Rhs[0], ctx); t != "" {
							results = []string{t, ""}
						}
					}
				}
				for i, lhs := range stmt.Lhs {
					if ident, ok := lhs.(*ast.Ident); ok {
						qual := ""
						if results != nil {
							qual = results[i]
						}
						set(ident.Name, qual)
					}
				}
				return true
			}
			for i, lhs := range stmt.Lhs {
				ident, ok := lhs.(*ast.Ident)
				if !ok {
					continue
				}
				set(ident.Name, inferRHSType(stmt.Rhs[i], ctx))
			}
		}
		return true
	})
	return locals
}

// inferRHSType attempts to determine the qualified type of an expression used as
// the right-hand side of a variable assignment. Returns "" when the type is not
// statically knowable.
func inferRHSType(expr ast.Expr, ctx resolveCtx) string {
	switch e := expr.(type) {
	case *ast.UnaryExpr:
		// &Foo{}
		if cl, ok := e.X.(*ast.CompositeLit); ok {
			return compositeLitType(cl, ctx)
		}
	case *ast.CompositeLit:
		// Foo{} or pkg.Foo{}
		return compositeLitType(e, ctx)
	case *ast.CallExpr:
		// What the callee is declared to return, or NewFoo(...) / pkg.NewFoo(...).
		return callResultType(e, ctx)
	case *ast.TypeAssertExpr, *ast.SelectorExpr, *ast.IndexExpr, *ast.ParenExpr:
		// `c := pool.Get().(*Context)`, `root := t[i].root`: what the source states.
		return exprType(e, ctx)
	}
	return ""
}

// compositeLitType returns the qualified type name of a composite literal, or ""
// when the literal has no named type (e.g. slice/map literals).
func compositeLitType(cl *ast.CompositeLit, ctx resolveCtx) string {
	if cl.Type == nil {
		return ""
	}
	typeStr := typeExprToString(cl.Type)
	if typeStr == "" {
		return ""
	}
	return resolveTypeName(typeStr, ctx)
}

// constructorReturnType infers the qualified return type of a call following the
// `New<Type>` convention (e.g. `NewService()` → "pkgDir.Service",
// `auth.NewClient()` → "internal/auth.Client"). Returns "" otherwise.
func constructorReturnType(fun ast.Expr, ctx resolveCtx) string {
	switch f := fun.(type) {
	case *ast.Ident:
		if t := newConventionType(f.Name); t != "" {
			return ctx.pkgDir + "." + t
		}
	case *ast.SelectorExpr:
		if x, ok := f.X.(*ast.Ident); ok {
			if t := newConventionType(f.Sel.Name); t != "" {
				if importPath, ok := ctx.imports[x.Name]; ok {
					return importPath + "." + t
				}
			}
		}
	}
	return ""
}

// newConventionType returns the type name implied by a constructor following the
// `New<Type>` convention (e.g. "NewService" → "Service"), or "" if the name does
// not follow it (e.g. "New", "Newton").
func newConventionType(name string) string {
	rest := strings.TrimPrefix(name, "New")
	if rest == name || rest == "" {
		return ""
	}
	if !unicode.IsUpper([]rune(rest)[0]) {
		return ""
	}
	return rest
}

// resolveTypeName converts a raw type string (e.g. "pkg.Type" or "LocalType")
// to a fully qualified "relPath.Type" form using the import alias map.
// Pre-qualified types (e.g. "..AuthService" stored by collectFieldTypes) are
// passed through unchanged when their alias part is not in the import map.
func resolveTypeName(typeStr string, ctx resolveCtx) string {
	if !strings.Contains(typeStr, ".") {
		return aliasTarget(ctx.pkgDir+"."+typeStr, ctx.aliases)
	}
	parts := strings.SplitN(typeStr, ".", 2)
	if resolvedPkg, ok := ctx.imports[parts[0]]; ok {
		return aliasTarget(resolvedPkg+"."+parts[1], ctx.aliases)
	}
	return typeStr
}

// classifyImport returns "stdlib", "internal", or "external" for a Go import path.
//
// Internal is checked first, and means one of the repository's modules owns the
// path. A module may be named without a dot ("module golf"), and checked second,
// every one of its own packages failed the stdlib test below.
func classifyImport(importPath string, mods goModules) string {
	if _, ok := mods.resolve(importPath); ok {
		return "internal"
	}
	// stdlib: first path segment has no dots
	firstSegment := importPath
	if i := strings.Index(importPath, "/"); i >= 0 {
		firstSegment = importPath[:i]
	}
	if !strings.Contains(firstSegment, ".") {
		return "stdlib"
	}
	return "external"
}

// typeExprToString converts a type expression to a string representation.
// isHTTPHandlerSignature reports whether a function's parameter list is exactly
// (http.ResponseWriter, *http.Request) — net/http's handler contract.
//
// Deliberately NOT written with typeExprToString: that helper strips the pointer
// (*ast.StarExpr recurses into X), so it renders `http.Request` and `*http.Request`
// identically and would tag a by-value `func(http.ResponseWriter, http.Request)` as a
// handler. The pointer is load-bearing here, so the star is matched explicitly.
//
// Param NAMES are irrelevant and never read: `func(rw http.ResponseWriter, req *http.Request)`
// is a handler, and so is the unnamed `func(http.ResponseWriter, *http.Request)`.
func isHTTPHandlerSignature(ft *ast.FuncType) bool {
	if ft == nil || ft.Params == nil {
		return false
	}
	// Count params, not fields: `func(w http.ResponseWriter, r *http.Request)` is two
	// fields of one name each, while a grouped `func(a, b int)` is one field of two.
	var params []ast.Expr
	for _, field := range ft.Params.List {
		n := len(field.Names)
		if n == 0 {
			n = 1 // unnamed parameter
		}
		for i := 0; i < n; i++ {
			params = append(params, field.Type)
		}
	}
	// gin: `func(c *gin.Context)` is the second handler shape in Go, and it is just as
	// structural as the net/http one — a single pointer-to-gin.Context parameter is a
	// request handler and nothing else. Recognising it here rather than in the binder
	// is what keeps the binder language-agnostic: it keys on the prop, never on the
	// framework, so a router gains handler binding by having its signature described
	// at the point the signature is parsed.
	if len(params) == 1 {
		return isPointerToQualifiedType(params[0], "gin", "Context")
	}
	if len(params) != 2 {
		return false
	}
	return isQualifiedType(params[0], "http", "ResponseWriter") && isPointerToQualifiedType(params[1], "http", "Request")
}

// isQualifiedType reports whether expr is the selector `pkg.name`.
func isQualifiedType(expr ast.Expr, pkg, name string) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok || sel.Sel == nil || sel.Sel.Name != name {
		return false
	}
	ident, ok := sel.X.(*ast.Ident)
	return ok && ident.Name == pkg
}

// isPointerToQualifiedType reports whether expr is `*pkg.name`.
func isPointerToQualifiedType(expr ast.Expr, pkg, name string) bool {
	star, ok := expr.(*ast.StarExpr)
	if !ok {
		return false
	}
	return isQualifiedType(star.X, pkg, name)
}

func typeExprToString(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return typeExprToString(t.X)
	case *ast.SelectorExpr:
		if x, ok := t.X.(*ast.Ident); ok {
			return x.Name + "." + t.Sel.Name
		}
	case *ast.IndexExpr:
		return typeExprToString(t.X)
	}
	return ""
}

// namedTypesIn appends the named types a type expression mentions, as written
// ("Layer", "other.Thing"), looking through whatever wraps them: pointers, slices,
// arrays, maps, channels, generic arguments, function signatures and anonymous
// structs. An interface literal names methods, not types it stores, and is skipped.
func namedTypesIn(expr ast.Expr, out []string) []string {
	fields := func(list *ast.FieldList) {
		if list == nil {
			return
		}
		for _, f := range list.List {
			out = namedTypesIn(f.Type, out)
		}
	}
	switch t := expr.(type) {
	case *ast.Ident:
		out = append(out, t.Name)
	case *ast.SelectorExpr:
		if x, ok := t.X.(*ast.Ident); ok {
			out = append(out, x.Name+"."+t.Sel.Name)
		}
	case *ast.StarExpr:
		out = namedTypesIn(t.X, out)
	case *ast.ParenExpr:
		out = namedTypesIn(t.X, out)
	case *ast.Ellipsis:
		out = namedTypesIn(t.Elt, out)
	case *ast.ArrayType:
		out = namedTypesIn(t.Elt, out)
	case *ast.MapType:
		out = namedTypesIn(t.Key, out)
		out = namedTypesIn(t.Value, out)
	case *ast.ChanType:
		out = namedTypesIn(t.Value, out)
	case *ast.IndexExpr:
		out = namedTypesIn(t.X, out)
		out = namedTypesIn(t.Index, out)
	case *ast.IndexListExpr:
		out = namedTypesIn(t.X, out)
		for _, arg := range t.Indices {
			out = namedTypesIn(arg, out)
		}
	case *ast.FuncType:
		fields(t.Params)
		fields(t.Results)
	case *ast.StructType:
		fields(t.Fields)
	}
	return out
}

// collectPackageScope returns every name a package declares at top level:
// functions (methods excluded), types, variables and constants.
// collectPackageConsts returns the names of the constants a package declares.
func collectPackageConsts(files []*ast.File) map[string]bool {
	out := map[string]bool{}
	for _, f := range files {
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				if vs, ok := spec.(*ast.ValueSpec); ok {
					for _, n := range vs.Names {
						out[n.Name] = true
					}
				}
			}
		}
	}
	return out
}

func collectPackageScope(files []*ast.File) map[string]bool {
	out := collectPackageFuncs(files)
	for _, f := range files {
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range gd.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					out[s.Name.Name] = true
				case *ast.ValueSpec:
					for _, n := range s.Names {
						out[n.Name] = true
					}
				}
			}
		}
	}
	return out
}

// collectPackageFuncs returns the names of every top-level function declared in
// a package, methods excluded — a method is never called as a bare identifier,
// so including one could only add a false positive.
func collectPackageFuncs(files []*ast.File) map[string]bool {
	out := map[string]bool{}
	for _, f := range files {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Name == nil {
				continue
			}
			out[fn.Name.Name] = true
		}
	}
	return out
}

// --- bounded-loop rules -----------------------------------------------------
//
// A loop only belongs in the Big-O exponent when its trip count grows with the
// input. Counting lexical nesting instead reports a cubic worst case for code that
// is linear, and on this repository's own tree that was most of the high tier: a
// hand-labeled sample of 35 high findings held 3 that scaled and 32 that counted a
// loop which cannot (internal/perf/testdata/high_tier_labels.jsonl).
//
// goForBounded and goRangeBounded were the first two rules. These are the next
// three. Each is a syntactic proof, and each fails closed: a loop it cannot prove
// bounded keeps counting, so the effect is only ever to remove an over-count.

// goForConstBounded reports whether a three-clause for loop runs a number of times
// fixed when it is written: `for d := 1; d <= 10; d++`. The constant may be large,
// which does not matter — it does not grow with the input, so it contributes no
// factor of n.
//
// Only a literal bound counts. A named constant would be sound too, but an
// identifier here cannot be told from a variable without type information, and
// reading `for i := 0; i < n; i++` as bounded would discount the commonest real
// loop there is.
// goForRetryLoop reports a retry loop: `for attempt := 0; attempt <= max; attempt++`
// whose body leaves the loop once the operation succeeds, and whose counter picks
// out no item: it is never an index, and never an argument to a call other than a
// numeric conversion (the backoff's time.Duration(1<<uint(attempt))). Such a loop
// repeats one operation until it works, at most a configured number of times; it does
// not do one per element, so a call in it is no N+1 candidate. A paging loop passes
// its counter to the fetch (fetch(page)), and a loop over a slice indexes with it, so
// both stay what they were.
func goForRetryLoop(x *ast.ForStmt, consts map[string]bool, imports map[string]string) bool {
	if x.Init == nil || x.Cond == nil || x.Post == nil || x.Body == nil {
		return false
	}
	// The bound is a configured limit: a number, a field or another package's constant
	// (c.config.MaxRetries), or a constant of this package (claimConflictRetries). A
	// length, a computed value or a compound condition bounds a walk over input.
	cmp, ok := x.Cond.(*ast.BinaryExpr)
	if !ok || (cmp.Op != token.LSS && cmp.Op != token.LEQ) {
		return false
	}
	// A field of a variable (c.config.MaxRetries) may be a configured limit or input
	// (opts.LineStart); the loop must then also leave on success, as a retry does and a
	// walk over input, which leaves on its end or an error, does not.
	needsSuccessExit := false
	switch b := cmp.Y.(type) {
	case *ast.BasicLit:
		if b.Kind != token.INT {
			return false
		}
	case *ast.SelectorExpr:
		imported := false
		if root, ok := b.X.(*ast.Ident); ok {
			_, imported = imports[root.Name]
		}
		needsSuccessExit = !imported
	case *ast.Ident:
		if !consts[b.Name] {
			return false
		}
	default:
		return false
	}
	as, ok := x.Init.(*ast.AssignStmt)
	if !ok || as.Tok != token.DEFINE || len(as.Lhs) != 1 || len(as.Rhs) != 1 || !isIntLiteral(as.Rhs[0]) {
		return false
	}
	name, ok := identNameOf(as.Lhs[0])
	if !ok {
		return false
	}
	if n, ok := identNameOf(cmp.X); !ok || n != name {
		return false
	}
	if inc, ok := x.Post.(*ast.IncDecStmt); !ok || inc.Tok != token.INC {
		return false
	} else if n, ok := identNameOf(inc.X); !ok || n != name {
		return false
	}
	exits, picksItem, successExit := false, false, false
	var visit func(n ast.Node, inIf bool) bool
	visit = func(n ast.Node, inIf bool) bool {
		ast.Inspect(n, func(c ast.Node) bool {
			switch v := c.(type) {
			case *ast.FuncLit:
				return false // a closure's statements do not leave this loop
			case *ast.IfStmt:
				if v != n {
					if comparesToNil(v.Cond, token.EQL) && leaves(v.Body) {
						successExit = true
					}
					if v.Init != nil {
						visit(v.Init, inIf)
					}
					visit(v.Cond, inIf)
					visit(v.Body, true)
					if v.Else != nil {
						visit(v.Else, true)
					}
					return false
				}
			case *ast.ReturnStmt:
				if inIf {
					exits = true
				}
			case *ast.BranchStmt:
				if inIf && v.Tok == token.BREAK && v.Label == nil {
					exits = true
				}
			case *ast.ForStmt, *ast.RangeStmt, *ast.SwitchStmt, *ast.TypeSwitchStmt, *ast.SelectStmt:
				// A break in a nested loop or switch leaves that, not this loop; a
				// return still leaves it, which the walk below the nested node finds.
				if c != n {
					visitNoBreak(c, func(found bool) { exits = exits || found && inIf }, &picksItem, name)
					return false
				}
			case *ast.IndexExpr:
				if usesIdent(v.Index, name) {
					picksItem = true
				}
			case *ast.SliceExpr:
				if usesIdent(v.Low, name) || usesIdent(v.High, name) || usesIdent(v.Max, name) {
					picksItem = true
				}
			case *ast.CallExpr:
				if !isNumericConversion(v) {
					for _, a := range v.Args {
						if usesIdentOutsideConversions(a, name) {
							picksItem = true
						}
					}
				}
			case *ast.AssignStmt:
				for _, l := range v.Lhs {
					if usesIdent(l, name) {
						picksItem = true // the counter is moved in the body: not a plain count
					}
				}
			}
			return true
		})
		return true
	}
	visit(x.Body, false)
	return exits && !picksItem && (!needsSuccessExit || successExit)
}

// comparesToNil reports a condition with a comparison to nil by op: err == nil.
func comparesToNil(cond ast.Expr, op token.Token) bool {
	found := false
	ast.Inspect(cond, func(c ast.Node) bool {
		if b, ok := c.(*ast.BinaryExpr); ok && b.Op == op {
			if id, ok := b.Y.(*ast.Ident); ok && id.Name == "nil" {
				found = true
			}
		}
		return !found
	})
	return found
}

// leaves reports a block that returns or breaks out of the loop directly.
func leaves(b *ast.BlockStmt) bool {
	for _, st := range b.List {
		switch v := st.(type) {
		case *ast.ReturnStmt:
			return true
		case *ast.BranchStmt:
			if v.Tok == token.BREAK && v.Label == nil {
				return true
			}
		}
	}
	return false
}

// visitNoBreak walks a nested loop, switch or select inside a retry loop's body: a
// return there still leaves the retry loop, a break does not, and the counter's uses
// count the same.
func visitNoBreak(n ast.Node, returned func(bool), picksItem *bool, name string) {
	ast.Inspect(n, func(c ast.Node) bool {
		switch v := c.(type) {
		case *ast.FuncLit:
			return false
		case *ast.ReturnStmt:
			returned(true)
		case *ast.IndexExpr:
			if usesIdent(v.Index, name) {
				*picksItem = true
			}
		case *ast.CallExpr:
			if !isNumericConversion(v) {
				for _, a := range v.Args {
					if usesIdentOutsideConversions(a, name) {
						*picksItem = true
					}
				}
			}
		}
		return true
	})
}

// usesIdent reports whether an expression mentions the identifier name.
func usesIdent(e ast.Node, name string) bool {
	if e == nil {
		return false
	}
	found := false
	ast.Inspect(e, func(c ast.Node) bool {
		if id, ok := c.(*ast.Ident); ok && id.Name == name {
			found = true
		}
		return !found
	})
	return found
}

// usesIdentOutsideConversions is usesIdent, not counting a mention inside a numeric
// conversion: time.Sleep(d * time.Duration(1<<uint(attempt))) computes a delay from the
// counter, it does not hand the counter over.
func usesIdentOutsideConversions(e ast.Node, name string) bool {
	if e == nil {
		return false
	}
	found := false
	ast.Inspect(e, func(c ast.Node) bool {
		if call, ok := c.(*ast.CallExpr); ok && isNumericConversion(call) {
			return false
		}
		if id, ok := c.(*ast.Ident); ok && id.Name == name {
			found = true
		}
		return !found
	})
	return found
}

// numericTypes are the conversions a backoff computes a delay with.
var numericTypes = map[string]bool{
	"int": true, "int8": true, "int16": true, "int32": true, "int64": true,
	"uint": true, "uint8": true, "uint16": true, "uint32": true, "uint64": true,
	"float32": true, "float64": true, "Duration": true,
}

// isNumericConversion reports a call that converts one value to a numeric type:
// uint(attempt), time.Duration(n).
func isNumericConversion(c *ast.CallExpr) bool {
	if len(c.Args) != 1 {
		return false
	}
	switch f := c.Fun.(type) {
	case *ast.Ident:
		return numericTypes[f.Name]
	case *ast.SelectorExpr:
		return numericTypes[f.Sel.Name]
	}
	return false
}

func goForConstBounded(x *ast.ForStmt) bool {
	if x.Init == nil || x.Cond == nil || x.Post == nil {
		return false
	}
	as, ok := x.Init.(*ast.AssignStmt)
	if !ok || as.Tok != token.DEFINE || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
		return false
	}
	name, ok := identNameOf(as.Lhs[0])
	if !ok || !isIntLiteral(as.Rhs[0]) {
		return false
	}
	// The step has to move the variable the bound is about, or the bound proves
	// nothing about how often the body runs.
	inc, ok := x.Post.(*ast.IncDecStmt)
	if !ok {
		return false
	}
	if n, ok := identNameOf(inc.X); !ok || n != name {
		return false
	}
	cmp, ok := x.Cond.(*ast.BinaryExpr)
	if !ok {
		return false
	}
	switch cmp.Op {
	case token.LSS, token.LEQ, token.GTR, token.GEQ, token.NEQ:
	default:
		return false
	}
	l, lok := identNameOf(cmp.X)
	return lok && l == name && isIntLiteral(cmp.Y)
}

// goForSharesOuterCursor reports whether a for loop advances an index that an
// enclosing for loop is already advancing.
//
//	for i := 0; i < len(b); i++ {
//	        if b[i] != '"' { continue }
//	        for i++; i < len(b) && b[i] != '"'; i++ { … }
//	}
//
// That is one pass over b, not a pass per byte: the inner loop moves the same
// cursor forward, so across the whole nest the body runs len(b) times. Lexically it
// is two levels, and counting it as two is what reported O(n²) for a scanner.
//
// The loop must declare no index of its own, and its condition must test the outer
// loop's induction variable. Both halves matter: a loop with its own init counts
// normally, and a condition that merely mentions some outer variable (a chain walk
// like `for x != nil { x = x.next }`) does not share a cursor.
func goForSharesOuterCursor(x *ast.ForStmt, scopes []loopScope) bool {
	if x.Init != nil || x.Cond == nil {
		return false
	}
	cursors := map[string]bool{}
	for _, s := range scopes {
		if s.cursor != "" {
			cursors[s.cursor] = true
		}
	}
	if len(cursors) == 0 {
		return false
	}
	shared := false
	ast.Inspect(x.Cond, func(n ast.Node) bool {
		if shared {
			return false
		}
		if id, ok := n.(*ast.Ident); ok && cursors[id.Name] {
			shared = true
		}
		return !shared
	})
	return shared
}

// forCursor returns the index variable a for loop declares and steps, or "" when it
// has none. It is what goForSharesOuterCursor matches an inner loop against.
func forCursor(x *ast.ForStmt) string {
	as, ok := x.Init.(*ast.AssignStmt)
	if !ok || as.Tok != token.DEFINE || len(as.Lhs) != 1 {
		return ""
	}
	name, ok := identNameOf(as.Lhs[0])
	if !ok {
		return ""
	}
	// Stepped in the post clause, or in the body — a scanner routinely advances its
	// own index and leaves the post clause empty.
	if inc, isInc := x.Post.(*ast.IncDecStmt); isInc {
		if n, ok := identNameOf(inc.X); ok && n == name {
			return name
		}
	}
	if x.Post == nil {
		return name
	}
	return ""
}

// goRangeBoundedLocal reports whether a range loop walks a local collection that
// cannot grow with the input — one built from a composite literal and appended to a
// fixed number of times, like the at-most-two path forms in
// internal/facts.Graph.GovernedByPage:
//
//	forms := []string{mf.File}
//	if trimmed := …; trimmed != mf.File { forms = append(forms, trimmed) }
//	for _, form := range forms { … }
//
// Ranging that adds a factor of two, not a factor of n, however many facts the graph
// holds. bounded is the set boundedLocals proved for this body.
func goRangeBoundedLocal(x *ast.RangeStmt, bounded map[string]bool) bool {
	if len(bounded) == 0 {
		return false
	}
	name, ok := identNameOf(x.X)
	return ok && bounded[name]
}

// boundedLocals returns the local variables whose contents cannot grow with the
// input: every assignment to them is a composite literal, every append adds a fixed
// number of elements, and no append happens anywhere the literal is not re-applied.
//
// That last condition is what separates `forms` above from an accumulator. Both are
// assigned a literal and appended to; the difference is WHERE. `forms` is rebuilt
// from its literal on every iteration of the loop that appends to it, so it never
// carries more than a couple of entries. An accumulator is initialised once outside
// the loop that fills it, and grows with whatever the loop walks. So an append
// counts only when the same loop body also re-initialises the variable.
//
// Name-based, and therefore approximate in the presence of shadowing — the same
// approximation the rest of this walker makes. It fails closed: an unproven
// variable is simply not in the set.
func boundedLocals(body ast.Node) map[string]bool {
	// For each name, the enclosing-loop extents (0 = function level) in which it is
	// assigned a composite literal, and in which it is appended to.
	inits := map[string]map[token.Pos]bool{}
	appends := map[string]map[token.Pos]bool{}
	disqualified := map[string]bool{}

	var loopEnds []token.Pos
	innermost := func() token.Pos {
		if len(loopEnds) == 0 {
			return 0
		}
		return loopEnds[len(loopEnds)-1]
	}
	note := func(m map[string]map[token.Pos]bool, name string) {
		if m[name] == nil {
			m[name] = map[token.Pos]bool{}
		}
		m[name][innermost()] = true
	}

	ast.Inspect(body, func(n ast.Node) bool {
		if n == nil {
			return false
		}
		for len(loopEnds) > 0 && n.Pos() >= loopEnds[len(loopEnds)-1] {
			loopEnds = loopEnds[:len(loopEnds)-1]
		}
		switch x := n.(type) {
		case *ast.ForStmt:
			loopEnds = append(loopEnds, x.End())
		case *ast.RangeStmt:
			loopEnds = append(loopEnds, x.End())
			// A variable that receives loop elements is whatever the loop walks.
			for _, v := range rangeLoopVars(x) {
				disqualified[v] = true
			}
		case *ast.AssignStmt:
			for i, lhs := range x.Lhs {
				name, ok := identNameOf(lhs)
				if !ok {
					// Writing through an index or a field grows a collection in a way
					// this rule cannot see; refuse the variable entirely.
					if id, found := rootIdent(lhs); found {
						disqualified[id] = true
					}
					continue
				}
				if i >= len(x.Rhs) {
					// A multi-value call (`a, b := f()`): the contents come from
					// somewhere this rule cannot follow.
					disqualified[name] = true
					continue
				}
				switch rhs := x.Rhs[i].(type) {
				case *ast.CompositeLit:
					note(inits, name)
				case *ast.CallExpr:
					if isFixedAppendTo(rhs, name) {
						note(appends, name)
					} else {
						disqualified[name] = true
					}
				default:
					disqualified[name] = true
				}
			}
		}
		return true
	})

	out := map[string]bool{}
	for name, initScopes := range inits {
		if disqualified[name] {
			continue
		}
		ok := true
		for scope := range appends[name] {
			if !initScopes[scope] {
				ok = false // appended where the literal is not re-applied: an accumulator
				break
			}
		}
		if ok {
			out[name] = true
		}
	}
	return out
}

// isFixedAppendTo reports whether call is `append(name, a, b, …)` with a fixed
// number of elements. A spread (`append(a, b...)`) adds however many b holds, which
// is exactly the growth this rule must not wave through.
func isFixedAppendTo(call *ast.CallExpr, name string) bool {
	id, ok := call.Fun.(*ast.Ident)
	if !ok || id.Name != "append" || call.Ellipsis.IsValid() || len(call.Args) == 0 {
		return false
	}
	first, ok := identNameOf(call.Args[0])
	return ok && first == name
}

// identNameOf returns the name of a bare identifier expression.
func identNameOf(e ast.Expr) (string, bool) {
	id, ok := e.(*ast.Ident)
	if !ok {
		return "", false
	}
	return id.Name, true
}

// rootIdent returns the identifier at the base of an index or selector chain, so an
// assignment through `m[k]` or `s.field` can be charged to `m` and `s`.
func rootIdent(e ast.Expr) (string, bool) {
	for {
		switch x := e.(type) {
		case *ast.Ident:
			return x.Name, true
		case *ast.IndexExpr:
			e = x.X
		case *ast.SelectorExpr:
			e = x.X
		case *ast.StarExpr:
			e = x.X
		case *ast.ParenExpr:
			e = x.X
		default:
			return "", false
		}
	}
}

// isIntLiteral reports whether e is an integer literal, optionally signed.
func isIntLiteral(e ast.Expr) bool {
	if u, ok := e.(*ast.UnaryExpr); ok && (u.Op == token.SUB || u.Op == token.ADD) {
		e = u.X
	}
	lit, ok := e.(*ast.BasicLit)
	return ok && lit.Kind == token.INT
}

// referencesParam reports whether expr reaches into a parameter or the receiver.
// A loop over one of those walks something the CALLER handed in, which is what makes
// it a continuation of the caller's traversal rather than a traversal of its own.
func referencesParam(expr ast.Expr, params map[string]bool) bool {
	// A for statement may have no condition (`for i := 0; ; i++`), and ast.Inspect
	// panics on a nil node rather than ignoring it.
	if expr == nil || len(params) == 0 {
		return false
	}
	found := false
	ast.Inspect(expr, func(n ast.Node) bool {
		if found {
			return false
		}
		if id, ok := n.(*ast.Ident); ok && params[id.Name] {
			found = true
		}
		return !found
	})
	return found
}

// passesLoopElement reports whether a call hands the callee an element of an
// enclosing loop, as an argument or as the receiver it is invoked on.
func passesLoopElement(call *ast.CallExpr, scopes []loopScope) bool {
	for _, arg := range call.Args {
		if referencesLoopVar(arg, scopes) {
			return true
		}
	}
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
		return referencesLoopVar(sel.X, scopes)
	}
	return false
}

// funcParamNames returns the parameter and receiver names of a function
// declaration, the set referencesParam matches a ranged collection against.
func funcParamNames(fn *ast.FuncDecl) map[string]bool {
	out := map[string]bool{}
	add := func(fl *ast.FieldList) {
		if fl == nil {
			return
		}
		for _, f := range fl.List {
			for _, n := range f.Names {
				if n.Name != "" && n.Name != "_" {
					out[n.Name] = true
				}
			}
		}
	}
	add(fn.Recv)
	if fn.Type != nil {
		add(fn.Type.Params)
	}
	return out
}

// rhsExpr wraps an assignment's right-hand side so it can be inspected as one node.
// A multi-value assignment has several, and any of them carrying the loop element is
// enough to taint the whole statement's targets — which is the conservative direction
// for a rule whose only effect is to REMOVE an over-count.
func rhsExpr(x *ast.AssignStmt) ast.Expr {
	if len(x.Rhs) == 1 {
		return x.Rhs[0]
	}
	return &ast.CompositeLit{Elts: x.Rhs}
}
