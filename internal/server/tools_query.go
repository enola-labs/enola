package server

// The fact lookup tools: query_facts and show_symbol.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// queryFactsArgs are the arguments for the query_facts tool.
type queryFactsArgs struct {
	Kind      string `json:"kind,omitempty" jsonschema:"module, symbol, route, storage, dependency, or service (a whole repo, a node in the cross-repo graph)."`
	File      string `json:"file,omitempty" jsonschema:"File path."`
	Name      string `json:"name,omitempty" jsonschema:"Name substring, case-insensitive."`
	Relation  string `json:"relation,omitempty" jsonschema:"Relation kind: declares, imports, calls, implements, or depends_on."`
	Prop      string `json:"prop,omitempty" jsonschema:"Property name, e.g. source, symbol_kind, exported, framework, storage_kind, role, method, unmatched_by_clients, unreached_by_clients. output_mode=summary lists the notable boolean flags present in the result."`
	PropValue string `json:"prop_value,omitempty" jsonschema:"Property value; requires prop."`

	// Batch filters — OR within dimension, AND across dimensions
	Names      []string `json:"names,omitempty" jsonschema:"Exact fact names, which are package-qualified (e.g. internal/server.Server, not Server). OR with each other and with name."`
	Files      []string `json:"files,omitempty" jsonschema:"File paths, OR. Use instead of file for batch lookups."`
	Kinds      []string `json:"kinds,omitempty" jsonschema:"Kinds, OR. Use instead of kind for batch lookups."`
	FilePrefix string   `json:"file_prefix,omitempty" jsonschema:"File path prefix, e.g. internal/server for every file under it."`
	Repo       string   `json:"repo,omitempty" jsonschema:"Repository label (multi-repo snapshots), e.g. 'go-service'."`

	// Pagination
	Offset int `json:"offset,omitempty" jsonschema:"Results to skip. Default 0."`
	Limit  int `json:"limit,omitempty" jsonschema:"Maximum results (1-500). Default 100."`

	// Relation expansion
	IncludeRelated bool `json:"include_related,omitempty" jsonschema:"Inline each relation target's full fact instead of just its name."`

	// Output format
	OutputMode string `json:"output_mode,omitempty" jsonschema:"'full' (DEFAULT, JSON facts), 'compact' (markdown table), 'names' (names and files), or 'summary' (total plus breakdown by kind and top files; cheapest)."`
	MaxTokens  int    `json:"max_tokens,omitempty" jsonschema:"Approximate token cap; output is truncated with a notice. Default: no cap."`
}

// enrichedFact wraps a Fact with resolved relation targets.
type enrichedFact struct {
	facts.Fact
	RelatedFacts []facts.Fact `json:"related_facts,omitempty"`
}

// queryResponse is the structured response for query_facts when advanced features are used.
type queryResponse struct {
	Filters string `json:"filters"`
	Facts   any    `json:"facts"`
	Total   int    `json:"total"`
	Offset  int    `json:"offset"`
	Limit   int    `json:"limit"`
	HasMore bool   `json:"has_more"`
}

// withoutRepoLabel returns opts with a leading repo label stripped from name and names,
// scoped to that repository, when any carried one. It reports the label, and false
// when nothing was stripped or the entries name different repositories.
func (s *Server) withoutRepoLabel(opts facts.QueryOpts) (facts.QueryOpts, string, bool) {
	labels := s.repoLabels()
	strip := func(n string) (string, string) {
		for _, l := range labels {
			if rest, ok := strings.CutPrefix(n, l+"/"); ok && rest != "" {
				return rest, l
			}
		}
		return n, ""
	}
	found := ""
	take := func(l string) bool {
		if l == "" {
			return true
		}
		if found != "" && found != l {
			return false
		}
		found = l
		return true
	}
	out := opts
	var l string
	out.Name, l = strip(opts.Name)
	if !take(l) {
		return opts, "", false
	}
	if len(opts.Names) > 0 {
		out.Names = make([]string, len(opts.Names))
		for i, n := range opts.Names {
			out.Names[i], l = strip(n)
			if !take(l) {
				return opts, "", false
			}
		}
	}
	if found == "" || (opts.Repo != "" && opts.Repo != found) {
		return opts, "", false
	}
	out.Repo = found
	return out, found, true
}

// broadNameNote warns when a name substring matched a large share of what the same
// query matches without it. name is a case-insensitive substring, so a short one
// matches inside other words: name="ai" matched tailwindcss, Domain and AiCoach, 37%
// of every dependency, and an agent read that as the filter being ignored. One extra
// count, paid only when name is set and matched enough to matter.
func broadNameNote(store *facts.Store, opts facts.QueryOpts, total int) string {
	if opts.Name == "" || total < 200 {
		return ""
	}
	o := opts
	o.Name, o.Offset = "", 0
	without := store.QueryEach(o, func(*facts.Fact) {})
	if without == 0 || total*4 < without {
		return ""
	}
	return fmt.Sprintf("name contains %q matched %d of %d facts (%d%%): it is a case-insensitive substring, so a short "+
		"name matches inside other words. Use a longer name, or names=[...] for exact names.",
		opts.Name, total, without, total*100/without)
}

// describeFilters states the filters a query_facts call actually carried, as the first
// line of its answer. An agent that meant to filter and sent a call without the filter
// reads the unfiltered result as the filter being ignored; seeing what arrived tells
// it which one was wrong. It says so outright when nothing narrowed the result, since
// that is the case that gets misread.
func describeFilters(args queryFactsArgs) string {
	var parts []string
	add := func(format string, v ...any) { parts = append(parts, fmt.Sprintf(format, v...)) }
	if args.Repo != "" {
		add("repo=%s", args.Repo)
	}
	if args.Kind != "" {
		add("kind=%s", args.Kind)
	}
	if len(args.Kinds) > 0 {
		add("kinds=%s", strings.Join(args.Kinds, ","))
	}
	if args.Name != "" {
		add("name contains %q (case-insensitive substring)", args.Name)
	}
	if len(args.Names) > 0 {
		add("names=%s (exact)", strings.Join(args.Names, ","))
	}
	if args.File != "" {
		add("file=%s", args.File)
	}
	if len(args.Files) > 0 {
		add("files=%s", strings.Join(args.Files, ","))
	}
	if args.FilePrefix != "" {
		add("file_prefix=%s", args.FilePrefix)
	}
	if args.Relation != "" {
		add("relation=%s", args.Relation)
	}
	if args.Prop != "" {
		if args.PropValue != "" {
			add("%s=%s", args.Prop, args.PropValue)
		} else {
			add("has prop %s", args.Prop)
		}
	}
	// repo alone scopes the answer to one repository but narrows nothing inside it.
	narrowed := len(parts) > 1 || len(parts) == 1 && args.Repo == ""
	if args.Offset > 0 {
		add("offset=%d", args.Offset)
	}
	if args.Limit > 0 {
		add("limit=%d", args.Limit)
	}
	line := "Filters applied: none"
	if len(parts) > 0 {
		line = "Filters applied: " + strings.Join(parts, ", ")
	}
	if !narrowed {
		scope := "the whole store"
		if args.Repo != "" {
			scope = "every fact in repo " + args.Repo
		}
		line += fmt.Sprintf(". No kind, name, file or prop filter was sent, so this is %s.", scope)
	}
	return line
}

// renderCompact formats facts as a markdown table for minimal token usage.
func renderCompact(results []facts.Fact, total int) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Found %d results (showing %d):\n\n", total, len(results)))
	sb.WriteString("| Kind | Name | File | Line |\n")
	sb.WriteString("|------|------|------|------|\n")
	for _, f := range results {
		sb.WriteString(fmt.Sprintf("| %s | %s | %s | %d |\n", f.Kind, f.Name, f.File, f.Line))
	}
	return sb.String()
}

// renderNamesOnly returns just names and files, one per line.
func renderNamesOnly(results []facts.Fact, total int) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Found %d results (showing %d):\n\n", total, len(results)))
	for _, f := range results {
		sb.WriteString(fmt.Sprintf("%s  %s:%d\n", f.Name, f.File, f.Line))
	}
	return sb.String()
}

// renderQuerySummary returns counts only — total plus a breakdown by kind and the
// top files — so the caller can size a result set before fetching the facts
// themselves. The caller passes every match (queryAllPages); should results still
// fall short of total, the breakdown is annotated as approximate.
// notableBoolProps are high-signal boolean fact properties surfaced in the
// query_facts summary so a caller sizing a result set discovers actionable
// flags (e.g. dead routes) without already knowing the prop name exists.
var notableBoolProps = []string{"unmatched_by_clients", "unreached_by_clients"}

// querySummary aggregates query_facts' summary one fact at a time, so summary mode
// can count straight off the store. It used to page through every match 500 at a
// time, and each page re-ran the whole query and copied every match to skip to its
// offset: quadratic, and an unfiltered summary of a 3M-fact store ran for minutes.
type querySummary struct {
	byKind, byFile, flags map[string]int
	seen                  int
}

func newQuerySummary() *querySummary {
	return &querySummary{byKind: map[string]int{}, byFile: map[string]int{}, flags: map[string]int{}}
}

func (q *querySummary) add(f *facts.Fact) {
	q.seen++
	q.byKind[f.Kind]++
	if f.File != "" {
		q.byFile[f.File]++
	}
	for _, p := range notableBoolProps {
		if f.Props != nil && f.PropAny(p) == true {
			q.flags[p]++
		}
	}
}

func (q *querySummary) render(total int) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Found **%d** matching facts.\n\n", total)
	if len(q.byKind) > 0 {
		sb.WriteString("## By kind\n\n")
		for _, k := range topCounts(q.byKind, len(q.byKind)) {
			fmt.Fprintf(&sb, "- %s: %d\n", k, q.byKind[k])
		}
		sb.WriteString("\n")
	}
	if len(q.flags) > 0 {
		sb.WriteString("## Flags\n\n")
		for _, p := range topCounts(q.flags, len(q.flags)) {
			fmt.Fprintf(&sb, "- %s=true: %d — list with query_facts(prop=%q, prop_value=true); see the summarized finding via query_insights\n", p, q.flags[p], p)
		}
		sb.WriteString("\n")
	}
	if len(q.byFile) > 0 {
		sb.WriteString("## Top files\n\n")
		for _, f := range topCounts(q.byFile, 10) {
			fmt.Fprintf(&sb, "- %s: %d\n", f, q.byFile[f])
		}
		sb.WriteString("\n")
	}
	if total > q.seen {
		fmt.Fprintf(&sb, "_Breakdown computed over a sample of %d of %d matches; counts are approximate. Re-run with filters to narrow, or output_mode=compact/names to list facts._\n", q.seen, total)
	}
	return sb.String()
}

// registerQueryFactsTool registers the query_facts tool.
func (s *Server) registerQueryFactsTool() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "query_facts",
		Description: "Precision filter over extracted facts, for subsets explore does not give you: all symbols in a file, all external dependencies, all routes. " +
			"Filters are OR within a dimension (name with names; file with files and file_prefix; kind with kinds) and AND across dimensions. " +
			"Size an unfamiliar result set with output_mode=summary first. " +
			"For dependencies, prop=source with prop_value internal, external, stdlib or framework cuts the noise.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args queryFactsArgs) (*mcp.CallToolResult, any, error) {
		store := s.eng.Store()
		if store.Count() == 0 {
			return errorResult("No facts available. Run generate_snapshot first."), nil, nil
		}

		// Normalize absolute filesystem paths to store-relative paths.
		normFile := s.normalizeToRelative(args.File)
		normPrefix := s.normalizeToRelative(args.FilePrefix)
		var normFiles []string
		for _, f := range args.Files {
			normFiles = append(normFiles, s.normalizeToRelative(f))
		}

		// In multi-repo mode, expand the file prefix to include repo labels
		// if the user provided a bare relative path (e.g. "src/" instead of "golf-ui/src/").
		prefixes := s.expandFilePrefix(normPrefix)

		mode := resolveOutputMode(args.OutputMode, modeFull)

		limit := args.Limit

		// Query with the first (or only) prefix.
		opts := facts.QueryOpts{
			Kind:       args.Kind,
			Kinds:      args.Kinds,
			File:       normFile,
			Files:      normFiles,
			FilePrefix: prefixes[0],
			Name:       args.Name,
			Names:      args.Names,
			Repo:       args.Repo,
			RelKind:    args.Relation,
			Prop:       args.Prop,
			PropValue:  args.PropValue,
			Offset:     args.Offset,
			Limit:      limit,
		}

		query := store.QueryAdvanced
		if mode == modeSummary {
			// Summary renders counts, not facts, so it can afford every match: a
			// breakdown over the first 500 read "993 matching facts" beside
			// "route: 500", which looks like a contradiction.
			// Counted, not collected: the breakdown is aggregated straight off the store
			// when the answer is rendered, so no call pays for a slice of every match.
			query = func(o facts.QueryOpts) ([]facts.Fact, int) {
				return nil, store.QueryEach(o, func(*facts.Fact) {})
			}
		}
		results, total := query(opts)

		// If multiple repo labels matched, merge results from additional prefixes.
		for _, p := range prefixes[1:] {
			opts.FilePrefix = p
			extra, extraTotal := query(opts)
			results = append(results, extra...)
			total += extraTotal
		}

		// A query_facts(kind=service) that comes back empty on a single-repo snapshot
		// means the cross-repo graph was never built, not that this service is a leaf.
		// Say so, mirroring coverage_report, instead of returning a bare null.
		// A name copied from a file path carries the repo label, which names never do,
		// so it matched nothing. Only an empty answer is retried, so a name that really
		// starts that way is never rewritten, and the answer says what was done.
		note := ""
		if total == 0 && len(prefixes) == 1 {
			if retry, label, ok := s.withoutRepoLabel(opts); ok {
				if r2, t2 := query(retry); t2 > 0 {
					results, total, opts = r2, t2, retry
					note = fmt.Sprintf("Nothing matched as given: names never carry the repo label, file paths do. "+
						"Retried without %q, scoped to repo=%s.", label+"/", label)
				}
			}
		}
		if note == "" && len(prefixes) == 1 {
			note = broadNameNote(store, opts, total)
		}

		if hint, ok := singleRepoServiceHint(args.Kind, total, store); ok {
			return textResult(hint), nil, nil
		}
		filters := describeFilters(args)
		if note != "" {
			filters += "\n" + note
		}

		// Non-JSON output modes: return text instead of JSON.
		switch mode {
		case modeSummary:
			q := newQuerySummary()
			for _, p := range prefixes {
				o := opts
				o.FilePrefix = p
				store.QueryEach(o, q.add)
			}
			return textResult(capTokens(filters+"\n\n"+q.render(total), args.MaxTokens, false)), nil, nil
		case modeCompact:
			return textResult(capTokens(filters+"\n\n"+renderCompact(results, total), args.MaxTokens, false)), nil, nil
		case modeNames:
			return textResult(capTokens(filters+"\n\n"+renderNamesOnly(results, total), args.MaxTokens, false)), nil, nil
		}

		// Determine if advanced features are in use (triggers structured response)
		useAdvanced := args.IncludeRelated || args.Offset > 0 || args.Limit > 0 ||
			len(args.Names) > 0 || len(args.Files) > 0 || len(args.Kinds) > 0 ||
			args.FilePrefix != "" || args.Repo != ""

		// Enrich with related facts if requested
		var output any
		if args.IncludeRelated {
			enriched := make([]enrichedFact, len(results))
			seen := make(map[string]struct{}) // deduplicate related facts
			for i, f := range results {
				enriched[i] = enrichedFact{Fact: f}
				for _, rel := range f.Relations {
					if _, dup := seen[rel.Target]; dup {
						continue
					}
					seen[rel.Target] = struct{}{}
					related := store.LookupByExactName(rel.Target)
					enriched[i].RelatedFacts = append(enriched[i].RelatedFacts, related...)
				}
			}
			output = enriched
		} else {
			output = results
		}

		if useAdvanced {
			limit := args.Limit
			if limit <= 0 {
				limit = 100
			}
			if limit > 500 {
				limit = 500
			}
			resp := queryResponse{
				Filters: filters,
				Facts:   output,
				Total:   total,
				Offset:  args.Offset,
				Limit:   limit,
				HasMore: total > args.Offset+len(results),
			}
			return jsonResultCapped(resp, args.MaxTokens)
		}

		// Legacy format: raw JSON array (backwards compatible)
		data, err := json.MarshalIndent(output, "", "  ")
		if err != nil {
			return errorResult(fmt.Sprintf("failed to marshal results: %v", err)), nil, nil
		}

		// The filters lead: max_tokens truncation cuts from the end, and an oversized
		// answer is exactly the one whose filters the agent needs to see.
		text := filters + "\n\n" + string(data)
		if total > len(results) {
			text += fmt.Sprintf("\n\n... (showing %d of %d results, refine your query or use offset/limit for pagination)", len(results), total)
		}

		return textResult(capTokens(text, args.MaxTokens, true)), nil, nil
	})
}

// registerShowSymbolTool registers the show_symbol tool.
func (s *Server) registerShowSymbolTool() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "show_symbol",
		Description: "Source code of a named symbol: an exact name match if there is one, otherwise up to 5 substring matches. " +
			"Works in multi-repo snapshots too.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args showSymbolArgs) (*mcp.CallToolResult, any, error) {
		snapshot := s.eng.Snapshot()
		if snapshot == nil {
			return errorResult("No snapshot available. Run generate_snapshot first."), nil, nil
		}

		store := s.eng.Store()
		if store.Count() == 0 {
			return errorResult("No facts available. Run generate_snapshot first."), nil, nil
		}

		if args.Name == "" {
			return errorResult("name is required"), nil, nil
		}

		// Prefer exact match to avoid substring noise (e.g. "Transaction" matching "AutoTransactionsTogglePatch").
		results := store.LookupByExactName(args.Name)
		// Filter to symbols only
		symbolResults := results[:0]
		for _, r := range results {
			if r.Kind == facts.KindSymbol {
				symbolResults = append(symbolResults, r)
			}
		}
		results = symbolResults
		if len(results) == 0 {
			for _, n := range s.resolver(store).Spellings(store, args.Name) {
				for _, r := range store.LookupByExactName(n) {
					if r.Kind == facts.KindSymbol {
						results = append(results, r)
					}
				}
			}
		}
		exact := len(results) > 0
		if len(results) == 0 {
			results = store.Query("symbol", "", args.Name, "")
		}
		if len(results) == 0 {
			return errorResult(fmt.Sprintf("No symbols matching %q", args.Name)), nil, nil
		}

		contextLines := args.ContextLines
		if contextLines <= 0 {
			contextLines = 60
		}

		var sb strings.Builder
		// Several symbols declaring the exact name are one graph node, with their
		// edges merged: say so, and say when the list below is cut.
		sameName := len(results) > 1
		for _, r := range results {
			sameName = sameName && r.Name == results[0].Name
		}
		if (exact && len(results) > 1) || sameName {
			sb.WriteString(fmt.Sprintf("_%d declarations share the name %s; the graph holds them as one node with their edges merged._", len(results), results[0].Name))
			if len(results) > 5 {
				sb.WriteString(" _Showing 5._")
			}
			sb.WriteString("\n\n")
		}

		// Limit to 5 results
		if len(results) > 5 {
			results = results[:5]
		}

		for i, fact := range results {
			if i > 0 {
				sb.WriteString("\n---\n\n")
			}

			// Header
			sb.WriteString(fmt.Sprintf("### %s\n", fact.Name))
			if sk := fact.PropString("symbol_kind"); sk != "" {
				sb.WriteString(fmt.Sprintf("Kind: %s  ", sk))
			}
			sb.WriteString(fmt.Sprintf("File: %s  Line: %d\n", fact.File, fact.Line))

			// Show props summary
			if sig, ok := fact.PropAny("signature").(string); ok {
				sb.WriteString(fmt.Sprintf("Signature:\n```\n%s\n```\n", sig))
			}
			if comp, ok := fact.PropAny("ios_component").(string); ok {
				sb.WriteString(fmt.Sprintf("iOS Component: %s\n", comp))
			}
			if g := store.Graph(); g != nil {
				if pages := g.GoverningIntentForFile(fact.Repo, fact.File); len(pages) > 0 {
					var refs []string
					for _, p := range pages {
						refs = append(refs, p.Page+pageMeta(p.Type, p.Status))
					}
					sb.WriteString("Governed by: " + strings.Join(refs, "; ") + "\n")
				}
			}

			sb.WriteString("\n")

			// Read source file (handles both single-repo and multi-repo paths)
			absFile := s.eng.ResolveFactFile(&fact)
			source, err := readSourceWindow(absFile, fact.Line, contextLines)
			if err != nil {
				sb.WriteString(fmt.Sprintf("_Could not read source: %v_\n", err))
				continue
			}

			lang := "go"
			if l, ok := fact.PropAny("language").(string); ok && l != "" {
				lang = l
			}
			sb.WriteString(fmt.Sprintf("```%s\n%s\n```\n", lang, source))
		}

		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: sb.String()},
			},
		}, nil, nil
	})
}

// showSymbolArgs are the arguments for the show_symbol tool.
type showSymbolArgs struct {
	Name         string `json:"name" jsonschema:"Symbol name, exact or substring."`
	ContextLines int    `json:"context_lines,omitempty" jsonschema:"Source lines to show around the declaration. Default 60: about 15 before, 45 after."`
}

// readSourceWindow reads lines from a file around the given line number.
// The window is asymmetric: 1/4 of context before the line, 3/4 after,
// since symbol declarations are at the start of the interesting code.
func readSourceWindow(absFile string, centerLine, contextLines int) (string, error) {
	data, err := os.ReadFile(absFile)
	if err != nil {
		return "", err
	}

	lines := strings.Split(string(data), "\n")
	before := contextLines / 4
	after := contextLines - before
	startLine := centerLine - before
	if startLine < 1 {
		startLine = 1
	}
	endLine := centerLine + after
	if endLine > len(lines) {
		endLine = len(lines)
	}

	var sb strings.Builder
	for i := startLine; i <= endLine; i++ {
		sb.WriteString(fmt.Sprintf("%4d│ %s\n", i, lines[i-1]))
	}
	return sb.String(), nil
}
