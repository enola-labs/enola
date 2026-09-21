// Package explain produces a human-readable statistical summary of an Enola
// architectural snapshot — the data behind `enola --explain <repository>`.
//
// Report is exported data and Render turns it into text. Compute works purely off
// the exported bootstrap.Engine API, so it sees whatever the engine currently
// holds: run GenerateSnapshot (or auto-load a snapshot) first.
package explain

import (
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/metrics"
	"github.com/enola-labs/enola/internal/orphans"
	"github.com/enola-labs/enola/internal/perf"
	"github.com/enola-labs/enola/pkg/bootstrap"
)

// Criticality thresholds for a module hotspot, scored by fan-in + fan-out.
// Mirrors the llm_context renderer so "critical module" means the same thing
// everywhere.
const (
	criticalHigh   = 10
	criticalMedium = 5
)

// blastDepth / blastNodes bound the reverse reachability used to estimate a
// hotspot's blast radius (the impact_analysis number). Kept modest so --explain
// stays fast even on large repos; the total is still accurate within the depth.
const (
	blastDepth  = 3
	blastNodes  = 500
	topHotspots = 8
)

// LabelCount is a named tally (a kind, a symbol kind, an HTTP method, …).
type LabelCount struct {
	Label string `json:"label"`
	Count int    `json:"count"`
}

// Hotspot is a module ranked by coupling, with its estimated change blast radius.
type Hotspot struct {
	Module      string `json:"module"`
	FanIn       int    `json:"fan_in"`
	FanOut      int    `json:"fan_out"`
	Criticality string `json:"criticality"`  // high | medium | low
	BlastRadius int    `json:"blast_radius"` // transitive reverse-dependents within blastDepth
}

// RankedItem is one offender in a code-health finding group: a symbol or module
// plus a pre-formatted metric (e.g. "147 dependents", "depth 11").
type RankedItem struct {
	Name   string `json:"name"`
	Detail string `json:"detail"`
}

// FindingGroup is one code-health explainer's contribution: its total count and
// the top offenders for display.
type FindingGroup struct {
	Label string       `json:"label"`
	Count int          `json:"count"`
	Top   []RankedItem `json:"top,omitempty"`
}

// VendoredReport summarises the vendored-candidates finding for the statistics
// report: how many directories look like vendored dependencies, how much code
// they hold, and the largest of them.
type VendoredReport struct {
	Count int          `json:"count"`
	Files int          `json:"files"`
	Top   []RankedItem `json:"top,omitempty"`
	// Omitted is how many candidates Top does not show. Reported explicitly rather
	// than left to the reader to infer: a truncated list that does not say it is
	// truncated is the same failure this whole finding exists to avoid.
	Omitted int `json:"omitted,omitempty"`
}

// Report is the full statistical picture of a snapshot. Fields are plain types
// only, so consumers in other modules can read them without importing enola's
// internal packages.
// ArchitectureStatement is one named layer order the snapshot recognised. There
// is one per language cohort, so a repository written in two things has two.
type ArchitectureStatement struct {
	Pattern    string  `json:"pattern"`
	Confidence float64 `json:"confidence"`
}

type Report struct {
	RepoPath    string   `json:"repo_path"`
	GeneratedAt string   `json:"generated_at,omitempty"`
	Duration    string   `json:"duration,omitempty"`
	Extractors  []string `json:"extractors,omitempty"`
	// Languages are the actual source languages present, most-prevalent first
	// (derived from the per-fact "language" prop, not the extractor names — the
	// C/C++ extractor is named "cpp" but a repo may be entirely C).
	Languages  []string `json:"languages,omitempty"`
	TotalFacts int      `json:"total_facts"`

	KindCounts     []LabelCount `json:"kind_counts"`     // module/symbol/route/storage/dependency/service
	RelationCounts []LabelCount `json:"relation_counts"` // declares/imports/calls/implements/depends_on/instantiates/injects/has_method
	SymbolKinds    []LabelCount `json:"symbol_kinds"`    // function/method/struct/…
	DepSources     []LabelCount `json:"dep_sources"`     // external/internal/stdlib/…

	Routes         int          `json:"routes"`
	RoutesByMethod []LabelCount `json:"routes_by_method,omitempty"`
	Storage        int          `json:"storage"`

	// Architecture and ArchConfidence are the STRONGEST statement, kept as scalars
	// so the existing JSON shape does not move. Architectures holds all of them,
	// which on a polyglot repository is more than one: a Rails monolith shipping an
	// Ember front end is named as both, over disjoint sets of modules, and
	// reporting only the higher-confidence half silently drops the other.
	Architecture    string                  `json:"architecture,omitempty"`
	ArchConfidence  float64                 `json:"architecture_confidence,omitempty"`
	Architectures   []ArchitectureStatement `json:"architectures,omitempty"`
	Cycles          int                     `json:"cyclic_dependencies"`
	LayerViolations int                     `json:"layer_violations"`
	CrossRepoEdges  int                     `json:"cross_repo_edges"`

	Modules           int       `json:"modules"`
	HighCriticality   int       `json:"high_criticality"`
	MediumCriticality int       `json:"medium_criticality"`
	Hotspots          []Hotspot `json:"hotspots,omitempty"`

	// CouplingUnresolved is true when dependency facts exist but none of their
	// import edges resolved to a module — coupling analysis is unavailable, not
	// genuinely zero. The renderer surfaces this as a note.
	CouplingUnresolved bool `json:"coupling_unresolved,omitempty"`

	// CodeHealth holds the per-explainer findings from the symbol/module-level
	// explainers (god-class, hotspots, dependency-depth, exported-surface,
	// complexity-outliers), each with a total count and its top offenders.
	CodeHealth []FindingGroup `json:"code_health,omitempty"`

	// Vendored holds the vendored-candidates explainer's report: directories that
	// look like in-tree copies of another project. It is a scope note rather than a
	// defect, which is why it is not folded into CodeHealth — nothing here is wrong
	// with the code, and nothing has been excluded from the snapshot.
	Vendored *VendoredReport `json:"vendored_candidates,omitempty"`

	// PackageMetrics, DeadCode and Performance are the three analyzers' aggregate
	// blocks. They are data rather than pre-rendered text, like everything above:
	// Render decides how the report reads, in one place.
	//
	// They are placed by ALTITUDE rather than in the order they were added.
	// PackageMetrics renders straight after Impact analysis because both describe
	// module coupling over the same edges, and reading Ca and Ce beside fan-in and
	// fan-out is how a reader sees they are the same numbers seen two ways. DeadCode
	// and Performance render straight after Code health because all three are
	// symbol-level findings.
	PackageMetrics *metrics.Summary `json:"package_metrics,omitempty"`
	DeadCode       *orphans.Summary `json:"dead_code,omitempty"`
	Performance    *perf.Summary    `json:"performance,omitempty"`
}

// Compute reads the engine's current fact store and snapshot and builds a Report.
// It does not generate a snapshot — callers do that first.
func Compute(eng *bootstrap.Engine) *Report {
	store := eng.Store()
	snap := eng.Snapshot()

	r := &Report{TotalFacts: store.Count()}
	if snap != nil {
		r.RepoPath = snap.Meta.RepoPath
		r.GeneratedAt = snap.Meta.GeneratedAt
		r.Duration = snap.Meta.Duration
		r.Extractors = snap.Meta.Extractors
	}
	r.Languages = languagesByPrevalence(store)

	// Architectural-kind tallies, in the canonical order from ARCHITECTURE.md.
	for _, k := range []string{
		facts.KindModule, facts.KindSymbol, facts.KindRoute,
		facts.KindStorage, facts.KindDependency, facts.KindService,
	} {
		if n := len(store.ByKind(k)); n > 0 {
			r.KindCounts = append(r.KindCounts, LabelCount{Label: k, Count: n})
		}
	}

	// Relation-kind tallies (edge counts, not fact counts) — relations live on
	// facts of every kind, so this scans the whole store, not one ByKind slice.
	relCount := map[string]int{}
	// FactsRef, not All: a read-only tally that retains nothing needs no copy.
	for _, f := range store.FactsRef() {
		for _, rel := range f.Relations {
			relCount[rel.Kind]++
		}
	}
	for _, k := range []string{
		facts.RelDeclares, facts.RelImports, facts.RelCalls, facts.RelImplements,
		facts.RelDependsOn, facts.RelInstantiates, facts.RelInjects, facts.RelHasMethod,
	} {
		if n := relCount[k]; n > 0 {
			r.RelationCounts = append(r.RelationCounts, LabelCount{Label: k, Count: n})
		}
	}

	// Symbol-kind breakdown (function/method/struct/…).
	skCount := map[string]int{}
	for _, f := range store.ByKind(facts.KindSymbol) {
		sk, _ := f.PropAny("symbol_kind").(string)
		if sk == "" {
			sk = "unknown"
		}
		skCount[sk]++
	}
	r.SymbolKinds = sortedCounts(skCount)

	// Routes, broken down by HTTP method.
	routes := store.ByKind(facts.KindRoute)
	r.Routes = len(routes)
	methodCount := map[string]int{}
	for _, f := range routes {
		m, _ := f.PropAny("method").(string)
		if m == "" {
			m = "(unspecified)"
		} else {
			m = strings.ToUpper(m)
		}
		methodCount[m]++
	}
	if r.Routes > 0 {
		r.RoutesByMethod = sortedCounts(methodCount)
	}

	r.Storage = len(store.ByKind(facts.KindStorage))

	// Dependency facts grouped by their declared source (external/internal/stdlib).
	srcCount := map[string]int{}
	for _, f := range store.ByKind(facts.KindDependency) {
		s, _ := f.PropAny("source").(string)
		if s == "" {
			s = "unclassified"
		}
		srcCount[s]++
	}
	if len(srcCount) > 0 {
		r.DepSources = sortedCounts(srcCount)
	}

	r.Modules = len(store.ByKind(facts.KindModule))

	// Insight-derived numbers: architecture pattern, cycles, layer violations,
	// cross-repo edges, plus the code-health explainer groups. Titles are matched
	// against the explainer formats (see each explainer's Title: site).
	if snap != nil {
		// Code-health groups, assembled in a fixed display order. Each accumulates
		// its count and (up to topPerGroup) top offenders; insights arrive already
		// severity-sorted within each explainer, so first-seen == top.
		godClass := &FindingGroup{Label: "god classes (high fan-in)"}
		hotspots := &FindingGroup{Label: "call-graph hotspots"}
		deepChains := &FindingGroup{Label: "deep dependency chains"}
		surfaces := &FindingGroup{Label: "large public surfaces"}
		complexFns := &FindingGroup{Label: "complexity outliers"}

		for _, in := range snap.Insights {
			switch {
			case strings.HasPrefix(in.Title, "Cyclic dependency"):
				r.Cycles++
			case strings.HasPrefix(in.Title, "Layer violation"):
				r.LayerViolations++
			case strings.HasPrefix(in.Title, "Architecture pattern:"):
				// A repository can produce several of these: one per language cohort,
				// plus a declared order at 1.00 where the repository states its own.
				// All of them are kept.
				name := strings.TrimSpace(strings.TrimPrefix(in.Title, "Architecture pattern:"))
				r.Architectures = append(r.Architectures, ArchitectureStatement{
					Pattern: name, Confidence: in.Confidence,
				})
				// The scalar keeps its old meaning — the strongest statement. A repo
				// that DECLARES its layer order emits the declaration at 1.00 and the
				// layout enola also recognised; last-one-wins would report the guess
				// and hide the declaration, on a repo whose gate enforces it.
				if in.Confidence > r.ArchConfidence {
					r.Architecture = name
					r.ArchConfidence = in.Confidence
				}
			case strings.HasPrefix(in.Title, "Cross-repo dependencies"):
				r.CrossRepoEdges = firstParenInt(in.Title)

			case strings.HasPrefix(in.Title, "High fan-in symbol:"):
				name := nameBetween(in.Title, "High fan-in symbol:", " (")
				addFinding(godClass, name, fmt.Sprintf("%d dependents", firstParenInt(in.Title)))
			case strings.HasPrefix(in.Title, "Call-graph hotspot:"):
				name := nameBetween(in.Title, "Call-graph hotspot:", " (")
				// Scan for metrics only after "(" so digits in the symbol name
				// (e.g. "Sha256Hash", "oauth2") aren't mistaken for fan-in/out.
				addFinding(hotspots, name, fanDetail(metricInts(in.Title, "(")))
			case strings.HasPrefix(in.Title, "Deep dependency chain:"):
				name := nameBetween(in.Title, "Deep dependency chain:", " (")
				addFinding(deepChains, name, fmt.Sprintf("depth %d", firstParenInt(in.Title)))
			case strings.HasPrefix(in.Title, "Large public surface:"):
				name := nameBetween(in.Title, "Large public surface:", " exports")
				// Scan for metrics only after " exports" so digits in the module
				// name (e.g. "oauth2", "utf8") aren't parsed as exported/total.
				addFinding(surfaces, name, surfaceDetail(metricInts(in.Title, " exports")))
			case strings.HasPrefix(in.Title, "High cyclomatic complexity:"):
				name := nameBetween(in.Title, "High cyclomatic complexity:", " (")
				addFinding(complexFns, name, fmt.Sprintf("complexity %d", firstParenInt(in.Title)))
			}
		}

		// Matched on Source rather than on a title prefix like the cases above. The
		// title formats are a contract those explainers keep deliberately; this one
		// carries its candidates as EVIDENCE, so there is nothing to parse out of a
		// string and no reason to invent a format to parse.
		for _, in := range snap.Insights {
			if in.Source != "vendored-candidates" {
				continue
			}
			v := &VendoredReport{Count: len(in.Evidence)}
			for _, ev := range in.Evidence {
				v.Files += leadingInt(ev.Detail)
				if len(v.Top) < topPerGroup {
					v.Top = append(v.Top, RankedItem{
						Name:   strings.TrimSuffix(path.Dir(ev.File), "/"),
						Detail: ev.Detail,
					})
				}
			}
			v.Omitted = v.Count - len(v.Top)
			if v.Count > 0 {
				r.Vendored = v
			}
			break
		}

		for _, g := range []*FindingGroup{godClass, hotspots, deepChains, surfaces, complexFns} {
			if g.Count > 0 {
				r.CodeHealth = append(r.CodeHealth, *g)
			}
		}
	}

	computeHotspots(store, r)

	// The three analyzers, each computed by the same core as its MCP tool so the
	// report and the tool cannot disagree. A block with nothing in it is left nil
	// rather than printed empty: a repository with no typed package has nothing to
	// say about the main sequence, and saying it in zeroes is worse than silence.
	if m := metrics.Summarize(store); m.Analyzed > 0 {
		r.PackageMetrics = &m
	}
	if d := orphans.Summarize(store); d.Candidates > 0 {
		r.DeadCode = &d
	}
	if p := perf.Summarize(store); p.FunctionsAnalyzed > 0 {
		r.Performance = &p
	}
	return r
}

// languagesByPrevalence returns the distinct source languages present across
// module facts, most-common first (ties broken alphabetically for determinism).
// It reads the per-fact "language" prop rather than the extractor names, so a
// repo parsed by the "cpp" extractor but written entirely in C reports "c".
// Returns nil when no module carries a language (e.g. a pre-language snapshot),
// letting the renderer fall back to the extractor list.
func languagesByPrevalence(store *facts.Store) []string {
	counts := map[string]int{}
	for _, f := range store.ByKind(facts.KindModule) {
		if l, _ := f.PropAny("language").(string); l != "" {
			counts[l]++
		}
	}
	if len(counts) == 0 {
		return nil
	}
	langs := make([]string, 0, len(counts))
	for l := range counts {
		langs = append(langs, l)
	}
	sort.Slice(langs, func(i, j int) bool {
		if counts[langs[i]] != counts[langs[j]] {
			return counts[langs[i]] > counts[langs[j]]
		}
		return langs[i] < langs[j]
	})
	return langs
}

// topPerGroup caps how many offenders each code-health group lists in the report.
const topPerGroup = 5

// addFinding increments a group's count and records the offender as a top item
// until the per-group display cap is reached.
// leadingInt reads the integer a detail string starts with ("305 indexed file(s), …"),
// or 0. Used only to total the candidate file counts.
func leadingInt(s string) int {
	n := 0
	for i := 0; i < len(s) && s[i] >= '0' && s[i] <= '9'; i++ {
		n = n*10 + int(s[i]-'0')
	}
	return n
}

func addFinding(g *FindingGroup, name, detail string) {
	g.Count++
	if len(g.Top) < topPerGroup {
		g.Top = append(g.Top, RankedItem{Name: name, Detail: detail})
	}
}

// fanDetail formats a hotspot's "fan-in N / out M" from the ints parsed out of
// its title (fan-in first, fan-out second).
func fanDetail(ints []int) string {
	in, out := 0, 0
	if len(ints) > 0 {
		in = ints[0]
	}
	if len(ints) > 1 {
		out = ints[1]
	}
	return fmt.Sprintf("fan-in %d / out %d", in, out)
}

// surfaceDetail formats "E/T (P%)" from the ints parsed out of a large-public-
// surface title (exported, total, percent — in that order).
func surfaceDetail(ints []int) string {
	for len(ints) < 3 {
		ints = append(ints, 0)
	}
	return fmt.Sprintf("%d/%d (%d%%)", ints[0], ints[1], ints[2])
}

// computeHotspots ranks modules by fan-in + fan-out (the same coupling signal as
// the llm_context "Critical Modules" table) and estimates each top module's
// change blast radius via reverse graph reachability.
func computeHotspots(store *facts.Store, r *Report) {
	modules := map[string]bool{}
	for _, f := range store.ByKind(facts.KindModule) {
		modules[f.Name] = true
	}

	fanIn := map[string]int{}
	fanOut := map[string]int{}
	// revAdj[dst] is the set of modules that directly import dst — a deduped reverse
	// module adjacency used to compute a module-granularity blast radius (below).
	revAdj := map[string]map[string]bool{}
	resolvedEdges := 0
	deps := store.ByKind(facts.KindDependency)
	for _, dep := range deps {
		src := fileDir(dep.File)
		for _, rel := range dep.Relations {
			if rel.Kind != facts.RelImports {
				continue
			}
			// Resolve the import target to its nearest enclosing module. Some
			// extractors (e.g. Kotlin) emit type-level targets one segment below the
			// module dir; graph.go and the package-metrics tool already walk up, so
			// resolve here too rather than requiring an exact module match. External
			// targets are dotted (no '/'), so the walk-up finds nothing and they are
			// correctly ignored.
			if dst := resolveToModule(rel.Target, modules); dst != "" {
				fanOut[src]++
				fanIn[dst]++
				resolvedEdges++
				if src != dst {
					if revAdj[dst] == nil {
						revAdj[dst] = map[string]bool{}
					}
					revAdj[dst][src] = true
				}
			}
		}
	}

	// Dependency facts exist but nothing resolved to a module: coupling could not
	// be computed (e.g. an extractor whose import targets don't match module
	// names). Flag it so the renderer says so rather than implying zero coupling.
	if len(deps) > 0 && resolvedEdges == 0 {
		r.CouplingUnresolved = true
	}

	type scored struct {
		name          string
		fanIn, fanOut int
		score         int
	}
	var ranked []scored
	for mod := range modules {
		s := scored{name: mod, fanIn: fanIn[mod], fanOut: fanOut[mod], score: fanIn[mod] + fanOut[mod]}
		if s.score == 0 {
			continue
		}
		switch {
		case s.score >= criticalHigh:
			r.HighCriticality++
		case s.score >= criticalMedium:
			r.MediumCriticality++
		}
		ranked = append(ranked, s)
	}

	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		return ranked[i].name < ranked[j].name // stable, deterministic
	})

	limit := topHotspots
	if len(ranked) < limit {
		limit = len(ranked)
	}
	for _, s := range ranked[:limit] {
		r.Hotspots = append(r.Hotspots, Hotspot{
			Module:      s.name,
			FanIn:       s.fanIn,
			FanOut:      s.fanOut,
			Criticality: criticalityLabel(s.score),
			// Blast radius is the count of distinct MODULES that transitively depend on
			// this one within blastDepth hops. Computing it over the module graph (not
			// the full symbol/fact graph via ImpactSet) keeps it bounded by the module
			// count and comparable to the fan-in column: in a densely-coupled autoloaded
			// codebase (Rails) an all-node reverse count saturates into the tens of
			// thousands and stops discriminating between modules.
			BlastRadius: reverseModuleReach(revAdj, s.name, blastDepth),
		})
	}
}

// reverseModuleReach counts the distinct modules that transitively depend on start
// within maxDepth hops of the reverse module adjacency (breadth-first, each module
// counted once, the start itself excluded).
func reverseModuleReach(revAdj map[string]map[string]bool, start string, maxDepth int) int {
	if maxDepth <= 0 {
		maxDepth = 3
	}
	visited := map[string]bool{start: true}
	frontier := []string{start}
	for depth := 0; depth < maxDepth && len(frontier) > 0; depth++ {
		var next []string
		for _, mod := range frontier {
			for dep := range revAdj[mod] {
				if !visited[dep] {
					visited[dep] = true
					next = append(next, dep)
				}
			}
		}
		frontier = next
	}
	return len(visited) - 1 // exclude start
}

func criticalityLabel(score int) string {
	switch {
	case score >= criticalHigh:
		return "high"
	case score >= criticalMedium:
		return "medium"
	default:
		return "low"
	}
}

// sortedCounts converts a tally map into a slice ordered by count desc, then
// label asc, for deterministic output.
func sortedCounts(m map[string]int) []LabelCount {
	out := make([]LabelCount, 0, len(m))
	for k, v := range m {
		out = append(out, LabelCount{Label: k, Count: v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Label < out[j].Label
	})
	return out
}

// firstParenInt extracts the first integer appearing inside parentheses, e.g.
// "Cross-repo dependencies (7 edges)" -> 7. Returns 0 if none is found.
func firstParenInt(s string) int {
	open := strings.IndexByte(s, '(')
	if open < 0 {
		return 0
	}
	rest := s[open+1:]
	digits := strings.Builder{}
	for _, ch := range rest {
		if ch >= '0' && ch <= '9' {
			digits.WriteRune(ch)
		} else if digits.Len() > 0 {
			break
		}
	}
	if digits.Len() == 0 {
		return 0
	}
	n, _ := strconv.Atoi(digits.String())
	return n
}

// nameBetween returns the symbol/module name in an insight title: the text after
// prefix up to the first occurrence of stop (e.g. " (" or " exports"), trimmed.
// Symbol and module names contain no "(", so the cut is unambiguous.
func nameBetween(title, prefix, stop string) string {
	s := strings.TrimPrefix(title, prefix)
	if i := strings.Index(s, stop); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// metricInts parses the integer metrics out of an insight title, starting the
// scan at the first occurrence of marker. Names precede the metric section (a
// symbol/module name has no "(" and appears before " exports"), so cutting at
// the marker keeps digits inside a name — "Sha256Hash", "oauth2", "utf8" — from
// being misread as metrics. Falls back to scanning the whole title if the marker
// is absent.
func metricInts(title, marker string) []int {
	if i := strings.Index(title, marker); i >= 0 {
		return allInts(title[i:])
	}
	return allInts(title)
}

// allInts returns every run of digits in s as integers, in order. Used to pull
// the metrics out of insight titles (e.g. "(fan-in 64, fan-out 20)" -> [64,20]).
func allInts(s string) []int {
	var out []int
	digits := strings.Builder{}
	flush := func() {
		if digits.Len() > 0 {
			n, _ := strconv.Atoi(digits.String())
			out = append(out, n)
			digits.Reset()
		}
	}
	for _, ch := range s {
		if ch >= '0' && ch <= '9' {
			digits.WriteRune(ch)
		} else {
			flush()
		}
	}
	flush()
	return out
}

// resolveToModule returns the nearest enclosing module of target: target itself if
// it is a module, else its closest ancestor directory that is. Returns "" if none.
// Mirrors graph.go's resolveToModule (unexported there), so hotspot coupling sees
// the same edges as traversal and package metrics.
func resolveToModule(target string, modules map[string]bool) string {
	cur := target
	for cur != "" {
		if modules[cur] {
			return cur
		}
		i := strings.LastIndex(cur, "/")
		if i < 0 {
			return ""
		}
		cur = cur[:i]
	}
	return ""
}

// fileDir returns the directory portion of a repo-relative file path (the module
// a fact belongs to). Mirrors the llm_context renderer.
func fileDir(file string) string {
	parts := strings.Split(file, "/")
	if len(parts) <= 1 {
		return "."
	}
	return strings.Join(parts[:len(parts)-1], "/")
}
