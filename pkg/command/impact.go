package command

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/impact"
	"github.com/enola-labs/enola/pkg/bootstrap"
	"github.com/enola-labs/enola/pkg/check"
)

// Impact is the impact_analysis tool without a session, as endpoint is endpoint_impact.
func (r *Runner) Impact(ctx context.Context, args []string) {
	os.Exit(r.runImpact(ctx, args, os.Stdout, os.Stderr))
}

func (r *Runner) runImpact(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	usageError := check.StatusUsageError.ExitCode()
	say := func(format string, args ...any) {
		_, _ = fmt.Fprintf(stderr, r.name()+" impact: "+format+"\n", args...)
	}

	fs := flag.NewFlagSet("impact", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		asJSON         = fs.Bool("json", false, "emit the raw graph as JSON: every dependent with its file, line and depth, and the edges between them")
		asList         = fs.Bool("list", false, "list the dependents by depth instead of summarising them")
		maxDepth       = fs.Int("max-depth", 3, "hops of impact to follow (1-10)")
		maxNodes       = fs.Int("max-nodes", 200, "cap on the nodes listed (1-500); the total reported stays exact")
		includeForward = fs.Bool("include-forward", false, "also report what the target itself depends on")
	)
	fs.Usage = func() {
		_, _ = fmt.Fprint(stderr,
			"Usage: "+r.name()+" impact [flags] <target> [repo_path|config_path]\n\n"+
				"Report the blast radius of changing a node: everything that transitively\n"+
				"depends on it, by hop depth. With no flags it prints a summary - the exact\n"+
				"dependent count, the breakdown by kind and depth, and the modules holding the\n"+
				"most dependents.\n\n"+
				"The target is matched as a substring of a fact name. Narrow it with a scope\n"+
				"prefix, quoted as one argument: 'kind:symbol Alpha', 'file:pkg/a Alpha',\n"+
				"'repo:backend Alpha'. A target\n"+
				"matching too many nodes to choose between is not guessed at: the candidates\n"+
				"are printed and nothing is traversed. A target matching a few is resolved to\n"+
				"the best of them, and the report says which.\n\n"+
				"Use endpoint when what you have is a URL; use this when you have a symbol.\n\n"+
				"Exit codes:\n"+
				"  0  the report was produced\n"+
				"  2  the command could not run, or nothing was traversed because the target\n"+
				"     matched nothing, or too many nodes to pick from\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return usageError
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fs.Usage()
		return usageError
	}
	if *asJSON && *asList {
		say("--json and --list are two renderings of one report; pass one")
		return usageError
	}
	// Refused, not clamped: a walk cut short would still report an "exact" total.
	if *maxDepth < 1 || *maxDepth > 10 {
		say("--max-depth %d is outside 1-10", *maxDepth)
		return usageError
	}
	if *maxNodes < 1 || *maxNodes > 500 {
		say("--max-nodes %d is outside 1-500", *maxNodes)
		return usageError
	}
	// Flag parsing stops at the target, so a later flag or a third word would be misread silently.
	if len(rest) > 2 {
		say("expected <target> [repo_path|config_path], got %d arguments; quote a target that contains a space", len(rest))
		return usageError
	}
	for _, arg := range rest {
		if strings.HasPrefix(arg, "-") {
			say("flags go before the target; %q came after it", arg)
			return usageError
		}
	}
	query := rest[0]
	target := ""
	if len(rest) > 1 {
		target = rest[1]
	}

	tgt := r.resolveTarget(target)
	say("%s", tgt.configNote)

	// Read-only, like endpoint and check.
	tgt.engine.SetPersistCache(false)
	// Reuse the snapshot on disk only when it matches the tree, the test `baseline pin` trusts.
	generatedAt, stale := snapshotIsCurrent(tgt.engine, tgt.repoPaths)
	if stale == "" {
		if err := bootstrap.RestoreSnapshot(tgt.engine, tgt.repoPaths); err != nil {
			stale = "the snapshot on disk could not be read: " + err.Error()
		}
	}
	if stale == "" {
		say("reading the snapshot written %s, which matches the working tree", generatedAt)
	} else {
		say("taking a snapshot for this run, %s", stale)
		for i, repoPath := range tgt.repoPaths {
			// Defer linking to the last member, as --generate does.
			tgt.engine.SetDeferLinking(i < len(tgt.repoPaths)-1)
			if _, err := tgt.engine.GenerateSnapshot(ctx, repoPath, i > 0); err != nil {
				say("snapshot generation failed for %s: %v", repoPath, err)
				return usageError
			}
		}
	}

	report, err := impact.Analyze(tgt.engine.Resolver(), impact.Request{
		Target:         query,
		MaxDepth:       *maxDepth,
		MaxNodes:       *maxNodes,
		IncludeForward: *includeForward,
	})
	if err != nil {
		say("%v", err)
		return usageError
	}

	var out strings.Builder
	switch {
	case *asJSON:
		doc, err := report.JSON()
		if err != nil {
			say("encoding the report: %v", err)
			return usageError
		}
		out.Write(doc)
		out.WriteByte('\n')
	case *asList:
		writeImpactList(&out, report)
	default:
		writeImpactSummary(&out, report)
	}
	_, _ = io.WriteString(stdout, out.String())

	// An empty list that exits 0 would read as "nothing depends on this".
	res := report.Resolution
	if !report.Resolved() {
		say("%q matches too many nodes to pick one, so nothing was traversed; "+
			"pass an exact name or scope it with repo:, kind: or file:", query)
		return usageError
	}
	if res != nil && res.Ambiguous {
		say("%q matched several nodes; this report is about %s. "+
			"Pass that name, or a scope, to ask without the guess", query, res.Matched)
	}
	return 0
}

// writeImpactHead reports false when nothing was traversed.
func writeImpactHead(w *strings.Builder, report impact.Report) bool {
	if res := report.Resolution; res != nil {
		if res.Matched == "" {
			fmt.Fprintf(w, "%q matches several nodes; none was picked. Candidates:\n", res.Query)
			for _, c := range res.Candidates {
				fmt.Fprintf(w, "  %s (%s)  %s\n", c.Name, c.Kind, c.File)
			}
			return false
		}
		if n := len(report.Seeds); n > 0 {
			fmt.Fprintf(w, "%s is a file, not a node; this is the impact of the %d symbols it declares\n", res.Matched, n)
		} else {
			fmt.Fprintf(w, "resolved %q to %s\n", res.Query, res.Matched)
		}
		if len(res.Alternatives) > 0 {
			fmt.Fprintf(w, "  also matched: %s\n", strings.Join(res.Alternatives, ", "))
		}
	}
	if report.Summary == "" {
		fmt.Fprintln(w, "No dependents found.")
	} else {
		fmt.Fprintln(w, report.Summary)
	}
	return true
}

func writeImpactSummary(w *strings.Builder, report impact.Report) {
	if !writeImpactHead(w, report) {
		return
	}
	byKind, byModule := map[string]int{}, map[string]int{}
	depths := impactDepths(report.ByDepth)
	shown := 0
	for _, d := range depths {
		shown += len(report.ByDepth[d])
		for _, n := range report.ByDepth[d] {
			byKind[n.Kind]++
			byModule[impactModule(n)]++
		}
	}
	counts := func(title string, tally map[string]int, limit int) {
		if len(tally) == 0 {
			return
		}
		fmt.Fprintf(w, "\n%s:\n", title)
		for _, key := range topTally(tally, limit) {
			fmt.Fprintf(w, "  %-40s %d\n", key, tally[key])
		}
	}
	counts("by kind", byKind, len(byKind))
	if len(depths) > 0 {
		fmt.Fprint(w, "\nby depth:\n")
		for _, d := range depths {
			fmt.Fprintf(w, "  %-40d %d\n", d, len(report.ByDepth[d]))
		}
	}
	counts("modules with the most dependents", byModule, 5)
	writeImpactRiders(w, report)
	if report.Forward != nil {
		forward := map[string]int{}
		for _, n := range report.Forward.Nodes {
			if n.Depth > 0 {
				forward[n.Kind]++
			}
		}
		counts("what the target depends on, by kind", forward, len(forward))
	}
	if report.Stats.Truncated {
		// Not Stats.NodesVisited: that counts the walk, the target included, not what was kept.
		fmt.Fprintf(w, "\nThe counts above cover the %d dependents --max-nodes allowed; the total of %d is exact.\n",
			shown, report.TotalDependents)
	}
}

func writeImpactList(w *strings.Builder, report impact.Report) {
	if !writeImpactHead(w, report) {
		return
	}
	nodes := func(byDepth map[int][]facts.TraversalNode) {
		for _, d := range impactDepths(byDepth) {
			fmt.Fprintf(w, "\ndepth %d (%d):\n", d, len(byDepth[d]))
			for _, n := range byDepth[d] {
				where := n.File
				if n.Line > 0 {
					where = fmt.Sprintf("%s:%d", n.File, n.Line)
				}
				fmt.Fprintf(w, "  %s (%s)  %s\n", n.Name, n.Kind, where)
			}
		}
	}
	nodes(report.ByDepth)
	writeImpactRiders(w, report)
	if report.Forward != nil {
		forward := map[int][]facts.TraversalNode{}
		for _, n := range report.Forward.Nodes {
			if n.Depth > 0 {
				forward[n.Depth] = append(forward[n.Depth], n)
			}
		}
		if len(forward) > 0 {
			fmt.Fprint(w, "\nwhat the target depends on:\n")
			nodes(forward)
		}
	}
	if report.Stats.Truncated {
		fmt.Fprintf(w, "\nThe list stops at --max-nodes; the total of %d is exact.\n", report.TotalDependents)
	}
}

func writeImpactRiders(w *strings.Builder, report impact.Report) {
	if len(report.CrossRepoImpact) > 0 {
		fmt.Fprintf(w, "\nother repositories with a dependent:\n  %s\n", strings.Join(report.CrossRepoImpact, "\n  "))
	}
	if len(report.GoverningIntent) > 0 {
		fmt.Fprint(w, "\ngoverning pages:\n")
		for _, page := range report.GoverningIntent {
			line := page.Page
			if page.Type != "" || page.Status != "" {
				line += "  (" + strings.TrimSpace(page.Type+" "+page.Status) + ")"
			}
			fmt.Fprintf(w, "  %s\n", line)
		}
	}
}

func impactDepths(byDepth map[int][]facts.TraversalNode) []int {
	depths := make([]int, 0, len(byDepth))
	for d := range byDepth {
		depths = append(depths, d)
	}
	sort.Ints(depths)
	return depths
}

func impactModule(n facts.TraversalNode) string {
	if dir := path.Dir(n.File); n.File != "" && dir != "" {
		return dir
	}
	return n.Name
}

func topTally(tally map[string]int, limit int) []string {
	keys := make([]string, 0, len(tally))
	for key := range tally {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if tally[keys[i]] != tally[keys[j]] {
			return tally[keys[i]] > tally[keys[j]]
		}
		return keys[i] < keys[j]
	})
	if len(keys) > limit {
		keys = keys[:limit]
	}
	return keys
}
