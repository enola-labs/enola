package goextractor

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/enola-labs/enola/internal/extractors/detectnames"
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
// manifests whose contents steer resolution (readModulePath reads go.mod). It
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
	modulePath := readModulePath(repoPath)

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
	globalFieldTypes := make(map[string]string)
	for _, pkgDir := range pkgDirs {
		select {
		case <-ctx.Done():
			return allFacts, ctx.Err()
		default:
		}
		for k, v := range collectFieldTypes(parsedPkgs[pkgDir].parsedFiles, pkgDir, modulePath, pkgNames) {
			globalFieldTypes[k] = v
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
	routePrefixes := buildRoutePrefixIndex(parsedPkgs, modulePath, pkgNames, globalFieldTypes)

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
		pkgFacts := e.extractPackage(fset, pkgDir, pp, modulePath, globalFieldTypes, pkgNames, grpcStubs, routePrefixes)
		allFacts = append(allFacts, pkgFacts...)
	}

	return allFacts, nil
}

func (e *GoExtractor) extractPackage(fset *token.FileSet, pkgDir string, pp *parsedPkg, modulePath string, fieldTypes map[string]string, pkgNames map[string]string, grpcStubs *goGRPCStubIndex, routePrefixes routePrefixIndex) []facts.Fact {
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

	for _, relFile := range pp.relFiles {
		f, ok := pp.fileMap[relFile]
		if !ok {
			continue
		}
		result = append(result, e.extractFile(fset, f, relFile, pkgDir, modulePath, fieldTypes, pkgNames, grpcStubs, pkgVarClients, baseURLLits, routePrefixes, pkgFuncs)...)
	}

	moduleFact := facts.Fact{
		Kind: facts.KindModule,
		Name: pkgDir,
		File: pkgDir,
		Props: map[string]any{
			"package":  pp.pkgName,
			"language": "go",
		},
	}
	// Store the full Go module path on the root package fact so that the graph
	// layer can normalise cross-repo call targets (Bug 2).
	if pkgDir == "." && modulePath != "" {
		moduleFact.Props["modulePath"] = modulePath
	}
	result = append(result, moduleFact)

	return result
}

func (e *GoExtractor) extractFile(fset *token.FileSet, f *ast.File, relFile, pkgDir, modulePath string, fieldTypes map[string]string, pkgNames map[string]string, grpcStubs *goGRPCStubIndex, pkgVarClients map[string]string, baseURLLits map[string][]string, routePrefixes routePrefixIndex, pkgFuncs map[string]bool) []facts.Fact {
	var result []facts.Fact

	// Build per-file import alias map for call resolution.
	fileImports := buildFileImports(f, modulePath, pkgNames)

	// Extract imports
	for _, imp := range f.Imports {
		importPath := strings.Trim(imp.Path.Value, `"`)

		// Normalize internal import targets to short paths (e.g.
		// "github.com/foo/bar/internal/pkg" → "internal/pkg") so they
		// match the module fact names used elsewhere in the store.
		relTarget := importPath
		if modulePath != "" {
			if importPath == modulePath {
				relTarget = "."
			} else if strings.HasPrefix(importPath, modulePath+"/") {
				relTarget = strings.TrimPrefix(importPath, modulePath+"/")
			}
		}

		result = append(result, facts.Fact{
			Kind: facts.KindDependency,
			Name: pkgDir + " -> " + importPath,
			File: relFile,
			Line: fset.Position(imp.Pos()).Line,
			Props: map[string]any{
				"language": "go",
				"source":   classifyImport(importPath, modulePath),
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
			result = append(result, e.extractFunc(fset, d, relFile, pkgDir, modulePath, fileImports, fieldTypes, pkgFuncs)...)
		case *ast.GenDecl:
			result = append(result, e.extractGenDecl(fset, d, relFile, pkgDir, modulePath, fileImports)...)
		}
	}

	// Extract route registrations
	result = append(result, extractRoutes(fset, f, relFile, pkgDir, routePrefixes)...)

	// Extract outbound HTTP-client calls
	result = append(result, extractHTTPClientFacts(fset, f, relFile, pkgDir, baseURLLits)...)

	// Extract outbound gRPC-client calls
	result = append(result, extractGRPCClientFacts(fset, f, relFile, pkgDir, modulePath, fileImports, fieldTypes, pkgVarClients, grpcStubs)...)

	// Extract storage patterns
	result = append(result, extractStorage(fset, f, relFile, pkgDir)...)

	// Extract Kafka topic references (produced/consumed), for async cross-repo links
	result = append(result, extractKafkaFacts(fset, f, relFile, pkgDir)...)

	return result
}

func (e *GoExtractor) extractFunc(fset *token.FileSet, fn *ast.FuncDecl, relFile, pkgDir, modulePath string, fileImports map[string]string, fieldTypes map[string]string, pkgFuncs map[string]bool) []facts.Fact {
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
		symbolFact.Props["receiver"] = receiver
	}

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
		symbolFact.Props["http_handler"] = true
	}

	// Extract function calls and per-function complexity metrics in a single
	// body walk. The metrics ride on Props (map[string]any) and feed the
	// performance analyzer; they are parser-derived, never inferred.
	if fn.Body != nil {
		ctx := resolveCtx{
			pkgDir:     pkgDir,
			modulePath: modulePath,
			imports:    fileImports,
			recvVar:    recvVar,
			recvType:   receiver,
			fieldTypes: fieldTypes,
			pkgFuncs:   pkgFuncs,
		}
		ctx.localTypes = collectLocalTypes(fn.Body, ctx)
		m := analyzeBody(fn.Body, ctx, qualifiedName)
		for _, call := range m.calls {
			symbolFact.Relations = append(symbolFact.Relations, facts.Relation{
				Kind:   facts.RelCalls,
				Target: call,
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
		symbolFact.Props["cyclomatic"] = m.cyclomatic
		if m.loopDepth > 0 {
			symbolFact.Props["loop_depth"] = m.loopDepth
			// Emit the scaling depth (bounded loops discounted) alongside — even when 0 —
			// so the consumer distinguishes "all loops bounded" from "signal absent".
			symbolFact.Props["scaling_loop_depth"] = m.scalingLoopDepth
		}
		if m.loopCount > 0 {
			symbolFact.Props["loop_count"] = m.loopCount
		}
		if len(m.clientPathCalls) > 0 {
			symbolFact.Props["client_path_calls"] = m.clientPathCalls
		}
		if len(m.callsInLoop) > 0 {
			symbolFact.Props["calls_in_loop"] = m.callsInLoop
			// Emit the N+1 subset alongside — even when EMPTY — so the consumer
			// distinguishes "no call repeats" from "signal absent". An omitted key makes
			// perf.scalingLoopCalls() fall back to the unfiltered calls_in_loop, which
			// silently defeats the discount in exactly the case it exists for: every
			// in-loop call sitting inside a constant loop.
			if m.callsInScalingLoop == nil {
				m.callsInScalingLoop = []string{}
			}
			symbolFact.Props["calls_in_scaling_loop"] = m.callsInScalingLoop
		}
		if m.recursiveSelf {
			symbolFact.Props["recursive_self"] = true
		}
	}

	result = append(result, symbolFact)
	return result
}

func (e *GoExtractor) extractGenDecl(fset *token.FileSet, gd *ast.GenDecl, relFile, pkgDir, modulePath string, fileImports map[string]string) []facts.Fact {
	var result []facts.Fact

	for _, spec := range gd.Specs {
		switch s := spec.(type) {
		case *ast.TypeSpec:
			result = append(result, e.extractTypeSpec(fset, gd, s, relFile, pkgDir, modulePath, fileImports)...)
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

func (e *GoExtractor) extractTypeSpec(fset *token.FileSet, gd *ast.GenDecl, ts *ast.TypeSpec, relFile, pkgDir, modulePath string, fileImports map[string]string) []facts.Fact {
	var result []facts.Fact

	name := ts.Name.Name
	exported := ts.Name.IsExported()
	qualifiedName := pkgDir + "." + name
	ctx := resolveCtx{pkgDir: pkgDir, modulePath: modulePath, imports: fileImports}

	var kind string
	var implements []string

	var instantiates []string
	switch t := ts.Type.(type) {
	case *ast.StructType:
		kind = facts.SymbolStruct
		if t.Fields != nil {
			for _, field := range t.Fields.List {
				if len(field.Names) == 0 {
					// Embedded type — a potential interface implementation.
					if embeddedName := typeExprToString(field.Type); embeddedName != "" {
						implements = append(implements, embeddedName)
					}
					continue
				}
				// A named field of an internal struct type USES that type. Emit a
				// usage edge so a struct referenced only as a field type is not a
				// dead-code false positive (type usage is otherwise not edge-tracked).
				if target := resolveTypeName(typeExprToString(field.Type), ctx); target != "" && isInternalTypeTarget(target, pkgDir) {
					instantiates = append(instantiates, target)
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
	modulePath string
	imports    map[string]string // alias → relative package path
	recvVar    string            // receiver variable name, e.g. "h"
	recvType   string            // receiver type (star stripped), e.g. "AuthHandler"
	fieldTypes map[string]string // "pkgDir.TypeName.fieldName" → pre-qualified typeString
	localTypes map[string]string // local variable name → qualified type, e.g. "svc" → "internal/auth.Service"
	pkgFuncs   map[string]bool   // this package's top-level function names
}

// bodyMetrics holds the call list and the per-function complexity signals
// derived from a single walk of a function body.
type bodyMetrics struct {
	calls        []string // resolved call targets, deduped, in source order
	instantiates []string // resolved internal struct types constructed as composite literals, deduped
	callsInLoop  []string // subset of calls invoked at loop nesting depth >= 1
	// clientPathCalls records "callee\x00path\x00METHOD" for every call passing a
	// path-shaped string literal. It is a CANDIDATE list and nothing more: whether
	// the callee is an HTTP seam cannot be known here, because the seam usually
	// lives in another package and this pass sees one. The seam binder decides.
	clientPathCalls    []string
	callsInScalingLoop []string // subset of calls invoked at scaling (unbounded) nesting depth >= 1
	loopDepth          int      // max nesting depth of for/range loops
	scalingLoopDepth   int      // max nesting counting only unbounded (input-scaling) loops
	loopCount          int      // total number of for/range loops
	cyclomatic         int      // McCabe complexity (1 + decision points)
	recursiveSelf      bool     // body directly calls the enclosing function
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
func analyzeBody(body ast.Node, ctx resolveCtx, selfName string) bodyMetrics {
	var m bodyMetrics
	decisions := 0
	seen := make(map[string]bool)
	instSeen := make(map[string]bool)
	inLoopSeen := make(map[string]bool)
	inScalingSeen := make(map[string]bool)
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

	ast.Inspect(body, func(n ast.Node) bool {
		if n == nil {
			return false
		}
		// Pop loops whose extent we have now left.
		for len(loopEnds) > 0 && n.Pos() >= loopEnds[len(loopEnds)-1] {
			loopEnds = loopEnds[:len(loopEnds)-1]
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
			if !goForBounded(x) {
				scalingEnds = append(scalingEnds, x.End())
				if len(scalingEnds) > m.scalingLoopDepth {
					m.scalingLoopDepth = len(scalingEnds)
				}
			}
			// A `for {}` is infinite, not constant: it repeats.
			repeatEnds = append(repeatEnds, x.End())
			loopScopes = append(loopScopes, loopScope{end: x.End(), vars: forLoopVars(x)})
		case *ast.RangeStmt:
			m.loopCount++
			decisions++
			loopEnds = append(loopEnds, x.End())
			if len(loopEnds) > m.loopDepth {
				m.loopDepth = len(loopEnds)
			}
			if !goRangeBounded(x) {
				// A hierarchical loop — one whose ranged collection is reached THROUGH
				// an enclosing loop variable (`range pkg.Files`, `range pkgs[pkgDir]…`)
				// — visits each element once across the whole nest, so it adds no factor
				// of n to the scaling exponent. Only a loop over an independent or same
				// collection (all-pairs) multiplies. Derived loops still repeat, so they
				// stay N+1 candidates (repeatEnds) — only the scaling depth is spared.
				if !referencesLoopVar(x.X, loopScopes) {
					scalingEnds = append(scalingEnds, x.End())
					if len(scalingEnds) > m.scalingLoopDepth {
						m.scalingLoopDepth = len(scalingEnds)
					}
				}
				repeatEnds = append(repeatEnds, x.End())
			}
			loopScopes = append(loopScopes, loopScope{end: x.End(), vars: rangeLoopVars(x)})
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
			chain := flattenSelector(callee)
			if chain == nil {
				return true
			}
			resolved := resolveChain(chain, ctx)
			if resolved == "" {
				return true
			}
			if !seen[resolved] {
				seen[resolved] = true
				m.calls = append(m.calls, resolved)
			}
			if len(loopEnds) > 0 && !inLoopSeen[resolved] {
				inLoopSeen[resolved] = true
				m.callsInLoop = append(m.callsInLoop, resolved)
			}
			// A call inside a loop that repeats a non-constant number of times is an N+1
			// candidate. A call only ever inside a constant loop (range over a composite
			// literal) runs a fixed number of times and is not — but a `for {}` DOES
			// repeat, so its calls stay candidates even though its depth is discounted.
			if len(repeatEnds) > 0 && !inScalingSeen[resolved] {
				inScalingSeen[resolved] = true
				m.callsInScalingLoop = append(m.callsInScalingLoop, resolved)
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
			// edge, guarded to module-internal types so we never point at stdlib or
			// third-party types (which would create phantom "used" marks).
			if t := compositeLitType(x, ctx); t != "" && isInternalTypeTarget(t, ctx.pkgDir) && !instSeen[t] {
				instSeen[t] = true
				m.instantiates = append(m.instantiates, t)
			}
		}
		return true
	})
	m.cyclomatic = 1 + decisions
	return m
}

// loopScope records the variables an enclosing loop introduces, with the loop's
// end position so it can be popped by the position-based nesting walk.
type loopScope struct {
	end  token.Pos
	vars []string
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
	vars := map[string]bool{}
	for _, s := range scopes {
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

// isInternalTypeTarget reports whether a resolved type target (pkg.Type) names a
// module-internal type — either the same package, or an internal subpackage whose
// path segment contains "/" but no domain "." (excluding stdlib single-segment
// packages like "bytes" and third-party paths like "github.com/x/y"). Conservative:
// it may miss an internal package located at a single-segment repo-root dir, which
// only forgoes an edge (never creates a wrong one).
func isInternalTypeTarget(resolved, pkgDir string) bool {
	i := strings.LastIndex(resolved, ".")
	if i < 0 {
		return false
	}
	pkg := resolved[:i]
	if pkg == pkgDir {
		return true
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
// imports in f. The relative path strips the module prefix to match fact naming.
// Exact-match self-imports (importPath == modulePath) map to "." (the root package).
// Blank ("_") and dot (".") imports are excluded.
// pkgNames maps pkgDir → declared package name (from parsing); it is used to
// resolve the implicit alias for packages whose path base is not a valid identifier
// (e.g. "github.com/x/go-auth" has base "go-auth" but package name "auth").
func buildFileImports(f *ast.File, modulePath string, pkgNames map[string]string) map[string]string {
	m := make(map[string]string)
	for _, imp := range f.Imports {
		importPath := strings.Trim(imp.Path.Value, `"`)

		if imp.Name != nil {
			if imp.Name.Name == "_" || imp.Name.Name == "." {
				continue
			}
		}

		relTarget := importPath
		if modulePath != "" {
			if importPath == modulePath {
				// Subpackage importing the module root — map to "." so that
				// call targets resolve to root-package fact names.
				relTarget = "."
			} else if strings.HasPrefix(importPath, modulePath+"/") {
				relTarget = strings.TrimPrefix(importPath, modulePath+"/")
			}
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
func collectFieldTypes(files []*ast.File, pkgDir, modulePath string, pkgNames map[string]string) map[string]string {
	m := make(map[string]string)
	for _, f := range files {
		fileImports := buildFileImports(f, modulePath, pkgNames)
		ctx := resolveCtx{pkgDir: pkgDir, imports: fileImports}
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
				}
			}
		}
	}
	return m
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
			key := qualType + "." + fieldName
			nextType, ok := ctx.fieldTypes[key]
			if !ok {
				return strings.Join(chain, ".")
			}
			qualType = resolveTypeName(nextType, ctx)
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
//
// Variable names are recorded so that calls like `svc.Do()` resolve to the
// canonical method fact name instead of dangling on a raw join.
func collectLocalTypes(body ast.Node, ctx resolveCtx) map[string]string {
	locals := make(map[string]string)
	ast.Inspect(body, func(n ast.Node) bool {
		switch stmt := n.(type) {
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
							if name.Name != "_" {
								locals[name.Name] = qual
							}
						}
					}
					continue
				}
				// `var x = <rhs>` — infer from the initializer.
				if len(vs.Names) == len(vs.Values) {
					for i, name := range vs.Names {
						if name.Name == "_" {
							continue
						}
						if qual := inferRHSType(vs.Values[i], ctx); qual != "" {
							locals[name.Name] = qual
						}
					}
				}
			}
		case *ast.AssignStmt:
			if stmt.Tok != token.DEFINE || len(stmt.Lhs) != len(stmt.Rhs) {
				return true
			}
			for i, lhs := range stmt.Lhs {
				ident, ok := lhs.(*ast.Ident)
				if !ok || ident.Name == "_" {
					continue
				}
				if qual := inferRHSType(stmt.Rhs[i], ctx); qual != "" {
					locals[ident.Name] = qual
				}
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
		// NewFoo(...) / pkg.NewFoo(...)
		return constructorReturnType(e.Fun, ctx)
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
		return ctx.pkgDir + "." + typeStr
	}
	parts := strings.SplitN(typeStr, ".", 2)
	if resolvedPkg, ok := ctx.imports[parts[0]]; ok {
		return resolvedPkg + "." + parts[1]
	}
	return typeStr
}

// readModulePath reads the module path from go.mod in the given repo.
func readModulePath(repoPath string) string {
	data, err := os.ReadFile(filepath.Join(repoPath, "go.mod"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module "))
		}
	}
	return ""
}

// classifyImport returns "stdlib", "internal", or "external" for a Go import path.
func classifyImport(importPath, modulePath string) string {
	// stdlib: first path segment has no dots
	firstSegment := importPath
	if i := strings.Index(importPath, "/"); i >= 0 {
		firstSegment = importPath[:i]
	}
	if !strings.Contains(firstSegment, ".") {
		return "stdlib"
	}
	// internal: starts with the module path
	if modulePath != "" && (importPath == modulePath || strings.HasPrefix(importPath, modulePath+"/")) {
		return "internal"
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
