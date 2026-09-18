// Package common holds helpers shared by multiple explainers: module-level
// dependency-graph construction and statistical-outlier detection. Extracting
// these here keeps the per-explainer packages small and avoids the
// copy-pasted graph/path logic that previously lived in cycles and layers.
package common

import (
	"math"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// OversizedClusterModules is the module-count above which a strongly-connected
// component is treated as a coupling *cluster* rather than a discrete, actionable
// tangle. In an autoloaded language (Ruby/Rails) mutual constant references across
// many directories are the expected topology, so a large SCC is a coupling-density
// signal, not a fixable cycle or a genuine deep layering. Shared by the cycles
// explainer (softens such SCCs into a cluster note) and the depth explainer (counts
// such an SCC as one logical layer instead of its full size).
const OversizedClusterModules = 8

// MaxHeuristicConfidence is the ceiling for any finding that is inferred rather than
// proven, and it exists to keep one published promise true: confidence `1.0` means a
// structural fact, anything below it is a heuristic candidate. That promise is what
// `snapshot_receipt`'s heuristic-insight count, the dashboard's structural/candidate
// split and `query_insights(min_confidence=…)` all rest on.
//
// Two explainers compute a confidence that can otherwise saturate — god-class, whose
// score is a fan-in ratio against a statistical threshold, and layers, whose pattern
// confidence is a coverage share. Both are strong signals and neither is a proof, so
// both clamp here instead of at 1.0. A cycle is the only claim enola computes with
// certainty, and it is the only thing that may reach 1.0.
const MaxHeuristicConfidence = 0.95

// FileDir returns the directory portion of a file path, which enola uses as the
// canonical module name. A path with no separator maps to ".".
func FileDir(file string) string {
	parts := strings.Split(file, "/")
	if len(parts) <= 1 {
		return "."
	}
	return strings.Join(parts[:len(parts)-1], "/")
}

// ModuleDir returns the directory of a fact's file in MODULE-NAME space: FileDir with
// the repo label stripped first.
//
// The two spaces diverge in a multi-repo (append-mode) snapshot and only there. A
// fact's File is repo-prefixed ("server/index.js") while module facts keep their bare
// name (".", "routes", "src"), so deriving a module from a raw File yields "server" —
// a name no module has, in any repo. Anything resolving a file back to its module has
// to cross that gap explicitly.
//
// Repo is populated in BOTH modes; only the File prefix is append-mode-specific.
//
// The strip is NOT unconditionally safe, which is why callers that know the module
// namespace should prefer ModuleDirCandidates. It assumes the repo label cannot also
// be a real leading path segment — false for the dominant Python layout, where the
// package inside the repo carries the repo's own name (cognee/cognee,
// superset/superset). There "cognee/pkg/x.py" strips to "pkg", which names no module,
// so every edge out of the tree hangs off a phantom node.
func ModuleDir(f facts.Fact) string {
	file := f.File
	if f.Repo != "" {
		file = strings.TrimPrefix(file, f.Repo+"/")
	}
	return FileDir(file)
}

// ModuleDirCandidates returns the directories a fact's file may name in MODULE-NAME
// space. Both are legitimate; which one is correct depends on the snapshot's shape,
// and that cannot be decided from the fact alone:
//
//   - the raw repo-relative directory — correct in a single-repo snapshot, including
//     the case where the first path segment genuinely equals the repo name;
//   - the same directory with the repo label stripped — correct in an append-mode
//     (multi-repo) snapshot, where File is repo-prefixed and module names are not.
//
// Callers resolve against the module namespace they hold rather than guessing. A
// caller that used ModuleDir alone silently lost every edge in a repo whose top-level
// source directory shares the repo's name.
func ModuleDirCandidates(f facts.Fact) []string {
	raw := FileDir(f.File)
	stripped := ModuleDir(f)
	if raw == stripped {
		return []string{raw}
	}
	return []string{raw, stripped}
}

// resolveModuleDir picks the module a fact's file belongs to, trying every candidate
// directory against the known module names.
//
// EXACT matches on all candidates are tried before walking any of them up. The order
// is load-bearing: walking a candidate up can reach a short ancestor that happens to
// exist in the other snapshot shape (the bare repo label "consumer" in an append-mode
// graph), so letting a walk-up beat an exact match would let the two shapes
// cross-match and attribute an edge to the wrong module.
func resolveModuleDir(f facts.Fact, prod, test map[string]bool) string {
	candidates := ModuleDirCandidates(f)
	for _, c := range candidates {
		if prod[c] || test[c] {
			return c
		}
	}
	for _, c := range candidates {
		if m := nearestModuleOrEmpty(c, prod, test); m != "" {
			return m
		}
	}
	// Unchanged fallback: the repo-stripped directory, so a fact that resolves to no
	// module at all behaves exactly as it did before.
	return candidates[len(candidates)-1]
}

// ModuleNameSets splits a store's module facts into the production and test name sets
// that ResolveModule and nearestModule resolve against.
//
// Exported because the classification is not something a caller can redo safely: it
// runs through IsTestModule, which consults the extractor's authoritative
// `module_role` prop before falling back to the path. A caller that filtered test
// modules by path alone would disagree with every explainer in this package.
func ModuleNameSets(store *facts.Store) (prod, test map[string]bool) {
	prod, test = map[string]bool{}, map[string]bool{}
	for _, m := range store.ByKind(facts.KindModule) {
		if IsTestModule(m) {
			test[m.Name] = true
			continue
		}
		prod[m.Name] = true
	}
	return prod, test
}

// ResolveModule reports which module a fact's file belongs to, resolving against the
// module namespace the caller holds.
//
// This is the only correct file-to-module mapping in the codebase: it tries both the
// raw and the repo-stripped directory (see ModuleDirCandidates for why neither alone
// is safe) and then walks up to the nearest enclosing module for nested layouts.
// Anything outside this package that needs to answer "which module is this file in"
// must come through here rather than reaching for FileDir.
func ResolveModule(f facts.Fact, prod, test map[string]bool) string {
	return resolveModuleDir(f, prod, test)
}

// nearestModuleOrEmpty walks dir up its path until it reaches a known production or
// test module, returning "" when nothing encloses it — the difference from
// nearestModule, which reports the input unchanged and so cannot express a miss.
func nearestModuleOrEmpty(dir string, prod, test map[string]bool) string {
	cur := dir
	for {
		if prod[cur] || test[cur] {
			return cur
		}
		i := strings.LastIndex(cur, "/")
		if i < 0 {
			return ""
		}
		cur = cur[:i]
	}
}

// nearestModule walks dir up its path until it reaches a known production or test
// module, and returns that module name. If nothing encloses dir it is returned
// unchanged — so the result is identical to a raw FileDir wherever the file's
// directory is itself the module (Go packages, Rails/TS directory-modules), and
// only differs for nested layouts (Swift/Xcode targets) where the module lives
// above the file's leaf directory. Mirrors package_metrics' resolveToModule.
func nearestModule(dir string, prod, test map[string]bool) string {
	if m := nearestModuleOrEmpty(dir, prod, test); m != "" {
		return m
	}
	return dir
}

// ResolveRelativeImport resolves a "./x" or "../x" import target against the
// source module's path, yielding an absolute (repo-relative) module path.
func ResolveRelativeImport(sourceModule, target string) string {
	if !strings.HasPrefix(target, ".") {
		return target
	}

	parts := strings.Split(sourceModule, "/")
	targetParts := strings.Split(target, "/")

	for _, tp := range targetParts {
		switch tp {
		case ".":
			continue
		case "..":
			if len(parts) > 0 {
				parts = parts[:len(parts)-1]
			}
		default:
			parts = append(parts, tp)
		}
	}

	return strings.Join(parts, "/")
}

// BuildModuleGraph extracts the module-level import adjacency list from the
// store: module name -> list of internal modules it imports. External imports
// are dropped and relative imports are normalized. Every declared module is
// present as a key (with a possibly-empty edge list).
//
// Test-role modules (test bundles / spec trees, tagged module_role=test) are
// excluded as both nodes and edge endpoints: they are not part of the production
// architecture, and a test target normally imports the very module it exercises,
// which would otherwise drag test bundles into cycle, layer-violation, and
// depth findings (the classic "the cycle chain mixes Tests/ and Sources/"
// artifact). This mirrors package-metrics, which already filters non-production
// roles. Modules with an absent or non-test role are kept (consumers treat an
// absent role as included).
func BuildModuleGraph(store *facts.Store) map[string][]string {
	return BuildModuleGraphExcluding(store)
}

// IsTestModule reports whether a module is test scaffolding rather than production
// architecture, and must therefore be kept out of the coupling graph the cycles and
// dependency-depth explainers read, and out of the module population the layers
// explainer measures architecture-pattern coverage against.
//
// Callers holding a module FACT should use this rather than facts.IsTestPath on the
// name, because only this consults `module_role` — see below.
//
// The extractor's `module_role` prop is AUTHORITATIVE where it exists: it comes from
// a build file, not a guess — an SPM/Xcode target type, a Gradle source set. A module
// the extractor tagged `production` stays in the graph even if its path looks like a
// test tree, because the build system is right and the path is not. A large Android app
// ships a real package called `app/src/main/java/…/ui/base/testing`; Gradle says
// `src/main`, so it is production, and no path heuristic gets to overrule that.
//
// But only java, kotlin, ruby and swift emit the prop at all. Go, Python, TypeScript,
// PHP and C/C++ emit NONE — and the exclusion used to key on the prop alone, so their
// test trees walked straight into the graph. On python/superset that meant ALL TEN
// dependency-depth findings were test modules (`tests/unit_tests/dao (depth 11)`, …)
// and 2 of the 10 cycles ran through them: an explainer whose entire output was test
// scaffolding. Where the prop is absent, fall back to the path.
func IsTestModule(m facts.Fact) bool {
	switch role, _ := m.Props[facts.PropModuleRole].(string); role {
	case facts.ModuleRoleTest:
		return true
	case facts.ModuleRoleProduction, facts.ModuleRoleTooling:
		return false // authoritative: the build file said so
	}
	// No usable signal (absent, "", or "unknown") — the five untagged languages.
	return facts.IsTestPath(m.Name)
}

// BuildModuleGraphExcluding is BuildModuleGraph with the ability to drop synthetic
// coupling edges by their Props["coupling_kind"] (see facts.Coupling* constants).
// A dependency fact whose coupling_kind is in excludeKinds contributes no edge.
// The cycles explainer uses this to exclude ActiveRecord associations, whose
// inherent bidirectionality would otherwise manufacture false cycles. With no
// excludeKinds it is identical to BuildModuleGraph.
func BuildModuleGraphExcluding(store *facts.Store, excludeKinds ...string) map[string][]string {
	graph := make(map[string][]string)

	var excluded map[string]bool
	if len(excludeKinds) > 0 {
		excluded = make(map[string]bool, len(excludeKinds))
		for _, k := range excludeKinds {
			excluded[k] = true
		}
	}

	moduleNames, testModules := ModuleNameSets(store)
	for name := range moduleNames {
		if _, ok := graph[name]; !ok {
			graph[name] = nil
		}
	}

	deps := store.ByKind(facts.KindDependency)
	for _, dep := range deps {
		// Attribute the edge to the file's nearest ENCLOSING module, not its raw
		// leaf directory. Where files sit directly in the module directory (Go
		// packages, Rails/TS directory-modules) these coincide; but in a nested
		// layout — a Swift/Xcode target `Sources/Foo` with files under
		// `Sources/Foo/Bar/…` — the leaf dir `Sources/Foo/Bar` is not a module, so a
		// raw-FileDir source node never matched a module-name target and no cycle or
		// edge could form (which silently zeroed cycle detection on such projects).
		// Resolving up keeps this graph consistent with package_metrics.
		//
		// Resolved against BOTH the raw and repo-stripped directories, because which
		// one is right depends on the snapshot's shape. In an append-mode snapshot the
		// file is repo-prefixed ("consumer/src/client.ts") while module facts keep
		// their bare name ("src"), so the raw dir resolves to nothing. But stripping
		// unconditionally breaks the opposite case — a single-repo Python layout whose
		// package carries the repo's name ("cognee/benchcyca" -> "benchcyca") — where
		// the stripped dir is the one that names no module. Either way the edge used
		// to hang off a phantom node nothing else referenced, silently zeroing cycle
		// and depth detection across the whole tree. See resolveModuleDir.
		sourceModule := resolveModuleDir(dep, moduleNames, testModules)
		if testModules[sourceModule] {
			continue // edge out of a test bundle — not production architecture
		}
		if excluded != nil {
			if ck, _ := dep.Props[facts.PropCouplingKind].(string); excluded[ck] {
				continue
			}
		}

		for _, rel := range dep.Relations {
			if rel.Kind != facts.RelImports {
				continue
			}
			target := rel.Target

			if strings.HasPrefix(target, ".") {
				target = ResolveRelativeImport(sourceModule, target)
			}

			// Resolve the target up to its enclosing module too. Extractors emit
			// import targets at differing granularity — a bare module dir (Go/Rails),
			// a module dir + symbol name (Kotlin/Java `import a.b.C` -> "a/b/C"), or a
			// file stem (TypeScript) — so an exact module-name match drops every
			// class-/file-suffixed target and silently loses those edges (e.g. all
			// Kotlin-sourced cycles). Walking up mirrors package_metrics and the source
			// resolution above; it is a no-op where the target is already a module.
			target = nearestModule(target, moduleNames, testModules)

			// A target is included only if it names a known internal PRODUCTION module.
			// This gate is authoritative; we deliberately do NOT pre-filter with
			// IsExternalImport, which misclassifies single-segment internal modules
			// (top-level "cmd", "config") as external and would drop real edges here.
			if moduleNames[target] {
				graph[sourceModule] = append(graph[sourceModule], target)
			}
		}
	}

	return graph
}

// rubyFrameworkBaseClasses are exact Rails/framework base-class names whose high
// fan-in comes from being inherited, not from being a god class.
var rubyFrameworkBaseClasses = map[string]bool{
	"ApplicationRecord": true, "ApplicationController": true, "ApplicationJob": true,
	"ApplicationMailer": true, "ApplicationService": true, "ApplicationSerializer": true,
	"ApplicationCable": true, "ApplicationPolicy": true, "ApplicationInteractor": true,
	"ApplicationPresenter": true,
}

// rubyBaseClassSuffixes are naming conventions for user-defined base classes
// (MessengerBase, ApiBaseController, Widget::Base, ...). A class named like this is
// a base others inherit from, so its inbound degree is inheritance, not coupling.
var rubyBaseClassSuffixes = []string{
	"BaseController", "BaseJob", "BaseService", "BaseMailer", "BaseSerializer",
	"BasePolicy", "BasePresenter", "BaseInteractor", "Base",
}

// IsRubyFrameworkBaseSymbol reports whether a symbol is a Rails/framework base
// class — one whose high fan-in is a product of inheritance (every subclass
// "depends on" it) rather than a design smell. Gated on a .rb file so non-Ruby
// symbols are never affected. Used by the god-class and hotspots explainers to keep
// framework scaffolding (ApplicationRecord/Controller/Job, *BaseController, *::Base)
// out of their findings while still surfacing genuine central domain types.
func IsRubyFrameworkBaseSymbol(name, file string) bool {
	if !strings.HasSuffix(file, ".rb") {
		return false
	}
	seg := name
	if i := strings.LastIndex(seg, "::"); i >= 0 {
		seg = seg[i+2:]
	}
	if rubyFrameworkBaseClasses[seg] {
		return true
	}
	for _, suffix := range rubyBaseClassSuffixes {
		if strings.HasSuffix(seg, suffix) {
			return true
		}
	}
	return false
}

// SymbolModule returns the module a symbol belongs to, guessed from the name
// alone: the prefix before the first ".", e.g. "internal/auth.Login.Verify" ->
// "internal/auth". Names without a "." are returned unchanged.
//
// This is a GUESS, and it is only correct when a directory cannot contain a "."
// — true of Go, Ruby and TypeScript, false of .NET. Prefer SymbolModuleIn, which
// resolves against the modules that actually exist; this remains as its fallback
// for a symbol no module declares.
func SymbolModule(name string) string {
	if i := strings.Index(name, "."); i >= 0 {
		return name[:i]
	}
	return name
}

// SymbolModuleIn resolves a symbol to the module fact that declares it, by longest
// matching name prefix, falling back to SymbolModule when none matches.
//
// The name alone cannot settle this for every language. A fact is named
// "<module>.<declaration>", and .NET puts dots in both halves:
// "Jellyfin.Api.BaseJellyfinApiController" is module "Jellyfin.Api" + type
// "BaseJellyfinApiController", but reads identically to module "Jellyfin" + type
// "Api.BaseJellyfinApiController". Guessing split every jellyfin project at its
// first dot and attributed 9,106 symbols to a module called "MediaBrowser" that no
// module fact declares — eleven independent projects collapsed into one phantom,
// which exported-surface then reported on.
//
// Matching against the real module set removes the guess. It is also the only
// approach that stays correct for Swift, whose facts are named by their SPM/Xcode
// TARGET rather than by their file's directory — so deriving the module from the
// file path would be wrong there in a different way.
//
// Scans from the end, so the first hit is the longest prefix: a symbol in a nested
// module resolves to that module rather than to its parent.
func SymbolModuleIn(name string, modules map[string]bool) string {
	for i := len(name) - 1; i > 0; i-- {
		if name[i] == '.' && modules[name[:i]] {
			return name[:i]
		}
	}
	return SymbolModule(name)
}

// MeanStdDev returns the arithmetic mean and population standard deviation of
// the given values. Both are 0 for an empty slice.
//
// The result depends only on the MULTISET of values, never on the order the caller
// supplied them in. That is a guarantee, not an accident: floating-point addition is
// not associative, so a sequential reduction over the same numbers in a different
// order differs in the last bits. Callers build these slices by iterating the fact
// store, whose order reflects concurrent extraction and varies between runs — which
// made insights.json non-reproducible across runs of an unchanged tree, on the one
// explainer (god-class) that puts a threshold into its output rather than only using
// it to select. facts.jsonl hid this because it is sorted before it is written.
//
// Sorting here rather than at each call site fixes every caller at once, including
// the two where the same wobble is currently latent: hotspots and
// complexity-outliers emit a constant confidence, so their thresholds affect only
// WHICH items are selected — robust to 1e-15 until a value sits exactly on the
// cutoff. The codebase already holds this discipline elsewhere; see
// StronglyConnectedComponents, which documents the same determinism requirement for
// map iteration.
func MeanStdDev(values []float64) (mean, std float64) {
	n := len(values)
	if n == 0 {
		return 0, 0
	}

	// Copied before sorting: callers pass slices they keep using, and reordering
	// one under them would be a silent action at a distance.
	v := append([]float64(nil), values...)
	sort.Float64s(v)

	var sum float64
	for _, x := range v {
		sum += x
	}
	mean = sum / float64(n)

	// The second pass must read the sorted copy too. Sorting only the mean's
	// reduction would leave the variance order-dependent, and the threshold with it.
	var variance float64
	for _, x := range v {
		d := x - mean
		variance += d * d
	}
	variance /= float64(n)
	return mean, math.Sqrt(variance)
}

// OutlierThreshold returns mean + k*stddev for the given values — the cutoff
// above which a value is treated as a high outlier. Returns 0 for empty input.
func OutlierThreshold(values []float64, k float64) float64 {
	mean, std := MeanStdDev(values)
	return mean + k*std
}

// StronglyConnectedComponents returns the strongly-connected components of a
// directed graph (adjacency list: node -> successors) using Tarjan's algorithm.
//
// The result is deterministic regardless of Go's randomized map iteration: nodes
// are visited in sorted order with sorted neighbor lists, each component's members
// are sorted, and the component list is ordered by each component's smallest
// member. Every key of graph appears in exactly one component (a node that appears
// only as a neighbor is included as its own singleton). Shared by the cycles and
// dependency-depth explainers, which both need a cycle-safe view of the module
// graph — factoring it here keeps a single, tested implementation.
func StronglyConnectedComponents(graph map[string][]string) [][]string {
	nodes := make([]string, 0, len(graph))
	for v := range graph {
		nodes = append(nodes, v)
	}
	sort.Strings(nodes)

	var (
		index    int
		stack    []string
		onStack  = make(map[string]bool)
		indices  = make(map[string]int)
		lowlinks = make(map[string]int)
		sccs     [][]string
	)

	var strongConnect func(v string)
	strongConnect = func(v string) {
		indices[v] = index
		lowlinks[v] = index
		index++
		stack = append(stack, v)
		onStack[v] = true

		neighbors := append([]string(nil), graph[v]...)
		sort.Strings(neighbors)
		for _, w := range neighbors {
			if _, visited := indices[w]; !visited {
				strongConnect(w)
				if lowlinks[w] < lowlinks[v] {
					lowlinks[v] = lowlinks[w]
				}
			} else if onStack[w] {
				if indices[w] < lowlinks[v] {
					lowlinks[v] = indices[w]
				}
			}
		}

		if lowlinks[v] == indices[v] {
			var scc []string
			for {
				w := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[w] = false
				scc = append(scc, w)
				if w == v {
					break
				}
			}
			sort.Strings(scc)
			sccs = append(sccs, scc)
		}
	}

	for _, v := range nodes {
		if _, visited := indices[v]; !visited {
			strongConnect(v)
		}
	}

	sort.Slice(sccs, func(i, j int) bool { return sccs[i][0] < sccs[j][0] })
	return sccs
}
