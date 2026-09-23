// Package metrics implements the "package_metrics" MCP tool, which
// computes the classic Robert C. Martin / JDepend software package metrics from
// an Enola architectural snapshot.
//
// Engine-boundary note: this package knows nothing about the engine. Register
// takes an accessor that yields the store the latest snapshot published, because
// a tool handler runs per call and must not be pinned to the store that existed
// when it was registered; ExplainSection takes a store outright, because it runs
// once, straight after a snapshot. Keeping the engine out is what lets this code
// live either side of the module boundary.
package metrics

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/pkg/mcputil"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Prop keys are the extractors' own string keys, which internal/facts does not
// export as constants. Fact-kind, relation, and symbol-kind values come from it
// (facts.Kind*, facts.Rel*, facts.Symbol*).
const (
	propSymbolKind = "symbol_kind"
	propAbstract   = "abstract"
	propModuleRole = "module_role"
	propLanguage   = "language"   // extractor language tag, e.g. "typescript"
	propEnum       = "enum"       // Kotlin enum class (symbol_kind stays "class")
	propDataClass  = "data_class" // Kotlin data class
	propRecord     = "record"     // Java/C# record
	// propDataHolder is the STRUCTURAL form of the same signal, for languages with
	// no data-class construct in common use: the extractor reports a type that
	// declares state and no behaviour. C# needs it — its DTOs are plain classes
	// with auto-properties.
	propDataHolder  = "data_holder"
	propDIComponent = "di_component" // Dagger/Hilt @Component/@Subcomponent
	propDIModule    = "di_module"    // Dagger/Hilt @Module

	// langTypeScript tags TS/JS symbols. TS interfaces are structural data
	// shapes, not implemented abstractions, so they are excluded from N.
	langTypeScript = "typescript"
)

// nonProductionRoles are the module_role values excluded from package metrics.
// Martin's I/A/D metrics describe the production architecture; test bundles and
// build-tooling modules are structurally unstable (they depend on production,
// nothing depends on them) and would skew the aggregates. Modules with no
// module_role prop, or role "production"/"unknown", are always included, so every
// language and every un-tagged module behaves exactly as before.
var nonProductionRoles = map[string]bool{
	"test":    true,
	"tooling": true,
}

// pkgInput is the per-package input to the pure metric computation.
type pkgInput struct {
	Name        string
	Repo        string
	Classes     int // concrete types: struct + class
	Interfaces  int // abstract types: interfaces + abstract classes (incl. Python ABC/Protocol)
	DataHolders int // of the counted types, how many are data holders (data class / record / enum)
}

// importEdge is a directed internal package→package dependency, resolved to
// module names.
type importEdge struct {
	From string
	To   string
}

// PackageMetric is the per-package result. Ca/Ce are package-granular: they count
// other packages, derived from the reliable internal import graph (true class→class
// edges are not present in the snapshot for most languages).
type PackageMetric struct {
	Package           string  `json:"package"`
	Repo              string  `json:"repo,omitempty"`
	ClassesInterfaces int     `json:"classes_interfaces"` // N: concrete + abstract types + interfaces
	Ca                int     `json:"afferent_couplings"` // packages that depend upon this package
	Ce                int     `json:"efferent_couplings"` // packages this package depends upon
	Instability       float64 `json:"instability"`        // I = Ce / (Ca + Ce)
	Abstractness      float64 `json:"abstractness"`       // A = interfaces / N
	Distance          float64 `json:"distance"`           // D = |A + I - 1|
	DataHolderRatio   float64 `json:"data_holder_ratio"`  // fraction of N that are data holders (data class/record/enum)

	// RigidCaFloor is how depended-upon a package must be, IN THIS SNAPSHOT, for a
	// rigid off-main-sequence finding to be worth reporting. It is a property of the
	// population rather than of the package, stamped on every metric by compute so
	// that isOffMainSequence and Classify stay pure predicates over one value. Not
	// serialized: it is an internal threshold, not a measurement of this package.
	RigidCaFloor int `json:"-"`
}

// compute is the pure metric core: given per-package type counts and the set of
// internal import edges, it returns one PackageMetric per package. It dedups
// edges, ignores self-edges, computes afferent/efferent fan-in/out, and guards
// against divide-by-zero (Ca+Ce==0 ⇒ I=0; N==0 ⇒ A=0).
func compute(pkgs []pkgInput, edges []importEdge) []PackageMetric {
	// Distinct out-neighbours (efferent) and in-neighbours (afferent) per package.
	ce := make(map[string]map[string]struct{}, len(pkgs))
	ca := make(map[string]map[string]struct{}, len(pkgs))
	for _, e := range edges {
		if e.From == "" || e.To == "" || e.From == e.To {
			continue
		}
		if ce[e.From] == nil {
			ce[e.From] = make(map[string]struct{})
		}
		ce[e.From][e.To] = struct{}{}
		if ca[e.To] == nil {
			ca[e.To] = make(map[string]struct{})
		}
		ca[e.To][e.From] = struct{}{}
	}

	out := make([]PackageMetric, 0, len(pkgs))
	for _, p := range pkgs {
		n := p.Classes + p.Interfaces
		caN := len(ca[p.Name])
		ceN := len(ce[p.Name])

		var instability float64
		if caN+ceN > 0 {
			instability = float64(ceN) / float64(caN+ceN)
		}
		var abstractness float64
		if n > 0 {
			abstractness = float64(p.Interfaces) / float64(n)
		}
		distance := math.Abs(abstractness + instability - 1)
		var dataHolderRatio float64
		if n > 0 {
			dataHolderRatio = float64(p.DataHolders) / float64(n)
		}

		out = append(out, PackageMetric{
			Package:           p.Name,
			Repo:              p.Repo,
			ClassesInterfaces: n,
			Ca:                caN,
			Ce:                ceN,
			Instability:       round3(instability),
			Abstractness:      round3(abstractness),
			Distance:          round3(distance),
			DataHolderRatio:   round3(dataHolderRatio),
		})
	}
	// The rigid gate is relative to this population, so it can only be known once
	// every package is counted. Stamping it here keeps isOffMainSequence and Classify
	// pure predicates that any caller holding a metric can apply.
	floor := rigidCaFloor(out)
	for i := range out {
		out[i].RigidCaFloor = floor
	}
	return out
}

func round3(f float64) float64 {
	return math.Round(f*1000) / 1000
}

// collect reads the fact store via the public bootstrap API and produces the
// pure-computation inputs.
// Compute returns one PackageMetric per production package in the store, using
// the same collect+compute core as the package_metrics tool (unfiltered — the
// caller narrows). It is exported for the dashboard's Package Metrics panel, which
// scores the live store rather than going through the tool.
func Compute(store *facts.Store) []PackageMetric {
	pkgs, edges, _ := collect(store)
	return compute(pkgs, edges)
}

func collect(store *facts.Store) ([]pkgInput, []importEdge, []string) {
	// A dashboard renders before the first snapshot is generated, and the whole page
	// is written to degrade to a note rather than fail. The sibling readers on that
	// page (coverageDetails, graphDetails) all guard the same way.
	if store == nil {
		return nil, nil, nil
	}
	// 1. Seed packages from module facts and remember their names + short names.
	type pkgAcc struct {
		repo        string
		classes     int
		interfaces  int
		dataHolders int
	}
	acc := make(map[string]*pkgAcc)
	order := make([]string, 0)
	moduleSet := make(map[string]struct{})
	// excluded holds module names tagged as non-production (test/tooling). They are
	// kept OUT of moduleSet so their symbols aren't counted and — because
	// resolveToModule only resolves to moduleSet — import edges touching them (and
	// their children) are dropped, so production Ca isn't inflated by test imports.
	excluded := make(map[string]struct{})
	// excludedNames keeps the NAMES, not just a count, so the summary can report the
	// exclusions that match the caller's filter instead of the repo-wide total.
	excludedNames := make([]string, 0)
	for _, f := range store.ByKind(facts.KindModule) {
		if mcputil.IsGeneratedPath(f.Name) {
			continue
		}
		role, _ := f.PropAny(propModuleRole).(string)
		if nonProductionRoles[role] {
			excluded[f.Name] = struct{}{}
			excludedNames = append(excludedNames, f.Name)
			continue
		}
		// Extractors that don't tag module_role (Python, Go, TypeScript) leave role
		// "" / "unknown". Without this fallback their test/tooling dirs enter the
		// production population — Python is the worst case, emitting a module fact for
		// EVERY directory, so the on-the-fly path check on the symbol branch (which is
		// gated on the package having no module fact) never fires for them. Apply the
		// same path heuristic here so every language classifies test/tooling dirs
		// consistently. Excluding the module keeps it out of moduleSet, which also
		// drops its import edges (resolveToModule only resolves into moduleSet), so
		// production Ca isn't inflated by test→production imports.
		if (role == "" || role == facts.ModuleRoleUnknown) && looksNonProduction(f.Name) {
			excluded[f.Name] = struct{}{}
			excludedNames = append(excludedNames, f.Name)
			continue
		}
		moduleSet[f.Name] = struct{}{}
		if _, ok := acc[f.Name]; !ok {
			acc[f.Name] = &pkgAcc{repo: f.Repo}
			order = append(order, f.Name)
		}
	}

	// 2. Count classes/interfaces per owning package (via the declares relation).
	// CanonicalSymbols collapses #if/#else duplicates so a type declared once per
	// branch is not counted twice toward N; genuine overloads are preserved.
	for _, f := range facts.CanonicalSymbols(store.ByKind(facts.KindSymbol)) {
		sk, _ := f.PropAny(propSymbolKind).(string)
		if sk != facts.SymbolStruct && sk != facts.SymbolClass && sk != facts.SymbolInterface {
			continue
		}
		// TS/JS interfaces are structural data shapes (props/DTOs), not implemented
		// abstractions the way Go/Java/Kotlin interfaces are — and the sibling
		// `type X = {...}` (symbol_kind "type") is the same construct but is never
		// counted. Counting only `interface` would (a) inflate abstractness to A=1.0
		// for any props/`types.ts` bag, yielding false "useless" findings, and
		// (b) treat two equivalent constructs asymmetrically. Exclude TS interfaces
		// from N for parity; TS abstractness then comes solely from `abstract class`.
		if sk == facts.SymbolInterface {
			if lang, _ := f.PropAny(propLanguage).(string); lang == langTypeScript {
				continue
			}
		}
		pkg := declaringModule(f, moduleSet)
		if pkg == "" || mcputil.IsGeneratedPath(pkg) {
			continue
		}
		if _, ex := excluded[pkg]; ex {
			continue // symbol belongs to an excluded test/tooling module
		}
		a := acc[pkg]
		if a == nil {
			// Symbol declared in a package with no module fact. Skip it if the path
			// looks like test/tooling (defense-in-depth: an un-modeled test leaf must
			// not sneak back into the production population); otherwise track it.
			if looksNonProduction(pkg) {
				continue
			}
			a = &pkgAcc{repo: f.Repo}
			acc[pkg] = a
			order = append(order, pkg)
			moduleSet[pkg] = struct{}{}
		}
		// DI infrastructure and enums are not domain types for A/D. Dagger/Hilt
		// @Component/@Subcomponent and @Module wiring would inflate abstractness and
		// coupling (a pure-DI package showed A≈1.0 → false "useless"); enums are
		// concrete value enumerations — Java already excludes them via
		// symbol_kind=="enum", so exclude Kotlin enum classes (symbol_kind=="class"
		// + enum:true) for parity. Excluded from N, so a pure-DI/enum package becomes
		// type-less and drops out of the off-main findings and A/D averages.
		if boolProp(f, propDIComponent) || boolProp(f, propDIModule) || boolProp(f, propEnum) {
			continue
		}
		// A symbol is "abstract" (counts toward A) if it is an interface-kind
		// symbol, UNLESS the extractor set an explicit `abstract` prop — in which
		// case that prop is authoritative (it can both promote and demote). Go/TS
		// interfaces never set the prop, so they stay abstract. Ruby modules set
		// abstract:true for mixins/Concerns and abstract:false for namespace/utility
		// modules (demoting them to concrete so Rails namespaces don't inflate A).
		// Java/Kotlin/Python abstract classes and Python ABC/Protocol set true.
		isAbstract := sk == facts.SymbolInterface
		if ab, ok := f.PropAny(propAbstract).(bool); ok {
			isAbstract = ab
		}
		if isAbstract {
			a.interfaces++
		} else {
			a.classes++
		}
		// Track data holders among the counted types so a package that is mostly
		// value carriers can be reclassified away from the "rigid — extract
		// interfaces" advice, which is not actionable for data holders. (Enums are
		// already excluded from N above.)
		//
		// A dedicated construct (Kotlin data class, Java/C# record) says so
		// outright. Where a language has none in common use, the extractor may
		// instead report the STRUCTURE — state and no behaviour — via data_holder:
		// C# writes its DTOs as plain classes with auto-properties, so jellyfin
		// declares 1,552 classes and 13 records while 278 of those classes are
		// property-only carriers. Reading the construct alone saw none of them, and
		// a third of this explainer's findings there were DTO, constant and
		// attribute packages told to extract interfaces.
		if boolProp(f, propDataClass) || boolProp(f, propRecord) || boolProp(f, propDataHolder) {
			a.dataHolders++
		}
	}

	pkgs := make([]pkgInput, 0, len(order))
	for _, name := range order {
		a := acc[name]
		pkgs = append(pkgs, pkgInput{
			Name:        name,
			Repo:        a.repo,
			Classes:     a.classes,
			Interfaces:  a.interfaces,
			DataHolders: a.dataHolders,
		})
	}

	// 3. Build internal import edges from dependency facts. The importing package
	// is the text left of " -> " in the fact name (it matches module names exactly;
	// the file path is repo-prefixed and so is not used). The imported package is
	// the imports-relation target, resolved to the nearest enclosing module.
	// NOTE: we deliberately do NOT trust the dependency's "source" prop to detect
	// internal imports — extractors frequently mislabel internal imports as
	// "stdlib". The reliable signal is whether the import target resolves to a
	// known module; external/stdlib targets simply do not and are dropped.
	edges := make([]importEdge, 0)
	for _, f := range store.ByKind(facts.KindDependency) {
		fromRaw := importerOf(f.Name)
		if fromRaw == "" {
			continue
		}
		// Resolve the importing side to its enclosing module, exactly as the
		// imported side is resolved below. For Go/Java/Kotlin/TS the importer is
		// already a module dir (resolveToModule returns it unchanged); for Python
		// it is a file-stem path (e.g. ".../hooks/http") that must walk up to its
		// package dir, otherwise every internal edge is dropped.
		from := resolveToModule(fromRaw, moduleSet)
		if from == "" {
			continue
		}
		for _, r := range f.Relations {
			if r.Kind != facts.RelImports {
				continue
			}
			to := resolveToModule(r.Target, moduleSet)
			if to == "" || to == from {
				continue
			}
			edges = append(edges, importEdge{From: from, To: to})
		}
	}

	return pkgs, edges, excludedNames
}

// declaringModule returns the module a symbol belongs to, read from its `declares`
// relations.
//
// A KNOWN module wins, and that is the whole point. Taking the first `declares` target
// and stopping — which this used to do — silently assumes a symbol declares nothing but
// its own module. That holds for Go, whose symbols carry exactly one such edge, and
// fails for any language whose TYPES declare their own members: a Dart class emits
// `declares` edges to each of its methods, so the first target was a method name and
// every class minted a phantom package named after it. Measured on one repository:
// 1,746 "packages" against 199 real modules, average instability 0.00 because none of
// those phantoms had edges, and the most depended-upon package of a real application
// was a generated platform directory.
//
// Resolving against moduleSet makes that structurally impossible rather than a matter
// of emission order — a member name is never a module name — so an extractor cannot
// reintroduce the bug by appending a relation in the wrong place. moduleSet is fully
// populated from module facts before this loop runs.
//
// The first-target fallback is kept for symbols whose module has no fact of its own;
// the caller already handles that case by tracking the package it names.
func declaringModule(f facts.Fact, moduleSet map[string]struct{}) string {
	first := ""
	for _, r := range f.Relations {
		if r.Kind != facts.RelDeclares {
			continue
		}
		if _, known := moduleSet[r.Target]; known {
			return r.Target
		}
		if first == "" {
			first = r.Target
		}
	}
	return first
}

// boolProp reports whether the fact carries prop key set to boolean true.
func boolProp(f facts.Fact, key string) bool {
	b, ok := f.PropAny(key).(bool)
	return ok && b
}

// looksNonProduction reports whether a package path (used when a symbol declares
// into a package that has no module fact) is a test or build-tooling path — i.e.
// whether it should be kept out of the Martin I/A/D production population.
//
// It is the UNION of two predicates, and needs both:
//
//   - facts.ModuleRoleForPath — what the extractors themselves use, and the only one
//     of the two that models `tooling` (Scripts/, bin/, fastlane/, ci_scripts/). It
//     also carries the sub-token rule for compound module names (release-tests,
//     ui-test-utils, test-lab).
//   - facts.IsTestPath — the shared test-path definition, which knows the spellings
//     ModuleRoleForPath predates: Mocks/, __tests__/, testdata/, fixtures/, and the
//     Kotlin-Multiplatform trees (commonTest/jvmTest/iosTest/nativeTest).
//
// This matters most for the five languages that emit no module_role at all (Go,
// Python, TypeScript, PHP, C/C++), for which this fallback is the ONLY thing keeping
// test packages out of the metrics.
func looksNonProduction(pkgPath string) bool {
	return nonProductionRoles[facts.ModuleRoleForPath(pkgPath)] || facts.IsTestPath(pkgPath)
}

// importerOf returns the importing package name encoded in a dependency fact's
// name, which has the form "<importer> -> <imported>".
func importerOf(factName string) string {
	if i := strings.Index(factName, " -> "); i >= 0 {
		return factName[:i]
	}
	return ""
}

// resolveToModule returns target if it is a known module, otherwise walks up its
// parent directories until a known module is found (mirrors the OSS graph's
// resolution so import paths pointing inside a module dir map to the module).
func resolveToModule(target string, moduleSet map[string]struct{}) string {
	cur := target
	for {
		if _, ok := moduleSet[cur]; ok {
			return cur
		}
		i := strings.LastIndex(cur, "/")
		if i < 0 {
			return ""
		}
		cur = cur[:i]
	}
}

// args are the arguments for the package_metrics tool.
type args struct {
	Package    string `json:"package,omitempty" jsonschema:"Filter to packages whose name contains this substring (e.g. 'internal/app')"`
	Repo       string `json:"repo,omitempty" jsonschema:"Filter by repository label (set in multi-repo/append mode, e.g. 'go-service')"`
	SortBy     string `json:"sort_by,omitempty" jsonschema:"Sort key: 'ca', 'ce', 'classes', 'instability', 'distance', or 'name'. Default 'ca' (most-depended-upon first)."`
	Limit      int    `json:"limit,omitempty" jsonschema:"Maximum number of packages to return in compact/full output (1-1000). Default 100."`
	OutputMode string `json:"output_mode,omitempty" jsonschema:"'summary' (DEFAULT — aggregate health: avg instability/distance, off-main-sequence count, most depended-upon package) → 'compact' (markdown table) → 'full' (complete JSON)."`
	MaxTokens  int    `json:"max_tokens,omitempty" jsonschema:"Optional hard cap on output size (approx tokens). Default: no cap."`
}

type response struct {
	Packages []PackageMetric `json:"packages"`
	// Total is the number of packages MATCHING THE FILTER — the population `packages`
	// was drawn from, before the display limit. It used to be captured before
	// filterMetrics ran, so a filtered query returned the repo-wide count beside a
	// filtered `returned`: package_metrics(package="X", full) answered
	// `total: 26, returned: 1`. Same defect as analyze_performance (fixed/56), under
	// the field name a consumer is most likely to divide by.
	Total int `json:"total"`
	// RepoWidePackages is the whole snapshot's package count. The only unfiltered
	// number here, named so it cannot be mistaken for Total.
	RepoWidePackages int    `json:"repo_wide_packages"`
	Returned         int    `json:"returned"`
	Note             string `json:"note"`
}

// buildResponse pairs the filtered set with its repo-wide population. `filtered` is
// pre-limit: Total is the size of what matched, Returned is what was actually sent.
func buildResponse(population, filtered []PackageMetric) response {
	return response{
		Packages:         filtered,
		Total:            len(filtered),
		RepoWidePackages: len(population),
		Returned:         len(filtered),
		Note: "Ca/Ce are package-granular (counts of other packages) from the internal import graph; " +
			"N is the exact class+interface count per package.",
	}
}

// countExcluded counts the test/tooling modules that match the active filter, so the
// summary's "(M test/tooling excluded)" describes the same population as the numbers
// beside it. It used to be collect()'s repo-wide count, sitting in the same
// parenthesis as a type-less count computed over the FILTERED set.
func countExcluded(excluded []string, pkg, repo string) int {
	if pkg == "" && repo == "" {
		return len(excluded)
	}
	n := 0
	for _, name := range excluded {
		if pkg != "" && !strings.Contains(name, pkg) {
			continue
		}
		// Excluded modules are recorded by name only; a repo filter cannot be applied
		// to them, so a repo-filtered query reports the package-matched count.
		n++
	}
	return n
}

const toolDescription = "Compute software package metrics (Robert C. Martin / JDepend) for every " +
	"package in the snapshot, to assess responsibility, extensibility, and stability. " +
	"Run after generate_snapshot. Per package it reports: " +
	"classes_interfaces (N) — concrete + abstract classes and interfaces, an extensibility indicator; " +
	"afferent_couplings (Ca) — how many OTHER packages depend on this one (inward responsibility); " +
	"efferent_couplings (Ce) — how many OTHER packages this one depends on (outward dependence); " +
	"instability (I = Ce/(Ca+Ce), 0=stable..1=unstable); " +
	"abstractness (A = abstract types / N, where abstract types = interfaces plus abstract classes " +
	"(incl. Python ABC/Protocol/@abstractmethod), 0=concrete..1=abstract; NOTE: TypeScript/JS " +
	"interfaces are structural data shapes (like `type` aliases), not implemented abstractions, " +
	"so they are excluded from N — TS abstractness comes from `abstract class` only; SCALA is the " +
	"mirror image: a trait routinely CARRIES its implementation, so one is counted abstract only " +
	"when it declares an unimplemented member, and a case class is marked a data holder); " +
	"distance (D = |A+I-1|) — distance from the 'main sequence'; high D flags packages that are " +
	"either rigid (stable+concrete) or useless (unstable+abstract). " +
	"NOTE: Ca/Ce are PACKAGE-granular, derived from the reliable internal import graph — they count " +
	"packages, not individual classes (true class→class edges are not extracted for most languages). " +
	"For class-level blast radius use impact_analysis; for graph walks use traverse. " +
	"Filter with package=/repo=, order with sort_by= (default 'ca'). " +
	"output_mode='summary' (DEFAULT) → 'compact' → 'full'; pass max_tokens to hard-cap output. " +
	"Off-main-sequence packages also surface via query_insights(explainer=\"package-metrics\")."

// Register adds the package_metrics tool to the given MCP server. Calls are
// recorded by the OSS value middleware, which is registered once on this shared
// MCP server and so observes the analyzers too.
func Register(srv *mcp.Server, store func() *facts.Store) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "package_metrics",
		Description: toolDescription,
	}, func(_ context.Context, _ *mcp.CallToolRequest, in args) (*mcp.CallToolResult, any, error) {
		if store().Count() == 0 {
			return mcputil.ErrorResult("No facts available. Run generate_snapshot first."), nil, nil
		}

		pkgs, edges, excluded := collect(store())
		population := compute(pkgs, edges)

		// Filter FIRST. `total` used to be captured here, before filterMetrics ran, and
		// was then returned as the JSON `total` beside a filtered `returned` — the same
		// defect fixed in analyze_performance. `population` is kept only to report the
		// repo-wide count under a name that says so.
		results := filterMetrics(population, in.Package, in.Repo)
		sortMetrics(results, in.SortBy)

		mode := mcputil.ResolveOutputMode(in.OutputMode, mcputil.ModeSummary)

		// Summary reports aggregate health over the full (filtered) set, before the
		// per-package limit is applied.
		if mode == mcputil.ModeSummary {
			return mcputil.TextResult(mcputil.CapTokens(
				renderSummary(population, results, excluded, in.Package, in.Repo), in.MaxTokens, false)), nil, nil
		}

		limit := in.Limit
		if limit <= 0 {
			limit = 100
		}
		if limit > 1000 {
			limit = 1000
		}

		if mode == mcputil.ModeCompact {
			// Hide type-less (N==0) packages from the human-facing table: they have no
			// A/D (they always sit at D=1.0), are excluded from the headline analyzed
			// count and the off-main-sequence finding, and — when sorting by distance —
			// would otherwise bury every real package under a wall of D=1.0 leaves. The
			// full JSON view keeps them (it is the complete export).
			typed := make([]PackageMetric, 0, len(results))
			for _, m := range results {
				if m.ClassesInterfaces > 0 {
					typed = append(typed, m)
				}
			}
			hiddenTypeless := len(results) - len(typed)
			matchedTyped := len(typed)
			if len(typed) > limit {
				typed = typed[:limit]
			}
			return mcputil.TextResult(mcputil.CapTokens(renderCompact(typed, matchedTyped, hiddenTypeless), in.MaxTokens, false)), nil, nil
		}

		// Total is the size of the MATCHED set, captured before the display limit;
		// Returned is what actually went out.
		resp := buildResponse(population, results)
		if len(results) > limit {
			results = results[:limit]
		}
		resp.Packages = results
		resp.Returned = len(results)

		return mcputil.JSONResultCapped(resp, in.MaxTokens)
	})
}

// aggregate holds the summary health numbers, all computed over ONE consistent
// population so the analyzed count, avg I/avg D, and the off-main-sequence count
// never describe different sets. Averages and the analyzed count cover only
// packages with at least one class/interface (N≥1); "type-less" packages (N==0,
// e.g. Kotlin/Compose packages that are entirely top-level functions and vals)
// are counted separately and kept OUT of the abstractness/distance averages,
// where A and D are undefined for them. The most-depended-upon package is chosen
// across the whole population, since Ca is meaningful regardless of N.
// rigidCaFloor returns the afferent-coupling floor a rigid off-main-sequence
// finding must clear in this population: the larger of minRigidCa and the 90th
// percentile of Ca among packages with types.
//
// A fixed floor cannot serve both sizes of repository. Martin's "zone of pain" is
// about a package MANY things depend on, and what counts as many is relative: Ca=5
// is a hub in a forty-package service and background noise in a four-thousand-package
// monorepo. Scaling to the population keeps the finding proportionate, and minRigidCa
// stops a repository where almost nothing is coupled from reporting its handful of
// two-dependent packages as architectural pain.
func rigidCaFloor(results []PackageMetric) int {
	cas := make([]int, 0, len(results))
	for _, m := range results {
		if m.ClassesInterfaces > 0 {
			cas = append(cas, m.Ca)
		}
	}
	if len(cas) == 0 {
		return minRigidCa
	}
	sort.Ints(cas)
	// Nearest-rank: the smallest value at or above the 90th percentile.
	idx := (len(cas)*9 + 9) / 10
	if idx >= len(cas) {
		idx = len(cas) - 1
	}
	if p90 := cas[idx]; p90 > minRigidCa {
		return p90
	}
	return minRigidCa
}

type aggregate struct {
	analyzed    int // packages with N≥1 (the A/D metrics apply)
	typeless    int // packages with N==0, excluded from the averages
	avgI, avgD  float64
	offMain     int
	mostCoupled PackageMetric
	// rigidCaFloor is the population's own bar for a rigid finding, reported so the
	// summary line states the threshold it applied rather than a constant.
	rigidCaFloor int
}

func aggregateMetrics(results []PackageMetric) aggregate {
	var agg aggregate
	agg.rigidCaFloor = rigidCaFloor(results)
	var sumI, sumD float64
	for _, m := range results {
		if m.Ca > agg.mostCoupled.Ca {
			agg.mostCoupled = m
		}
		if m.ClassesInterfaces == 0 {
			agg.typeless++
			continue // A and D are undefined for a package with no types
		}
		agg.analyzed++
		sumI += m.Instability
		sumD += m.Distance
		if isOffMainSequence(m) {
			agg.offMain++
		}
	}
	if agg.analyzed > 0 {
		agg.avgI = round3(sumI / float64(agg.analyzed))
		agg.avgD = round3(sumD / float64(agg.analyzed))
	}
	return agg
}

// ExcludedNote renders the "(N type-less, M test/tooling excluded)" suffix shared
// by the summary and --explain views. Both categories are set aside from the
// analyzed population: type-less (N==0) packages have no A/D, and test/tooling
// modules describe non-production structure. Only non-zero parts are shown.
func ExcludedNote(typeless, excludedTestTooling int) string {
	var parts []string
	if typeless > 0 {
		parts = append(parts, fmt.Sprintf("%d type-less", typeless))
	}
	if excludedTestTooling > 0 {
		parts = append(parts, fmt.Sprintf("%d test/tooling", excludedTestTooling))
	}
	if len(parts) == 0 {
		return ""
	}
	return fmt.Sprintf(" (%s excluded)", strings.Join(parts, ", "))
}

// renderSummary is the default (token-light) view: aggregate package-health numbers
// over the filtered set, mirroring the ExplainSection framing.
//
// Every number here now describes the same population. The excluded count used to be
// collect()'s repo-wide total, sitting in the same parenthesis as a type-less count
// computed over the FILTERED results — two numbers, one clause, different populations.
func renderSummary(population, results []PackageMetric, excluded []string, pkg, repo string) string {
	agg := aggregateMetrics(results)
	var b strings.Builder

	// The headline counts the ANALYZED packages (type-less ones have no A/D and are
	// reported separately in the note), so the Scoped set is the analyzed subset — not
	// `results`, which would silently change the number this line has always shown.
	analyzed := make([]PackageMetric, 0, len(results))
	for _, m := range results {
		if m.ClassesInterfaces > 0 {
			analyzed = append(analyzed, m)
		}
	}
	scope := mcputil.Scope(len(population), analyzed, mcputil.DescribeFilters("package", pkg, "repo", repo))

	// Headline() ends in a period; the excluded note and colon follow it here.
	fmt.Fprintf(&b, "Package metrics — %s", strings.TrimSuffix(scope.Headline("package(s) analyzed"), "."))
	b.WriteString(ExcludedNote(agg.typeless, countExcluded(excluded, pkg, repo)))
	b.WriteString(":\n")
	fmt.Fprintf(&b, "  avg instability (I): %.2f\n", agg.avgI)
	fmt.Fprintf(&b, "  avg distance (D):    %.2f\n", agg.avgD)
	fmt.Fprintf(&b, "  off main sequence:   %d  (D > %.1f, N ≥ %d, rigid needs Ca ≥ %d — rigid or useless)\n", agg.offMain, painfulDistance, minPainfulTypes, agg.rigidCaFloor)
	if agg.mostCoupled.Package != "" {
		fmt.Fprintf(&b, "  most depended-upon:  %s (Ca=%d)\n", agg.mostCoupled.Package, agg.mostCoupled.Ca)
	}
	b.WriteString("\nUse output_mode=compact for the per-package table, or full for JSON.\n")
	return b.String()
}

func filterMetrics(in []PackageMetric, pkg, repo string) []PackageMetric {
	if pkg == "" && repo == "" {
		return in
	}
	out := in[:0:0]
	for _, m := range in {
		if pkg != "" && !strings.Contains(m.Package, pkg) {
			continue
		}
		if repo != "" && m.Repo != repo {
			continue
		}
		out = append(out, m)
	}
	return out
}

func sortMetrics(m []PackageMetric, sortBy string) {
	less := func(i, j int) bool { return m[i].Ca > m[j].Ca } // default: 'ca'
	switch sortBy {
	case "ce":
		less = func(i, j int) bool { return m[i].Ce > m[j].Ce }
	case "classes":
		less = func(i, j int) bool { return m[i].ClassesInterfaces > m[j].ClassesInterfaces }
	case "instability":
		less = func(i, j int) bool { return m[i].Instability > m[j].Instability }
	case "distance":
		less = func(i, j int) bool { return m[i].Distance > m[j].Distance }
	case "name":
		less = func(i, j int) bool { return m[i].Package < m[j].Package }
	}
	// Stable sort with package name as the tie-breaker for deterministic output.
	sort.SliceStable(m, func(i, j int) bool {
		if less(i, j) {
			return true
		}
		if less(j, i) {
			return false
		}
		return m[i].Package < m[j].Package
	})
}

func renderCompact(results []PackageMetric, matchedTyped, hiddenTypeless int) string {
	var sb strings.Builder
	hidden := ""
	if hiddenTypeless > 0 {
		hidden = fmt.Sprintf("; %d type-less hidden", hiddenTypeless)
	}
	fmt.Fprintf(&sb, "Package metrics — %d package(s) with types (showing %d%s):\n\n", matchedTyped, len(results), hidden)
	sb.WriteString("| Package | N | Ca | Ce | I | A | D |\n")
	sb.WriteString("|---------|---|----|----|---|---|---|\n")
	for _, m := range results {
		fmt.Fprintf(&sb, "| %s | %d | %d | %d | %.2f | %.2f | %.2f |\n",
			m.Package, m.ClassesInterfaces, m.Ca, m.Ce, m.Instability, m.Abstractness, m.Distance)
	}
	return sb.String()
}

// painfulDistance is the "distance from the main sequence" above which a package
// is flagged as architecturally painful (rigid or useless) in the explain summary.
const painfulDistance = 0.7

// minPainfulTypes is the smallest package size (N = classes + interfaces) for which
// distance-from-main-sequence is treated as architecturally meaningful. A concrete
// leaf package (nothing internal depends on it, so Ce=0 ⇒ I=0, A=0) always sits at
// D=1.0 regardless of size, so tiny 1–2 type packages would otherwise flood the
// "rigid or useless" findings with noise. Their D is still reported in the
// per-package table; they are only excluded from the off-main-sequence count/insights.
const minPainfulTypes = 3

// dataHolderReclassifyRatio is the fraction of a package's counted types that must
// be value carriers (Kotlin data class / Java record) for an off-main "rigid"
// finding to be reclassified as an expected data/model package rather than a code
// smell — "extract interfaces" is not actionable for DTOs/models.
const dataHolderReclassifyRatio = 0.6

// mainSequenceBand is the maximum distance from the main sequence at which a
// typed package is considered healthily "on the line" (Zone of Excellence). It is
// only a presentation band for Classify — it does NOT affect the off-main-sequence
// count/insights, which are governed solely by painfulDistance/isOffMainSequence.
const mainSequenceBand = 0.2

// Zone is the canonical main-sequence classification of a package (Robert Martin's
// "Distance from the Main Sequence"). It is the single source of truth shared by
// the package-metrics explainer and the dashboard so their labels can
// never drift from what package_metrics reports.
type Zone string

const (
	ZoneMainSequence Zone = "main-sequence" // near the A+I=1 line — healthy balance
	ZonePain         Zone = "pain"          // off-main, stable + concrete (I < 0.5) — rigid
	ZoneUseless      Zone = "useless"       // off-main, unstable + abstract (I ≥ 0.5) — unused abstraction
	ZoneNeutral      Zone = "neutral"       // too small/uncoupled/mid-distance to judge
)

// Classify returns the canonical main-sequence zone for a package, using the exact
// thresholds isOffMainSequence and the explainer already use. A package is only
// "pain" or "useless" when it is genuinely off the main sequence (D > painfulDistance,
// substantial and coupled); of those, instability alone separates the two corners
// (D > 0.7 forces A+I<0.3 → both low → pain, or A+I>1.7 → both high → useless), so
// an unstable+concrete package can never be off-main and thus never reaches here.
func Classify(m PackageMetric) Zone {
	if isOffMainSequence(m) {
		if m.Instability >= 0.5 {
			return ZoneUseless
		}
		return ZonePain
	}
	if m.ClassesInterfaces > 0 && m.Distance <= mainSequenceBand {
		return ZoneMainSequence
	}
	return ZoneNeutral
}

// isOffMainSequence reports whether a package is both far enough from the main
// sequence AND substantial/coupled enough for that distance to be actionable.
// Requiring N ≥ minPainfulTypes drops trivial leaves; requiring Ca+Ce > 0 drops
// isolated packages, which can be neither "rigid" (nothing depends on them) nor
// "useless" (they are not coupled at all). A package that is overwhelmingly value
// carriers (DataHolderRatio ≥ dataHolderReclassifyRatio) is concrete BY DESIGN —
// a DTO/schema/model bundle (e.g. Pydantic/@dataclass "datamodels" packages, Java
// records) that lands at high D purely for being stable+concrete. "Extract
// interfaces" is not actionable for it, so it is not an off-main-sequence smell;
// its real change-risk is still surfaced by the most-depended-upon insight.
// Shared by the summary count, the --explain section, and the package-metrics
// explainer so all three agree.
func isOffMainSequence(m PackageMetric) bool {
	if m.Distance <= painfulDistance ||
		m.ClassesInterfaces < minPainfulTypes ||
		m.Ca+m.Ce == 0 ||
		m.DataHolderRatio >= dataHolderReclassifyRatio {
		return false
	}
	// The two corners are asymmetric, because their claims are. "Useless" says
	// almost nothing depends on this abstraction, so a low Ca is the finding and
	// gating on it would delete it. "Rigid" says many things depend on it and cannot
	// move, which is false of a package one other package imports — and that was most
	// of what this reported: on enola's own tree, four of nine rigid findings had
	// Ca ≤ 3, each carrying the sentence "many packages depend on it".
	if m.Instability >= 0.5 {
		return true // useless corner; see Classify for the same split
	}
	return m.Ca >= m.RigidCaFloor
}

// minRigidCa is the floor under rigidCaFloor: below this, "many packages depend on
// it" is not a sentence worth printing however small the repository.
const minRigidCa = 5

// Summary is the aggregate health block `enola --explain` prints. It is data
// rather than a rendered string, so pkg/explain formats it beside every other
// section of the report instead of receiving one section pre-rendered.
type Summary struct {
	Analyzed int
	// Typeless and ExcludedTestTooling say what the analyzed count leaves out:
	// packages with no types, for which A and D are undefined, and modules tagged
	// as test or tooling.
	Typeless            int
	ExcludedTestTooling int
	AvgI, AvgD          float64
	// OffMain counts packages past PainfulDistance that are large enough and
	// coupled enough for the distance to mean something; the thresholds travel with
	// it so the report can state them rather than hard-coding a second copy.
	OffMain         int
	PainfulDistance float64
	MinPainfulTypes int
	// RigidCaFloor is the afferent-coupling bar a rigid finding had to clear in this
	// snapshot. It travels with the other two thresholds so the report states what it
	// applied instead of restating a constant that is no longer the whole rule.
	RigidCaFloor int
	// MostCoupledPackage is empty when nothing is depended upon at all.
	MostCoupledPackage string
	MostCoupledCa      int
}

// Summarize computes the aggregate health numbers over the whole store, using the
// same collect/compute core as the package_metrics tool so the two agree.
func Summarize(store *facts.Store) Summary {
	pkgs, edges, excluded := collect(store)
	agg := aggregateMetrics(compute(pkgs, edges))
	return Summary{
		Analyzed:            agg.analyzed,
		Typeless:            agg.typeless,
		ExcludedTestTooling: len(excluded),
		AvgI:                agg.avgI,
		AvgD:                agg.avgD,
		OffMain:             agg.offMain,
		PainfulDistance:     painfulDistance,
		MinPainfulTypes:     minPainfulTypes,
		RigidCaFloor:        agg.rigidCaFloor,
		MostCoupledPackage:  agg.mostCoupled.Package,
		MostCoupledCa:       agg.mostCoupled.Ca,
	}
}
