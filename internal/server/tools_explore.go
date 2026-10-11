package server

// The explore tool: one focus, answered as a module, a file, a directory or a
// symbol.

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/resolve"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// registerExploreTool registers the explore tool.
func (s *Server) registerExploreTool() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "explore",
		Description: "Primary exploration tool, the first call after generate_snapshot. " +
			"For a module, file path, directory prefix or symbol it returns markdown: symbols (kinds, line numbers), direct dependencies and reverse dependents. " +
			"A module is a package-level grouping (a Go package, a TypeScript file group), not a repo. Absolute paths are accepted. " +
			"Use query_facts for precise filtering, traverse for multi-hop walks.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args exploreArgs) (*mcp.CallToolResult, any, error) {
		store := s.eng.Store()
		if store.Count() == 0 {
			return errorResult("No facts available. Run generate_snapshot first."), nil, nil
		}

		if args.Focus == "" {
			return errorResult("focus is required"), nil, nil
		}

		depth := args.Depth
		if depth <= 0 {
			depth = 1
		}
		if depth > 2 {
			depth = 2
		}

		var sb strings.Builder

		// Normalize absolute filesystem paths to store-relative paths.
		focus := s.normalizeToRelative(args.Focus)

		// Try to determine focus type by matching against store indexes.
		// Priority: exact module name > exact file > symbol name substring > file prefix (directory)
		// Special case: "." means the repo root (from normalizing an absolute path that
		// equals the snapshot RepoPath). Route directly to directory exploration to avoid
		// "." accidentally substring-matching dotted symbol names.
		// At depth=2 the default 'summary' mode replaces the raw per-symbol relations
		// dump with an aggregated Insights section. compact/full keep the dump.
		mode := resolveOutputMode(args.OutputMode, modeSummary)

		switch {
		case focus == "." && s.exploreDirectory(store, focus, &sb):
		case focus != "." && s.exploreModule(store, focus, depth, mode, &sb):
		case focus != "." && s.exploreModuleSubstring(store, focus, depth, mode, &sb):
		case focus != "." && s.exploreFile(store, focus, depth, &sb):
		case focus != "." && s.exploreSymbol(store, focus, depth, &sb):
		case s.exploreDirectory(store, focus, &sb):
		default:
			return errorResult(fmt.Sprintf("No facts matching focus %q. Try a module name, file path, symbol name, or directory prefix.", focus)), nil, nil
		}

		return textResult(capTokens(sb.String(), args.MaxTokens, false)), nil, nil
	})
}

// exploreArgs are the arguments for the explore tool.
type exploreArgs struct {
	Focus      string `json:"focus" jsonschema:"Module name, file path, directory prefix or symbol name."`
	Depth      int    `json:"depth,omitempty" jsonschema:"1 for direct relations (default), 2 to include relations of relations. Max 2."`
	OutputMode string `json:"output_mode,omitempty" jsonschema:"Only at depth=2. 'summary' (default) returns aggregated insights (dependency hotspots, cycle and layer warnings, size metrics): what is architecturally significant. 'compact' or 'full' list per-symbol relations instead."`
	MaxTokens  int    `json:"max_tokens,omitempty" jsonschema:"Approximate token cap; output is truncated with a notice. Default: no cap."`
}

// exploreModule renders a module exploration if the focus matches a module name.
// sameRepo keeps the facts that belong to repo. Graph lookups are keyed on names, and a
// module name such as internal/svc recurs across the repositories of a multi-repo
// store, so a reverse lookup returns every repository's declarers and importers. A
// fact with no repo is kept, and an empty repo keeps everything.
func sameRepo(fs []facts.Fact, repo string) []facts.Fact {
	if repo == "" {
		return fs
	}
	out := fs[:0:0]
	for _, f := range fs {
		if f.Repo == "" || f.Repo == repo {
			out = append(out, f)
		}
	}
	return out
}

// lookupModule finds the module fact focus names. A focus that starts with a repo
// label, the form every file path in a multi-repo store takes, names that repo's
// module without the label; module names never carry it. Without this the path fell
// through to the file match, where the only fact filed under a directory is its own
// module, and explore answered "Total facts: 1".
func (s *Server) lookupModule(store *facts.Store, focus string) *facts.Fact {
	// find returns the first module fact named name, from repo when repo is set.
	find := func(name, repo string) *facts.Fact {
		for _, f := range store.LookupByExactName(name) {
			if f.Kind == facts.KindModule && (repo == "" || f.Repo == repo) {
				m := f
				return &m
			}
		}
		return nil
	}
	if mod := find(focus, ""); mod != nil {
		return mod
	}
	for _, label := range s.repoLabels() {
		if rest, ok := strings.CutPrefix(focus, label+"/"); ok && rest != "" {
			if mod := find(rest, label); mod != nil {
				return mod
			}
		}
	}
	return nil
}

func (s *Server) exploreModule(store *facts.Store, focus string, depth int, mode string, sb *strings.Builder) bool {
	mod := s.lookupModule(store, focus)
	if mod == nil {
		return false
	}

	sb.WriteString(fmt.Sprintf("# Module: %s\n\n", mod.Name))

	// Props summary
	if lang, ok := mod.PropAny("language").(string); ok {
		sb.WriteString(fmt.Sprintf("- Language: %s\n", lang))
	}
	if pkg, ok := mod.PropAny("package").(string); ok {
		sb.WriteString(fmt.Sprintf("- Package: %s\n", pkg))
	}
	sb.WriteString("\n")

	// Find symbols declared in this module (symbols whose "declares" relation targets this module)
	declaredSymbols := sameRepo(store.ReverseLookup(mod.Name, facts.RelDeclares), mod.Repo)
	slices.SortFunc(declaredSymbols, bySourcePosition)
	if len(declaredSymbols) > 0 {
		sb.WriteString(fmt.Sprintf("## Symbols (%d)\n\n", len(declaredSymbols)))
		sb.WriteString("| Name | Kind | File | Line | Exported |\n")
		sb.WriteString("|------|------|------|------|----------|\n")
		for _, sym := range declaredSymbols {
			symKind, _ := sym.PropAny("symbol_kind").(string)
			exported := "no"
			if exp, ok := sym.PropAny("exported").(bool); ok && exp {
				exported = "yes"
			}
			sb.WriteString(fmt.Sprintf("| %s | %s | %s | %d | %s |\n",
				sym.Name, symKind, sym.File, sym.Line, exported))
		}
		sb.WriteString("\n")
	}

	// Dependencies: facts with kind=dependency whose file starts with the module path,
	// plus direct depends_on relations from the module fact itself (packwerk).
	deps := allMatching(store, facts.QueryOpts{Kind: facts.KindDependency, FilePrefix: mod.Name + "/"})
	// In a multi-repo store a file path carries its repo label and a module name does
	// not, so the name alone matched none of the module's files and the section was
	// silently empty. The module's own directory carries the label.
	if mod.File != "" && mod.File != mod.Name {
		deps = append(deps, allMatching(store, facts.QueryOpts{Kind: facts.KindDependency, FilePrefix: mod.File + "/"})...)
	}
	// Collect all dependency targets grouped by relation kind.
	depsByKind := make(map[string][]string) // relKind → targets
	seen := make(map[string]struct{})
	for _, dep := range deps {
		for _, r := range dep.Relations {
			key := r.Kind + ":" + r.Target
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			depsByKind[r.Kind] = append(depsByKind[r.Kind], r.Target)
		}
	}
	// Also include the module's own depends_on relations (from packwerk).
	for _, r := range mod.Relations {
		key := r.Kind + ":" + r.Target
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		depsByKind[r.Kind] = append(depsByKind[r.Kind], r.Target)
	}
	totalDeps := 0
	for _, targets := range depsByKind {
		sort.Strings(targets)
		totalDeps += len(targets)
	}
	if totalDeps > 0 {
		sb.WriteString(fmt.Sprintf("## Dependencies (%d)\n\n", totalDeps))
		for _, relKind := range []string{facts.RelDependsOn, facts.RelImports, facts.RelImplements} {
			targets := depsByKind[relKind]
			if len(targets) == 0 {
				continue
			}
			sb.WriteString(fmt.Sprintf("### %s (%d)\n\n", capitalize(relKind), len(targets)))
			for _, t := range targets {
				sb.WriteString(fmt.Sprintf("- %s\n", t))
			}
			sb.WriteString("\n")
		}
	}

	// Reverse dependencies: who depends on or imports this module
	dependents := sameRepo(store.ReverseLookup(mod.Name, facts.RelImports), mod.Repo)
	revDeps := sameRepo(store.ReverseLookup(mod.Name, facts.RelDependsOn), mod.Repo)
	allDependents := append(dependents, revDeps...)
	slices.SortFunc(allDependents, byName)
	if len(allDependents) > 0 {
		depSeen := make(map[string]struct{})
		sb.WriteString(fmt.Sprintf("## Dependents (%d)\n\n", len(allDependents)))
		for _, dep := range allDependents {
			if _, dup := depSeen[dep.Name]; dup {
				continue
			}
			depSeen[dep.Name] = struct{}{}
			sb.WriteString(fmt.Sprintf("- %s\n", dep.Name))
		}
		sb.WriteString("\n")
	}

	// Nested subtree: modules and symbols beneath this directory. TypeScript/JS
	// (and any per-directory module language) nests modules per directory, so a
	// directory that is itself a module often has a large subtree of child
	// modules whose symbols would otherwise be hidden by this exact-module match.
	s.writeNestedModules(store, mod.Name, len(declaredSymbols), sb)

	// At depth=2: the default 'summary' mode emits an aggregated Insights section
	// (what is architecturally significant); compact/full keep the raw per-symbol
	// relations dump.
	if depth >= 2 {
		if mode == modeCompact || mode == modeFull {
			s.writeSymbolRelations(declaredSymbols, sb)
		} else {
			s.writeModuleInsights(mod.Name, declaredSymbols, depsByKind, allDependents, sb)
		}
	}

	return true
}

// writeSymbolRelations writes the raw per-symbol relation dump (depth=2 detail
// for explore in compact/full mode).
func (s *Server) writeSymbolRelations(declaredSymbols []facts.Fact, sb *strings.Builder) {
	if len(declaredSymbols) == 0 {
		return
	}
	sb.WriteString("## Symbol Relations\n\n")
	limit := len(declaredSymbols)
	if limit > 20 {
		limit = 20
	}
	for _, sym := range declaredSymbols[:limit] {
		if len(sym.Relations) <= 1 {
			continue // skip symbols with only a "declares" relation
		}
		fmt.Fprintf(sb, "**%s**\n", sym.Name)
		for _, r := range sortedRelations(sym.Relations) {
			if r.Kind == facts.RelDeclares {
				continue
			}
			fmt.Fprintf(sb, "  - %s → %s\n", r.Kind, r.Target)
		}
		sb.WriteString("\n")
	}
}

// writeModuleInsights writes the aggregated depth=2 Insights for a module:
// cross-module dependency hotspots, architectural warnings (cycles / layer
// violations from the explainers), and size metrics. This replaces the raw
// symbol-relations dump in the default summary view.
func (s *Server) writeModuleInsights(modName string, declaredSymbols []facts.Fact, depsByKind map[string][]string, allDependents []facts.Fact, sb *strings.Builder) {
	sb.WriteString("## Insights\n\n")

	// Cross-module dependency hotspots: which modules this one depends on most,
	// and which modules depend on it most.
	outByModule := map[string]int{}
	for _, targets := range depsByKind {
		for _, t := range targets {
			outByModule[moduleForName(t)]++
		}
	}
	if len(outByModule) > 0 {
		sb.WriteString("### Depends most on\n\n")
		for _, m := range topCounts(outByModule, 5) {
			fmt.Fprintf(sb, "- %s (%d)\n", m, outByModule[m])
		}
		sb.WriteString("\n")
	}

	inByModule := map[string]int{}
	depSeen := map[string]struct{}{}
	for _, d := range allDependents {
		if _, dup := depSeen[d.Name]; dup {
			continue
		}
		depSeen[d.Name] = struct{}{}
		inByModule[moduleForName(d.Name)]++
	}
	if len(inByModule) > 0 {
		sb.WriteString("### Most depended on by\n\n")
		for _, m := range topCounts(inByModule, 5) {
			fmt.Fprintf(sb, "- %s (%d)\n", m, inByModule[m])
		}
		sb.WriteString("\n")
	}

	// Architectural warnings already computed by the explainers (cycles, layers).
	writeInsightList(sb, "Architectural warnings", s.insightsFor(modName))

	// Size metrics.
	totalDeps := 0
	for _, targets := range depsByKind {
		totalDeps += len(targets)
	}
	fmt.Fprintf(sb, "### Size metrics\n\n- Declared symbols: %d\n- Outgoing dependencies: %d\n- Dependents: %d\n\n",
		len(declaredSymbols), totalDeps, len(depSeen))
	sb.WriteString("_Use output_mode=compact or full for the per-symbol relation list._\n")
}

// moduleForName derives a module/package key from a fact's dotted or slashed name
// for aggregating insights by module.
func moduleForName(name string) string {
	if i := strings.LastIndexByte(name, '/'); i > 0 {
		return name[:i]
	}
	if i := strings.LastIndexByte(name, '.'); i > 0 {
		return name[:i]
	}
	return name
}

// writeNestedModules appends a summary of the modules and symbols nested beneath
// modName (i.e. facts whose file path is under "<modName>/"). It lets a directory
// that is itself a module surface its descendant modules and an aggregate symbol
// count, instead of stopping at the module's own directly-declared symbols.
func (s *Server) writeNestedModules(store *facts.Store, modName string, directSymbols int, sb *strings.Builder) {
	prefix := modName + "/"

	// Accurate totals (QueryAdvanced returns the pre-limit total). Symbols declared
	// directly in this module also live under "<modName>/" (e.g. src/app/layout.tsx),
	// so subtract them to count only symbols in nested child modules.
	_, symTotal := store.QueryAdvanced(facts.QueryOpts{Kind: facts.KindSymbol, FilePrefix: prefix, Limit: 1})
	nestedSymbols := symTotal - directSymbols
	if nestedSymbols < 0 {
		nestedSymbols = 0
	}
	// Group descendant modules by their immediate child segment under modName,
	// counting how many modules live under each child. Every module is counted, so
	// the children agree with the subtree total printed beside them.
	childCounts := make(map[string]int)
	modTotal := store.QueryEach(facts.QueryOpts{Kind: facts.KindModule, FilePrefix: prefix}, func(m *facts.Fact) {
		rest := strings.TrimPrefix(m.Name, prefix)
		seg := rest
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			seg = rest[:i]
		}
		if seg != "" {
			childCounts[seg]++
		}
	})
	if modTotal == 0 && nestedSymbols == 0 {
		return
	}

	children := make([]string, 0, len(childCounts))
	for seg := range childCounts {
		children = append(children, seg)
	}
	sort.Strings(children)

	sb.WriteString(fmt.Sprintf("## Nested modules (%d)\n\n", len(children)))
	sb.WriteString(fmt.Sprintf("Subtree: %d modules, %d symbols\n\n", modTotal, nestedSymbols))

	const maxChildren = 50
	shown := children
	if len(shown) > maxChildren {
		shown = shown[:maxChildren]
	}
	for _, seg := range shown {
		full := prefix + seg
		if n := childCounts[seg]; n > 1 {
			sb.WriteString(fmt.Sprintf("- %s (%d modules)\n", full, n))
		} else {
			sb.WriteString(fmt.Sprintf("- %s\n", full))
		}
	}
	if len(children) > maxChildren {
		sb.WriteString(fmt.Sprintf("\n... and %d more nested modules\n", len(children)-maxChildren))
	}
	sb.WriteString("\nUse explore on a nested module above to drill in.\n\n")
}

// maxModuleSubstringList caps how many candidate modules a non-delegated substring
// match lists, so a broad term (e.g. "golf" across several repos) yields a compact,
// actionable summary instead of dumping hundreds of lines.
const maxModuleSubstringList = 15

// exploreModuleSubstring tries substring matching on module names when exact
// module match fails. It ranks matches (exact > suffix-exact > prefix > substring)
// and, when a single match clearly dominates, delegates to the full exploreModule
// rendering. Otherwise it renders a bounded, repo-grouped summary so a broad term
// does not dump 100 raw module lines; the caller is told how to narrow it.
func (s *Server) exploreModuleSubstring(store *facts.Store, focus string, depth int, mode string, sb *strings.Builder) bool {
	// Every match is ranked, so the list is the best of them and not the first the
	// store returned.
	matches := allMatching(store, facts.QueryOpts{Kind: facts.KindModule, Name: focus})
	total := len(matches)
	if total == 0 {
		return false
	}

	term := strings.ToLower(focus)
	tier := func(name string) int {
		n := strings.ToLower(name)
		switch {
		case n == term:
			return 3
		case resolve.HasShortName(n, term):
			return 2
		case strings.HasPrefix(n, term):
			return 1
		default:
			return 0
		}
	}
	sort.SliceStable(matches, func(i, j int) bool {
		ti, tj := tier(matches[i].Name), tier(matches[j].Name)
		if ti != tj {
			return ti > tj
		}
		return byName(matches[i], matches[j]) < 0
	})

	// A single match, or a unique exact/suffix-exact top match, is unambiguous —
	// drill straight in.
	if len(matches) == 1 || (tier(matches[0].Name) >= 2 && tier(matches[1].Name) < tier(matches[0].Name)) {
		return s.exploreModule(store, matches[0].Name, depth, mode, sb)
	}

	// Multiple plausible matches — summarize, grouped by repo, capped.
	shown := matches
	if len(shown) > maxModuleSubstringList {
		shown = shown[:maxModuleSubstringList]
	}
	fmt.Fprintf(sb, "# Modules matching %q (showing %d of %d)\n\n", focus, len(shown), total)

	byRepo := make(map[string]int)
	repoOrder := make([]string, 0)
	for _, m := range matches {
		r := m.Repo
		if r == "" {
			r = "(primary)"
		}
		if _, ok := byRepo[r]; !ok {
			repoOrder = append(repoOrder, r)
		}
		byRepo[r]++
	}
	if len(repoOrder) > 1 {
		sort.Strings(repoOrder)
		sb.WriteString("By repo: ")
		parts := make([]string, 0, len(repoOrder))
		for _, r := range repoOrder {
			parts = append(parts, fmt.Sprintf("%s (%d)", r, byRepo[r]))
		}
		sb.WriteString(strings.Join(parts, ", "))
		sb.WriteString("\n\n")
	}

	for _, m := range shown {
		if m.Repo != "" {
			fmt.Fprintf(sb, "- `%s` (repo:%s)\n", m.Name, m.Repo)
		} else {
			fmt.Fprintf(sb, "- `%s`\n", m.Name)
		}
	}
	sb.WriteString("\nNarrow with a `repo:<label>` scope, a full module name, or a more specific substring.\n")
	return true
}

// exploreFile renders a file exploration if the focus matches an exact file path.
// In multi-repo mode, it also tries repo-label prefixed paths and common extensions.
func (s *Server) exploreFile(store *facts.Store, focus string, depth int, sb *strings.Builder) bool {
	fileFacts := store.ByFile(focus)

	// In multi-repo mode, try repo-label prefixed paths.
	if len(fileFacts) == 0 {
		for _, label := range s.repoLabels() {
			fileFacts = store.ByFile(label + "/" + focus)
			if len(fileFacts) > 0 {
				focus = label + "/" + focus
				break
			}
		}
	}

	// Try appending common extensions (with and without repo labels).
	if len(fileFacts) == 0 {
		extensions := []string{".go", ".ts", ".tsx", ".kt", ".swift", ".rb"}
		candidates := make([]string, 0, len(extensions)*(1+len(s.repoLabels())))
		for _, ext := range extensions {
			candidates = append(candidates, focus+ext)
			for _, label := range s.repoLabels() {
				candidates = append(candidates, label+"/"+focus+ext)
			}
		}
		for _, c := range candidates {
			fileFacts = store.ByFile(c)
			if len(fileFacts) > 0 {
				focus = c
				break
			}
		}
	}

	if len(fileFacts) == 0 {
		return false
	}

	sb.WriteString(fmt.Sprintf("# File: %s\n\n", focus))
	sb.WriteString(fmt.Sprintf("Total facts: %d\n\n", len(fileFacts)))

	// Group by kind, each group in source order: the store's own order is not one
	// order, it differs between a graph just generated and one read back from disk.
	byKind := make(map[string][]facts.Fact)
	for _, f := range fileFacts {
		byKind[f.Kind] = append(byKind[f.Kind], f)
	}
	for _, ff := range byKind {
		slices.SortFunc(ff, bySourcePosition)
	}

	for _, kind := range []string{facts.KindModule, facts.KindSymbol, facts.KindDependency, facts.KindRoute, facts.KindStorage} {
		ff := byKind[kind]
		if len(ff) == 0 {
			continue
		}
		sb.WriteString(fmt.Sprintf("## %s (%d)\n\n", capitalize(facts.PluralKind(kind)), len(ff)))
		for _, f := range ff {
			sb.WriteString(fmt.Sprintf("- **%s**", f.Name))
			if f.Line > 0 {
				sb.WriteString(fmt.Sprintf(" (line %d)", f.Line))
			}
			if sk, ok := f.PropAny("symbol_kind").(string); ok {
				sb.WriteString(fmt.Sprintf(" [%s]", sk))
			}
			sb.WriteString("\n")
			if depth >= 2 {
				for _, r := range sortedRelations(f.Relations) {
					sb.WriteString(fmt.Sprintf("  - %s → %s\n", r.Kind, r.Target))
				}
			}
		}
		sb.WriteString("\n")
	}

	return true
}

// exploreSymbol renders a symbol exploration if the focus matches symbol names via substring.
func (s *Server) exploreSymbol(store *facts.Store, focus string, depth int, sb *strings.Builder) bool {
	results := store.Query(facts.KindSymbol, "", focus, "")
	if len(results) == 0 {
		return false
	}
	// The ten shown are the ten nearest the focus: the shortest names holding it.
	slices.SortFunc(results, func(a, b facts.Fact) int {
		return cmp.Or(cmp.Compare(len(a.Name), len(b.Name)), byName(a, b))
	})

	if len(results) > 10 {
		results = results[:10]
	}

	sb.WriteString(fmt.Sprintf("# Symbol: %s\n\n", focus))

	for i, sym := range results {
		if i > 0 {
			sb.WriteString("---\n\n")
		}
		relations := sortedRelations(sym.Relations)

		sb.WriteString(fmt.Sprintf("## %s\n\n", sym.Name))
		sb.WriteString(fmt.Sprintf("- File: %s\n", sym.File))
		sb.WriteString(fmt.Sprintf("- Line: %d\n", sym.Line))
		if sk, ok := sym.PropAny("symbol_kind").(string); ok {
			sb.WriteString(fmt.Sprintf("- Kind: %s\n", sk))
		}
		if lang, ok := sym.PropAny("language").(string); ok {
			sb.WriteString(fmt.Sprintf("- Language: %s\n", lang))
		}
		if exp, ok := sym.PropAny("exported").(bool); ok {
			sb.WriteString(fmt.Sprintf("- Exported: %v\n", exp))
		}
		sb.WriteString("\n")

		// Relations
		if len(relations) > 0 {
			sb.WriteString("### Relations\n\n")
			for _, r := range relations {
				sb.WriteString(fmt.Sprintf("- %s → %s\n", r.Kind, r.Target))
			}
			sb.WriteString("\n")
		}

		// Resolve relation targets (depth >= 1)
		if depth >= 1 && len(relations) > 0 {
			sb.WriteString("### Related Facts\n\n")
			seen := make(map[string]struct{})
			for _, r := range relations {
				if _, dup := seen[r.Target]; dup {
					continue
				}
				seen[r.Target] = struct{}{}
				related := store.LookupByExactName(r.Target)
				slices.SortFunc(related, bySourcePosition)
				for _, rf := range related {
					sb.WriteString(fmt.Sprintf("- **%s** (%s) — %s", rf.Name, rf.Kind, rf.File))
					if rf.Line > 0 {
						sb.WriteString(fmt.Sprintf(":%d", rf.Line))
					}
					sb.WriteString("\n")
				}
			}
			sb.WriteString("\n")
		}

		// Reverse relations: who calls/imports/depends on this symbol
		callers := store.ReverseLookup(sym.Name, "")
		slices.SortFunc(callers, byName)
		if len(callers) > 0 {
			sb.WriteString("### Referenced By\n\n")
			limit := len(callers)
			if limit > 20 {
				limit = 20
			}
			for _, c := range callers[:limit] {
				for _, r := range sortedRelations(c.Relations) {
					if r.Target == sym.Name {
						sb.WriteString(fmt.Sprintf("- %s (%s)\n", c.Name, r.Kind))
						break
					}
				}
			}
			if len(callers) > 20 {
				sb.WriteString(fmt.Sprintf("- ... and %d more\n", len(callers)-20))
			}
			sb.WriteString("\n")
		}
	}

	return true
}

// exploreDirectory renders a directory summary if the focus matches a file prefix.
func (s *Server) exploreDirectory(store *facts.Store, focus string, sb *strings.Builder) bool {
	prefix := focus
	if prefix == "." {
		// "." means repo root — match all files (no prefix filter).
		prefix = ""
	} else if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	// In multi-repo mode, expand bare prefixes to include repo labels.
	prefixes := s.expandFilePrefix(prefix)

	// Count every fact under the prefix. The summary used to be taken over the first
	// 500 the store returned, under a heading giving the full total, so the counts
	// were a sample and which sample depended on the store's order.
	const keySymbols = 30
	total, symbolCount := 0, 0
	kindCount := make(map[string]int)
	files := make(map[string]struct{})
	modules := make(map[string]struct{})
	symbols := make([]facts.Fact, 0, keySymbols+1)
	for _, p := range prefixes {
		total += store.QueryEach(facts.QueryOpts{FilePrefix: p}, func(f *facts.Fact) {
			kindCount[f.Kind]++
			if f.File != "" {
				files[f.File] = struct{}{}
			}
			switch f.Kind {
			case facts.KindModule:
				modules[f.Name] = struct{}{}
			case facts.KindSymbol:
				symbolCount++
				// Keep the first keySymbols in source order without holding them all.
				if len(symbols) == keySymbols && bySourcePosition(*f, symbols[keySymbols-1]) >= 0 {
					return
				}
				at, _ := slices.BinarySearchFunc(symbols, *f, bySourcePosition)
				symbols = slices.Insert(symbols, at, *f)
				if len(symbols) > keySymbols {
					symbols = symbols[:keySymbols]
				}
			}
		})
	}
	if total == 0 {
		return false
	}

	sb.WriteString(fmt.Sprintf("# Directory: %s\n\n", focus))
	sb.WriteString(fmt.Sprintf("Total facts: %d\n\n", total))

	sb.WriteString("## Summary\n\n")
	sb.WriteString(fmt.Sprintf("- Files: %d\n", len(files)))
	for _, kind := range []string{facts.KindModule, facts.KindSymbol, facts.KindDependency, facts.KindRoute, facts.KindStorage} {
		if c, ok := kindCount[kind]; ok {
			sb.WriteString(fmt.Sprintf("- %s: %d\n", capitalize(facts.PluralKind(kind)), c))
		}
	}
	sb.WriteString("\n")

	if len(modules) > 0 {
		sb.WriteString(fmt.Sprintf("## Modules (%d)\n\n", len(modules)))
		for _, name := range slices.Sorted(maps.Keys(modules)) {
			sb.WriteString(fmt.Sprintf("- %s\n", name))
		}
		sb.WriteString("\n")
	}

	if len(symbols) > 0 {
		sb.WriteString(fmt.Sprintf("## Key Symbols (showing up to %d)\n\n", keySymbols))
		sb.WriteString("| Name | Kind | File | Line |\n")
		sb.WriteString("|------|------|------|------|\n")
		for _, sym := range symbols {
			symKind, _ := sym.PropAny("symbol_kind").(string)
			sb.WriteString(fmt.Sprintf("| %s | %s | %s | %d |\n",
				sym.Name, symKind, sym.File, sym.Line))
		}
		if symbolCount > len(symbols) {
			sb.WriteString(fmt.Sprintf("\n... and %d more symbols\n", symbolCount-len(symbols)))
		}
		sb.WriteString("\n")
	}

	return true
}

// byName orders facts by name, then by where they are declared.
func byName(a, b facts.Fact) int {
	return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.Repo, b.Repo), bySourcePosition(a, b))
}

// sortedRelations returns a fact's relations by kind, then target. A fact holds them
// in the order its extractor emitted them, and facts.jsonl holds them sorted, so the
// same fact lists them differently before and after a restore.
func sortedRelations(rels []facts.Relation) []facts.Relation {
	out := slices.Clone(rels)
	slices.SortFunc(out, func(a, b facts.Relation) int {
		return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Target, b.Target))
	})
	return out
}

// allMatching returns every fact matching opts, with no limit, in source order.
func allMatching(store *facts.Store, opts facts.QueryOpts) []facts.Fact {
	var out []facts.Fact
	store.QueryEach(opts, func(f *facts.Fact) { out = append(out, *f) })
	slices.SortFunc(out, bySourcePosition)
	return out
}

// bySourcePosition orders facts by file, then line, then name and kind, which is the
// order a reader meets them in and does not depend on the store's.
func bySourcePosition(a, b facts.Fact) int {
	return cmp.Or(
		cmp.Compare(a.File, b.File),
		cmp.Compare(a.Line, b.Line),
		cmp.Compare(a.Name, b.Name),
		cmp.Compare(a.Kind, b.Kind),
	)
}
