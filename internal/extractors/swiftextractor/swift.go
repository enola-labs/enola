package swiftextractor

import (
	"bufio"
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/enola-labs/enola/internal/extractors/detectnames"
	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/parallel"
)

// SwiftExtractor extracts architectural facts from Swift source code using
// tree-sitter AST parsing (see swift_ast.go for the walker implementation).
type SwiftExtractor struct{}

// New creates a new SwiftExtractor.
func New() *SwiftExtractor {
	return &SwiftExtractor{}
}

func (e *SwiftExtractor) Name() string {
	return "swift"
}

// Detect returns true if the repository looks like a Swift or iOS project.
func (e *SwiftExtractor) Detect(repoPath string) (bool, error) {
	return e.DetectFiles(repoPath, detectnames.Walk(repoPath))
}

// DetectFiles implements plugin.FileListDetector.
//
// The rule this replaces was a root Package.swift or an .xcodeproj within two
// directory levels. On a Flutter monorepo neither exists near the root:
// flutterfire's projects are at packages/<pkg>/<pkg>/example/ios/Runner.xcodeproj,
// so all 147 of its Swift files were unindexed, and 482 of flutter-packages'. This
// was the single largest miss on that corpus, and it is one the depth-limited
// detectors' story did not cover — root-anchoring is the same bug wearing different
// clothes.
func (e *SwiftExtractor) DetectFiles(_ string, files []string) (bool, error) {
	for _, rel := range files {
		if detectnames.HasAnySegment(rel, "Pods", "Carthage", ".build", "DerivedData") {
			continue
		}
		name := detectnames.Base(rel)
		if name == "Package.swift" || strings.HasSuffix(name, ".swift") {
			return true, nil
		}
		// An Xcode project is a DIRECTORY, so it reaches the name set only through the
		// files inside it; match on the segment rather than the leaf.
		for _, seg := range strings.Split(rel, "/") {
			if matchesXcodeProject(seg) {
				return true, nil
			}
		}
	}
	return false, nil
}

// Extract parses Swift files with tree-sitter and emits architectural facts.
//
// It uses a two-pass approach:
//   - Pass 1: walk each file's AST (extractFileAST) to emit declaration, import,
//     iOS-classification and call-graph facts, while building a type→module index.
//   - A canonicalisation step rewrites bare call/instantiate/inject/depends_on
//     edge targets to canonical "<dir>.<Type>" fact names using that index, so
//     the graph's reverse traversal (impact_analysis) finds Swift dependents.
//   - Pass 2: scan type references to discover cross-module import dependencies.
func (e *SwiftExtractor) Extract(ctx context.Context, repoPath string, files []string) ([]facts.Fact, error) {
	var allFacts []facts.Fact

	isiOS := detectiOSProject(repoPath)

	modules := make(map[string]bool)
	typeIndex := make(map[string]string)     // simple type name -> module identity
	typeAmbiguous := make(map[string]bool)   // simple type name -> defined in >1 module
	methodIndex := make(map[string][]string) // method short name -> qualified dir.Type.method names
	funcIndex := make(map[string][]string)   // top-level function short name -> qualified dir.func names
	dirToFile := make(map[string]string)     // module identity -> a representative source file
	var swiftFiles []string
	var manifestFiles []string // Package.swift manifests, parsed after the walk

	// Pass 1: AST extraction + type index. Package.swift manifests are deferred so
	// that dirToFile is fully populated before the manifest parser resolves each
	// target's representative source file.
	for _, relFile := range files {
		if !isSwiftFile(relFile) {
			continue
		}
		if filepath.Base(relFile) == "Package.swift" {
			manifestFiles = append(manifestFiles, relFile)
			continue
		}
		swiftFiles = append(swiftFiles, relFile)
	}

	// Resolve modules at the target level. Parse the XcodeGen project.yml and the
	// SPM manifest target roots up-front, then build a resolver mapping each file
	// to its owning target module. Both signals are additive: when neither covers
	// a file, moduleForFile falls back to the file's leaf directory, preserving
	// behaviour for loose Swift projects.
	xp, xerr := parseXcodeGenProject(repoPath, projectManifestName)
	if xerr != nil {
		log.Printf("[swift-extractor] project.yml parse error: %v", xerr)
	}
	spmRoots := map[string]string{}
	for _, relFile := range manifestFiles {
		src, err := os.ReadFile(filepath.Join(repoPath, relFile))
		if err != nil {
			continue
		}
		for name, dir := range manifestTargetRoots(src, relFile) {
			spmRoots[name] = dir
		}
	}
	resolver := buildModuleResolver(xp, spmRoots)
	moduleForFile := func(relFile string) string {
		if id, ok := resolver.moduleFor(relFile); ok {
			return id
		}
		return factpath.Dir(relFile)
	}

	// The repo-wide default urlPrefixComponent (e.g. "v2") that endpoint types
	// inherit when they declare no prefix of their own. Only iOS uses the endpoint
	// idiom, so the scan is gated on isiOS to avoid a wasted read pass elsewhere.
	var defaultURLPrefix string
	if isiOS {
		defaultURLPrefix = detectDefaultURLPrefix(repoPath, files)
	}

	// extractFileAST/extractURLSessionFacts are pure; parse the source files in
	// parallel. The indices below are rebuilt by iterating the per-file results in
	// file order, so modules, dirToFile and typeIndex match a serial run exactly.
	// moduleForFile is pure and order-independent, so the identity a file resolves
	// to is the same in the parallel walk and the serial fold below.
	perFileFacts := parallel.MapFiles(ctx, swiftFiles, func(relFile string) []facts.Fact {
		src, err := os.ReadFile(filepath.Join(repoPath, relFile))
		if err != nil {
			log.Printf("[swift-extractor] error reading %s: %v", relFile, err)
			return nil
		}
		dir := moduleForFile(relFile)
		ff := extractFileASTWithDir(src, relFile, isiOS, dir)
		ff = append(ff, extractURLSessionFactsWithDir(src, relFile, dir)...)
		return append(ff, extractEndpointFacts(src, relFile, dir, defaultURLPrefix)...)
	})

	for i, fileFacts := range perFileFacts {
		relFile := swiftFiles[i]
		allFacts = append(allFacts, fileFacts...)

		dir := moduleForFile(relFile)
		modules[dir] = true
		if _, ok := dirToFile[dir]; !ok {
			dirToFile[dir] = relFile
		}

		// Index declared types so edge targets and cross-module references resolve.
		for _, fact := range fileFacts {
			if fact.Kind != facts.KindSymbol {
				continue
			}
			sk, _ := fact.PropAny("symbol_kind").(string)
			switch sk {
			case facts.SymbolStruct, facts.SymbolClass, facts.SymbolInterface:
				if simpleName := lastDotComponent(fact.Name); simpleName != "" {
					// A bare type name is not unique across modules: Swift namespaces
					// nested types by their enclosing type/module, so short names like
					// Event/State/Style/Coordinator recur in many targets. Track which
					// names are defined in >1 module so the cross-module reference pass
					// can refuse to fabricate an edge from an ambiguous name (which
					// would otherwise resolve to one arbitrary owning module and create
					// a false — often cyclic — dependency).
					if prev, ok := typeIndex[simpleName]; ok && prev != dir {
						typeAmbiguous[simpleName] = true
					}
					typeIndex[simpleName] = dir
				}
			case facts.SymbolMethod:
				// Index methods by short name so the resolveMethodCalls post-pass can
				// bind tentative member-call edges (self?.x(), coordinator?.y()) to a
				// concrete method — or drop them when no project method matches.
				if short := lastDotComponent(fact.Name); short != "" {
					methodIndex[short] = append(methodIndex[short], fact.Name)
				}
			case facts.SymbolFunc:
				short := lastDotComponent(fact.Name)
				if short == "" {
					break
				}
				// Operator overloads (func +, func <-, …) go in methodIndex so
				// custom-operator usage edges bind.
				if isOperatorToken(short) {
					methodIndex[short] = append(methodIndex[short], fact.Name)
				}
				// All top-level functions also go in funcIndex — the fallback used by
				// resolveMethodCalls when a member call's receiver type failed to parse
				// and its methods were flattened to top-level functions. Kept separate
				// from methodIndex so a genuine method call is only rescued by a
				// top-level function when no real method of that name exists.
				funcIndex[short] = append(funcIndex[short], fact.Name)
			}
		}
	}

	// Some endpoint idioms supply part of the request (verb, or the path itself) at
	// each instantiation site, so the per-file walk cannot resolve them. Index them
	// repo-wide and read the missing init arguments from every call site. iOS-only,
	// matching the endpoint idiom (and the defaultURLPrefix gate above).
	if isiOS {
		allFacts = append(allFacts,
			extractCallSiteEndpointFacts(repoPath, files, defaultURLPrefix, moduleForFile)...)
	}

	// Parse Package.swift manifests: emit SPM target module facts + the inter-target
	// dependency graph, and (by rerouting) keep the manifest's own `let package`
	// binding and `import PackageDescription` out of the symbol/dependency facts.
	manifestModules := make(map[string]bool)
	for _, relFile := range manifestFiles {
		absFile := filepath.Join(repoPath, relFile)
		src, err := os.ReadFile(absFile)
		if err != nil {
			log.Printf("[swift-extractor] error reading %s: %v", relFile, err)
			continue
		}
		mf := parsePackageManifest(src, relFile, dirToFile)
		allFacts = append(allFacts, mf...)
		for _, f := range mf {
			if f.Kind == facts.KindModule {
				manifestModules[f.Name] = true
			}
		}
	}

	// Canonicalise bare edge targets to "<dir>.<Type>" so reverse traversal
	// (impact_analysis) connects dependents to their targets.
	canonicalizeTargets(allFacts, typeIndex)

	// Bind the walker's tentative bare-short-name member-call edges to concrete
	// project methods (or drop stdlib/framework calls). Runs after
	// canonicalizeTargets so type-target rewrites are already settled.
	resolveMethodCalls(allFacts, methodIndex, funcIndex)

	// Resolve dangling inherited-method calls (a subclass calling a base-class or
	// protocol-extension method) to the declaring ancestor's method fact, so class /
	// protocol hierarchies are traversable. Runs before computePerformsIO so its
	// closure follows the newly-resolved inheritance edges.
	resolveInheritedCalls(allFacts)

	// Propagate the walk-time io_direct flag up the call graph into a transitive
	// performs_io prop, so callers of I/O methods are discoverable even when the
	// call chain runs through ambiguous (kept-bare) member-call edges.
	computePerformsIO(allFacts, methodIndex, funcIndex)

	// Emit XcodeGen target module facts + declared inter-target dependency edges.
	allFacts = append(allFacts, emitXcodeGenFacts(resolver, xp, spmRoots, dirToFile)...)

	// Emit module facts for leaf directories not already described by an SPM target
	// or a WHOLE XcodeGen target identity (files that fell back to leaf-directory
	// grouping, plus the per-directory packages of subdivided app targets). A
	// subdivided target's own root identity is deliberately NOT suppressed: files
	// sitting directly at the target root form a real per-directory package.
	for dir := range modules {
		if manifestModules[dir] || (resolver.identities[dir] && !resolver.subdivided[dir]) {
			continue
		}
		allFacts = append(allFacts, facts.Fact{
			Kind: facts.KindModule,
			Name: dir,
			File: dir,
			Props: map[string]any{
				"language":    "swift",
				"module_role": facts.ModuleRoleForPath(dir),
			},
		})
	}

	// Pass 2: resolve type references to discover cross-module dependencies.
	// Seed the seen-set with the declared (manifest/XcodeGen) edges so a used edge
	// that is also declared isn't emitted twice (which would double coupling
	// counts). Declared-dependency facts anchor File inside the source module dir,
	// so slashDir(File) recovers the source identity.
	type edge struct{ from, to string }
	seenEdges := make(map[edge]bool)
	for _, f := range allFacts {
		if f.Kind != facts.KindDependency {
			continue
		}
		for _, r := range f.Relations {
			if r.Kind == facts.RelImports {
				seenEdges[edge{slashDir(f.File), r.Target}] = true
			}
		}
	}

	for _, relFile := range swiftFiles {
		select {
		case <-ctx.Done():
			return allFacts, ctx.Err()
		default:
		}

		// When the file belongs to a resolved SPM/XcodeGen target, its module-level
		// dependencies are already captured completely and unambiguously by its
		// `import X` statements (resolveImports) plus the declared target graph
		// (emitXcodeGenFacts) — Swift requires an explicit import to use another
		// module's type, so those edges are a superset of every real cross-module
		// use. The type-reference inference below adds nothing correct there and,
		// because it resolves bare short names through a collision-prone index, is
		// the sole source of impossible back-edges (e.g. a Foundation-level target
		// "importing" a feature target) that SPM's acyclic-target guarantee forbids.
		// Skip it for WHOLE target-resolved files (framework/SPM/test); keep it for
		// loose Swift projects AND for the per-directory packages of a SUBDIVIDED app
		// target — within one Swift module nothing is imported, so type references are
		// the only source of the directory→directory coupling those sub-packages need.
		// (Intra-app cycles are legitimate, so the acyclic-graph concern above, which
		// motivated the skip across SPM targets, does not apply here; the typeAmbiguous
		// guard still drops collision-prone bare names.)
		if _, resolved := resolver.moduleFor(relFile); resolved && !resolver.subdividesFile(relFile) {
			continue
		}

		sourceModule := moduleForFile(relFile)
		absFile := filepath.Join(repoPath, relFile)
		refs := extractTypeReferences(absFile)

		for _, typeName := range refs {
			// An ambiguous short name (defined in >1 module) cannot be resolved to a
			// single owning module by name alone; emitting an edge to the arbitrary
			// index winner fabricates a false dependency. Skip it — the real edge, if
			// any, is still recovered from the file's import statements.
			if typeAmbiguous[typeName] {
				continue
			}
			targetModule, ok := typeIndex[typeName]
			if !ok || targetModule == sourceModule {
				continue
			}
			e := edge{sourceModule, targetModule}
			if seenEdges[e] {
				continue
			}
			seenEdges[e] = true

			allFacts = append(allFacts, facts.Fact{
				Kind: facts.KindDependency,
				Name: sourceModule + " -> " + targetModule,
				File: moduleAnchorFile(sourceModule, dirToFile),
				Props: map[string]any{
					"language": "swift",
					"internal": true,
				},
				Relations: []facts.Relation{
					{Kind: facts.RelImports, Target: targetModule},
				},
			})
		}
	}

	// Resolve bare `import X` targets to SPM/XcodeGen target module dirs and
	// classify stdlib/external, now that all module facts exist.
	resolveImports(allFacts)

	return allFacts, nil
}

// canonicalizeTargets rewrites bare simple-name targets of call-graph relations
// to their canonical "<dir>.<Type>" fact names using the type index. Targets that
// already contain "." (resolved methods/functions) or that name an unknown
// (external) type are left unchanged.
func canonicalizeTargets(allFacts []facts.Fact, typeIndex map[string]string) {
	for i := range allFacts {
		for j := range allFacts[i].Relations {
			r := &allFacts[i].Relations[j]
			switch r.Kind {
			case facts.RelInstantiates, facts.RelInjects, facts.RelCalls, facts.RelDependsOn:
				if strings.Contains(r.Target, ".") {
					continue
				}
				if dir, ok := typeIndex[r.Target]; ok {
					r.Target = dir + "." + r.Target
				}
			}
		}
	}
}

// resolveMethodCalls binds the walker's tentative bare-short-name RelCalls edges
// (member calls whose receiver type was unknown at walk time — self?.x() to a
// method in another extension, coordinator?.y(), delegate?.tap()) against the
// project-wide method index. A tentative edge is a RelCalls target with no "." and
// a non-capitalized head; every resolved edge the walker emits directly is either
// dir-qualified or a capitalized type, so this shape is unambiguous. Unique match
// -> rewrite to the qualified dir.Type.method name (a resolved graph edge, good for
// impact_analysis); ambiguous -> keep the bare name (still short-name matched by
// the dead-code detector, which is all that clears the false positive); no match
// -> drop the edge (a stdlib/framework call such as .map()/.dismissAnimated()).
//
// funcIndex (top-level function short name -> qualified names) is a fallback used
// only when methodIndex has no entry: when a type fails to parse (tree-sitter error
// recovery flattens its body to top level), its methods are emitted as top-level
// functions, so a member call on such a type has no method to bind to. Falling back
// to the function index recovers those edges. The precision cost — a member call
// whose short name coincides with an unrelated top-level free function resolves to
// it — is a false negative (a missed dead-code lead), the safe direction, and
// top-level free functions are rare in Swift.
func resolveMethodCalls(allFacts []facts.Fact, methodIndex, funcIndex map[string][]string) {
	for i := range allFacts {
		rels := allFacts[i].Relations
		if len(rels) == 0 {
			continue
		}
		out := rels[:0]
		for _, r := range rels {
			if r.Kind == facts.RelCalls && r.Target != "" &&
				!strings.Contains(r.Target, ".") && !isCapitalized(r.Target) {
				cands := methodIndex[r.Target]
				if len(cands) == 0 {
					cands = funcIndex[r.Target] // fallback: flattened/top-level function
				}
				switch len(cands) {
				case 0:
					continue // drop: no project method or function by this name
				case 1:
					r.Target = cands[0]
					// default (>1): keep the bare ambiguous name
				}
			}
			out = append(out, r)
		}
		allFacts[i].Relations = out
	}
}

// resolveInheritedCalls rewrites a caller's DANGLING call-graph edges — a phantom
// `dir.name` (a bare call to an inherited method, mis-shaped as a top-level function
// by resolveCall) or a resolver-kept ambiguous bare name — to the qualified method
// fact of the nearest ancestor (superclass or conformed protocol's extension) that
// declares it. This makes class / protocol hierarchies traversable: a subclass method
// calling an inherited base method now points at `dir.Base.method`.
//
// Conservative and additive: only targets that are NOT already a fact and whose short
// name IS declared by an ancestor of the caller's type are rewritten; every resolved
// edge (and thus dead-code short-name matching and coupling) is left untouched. The
// extractor does not distinguish a superclass from a protocol in `implements`, and it
// need not here — both put their methods in facts under `dir.Ancestor.method`, so the
// ancestry walk treats them uniformly. Nearest-first BFS matches Swift override order.
func resolveInheritedCalls(allFacts []facts.Fact) {
	factNames := make(map[string]bool, len(allFacts))
	typeSupers := make(map[string][]string)            // simple type -> supertype simple names
	methodByType := make(map[string]map[string]string) // simple type -> (method short -> qualified name)

	for i := range allFacts {
		f := &allFacts[i]
		if f.Kind != facts.KindSymbol {
			continue
		}
		factNames[f.Name] = true
		sk, _ := f.PropAny("symbol_kind").(string)
		switch sk {
		case facts.SymbolClass, facts.SymbolStruct, facts.SymbolInterface:
			simple := lastDotComponent(f.Name)
			for _, r := range f.Relations {
				if r.Kind == facts.RelImplements {
					typeSupers[simple] = append(typeSupers[simple], r.Target)
				}
			}
		case facts.SymbolMethod:
			recv, _ := f.PropAny("receiver").(string)
			if recv == "" {
				continue
			}
			t := lastDotComponent(recv)
			if methodByType[t] == nil {
				methodByType[t] = make(map[string]string)
			}
			// First declaration wins; overloads share a name and one target suffices.
			if _, ok := methodByType[t][lastDotComponent(f.Name)]; !ok {
				methodByType[t][lastDotComponent(f.Name)] = f.Name
			}
		}
	}

	// resolveInAncestry returns the qualified method fact for `short` declared by the
	// nearest ancestor of `callerType` (inclusive), or "" if none declares it.
	resolveInAncestry := func(callerType, short string) string {
		visited := map[string]bool{}
		queue := []string{callerType}
		for len(queue) > 0 {
			t := queue[0]
			queue = queue[1:]
			if visited[t] {
				continue
			}
			visited[t] = true
			if q, ok := methodByType[t][short]; ok {
				return q
			}
			queue = append(queue, typeSupers[t]...)
		}
		return ""
	}

	for i := range allFacts {
		f := &allFacts[i]
		if f.Kind != facts.KindSymbol {
			continue
		}
		if sk, _ := f.PropAny("symbol_kind").(string); sk != facts.SymbolMethod {
			continue
		}
		recv, _ := f.PropAny("receiver").(string)
		callerType := lastDotComponent(recv)
		if callerType == "" || len(typeSupers[callerType]) == 0 {
			continue // no supertypes → nothing to inherit; skip the common leaf case
		}
		for j := range f.Relations {
			r := &f.Relations[j]
			if r.Kind != facts.RelCalls || factNames[r.Target] {
				continue // not a call, or already resolved to a real fact
			}
			if q := resolveInAncestry(callerType, lastDotComponent(r.Target)); q != "" && q != f.Name {
				r.Target = q
			}
		}
	}
}

// ioFanoutCap bounds how many candidate methods an ambiguous (kept-bare) call
// target is expanded to during the performs_io closure. A small cap keeps the
// derived flag from over-propagating along very common method names (a bare `save`
// with dozens of candidates) while still crossing the narrow 2–3-way ambiguities
// that legitimately reach the network layer (e.g. `dataModel.updateMembershipRequest`).
const ioFanoutCap = 6

// computePerformsIO derives a transitive `performs_io` prop over the resolved call
// graph: a method performs I/O if its body directly invokes an I/O primitive
// (the walk-time `io_direct` flag) or it transitively calls a method that does.
//
// Ambiguous call targets — which resolveMethodCalls leaves as a bare short name
// because 2+ methods share it — are expanded through the methodIndex/funcIndex
// candidate sets DURING this closure only, so the closure can cross them without
// adding false edges to the shared call graph (dead-code / impact stay precise).
// A bounded fixpoint (correct under call cycles) replaces a memoized DFS to avoid
// the cycle-back-edge false negatives that a visiting-guard DFS would introduce.
func computePerformsIO(allFacts []facts.Fact, methodIndex, funcIndex map[string][]string) {
	// name -> indices of the method/func symbol facts with that name.
	byName := make(map[string][]int)
	for i := range allFacts {
		f := &allFacts[i]
		if f.Kind != facts.KindSymbol {
			continue
		}
		if sk, _ := f.PropAny("symbol_kind").(string); sk == facts.SymbolMethod || sk == facts.SymbolFunc {
			byName[f.Name] = append(byName[f.Name], i)
		}
	}

	// resolveTargets maps a call target to the candidate callee names to follow: the
	// target itself when it names a known method/func, else its (capped) methodIndex/
	// funcIndex candidates for the ambiguous bare case.
	resolveTargets := func(target string) []string {
		if _, ok := byName[target]; ok {
			return []string{target}
		}
		cands := methodIndex[target]
		if len(cands) == 0 {
			cands = funcIndex[target]
		}
		if len(cands) == 0 || len(cands) > ioFanoutCap {
			return nil
		}
		return cands
	}

	// Seed io[name] from the io_direct flag, and build name->callee-names adjacency.
	io := make(map[string]bool, len(byName))
	adj := make(map[string][]string, len(byName))
	for name, idxs := range byName {
		seen := make(map[string]bool)
		for _, i := range idxs {
			if b, _ := allFacts[i].PropAny("io_direct").(bool); b {
				io[name] = true
			}
			for _, r := range allFacts[i].Relations {
				if r.Kind != facts.RelCalls {
					continue
				}
				for _, c := range resolveTargets(r.Target) {
					if c != name && !seen[c] {
						seen[c] = true
						adj[name] = append(adj[name], c)
					}
				}
			}
		}
	}

	// Fixpoint: a method performs I/O if any callee does. Iterate to stability.
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

	// Emit the prop on every fact whose name reaches I/O (covers all overloads of a
	// name) and on any io_direct property/observer leaf not indexed above.
	for i := range allFacts {
		f := &allFacts[i]
		if f.Kind != facts.KindSymbol {
			continue
		}
		direct, _ := f.PropAny("io_direct").(bool)
		if io[f.Name] || direct {
			if f.Props == nil {
				f.Props = map[string]any{}
			}
			f.SetProp("performs_io", true)
		}
	}
}

// extractFile reads a Swift file and delegates to the tree-sitter walker. It
// preserves the legacy signature used by the test helper extractFromString.
func extractFile(f *os.File, relFile string, isiOS bool) []facts.Fact {
	src, err := io.ReadAll(f)
	if err != nil {
		return nil
	}
	return extractFileAST(src, relFile, isiOS)
}

// importRe matches a Swift import statement and captures the module name. Used by
// the AST walker to render import dependency facts.
var importRe = regexp.MustCompile(`^\s*import\s+(\w+)`)

// typeRefRe matches type annotations like "name: TypeName" in property declarations and parameters.
var typeRefRe = regexp.MustCompile(`:\s*([A-Z][A-Za-z0-9_]+)`)

// extractTypeReferences scans a Swift file for type references (property types, parameter types).
func extractTypeReferences(absFile string) []string {
	f, err := os.Open(absFile)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()

	seen := make(map[string]bool)
	var refs []string

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		// Skip comments and blank lines.
		if trimmed == "" || strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "/*") || strings.HasPrefix(trimmed, "*") {
			continue
		}

		matches := typeRefRe.FindAllStringSubmatch(line, -1)
		for _, m := range matches {
			typeName := m[1]
			// Skip common Swift/system types.
			if isSystemType(typeName) {
				continue
			}
			if !seen[typeName] {
				seen[typeName] = true
				refs = append(refs, typeName)
			}
		}
	}

	return refs
}

// isSystemType returns true for built-in Swift and framework types that should not be resolved.
func isSystemType(name string) bool {
	switch name {
	case "String", "Int", "Int8", "Int16", "Int32", "Int64",
		"UInt", "UInt8", "UInt16", "UInt32", "UInt64",
		"Float", "Double", "Bool", "Void", "Any", "AnyObject",
		"Data", "Date", "URL", "UUID", "Error",
		"Array", "Dictionary", "Set", "Optional",
		"Published", "State", "Binding", "ObservedObject", "StateObject", "EnvironmentObject", "Environment",
		"View", "App", "Scene", "Body",
		"Color", "Image", "Text", "Button", "NavigationView", "NavigationLink", "NavigationStack",
		"VStack", "HStack", "ZStack", "List", "ScrollView", "LazyVStack", "LazyHStack",
		"CGFloat", "CGPoint", "CGSize", "CGRect",
		"NSObject", "NSLock", "NSError",
		"URLRequest", "URLResponse", "HTTPURLResponse", "URLSession", "URLComponents", "URLQueryItem",
		"JSONDecoder", "JSONEncoder", "CodingKey", "CodingKeys",
		"AnyPublisher", "CurrentValueSubject", "PassthroughSubject", "AnyCancellable",
		"Task", "MainActor",
		"ObservableObject", "Identifiable", "Equatable", "Hashable", "Comparable",
		"Codable", "Decodable", "Encodable", "Sendable",
		"LocalizedError", "CustomStringConvertible",
		"Never":
		return true
	}
	return false
}

// lastDotComponent returns the part after the last "." in a name.
func lastDotComponent(name string) string {
	if idx := strings.LastIndex(name, "."); idx >= 0 {
		return name[idx+1:]
	}
	return name
}

// extractSupertypesFromText finds the supertype clause after ":" in text that may
// contain generic parameters. It skips content inside balanced parentheses and angle brackets.
//
// It is retained for direct unit testing of the supertype-clause parsing logic;
// the AST walker reads supertypes structurally from inheritance_specifier nodes.
func extractSupertypesFromText(text string) string {
	depth := 0
	for i, ch := range text {
		switch ch {
		case '(', '<':
			depth++
		case ')', '>':
			depth--
		case ':':
			if depth <= 0 {
				rest := text[i+1:]
				if braceIdx := strings.Index(rest, "{"); braceIdx >= 0 {
					rest = rest[:braceIdx]
				}
				// Stop at "where" clause.
				if whereIdx := strings.Index(rest, " where "); whereIdx >= 0 {
					rest = rest[:whereIdx]
				}
				return strings.TrimSpace(rest)
			}
		}
	}
	return ""
}

// --- iOS detection helpers ---

// detectiOSProject checks for Info.plist or other iOS markers.
func detectiOSProject(repoPath string) bool {
	// Walk up to two levels looking for Info.plist or Assets.xcassets
	entries, err := os.ReadDir(repoPath)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if entry.Name() == "Info.plist" {
			return true
		}
		if entry.IsDir() {
			subEntries, _ := os.ReadDir(filepath.Join(repoPath, entry.Name()))
			for _, sub := range subEntries {
				if sub.Name() == "Info.plist" || sub.Name() == "Assets.xcassets" {
					return true
				}
				if sub.IsDir() {
					deepEntries, _ := os.ReadDir(filepath.Join(repoPath, entry.Name(), sub.Name()))
					for _, deep := range deepEntries {
						if deep.Name() == "Info.plist" || deep.Name() == "Assets.xcassets" {
							return true
						}
					}
				}
			}
		}
	}
	return false
}

// addIOSProps classifies a declaration as an iOS component.
func addIOSProps(f *facts.Fact, name string, annotations []string, supertypes string) {
	// SwiftUI App entry point.
	if containsAnnotation(annotations, "main") && supertypeMatches(supertypes, "App") {
		f.SetProp("ios_component", "swiftui_app")
		f.SetProp("framework", "swiftui")
		return
	}

	// SwiftUI Views.
	if supertypeMatches(supertypes, "View") {
		f.SetProp("ios_component", "swiftui_view")
		f.SetProp("framework", "swiftui")
		return
	}

	// SwiftUI Scene.
	if supertypeMatches(supertypes, "Scene") {
		f.SetProp("ios_component", "swiftui_scene")
		f.SetProp("framework", "swiftui")
		return
	}

	// Combine ViewModels (ObservableObject conformance).
	if supertypeMatches(supertypes, "ObservableObject") {
		f.SetProp("ios_component", "viewmodel")
		f.SetProp("framework", "combine")
		return
	}

	// Swift 5.9+ Observable ViewModels.
	if containsAnnotation(annotations, "Observable") {
		f.SetProp("ios_component", "viewmodel")
		f.SetProp("framework", "observation")
		return
	}

	// UIKit ViewControllers.
	if supertypeMatches(supertypes, "UIViewController", "UITableViewController",
		"UICollectionViewController", "UINavigationController", "UITabBarController",
		"UIPageViewController") {
		f.SetProp("ios_component", "viewcontroller")
		f.SetProp("framework", "uikit")
		return
	}

	// UIKit Views.
	if supertypeMatches(supertypes, "UIView", "UITableViewCell", "UICollectionViewCell",
		"UIStackView", "UIScrollView") {
		f.SetProp("ios_component", "uiview")
		f.SetProp("framework", "uikit")
		return
	}

	// NSObject subclasses acting as delegates.
	if supertypeMatches(supertypes, "NSObject") {
		f.SetProp("framework", "foundation")
	}

	// Name-based architectural classification.
	if strings.HasSuffix(name, "ViewModel") {
		f.SetProp("ios_component", "viewmodel")
		return
	}
	if strings.HasSuffix(name, "Repository") || strings.HasSuffix(name, "RepositoryImpl") {
		f.SetProp("ios_component", "repository")
		return
	}
	if strings.HasSuffix(name, "UseCase") {
		f.SetProp("ios_component", "usecase")
		return
	}
	if strings.HasSuffix(name, "Coordinator") {
		f.SetProp("ios_component", "coordinator")
		return
	}
	if strings.HasSuffix(name, "APIService") || (strings.HasSuffix(name, "Service") && !strings.HasSuffix(name, "ServiceInterface")) {
		f.SetProp("ios_component", "service")
		return
	}
	if name == "DIContainer" || strings.HasSuffix(name, "Container") {
		f.SetProp("ios_component", "di_container")
		return
	}
}

// --- Parsing helpers ---

// parseSupertypes splits a supertype clause like "Foo, Bar, Baz<T>" into type names.
func parseSupertypes(clause string) []string {
	var result []string
	depth := 0
	start := 0
	for i, ch := range clause {
		switch ch {
		case '<', '(':
			depth++
		case '>', ')':
			depth--
		case ',':
			if depth == 0 {
				if t := extractTypeName(clause[start:i]); t != "" {
					result = append(result, t)
				}
				start = i + 1
			}
		}
	}
	if t := extractTypeName(clause[start:]); t != "" {
		result = append(result, t)
	}
	return result
}

// extractTypeName extracts the simple type name from a supertype entry like "Foo" or "Bar<T>".
func extractTypeName(s string) string {
	s = strings.TrimSpace(s)
	for i, ch := range s {
		if ch == '<' || ch == '(' || ch == ' ' {
			s = s[:i]
			break
		}
	}
	s = strings.TrimSpace(s)
	if idx := strings.LastIndex(s, "."); idx >= 0 {
		s = s[idx+1:]
	}
	if s == "" {
		return ""
	}
	return s
}

func containsAnnotation(annotations []string, name string) bool {
	for _, a := range annotations {
		if a == name {
			return true
		}
	}
	return false
}

func supertypeMatches(supertypes string, names ...string) bool {
	if supertypes == "" {
		return false
	}
	parsed := parseSupertypes(supertypes)
	for _, st := range parsed {
		for _, name := range names {
			if st == name {
				return true
			}
		}
	}
	return false
}

// privateRe / privateSetRe support isPrivateAccess.
var (
	privateRe    = regexp.MustCompile(`\b(private|fileprivate)\b`)
	privateSetRe = regexp.MustCompile(`\b(private|fileprivate)\s*\(set\)`)
)

// isPrivateAccess returns true if the text contains private or fileprivate access control
// that is NOT the private(set) pattern (which only restricts the setter, keeping the getter public).
func isPrivateAccess(text string) bool {
	if !privateRe.MatchString(text) {
		return false
	}
	// Remove all private(set) / fileprivate(set) occurrences and re-check.
	cleaned := privateSetRe.ReplaceAllString(text, "")
	return privateRe.MatchString(cleaned)
}

func isSwiftFile(path string) bool {
	return strings.HasSuffix(strings.ToLower(path), ".swift")
}

// OwnsFile implements plugin.FileOwner for incremental caching.
func (e *SwiftExtractor) OwnsFile(relFile string) bool { return isSwiftFile(relFile) }

func matchesXcodeProject(name string) bool {
	return strings.HasSuffix(name, ".xcodeproj") || strings.HasSuffix(name, ".xcworkspace")
}
