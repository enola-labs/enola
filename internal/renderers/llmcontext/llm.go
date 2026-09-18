package llmcontext

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/enola-labs/enola/internal/explainers/layers"
	"github.com/enola-labs/enola/internal/facts"
)

// maxLayerMappingLines caps how many module-to-layer lines the architecture
// section prints per pattern. The section is reserved out of the budget, so it
// has to be bounded, and the reader wants the shape rather than the census.
const maxLayerMappingLines = 12

// LLMContextRenderer produces a compact markdown summary optimized for LLM consumption.
type LLMContextRenderer struct {
	maxTokens int
}

// New creates a new LLMContextRenderer with the given token budget.
func New(maxTokens int) *LLMContextRenderer {
	if maxTokens <= 0 {
		maxTokens = 16000
	}
	return &LLMContextRenderer{maxTokens: maxTokens}
}

func (r *LLMContextRenderer) Name() string {
	return "llm_context"
}

// section holds a rendered section with its display name.
type section struct {
	name    string
	content string
	// reserve sets this section's budget aside before layout, so an oversized
	// earlier section cannot starve it.
	reserve bool
}

// cutAt returns the first n bytes of s, backed off to the nearest rune boundary.
// The token budget is counted in bytes but the content is UTF-8, so a naive slice
// can split a multi-byte character (a warning glyph, a non-ASCII identifier) and
// emit invalid UTF-8.
func cutAt(s string, n int) string {
	if n >= len(s) {
		return s
	}
	if n < 0 {
		n = 0
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// Render produces the llm_context.md artifact using progressive summarization.
// Sections are ordered by priority; lower-priority sections are omitted first
// when the token budget is tight.
func (r *LLMContextRenderer) Render(ctx context.Context, snapshot *facts.Snapshot) ([]facts.Artifact, error) {
	// Sections ordered by priority (most important first)
	sections := []section{
		{name: "Repository Map", content: r.renderRepoMap(snapshot)},
		// Extraction Quality is a compact trust-preface: it states how complete this
		// extraction was (thin extraction, parse errors, unresolved cross-repo edges)
		// before the reader relies on the graph. It is reserved rather than merely
		// placed high: Repository Map grows with the repo and, on a large multi-repo
		// snapshot, overran the whole budget — which truncated this section away on
		// exactly the clusters whose extraction is least complete.
		{name: "Extraction Quality", content: r.renderExtractionQuality(snapshot), reserve: true},
		// Reserved for the same reason Extraction Quality is, and discovered the same
		// way: Repository Map grows with the repository, and on a large one it spent
		// the entire budget before this section was reached. Two repositories in the
		// benchmark corpus rendered 64,000 characters of module list and not one word
		// of the architecture the snapshot had recognised — which is the single thing
		// this file exists to tell a reader. Its size is bounded below, so reserving
		// it costs a fixed and small amount.
		{name: "Architecture Pattern", content: r.renderArchPattern(snapshot), reserve: true},
		// Immediately after the statement, and reserved with it: the guide is a
		// rendering of the same layer order, and the two are read together.
		{name: "How to Add a Feature", content: r.renderFeatureGuide(snapshot), reserve: true},
		{name: "Cross-Repo Dependencies", content: r.renderCrossRepo(snapshot)},
		{name: "Entry Points", content: r.renderEntryPoints(snapshot)},
		{name: "Routes", content: r.renderRoutes(snapshot)},
		{name: "Storage", content: r.renderStorage(snapshot)},
		{name: "Dependency Rules", content: r.renderDependencyRules(snapshot)},
		{name: "Critical Modules", content: r.renderCriticalModules(snapshot)},
		{name: "Risk Zones", content: r.renderRiskZones(snapshot)},
		{name: "Meta", content: r.renderMeta(snapshot)},
	}

	header := "# Architecture Snapshot\n\n"
	maxChars := r.maxTokens * 4 // rough estimate: 1 token ~= 4 chars
	remaining := maxChars - len(header)

	// Set the reserved sections' budget aside before laying anything out, so the
	// running total the loop spends can never encroach on them. Sections are still
	// emitted in priority order; reserving buys survival, not precedence.
	for _, sec := range sections {
		if sec.reserve {
			remaining -= len(sec.content)
		}
	}

	var sb strings.Builder
	sb.WriteString(header)

	// Once the budget is spent, keep scanning: a later reserved section is still owed
	// the bytes held back for it.
	// NO UNRESERVED SECTION MAY TAKE MORE THAN ITS SHARE. Without this the first
	// oversized section spends the whole budget and every section after it is
	// omitted, which is how a repository of two thousand modules rendered a module
	// census and nothing else. Bounding the sections one at a time only moves the
	// problem down the list — the map was fixed, and Entry Points took the budget;
	// that was fixed, and Routes took it — because the cause is the allocation and
	// not any one section.
	//
	// A section that exceeds its share is truncated rather than dropped: half of
	// Routes plus Storage plus Risk Zones tells a reader more than all of Routes.
	// The share is generous enough that only a genuinely oversized section meets
	// it, so small documents lay out exactly as before.
	perSection := remaining
	if n := countUnreserved(sections); n > 1 {
		perSection = remaining * maxSectionSharePercent / 100
	}

	spent := false
	for i, sec := range sections {
		if sec.content == "" {
			continue
		}
		if sec.reserve {
			sb.WriteString(sec.content)
			continue
		}
		if spent {
			continue
		}

		budget := remaining
		if budget > perSection {
			budget = perSection
		}

		switch {
		case len(sec.content) <= budget:
			sb.WriteString(sec.content)
			remaining -= len(sec.content)
		case budget > 200:
			sb.WriteString(cutAt(sec.content, budget-100))
			fmt.Fprintf(&sb, "\n\n---\n*[Truncated in: %s]*\n", sec.name)
			remaining -= budget
			continue
		case remaining > 200:
			// Partially include this section
			sb.WriteString(cutAt(sec.content, remaining-100))
			fmt.Fprintf(&sb, "\n\n---\n*[Truncated in: %s]*\n", sec.name)
			spent = true
		default:
			// List omitted sections. Reserved ones are not omitted, so naming them
			// here would be a lie.
			var omitted []string
			for _, s := range sections[i:] {
				if s.content != "" && !s.reserve {
					omitted = append(omitted, s.name)
				}
			}
			fmt.Fprintf(&sb, "\n\n---\n*[Omitted: %s]*\n", strings.Join(omitted, ", "))
			spent = true
		}
	}

	return []facts.Artifact{
		{
			Name:    "llm_context.md",
			Content: []byte(sb.String()),
			Type:    "text/markdown",
		},
	}, nil
}

// maxSectionSharePercent is the largest share of the remaining budget one
// unreserved section may take. Calibrated so that the documents in the benchmark
// corpus lay out unchanged unless a section is genuinely oversized.
const maxSectionSharePercent = 35

// countUnreserved counts the sections competing for the shared budget.
func countUnreserved(sections []section) int {
	n := 0
	for _, sec := range sections {
		if !sec.reserve && sec.content != "" {
			n++
		}
	}
	return n
}

// maxTableRows is the row count above which a per-item table stops being a census
// and becomes a summary. It governs the repository map, the routes and the
// storage sections, which had the same shape and the same failure.
//
// The table is one row per module, sorted by NAME, and unbounded. On a repository
// of two thousand modules that is sixty thousand characters — the whole budget —
// and the truncation that followed cut it mid-table, so what an agent actually
// received was the alphabetically FIRST two thirds of a module census and no other
// section of the document. Alphabetical order carries no information about a
// codebase, which makes that the worst possible prefix to keep.
const maxTableRows = 60

// maxLargestModules caps the "largest modules" list in that summary.
const maxLargestModules = 20

func (r *LLMContextRenderer) renderRepoMap(snapshot *facts.Snapshot) string {
	var sb strings.Builder
	sb.WriteString("## Repository Map\n\n")

	modules := filterByKind(snapshot.Facts, facts.KindModule)
	if len(modules) == 0 {
		sb.WriteString("_No modules detected._\n\n")
		return sb.String()
	}

	// Group symbols by module
	symbolCounts := make(map[string]int)
	exportedCounts := make(map[string]int)
	for _, f := range snapshot.Facts {
		if f.Kind != facts.KindSymbol {
			continue
		}
		for _, rel := range f.Relations {
			if rel.Kind == facts.RelDeclares {
				symbolCounts[rel.Target]++
				if exported, ok := f.Props["exported"].(bool); ok && exported {
					exportedCounts[rel.Target]++
				}
			}
		}
	}

	// Two extractors can emit a fact for the same directory, and a map must not
	// list it twice.
	seen := make(map[string]bool, len(modules))
	unique := make([]facts.Fact, 0, len(modules))
	for _, m := range modules {
		key := m.Repo + "\x00" + m.Name
		if seen[key] {
			continue
		}
		seen[key] = true
		unique = append(unique, m)
	}

	sort.Slice(unique, func(i, j int) bool {
		if unique[i].Repo != unique[j].Repo {
			return unique[i].Repo < unique[j].Repo
		}
		return unique[i].Name < unique[j].Name
	})

	if len(unique) > maxTableRows {
		writeRepoMapSummary(&sb, unique, symbolCounts, multiRepo(unique))
		return sb.String()
	}

	sb.WriteString("| Module | Language | Symbols | Exported |\n")
	sb.WriteString("|--------|----------|---------|----------|\n")
	for _, mod := range unique {
		fmt.Fprintf(&sb, "| `%s` | %s | %d | %d |\n",
			mod.Name, moduleLanguage(mod), symbolCounts[mod.Name], exportedCounts[mod.Name])
	}
	sb.WriteString("\n")
	return sb.String()
}

// writeRepoMapSummary renders where the code LIVES rather than listing every
// module: one row per area — a repository's top-level directory, which is the
// grouping every layout in the corpus actually uses — and then the largest
// modules by symbol count.
//
// Size is the ordering because it is the one measure already computed here and
// the one a reader wants first: which parts of this tree hold the code. The full
// census remains a query away, and the line below says so rather than leaving a
// reader to wonder what was dropped.
func writeRepoMapSummary(sb *strings.Builder, modules []facts.Fact, symbolCounts map[string]int, labelled bool) {
	type area struct {
		name    string
		modules int
		symbols int
		langs   map[string]bool
	}
	byArea := map[string]*area{}
	var order []string
	totalSymbols := 0

	for _, m := range modules {
		key := areaOf(m, labelled)
		a := byArea[key]
		if a == nil {
			a = &area{name: key, langs: map[string]bool{}}
			byArea[key] = a
			order = append(order, key)
		}
		a.modules++
		a.symbols += symbolCounts[m.Name]
		totalSymbols += symbolCounts[m.Name]
		if l := moduleLanguage(m); l != "unknown" {
			a.langs[l] = true
		}
	}

	fmt.Fprintf(sb, "%d modules, %d symbols, grouped by area. Every module is in `facts.jsonl`, or query_facts(kind=\"module\").\n\n",
		len(modules), totalSymbols)

	sort.Slice(order, func(i, j int) bool {
		if byArea[order[i]].symbols != byArea[order[j]].symbols {
			return byArea[order[i]].symbols > byArea[order[j]].symbols
		}
		return order[i] < order[j]
	})

	sb.WriteString("| Area | Modules | Symbols | Languages |\n")
	sb.WriteString("|------|---------|---------|-----------|\n")
	for _, key := range order {
		a := byArea[key]
		langs := make([]string, 0, len(a.langs))
		for l := range a.langs {
			langs = append(langs, l)
		}
		sort.Strings(langs)
		if len(langs) == 0 {
			langs = []string{"unknown"}
		}
		fmt.Fprintf(sb, "| `%s` | %d | %d | %s |\n", a.name, a.modules, a.symbols, strings.Join(langs, ", "))
	}

	largest := append([]facts.Fact(nil), modules...)
	sort.Slice(largest, func(i, j int) bool {
		if symbolCounts[largest[i].Name] != symbolCounts[largest[j].Name] {
			return symbolCounts[largest[i].Name] > symbolCounts[largest[j].Name]
		}
		return largest[i].Name < largest[j].Name
	})
	if len(largest) > maxLargestModules {
		largest = largest[:maxLargestModules]
	}
	sb.WriteString("\nLargest modules:\n")
	for _, m := range largest {
		if symbolCounts[m.Name] == 0 {
			continue
		}
		fmt.Fprintf(sb, "- `%s` — %d symbols (%s)\n", m.Name, symbolCounts[m.Name], moduleLanguage(m))
	}
	sb.WriteString("\n")
}

// areaOf names the part of the tree a module belongs to: its first path segment,
// prefixed by the repository label only when the snapshot holds more than one.
// Repo is populated in single-repo snapshots too, so prefixing unconditionally
// put the same label in front of every row of a table that has nothing to
// distinguish.
func areaOf(m facts.Fact, labelled bool) string {
	name := m.Name
	if i := strings.Index(name, "/"); i > 0 {
		name = name[:i]
	}
	if labelled && m.Repo != "" {
		return m.Repo + "/" + name
	}
	return name
}

// multiRepo reports whether these modules come from more than one repository.
func multiRepo(modules []facts.Fact) bool {
	first, seen := "", false
	for _, m := range modules {
		if m.Repo == "" {
			continue
		}
		if !seen {
			first, seen = m.Repo, true
			continue
		}
		if m.Repo != first {
			return true
		}
	}
	return false
}

// moduleLanguage reads the language every extractor sets on a module fact.
func moduleLanguage(m facts.Fact) string {
	if l, ok := m.Props["language"].(string); ok && l != "" {
		return l
	}
	return "unknown"
}

func (r *LLMContextRenderer) renderArchPattern(snapshot *facts.Snapshot) string {
	var sb strings.Builder
	sb.WriteString("## Architecture Pattern\n\n")

	// Every architecture insight, not the first. A polyglot repository gets one
	// statement per language cohort — a Rails monolith shipping an Ember front
	// end is both — and returning at the first one showed the reader whichever
	// cohort happened to rank highest while hiding that the other half of the
	// repository had been described at all.
	found := false
	for _, insight := range snapshot.Insights {
		if !strings.HasPrefix(insight.Title, "Architecture pattern:") {
			continue
		}
		found = true
		fmt.Fprintf(&sb, "**%s** (confidence: %.0f%%)\n\n", insight.Title, insight.Confidence*100)
		sb.WriteString(insight.Description + "\n\n")

		// A SAMPLE of the layer mapping, not all of it. One evidence line per
		// classified module is a thousand lines on a large repository, and the
		// exhaustive list is available from query_insights; what belongs in a
		// budgeted summary is enough of it to see the shape.
		if len(insight.Evidence) > 0 {
			sb.WriteString("Layer mapping:\n")
			shown := insight.Evidence
			if len(shown) > maxLayerMappingLines {
				shown = shown[:maxLayerMappingLines]
			}
			for _, ev := range shown {
				fmt.Fprintf(&sb, "- %s\n", ev.Detail)
			}
			if omitted := len(insight.Evidence) - len(shown); omitted > 0 {
				fmt.Fprintf(&sb, "- … and %d more (query_insights(explainer=\"layers\") for all)\n", omitted)
			}
			sb.WriteString("\n")
		}
	}
	if !found {
		sb.WriteString("_No specific architecture pattern detected._\n\n")
	}
	return sb.String()
}

// renderCrossRepo surfaces the cross-repo "graph of graphs": one row per
// consumer→provider dependency synthesized by the crossrepo linker. It renders
// regardless of which explainers are enabled, so multi-repo dependencies are
// always visible in the context an agent reads. Returns "" when there are none
// (i.e. single-repo snapshots).
func (r *LLMContextRenderer) renderCrossRepo(snapshot *facts.Snapshot) string {
	var edges []facts.Fact
	for _, f := range snapshot.Facts {
		if f.Kind == facts.KindDependency && propStr(f, "type") == "cross_repo" {
			edges = append(edges, f)
		}
	}
	if len(edges) == 0 {
		return ""
	}

	sort.Slice(edges, func(i, j int) bool { return edges[i].Name < edges[j].Name })

	var sb strings.Builder
	sb.WriteString("## Cross-Repo Dependencies\n\n")
	sb.WriteString("How requests and code flow between repositories. Traverse from a repo label " +
		"(service node) to follow these edges.\n\n")
	sb.WriteString("| Consumer | Provider | Via | Detail |\n")
	sb.WriteString("|----------|----------|-----|--------|\n")
	for _, e := range edges {
		consumer := e.Repo
		provider := consumer
		for _, rel := range e.Relations {
			if rel.Kind == facts.RelDependsOn {
				provider = rel.Target
			}
		}
		// The provider also lives in the edge name ("consumer -> provider") for
		// facts loaded from JSONL where relations may be absent.
		if provider == consumer {
			if i := strings.Index(e.Name, " -> "); i >= 0 {
				provider = e.Name[i+4:]
			}
		}
		via := strings.Join(propStrSlice(e, "via"), "+")
		fmt.Fprintf(&sb, "| `%s` | `%s` | %s | %s |\n", consumer, provider, via, crossRepoDetail(e))
	}
	sb.WriteString("\n")
	return sb.String()
}

// crossRepoDetail renders the evidence behind an edge: endpoint/import counts
// plus a few samples.
func crossRepoDetail(e facts.Fact) string {
	var parts []string
	if n := propInt(e, "endpoint_count"); n > 0 {
		parts = append(parts, fmt.Sprintf("%d endpoint(s): %s", n, samplePreview(propStrSlice(e, "endpoints"))))
	}
	if n := propInt(e, "import_count"); n > 0 {
		parts = append(parts, fmt.Sprintf("%d import(s): %s", n, samplePreview(propStrSlice(e, "import_samples"))))
	}
	if n := propInt(e, "symbol_count"); n > 0 {
		parts = append(parts, fmt.Sprintf("%d shared symbol(s): %s", n, samplePreview(propStrSlice(e, "symbol_samples"))))
	}
	if len(parts) == 0 {
		return "cross-repo dependency"
	}
	return strings.Join(parts, "; ")
}

// samplePreview joins up to three samples, appending "…" when truncated.
func samplePreview(ss []string) string {
	if len(ss) <= 3 {
		return strings.Join(ss, ", ")
	}
	return strings.Join(ss[:3], ", ") + ", …"
}

func (r *LLMContextRenderer) renderEntryPoints(snapshot *facts.Snapshot) string {
	var sb strings.Builder
	sb.WriteString("## Entry Points\n\n")

	var entryPoints []string

	for _, f := range snapshot.Facts {
		if f.Kind != facts.KindSymbol {
			continue
		}

		symbolKind, _ := f.Props["symbol_kind"].(string)
		exported, _ := f.Props["exported"].(bool)

		// Main functions
		if strings.HasSuffix(f.Name, ".main") && symbolKind == facts.SymbolFunc {
			entryPoints = append(entryPoints, fmt.Sprintf("- **main**: `%s` (%s)", f.Name, f.File))
		}

		// HTTP handlers (common patterns)
		if exported && symbolKind == facts.SymbolFunc {
			nameLower := strings.ToLower(f.Name)
			if strings.Contains(nameLower, "handler") || strings.Contains(nameLower, "handle") ||
				strings.Contains(nameLower, "serve") {
				entryPoints = append(entryPoints, fmt.Sprintf("- **handler**: `%s` (%s)", f.Name, f.File))
			}
		}

		// iOS/macOS app entry point (@main struct conforming to App)
		if iosComp, _ := f.Props["ios_component"].(string); iosComp == "swiftui_app" {
			entryPoints = append(entryPoints, fmt.Sprintf("- **app**: `%s` (%s)", f.Name, f.File))
		}
	}

	// Routes as entry points
	routes := filterByKind(snapshot.Facts, facts.KindRoute)
	for _, route := range routes {
		method, _ := route.Props["method"].(string)
		entryPoints = append(entryPoints, fmt.Sprintf("- **route** %s `%s` (%s)", method, route.Name, route.File))
	}

	if len(entryPoints) == 0 {
		sb.WriteString("_No entry points detected._\n\n")
		return sb.String()
	}

	// Bounded and grouped, for the reason the repository map is. This was one
	// alphabetically sorted line per entry point with no cap, and an application
	// with nine hundred routes rendered nine hundred lines — which spent whatever
	// budget the map had left and truncated mid-list, so what survived was the
	// routes whose paths sort first.
	//
	// The kinds are not equivalent and are not capped alike. A `main` or an app
	// entry point is the handful of places execution actually begins, and all of
	// them are listed; routes and handlers are a population, where a sample plus
	// the total says more than a prefix does.
	byKind := map[string][]string{}
	var kinds []string
	for _, ep := range entryPoints {
		kind := entryPointKind(ep)
		if _, ok := byKind[kind]; !ok {
			kinds = append(kinds, kind)
		}
		byKind[kind] = append(byKind[kind], ep)
	}
	sort.Strings(kinds)

	for _, kind := range kinds {
		eps := byKind[kind]
		sort.Strings(eps)
		shown := eps
		if kind != "main" && kind != "app" && len(shown) > maxEntryPointSamples {
			shown = shown[:maxEntryPointSamples]
		}
		if omitted := len(eps) - len(shown); omitted > 0 {
			fmt.Fprintf(&sb, "%d %ss, %d shown:\n", len(eps), kind, len(shown))
		}
		for _, ep := range shown {
			sb.WriteString(ep + "\n")
		}
		if omitted := len(eps) - len(shown); omitted > 0 {
			fmt.Fprintf(&sb, "- … and %d more (query_facts(kind=\"route\") for all)\n", omitted)
		}
	}
	sb.WriteString("\n")
	return sb.String()
}

// maxEntryPointSamples caps how many entry points of one POPULATION kind — routes,
// handlers — the summary lists. Kinds that name where execution begins are listed
// in full.
const maxEntryPointSamples = 15

// entryPointKind reads back the kind from a rendered entry-point line, which is
// written as "- **kind**: ..." or "- **kind** METHOD ...".
func entryPointKind(line string) string {
	const marker = "- **"
	if !strings.HasPrefix(line, marker) {
		return "other"
	}
	rest := line[len(marker):]
	i := strings.Index(rest, "**")
	if i < 0 {
		return "other"
	}
	return rest[:i]
}

func (r *LLMContextRenderer) renderRoutes(snapshot *facts.Snapshot) string {
	routes := filterByKind(snapshot.Facts, facts.KindRoute)
	if len(routes) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("## Routes\n\n")

	sort.Slice(routes, func(i, j int) bool {
		if routes[i].Name != routes[j].Name {
			return routes[i].Name < routes[j].Name
		}
		return routes[i].File < routes[j].File
	})

	if len(routes) > maxTableRows {
		writeRouteSummary(&sb, routes)
		return sb.String()
	}

	sb.WriteString("| Method | Path | File | Type |\n")
	sb.WriteString("|--------|------|------|------|\n")
	for _, route := range routes {
		method, _ := route.Props["method"].(string)
		routeType, _ := route.Props["type"].(string)
		fmt.Fprintf(&sb, "| %s | `%s` | `%s` | %s |\n", method, route.Name, route.File, routeType)
	}
	sb.WriteString("\n")
	return sb.String()
}

// writeRouteSummary groups routes by the first segment of their path, which is
// how an HTTP surface is actually divided — /api against /admin against the rest
// — and is the only grouping available without asking the router.
//
// One example path per prefix, because a count alone does not tell a reader what
// the prefix serves and a full listing is what this replaces.
func writeRouteSummary(sb *strings.Builder, routes []facts.Fact) {
	type group struct {
		prefix  string
		count   int
		methods map[string]bool
		example string
	}
	byPrefix := map[string]*group{}
	var order []string
	for _, rt := range routes {
		key := pathPrefix(rt.Name)
		g := byPrefix[key]
		if g == nil {
			g = &group{prefix: key, methods: map[string]bool{}, example: rt.Name}
			byPrefix[key] = g
			order = append(order, key)
		}
		g.count++
		if m, _ := rt.Props["method"].(string); m != "" {
			g.methods[m] = true
		}
	}

	fmt.Fprintf(sb, "%d routes, grouped by path prefix. query_facts(kind=\"route\") for all of them.\n\n", len(routes))
	sort.Slice(order, func(i, j int) bool {
		if byPrefix[order[i]].count != byPrefix[order[j]].count {
			return byPrefix[order[i]].count > byPrefix[order[j]].count
		}
		return order[i] < order[j]
	})
	if len(order) > maxTableRows {
		order = order[:maxTableRows]
	}

	sb.WriteString("| Prefix | Routes | Methods | Example |\n")
	sb.WriteString("|--------|--------|---------|----------|\n")
	for _, key := range order {
		g := byPrefix[key]
		methods := make([]string, 0, len(g.methods))
		for m := range g.methods {
			methods = append(methods, m)
		}
		sort.Strings(methods)
		fmt.Fprintf(sb, "| `%s` | %d | %s | `%s` |\n", g.prefix, g.count, strings.Join(methods, ", "), g.example)
	}
	sb.WriteString("\n")
}

// pathPrefix returns the first segment of a route path, with the leading slash
// kept so the value reads as a path. A root route is its own group.
func pathPrefix(name string) string {
	trimmed := strings.TrimPrefix(name, "/")
	if trimmed == "" {
		return "/"
	}
	if i := strings.Index(trimmed, "/"); i > 0 {
		return "/" + trimmed[:i]
	}
	return "/" + trimmed
}

func (r *LLMContextRenderer) renderStorage(snapshot *facts.Snapshot) string {
	storage := filterByKind(snapshot.Facts, facts.KindStorage)
	if len(storage) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("## Storage\n\n")

	sort.Slice(storage, func(i, j int) bool {
		if storage[i].Name != storage[j].Name {
			return storage[i].Name < storage[j].Name
		}
		return storage[i].File < storage[j].File
	})

	if len(storage) > maxTableRows {
		writeStorageSummary(&sb, storage)
		return sb.String()
	}

	sb.WriteString("| Name | Kind | Operation | File |\n")
	sb.WriteString("|------|------|-----------|------|\n")
	for _, s := range storage {
		storageKind, _ := s.Props["storage_kind"].(string)
		operation, _ := s.Props["operation"].(string)
		fmt.Fprintf(&sb, "| `%s` | %s | %s | `%s` |\n",
			s.Name, storageKind, operation, s.File)
	}
	sb.WriteString("\n")
	return sb.String()
}

// writeStorageSummary groups data stores by their KIND — a Postgres table, an
// ActiveRecord model, a Kafka topic and an S3 bucket are all stores the code
// names, and the discriminator is the thing a reader sorts them by first — and
// then names as many as fit.
func writeStorageSummary(sb *strings.Builder, storage []facts.Fact) {
	byKind := map[string][]string{}
	var kinds []string
	for _, st := range storage {
		kind, _ := st.Props["storage_kind"].(string)
		if kind == "" {
			kind = "unknown"
		}
		if _, ok := byKind[kind]; !ok {
			kinds = append(kinds, kind)
		}
		byKind[kind] = append(byKind[kind], st.Name)
	}
	sort.Strings(kinds)

	fmt.Fprintf(sb, "%d data stores. query_facts(kind=\"storage\") for all of them.\n\n", len(storage))
	for _, kind := range kinds {
		names := dedupeSorted(byKind[kind])
		shown := names
		if len(shown) > maxStorageNames {
			shown = shown[:maxStorageNames]
		}
		fmt.Fprintf(sb, "- **%s** (%d): `%s`", kind, len(names), strings.Join(shown, "`, `"))
		if omitted := len(names) - len(shown); omitted > 0 {
			fmt.Fprintf(sb, " … and %d more", omitted)
		}
		sb.WriteString("\n")
	}
	sb.WriteString("\n")
}

// maxStorageNames caps how many store names are listed per kind.
const maxStorageNames = 25

// dedupeSorted removes repeats from an already-sorted slice. A model read in
// several files is one store.
func dedupeSorted(in []string) []string {
	out := in[:0:0]
	for i, v := range in {
		if i > 0 && v == in[i-1] {
			continue
		}
		out = append(out, v)
	}
	return out
}

func (r *LLMContextRenderer) renderDependencyRules(snapshot *facts.Snapshot) string {
	var sb strings.Builder
	sb.WriteString("## Dependency Rules\n\n")

	// Collect unique module-to-module internal dependencies
	type depEdge struct{ from, to string }
	seen := make(map[depEdge]bool)

	deps := filterByKind(snapshot.Facts, facts.KindDependency)
	modules := make(map[string]bool)
	for _, f := range snapshot.Facts {
		if f.Kind == facts.KindModule {
			modules[f.Name] = true
		}
	}

	var edges []string
	for _, dep := range deps {
		sourceModule := fileDir(dep.File)
		for _, rel := range dep.Relations {
			if rel.Kind != facts.RelImports {
				continue
			}
			if !modules[rel.Target] {
				continue
			}
			edge := depEdge{sourceModule, rel.Target}
			if !seen[edge] {
				seen[edge] = true
				edges = append(edges, fmt.Sprintf("- `%s` -> `%s`", edge.from, edge.to))
			}
		}
	}

	if len(edges) == 0 {
		sb.WriteString("_No internal dependency rules detected._\n\n")
		return sb.String()
	}

	if len(edges) > maxTableRows {
		// The raw edge list is what `traverse` and query_facts serve, and it is the
		// least summarisable thing in this document: on a large monorepo it is
		// thousands of alphabetically sorted lines. What a reader can act on is
		// which modules reach furthest, which no other section reports — Critical
		// Modules ranks fan-IN, and this is its opposite.
		outDegree := map[string]int{}
		for e := range seen {
			outDegree[e.from]++
		}
		names := make([]string, 0, len(outDegree))
		for name := range outDegree {
			names = append(names, name)
		}
		sort.Slice(names, func(i, j int) bool {
			if outDegree[names[i]] != outDegree[names[j]] {
				return outDegree[names[i]] > outDegree[names[j]]
			}
			return names[i] < names[j]
		})
		if len(names) > maxTableRows {
			names = names[:maxTableRows]
		}
		fmt.Fprintf(&sb, "%d internal module dependencies. The modules that reach furthest; traverse() or query_facts(kind=\"dependency\") for the edges themselves.\n\n", len(edges))
		sb.WriteString("| Module | Depends on |\n")
		sb.WriteString("|--------|------------|\n")
		for _, name := range names {
			fmt.Fprintf(&sb, "| `%s` | %d modules |\n", name, outDegree[name])
		}
		sb.WriteString("\n")
		return sb.String()
	}

	sort.Strings(edges)
	for _, e := range edges {
		sb.WriteString(e + "\n")
	}
	sb.WriteString("\n")
	return sb.String()
}

func (r *LLMContextRenderer) renderCriticalModules(snapshot *facts.Snapshot) string {
	var sb strings.Builder
	sb.WriteString("## Critical Modules\n\n")

	// Compute fan-in (imported by others) and fan-out (imports others)
	fanIn := make(map[string]int)
	fanOut := make(map[string]int)

	modules := make(map[string]bool)
	for _, f := range snapshot.Facts {
		if f.Kind == facts.KindModule {
			modules[f.Name] = true
		}
	}

	deps := filterByKind(snapshot.Facts, facts.KindDependency)
	for _, dep := range deps {
		sourceModule := fileDir(dep.File)
		for _, rel := range dep.Relations {
			if rel.Kind == facts.RelImports && modules[rel.Target] {
				fanOut[sourceModule]++
				fanIn[rel.Target]++
			}
		}
	}

	type modScore struct {
		Name   string
		FanIn  int
		FanOut int
		Score  int
	}

	var scored []modScore
	for mod := range modules {
		s := modScore{
			Name:   mod,
			FanIn:  fanIn[mod],
			FanOut: fanOut[mod],
			Score:  fanIn[mod] + fanOut[mod],
		}
		if s.Score > 0 {
			scored = append(scored, s)
		}
	}

	// Candidates are collected out of a map, so without the name key equal scores
	// order — and at the top-10 cut, are selected — by map iteration, and two runs
	// over one repository render different tables.
	sort.Slice(scored, func(i, j int) bool {
		if scored[i].Score != scored[j].Score {
			return scored[i].Score > scored[j].Score
		}
		return scored[i].Name < scored[j].Name
	})

	// Show top 10
	limit := 10
	if len(scored) < limit {
		limit = len(scored)
	}

	if limit == 0 {
		sb.WriteString("_No cross-module dependencies detected._\n\n")
		return sb.String()
	}

	sb.WriteString("| Module | Fan-In | Fan-Out | Criticality |\n")
	sb.WriteString("|--------|--------|---------|-------------|\n")
	for _, s := range scored[:limit] {
		criticality := "low"
		if s.Score >= 10 {
			criticality = "high"
		} else if s.Score >= 5 {
			criticality = "medium"
		}
		fmt.Fprintf(&sb, "| `%s` | %d | %d | %s |\n", s.Name, s.FanIn, s.FanOut, criticality)
	}
	sb.WriteString("\n")
	return sb.String()
}

func (r *LLMContextRenderer) renderRiskZones(snapshot *facts.Snapshot) string {
	type risk struct {
		line       string
		confidence float64
	}
	var risks []risk

	for _, insight := range snapshot.Insights {
		if strings.Contains(insight.Title, "Cyclic dependency") ||
			strings.Contains(insight.Title, "Layer violation") ||
			strings.Contains(insight.Title, "Coverage gap") ||
			strings.Contains(insight.Title, "Partial coverage") {
			risks = append(risks, risk{
				line: fmt.Sprintf("- **%s** (confidence: %.0f%%): %s",
					insight.Title, insight.Confidence*100, insight.Description),
				confidence: insight.Confidence,
			})
		}
	}

	if len(risks) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("## Risk Zones\n\n")
	// Highest confidence first and capped. Every finding here carries a full
	// description, so an application with hundreds of them is tens of thousands of
	// characters — and taking them in the order the explainers happened to emit
	// them meant a truncation kept whichever ones came first rather than the ones
	// most likely to be real.
	sort.SliceStable(risks, func(i, j int) bool { return risks[i].confidence > risks[j].confidence })
	shown := risks
	if len(shown) > maxRiskZones {
		shown = shown[:maxRiskZones]
	}
	if omitted := len(risks) - len(shown); omitted > 0 {
		fmt.Fprintf(&sb, "%d findings, the %d most confident shown. query_insights() for all.\n\n", len(risks), len(shown))
	}
	for _, risk := range shown {
		sb.WriteString(risk.line + "\n")
	}
	sb.WriteString("\n")
	return sb.String()
}

// maxRiskZones caps how many cycle, layer and coverage findings the summary
// carries. Each one is a full sentence of description.
const maxRiskZones = 25

// renderFeatureGuide writes where a new feature goes, DERIVED from the layer order
// the snapshot recognised rather than authored per taxonomy.
//
// It reads that order out of the pattern insight's metrics rather than looking the
// taxonomy up by name, which is what lets it serve a repository that DECLARES its
// architecture: a declared order lives in the repository's own intent facts, so
// there is nothing in the built-in taxonomies to look up, and enola's own tree —
// which declares six layers — got no guide at all.
//
// It used to be a switch over three pattern names with hand-written prose and a
// generic fallback for everything else, which is what most repositories got.
//
// One guide per language cohort, because a polyglot repository has one layer order
// per cohort and a reader adding a feature is working in one of them.
func (r *LLMContextRenderer) renderFeatureGuide(snapshot *facts.Snapshot) string {
	var sb strings.Builder
	sb.WriteString("## How to Add a Feature\n\n")

	written := false
	for _, insight := range snapshot.Insights {
		if !strings.HasPrefix(insight.Title, "Architecture pattern:") {
			continue
		}
		ordered := insight.MetricStrings(layers.MetricLayersOrdered)
		if len(ordered) == 0 {
			continue
		}
		examples := insight.MetricStringMap(layers.MetricLayerExamples)
		levels := insight.Metrics[layers.MetricLayerLevels]

		name := strings.TrimPrefix(insight.Title, "Architecture pattern: ")
		if written {
			sb.WriteString("\n")
		}
		written = true

		fmt.Fprintf(&sb, "This code is laid out as **%s**. A dependency runs from an outer layer to an inner one, so a feature is built inward:\n\n", name)

		// GROUPED BY LEVEL, because layers sharing one are PEERS and not steps.
		// Several taxonomies deliberately collapse many directories onto a single
		// tier — the Rails one puts models, services, jobs, mailers, policies and a
		// dozen more at the same level precisely because no order holds between
		// them — and numbering them one per line asserted an eighteen-step sequence
		// that does not exist.
		//
		// A layer with no example is a layer this repository does not have. Listing
		// it says only that the taxonomy has a word for something absent here.
		n := 0
		for i := 0; i < len(ordered); {
			if examples[ordered[i]] == "" {
				i++
				continue
			}
			level, hasLevel := layerLevel(levels, ordered[i])
			example := examples[ordered[i]]
			var names []string
			for ; i < len(ordered); i++ {
				if examples[ordered[i]] == "" {
					continue
				}
				if l, ok := layerLevel(levels, ordered[i]); hasLevel != ok || l != level {
					break
				}
				names = append(names, ordered[i])
			}
			if len(names) == 0 {
				continue
			}
			n++
			fmt.Fprintf(&sb, "%d. **%s** — e.g. `%s`\n", n, strings.Join(names, ", "), example)
		}

		// Naming the unordered layers matters as much as ordering the rest: they
		// are where wiring goes, and a reader who does not know they are exempt
		// will try to place them in the order.
		var neutral []string
		for _, layer := range insight.MetricStrings(layers.MetricLayersUnordered) {
			if examples[layer] != "" {
				neutral = append(neutral, layer)
			}
		}
		if len(neutral) > 0 {
			fmt.Fprintf(&sb, "\nOutside that order: %s — classified, but in no dependency direction, so nothing that touches them is a layer violation.\n",
				strings.Join(neutral, ", "))
		}
	}

	// Nothing recognised, nothing to say. The generic five-step advice this used to
	// print — identify the module, follow existing patterns, keep dependencies
	// flowing one way — is true of every codebase ever written, which is what makes
	// it worthless: it is not derived from this snapshot and it costs budget that a
	// section with real content could spend. An empty section is dropped.
	if !written {
		return ""
	}

	sb.WriteString("\n")
	return sb.String()
}

// layerLevel reads one layer's rank out of the metrics map, which arrives as
// map[string]any either way and carries its numbers as float64 once a snapshot has
// been through JSON.
func layerLevel(levels any, name string) (int, bool) {
	m, ok := levels.(map[string]any)
	if !ok {
		return 0, false
	}
	switch v := m[name].(type) {
	case int:
		return v, true
	case float64:
		return int(v), true
	}
	return 0, false
}

// renderExtractionQuality surfaces how complete the extraction was, so an agent
// reading the snapshot can SEE thin extraction (a bad ignore glob, a failing
// extractor, unresolved cross-repo edges) without calling snapshot_receipt — the
// in-loop signal for improving enola's own coverage. It emphasizes genuine
// signals (parse errors, coverage gaps, unresolved edges) and stays quiet when
// extraction is clean.
func (r *LLMContextRenderer) renderExtractionQuality(snapshot *facts.Snapshot) string {
	m := snapshot.Meta
	// Nothing meaningful to report on an old/auto-loaded snapshot with no receipt.
	if m.FilesSeen == 0 && m.ParseErrors == 0 && m.Coverage == nil {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("## Extraction Quality\n\n")
	fmt.Fprintf(&sb, "- Files parsed: **%d** / %d seen (%d file(s) + %d directory tree(s) skipped by ignore globs)\n",
		m.FilesParsed, m.FilesSeen, m.FilesSkipped, m.DirsSkipped)

	if m.ParseErrors > 0 {
		fmt.Fprintf(&sb, "- ⚠️ Parse errors: **%d** — some sources failed to extract; the graph may be missing symbols here\n", m.ParseErrors)
	} else {
		sb.WriteString("- Parse errors: 0\n")
	}

	// Gaps and unresolved edges are independent: a connected service has resolved an
	// outbound edge by definition, yet may still have call sites that resolved to no
	// loaded repo. Guarding the count on the gap total hid it on exactly the healthy
	// multi-repo snapshot an agent is most likely to trust. External edges are a third
	// signal — expected, not a blind spot — so they are reported without a warning.
	if m.Coverage != nil {
		if m.Coverage.CoverageGaps > 0 {
			fmt.Fprintf(&sb, "- ⚠️ Cross-repo coverage gaps: **%d** service(s) with no resolved outbound edges — their outbound links could not be resolved to a loaded repo\n",
				m.Coverage.CoverageGaps)
		}
		if m.Coverage.UnresolvedEdges > 0 {
			fmt.Fprintf(&sb, "- ⚠️ Unresolved outbound edges: **%d** — call site(s) that did not resolve to a loaded repo (an unloaded repo, or an extractor blind spot)\n",
				m.Coverage.UnresolvedEdges)
		}
		if m.Coverage.ExternalEdges > 0 {
			fmt.Fprintf(&sb, "- Outbound edges to external hosts: **%d** — third-party APIs, expected (not a coverage blind spot)\n",
				m.Coverage.ExternalEdges)
		}
	}

	if m.ParseErrors > 0 || (m.Coverage != nil && (m.Coverage.CoverageGaps > 0 || m.Coverage.UnresolvedEdges > 0)) {
		sb.WriteString("\n_These are extraction limits, not code defects — verify against source, and consider whether an extractor, detection, or ignore glob needs improving._\n")
	}

	// A single-repo snapshot has no service nodes (Coverage is nil), so the cross-repo
	// linker never ran and the Cross-Repo Dependencies section below renders empty —
	// byte-identical to a fully-resolved cluster's. State the difference here so its
	// silence is not read as "no unused routes / no coverage gaps".
	if m.Coverage == nil {
		sb.WriteString("- Cross-repo analysis: not run (single-repo snapshot — append client/backend repos to enable unused-routes and coverage)\n")
	}

	sb.WriteString("\n")
	return sb.String()
}

func (r *LLMContextRenderer) renderMeta(snapshot *facts.Snapshot) string {
	var sb strings.Builder
	sb.WriteString("---\n\n")
	fmt.Fprintf(&sb, "*Generated at %s in %s. %d facts, %d insights.*\n",
		snapshot.Meta.GeneratedAt, snapshot.Meta.Duration,
		snapshot.Meta.FactCount, snapshot.Meta.InsightCount)
	return sb.String()
}

func propStr(f facts.Fact, key string) string {
	if f.Props == nil {
		return ""
	}
	s, _ := f.Props[key].(string)
	return s
}

// propInt reads an int-valued prop, tolerating the float64 form produced by a
// JSON (facts.jsonl) round-trip.
func propInt(f facts.Fact, key string) int {
	if f.Props == nil {
		return 0
	}
	switch v := f.Props[key].(type) {
	case int:
		return v
	case float64:
		return int(v)
	}
	return 0
}

// propStrSlice reads a string-slice prop, tolerating the []interface{} form
// produced by a JSON round-trip.
func propStrSlice(f facts.Fact, key string) []string {
	if f.Props == nil {
		return nil
	}
	switch v := f.Props[key].(type) {
	case []string:
		return v
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, e := range v {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func filterByKind(ff []facts.Fact, kind string) []facts.Fact {
	var result []facts.Fact
	for _, f := range ff {
		if f.Kind == kind {
			result = append(result, f)
		}
	}
	return result
}

func fileDir(file string) string {
	parts := strings.Split(file, "/")
	if len(parts) <= 1 {
		return "."
	}
	return strings.Join(parts[:len(parts)-1], "/")
}
