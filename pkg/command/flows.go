package command

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/flows"
	"github.com/enola-labs/enola/pkg/bootstrap"
	"github.com/enola-labs/enola/pkg/check"
)

// Flows ranks the ways into a program by what each reaches.
func (r *Runner) Flows(ctx context.Context, args []string) {
	os.Exit(r.runFlows(ctx, args, os.Stdout, os.Stderr))
}

func (r *Runner) runFlows(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	usageError := check.StatusUsageError.ExitCode()
	say := func(format string, args ...any) {
		_, _ = fmt.Fprintf(stderr, r.name()+" flows: "+format+"\n", args...)
	}

	fs := flag.NewFlagSet("flows", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		top     = fs.Int("top", 10, "how many flows to list (1-500)")
		show    = fs.String("show", "", "describe one flow: its stages, what it reaches and a summary. Takes a flow's name or a part of it that only one has")
		through = fs.String("through", "", "list only the flows that reach this symbol")
		asJSON  = fs.Bool("json", false, "emit the report as JSON, every listed flow with its stages and summary")
	)
	fs.Usage = func() {
		_, _ = fmt.Fprint(stderr,
			"Usage: "+r.name()+" flows [flags] [repo_path|config_path]\n\n"+
				"Rank the ways into a program by what each reaches. An entry is a served\n"+
				"route's handler, a program's main, or a symbol a framework invokes; its flow\n"+
				"is every symbol the calls from it reach. With no flags it prints the largest\n"+
				"flows, one line each.\n\n"+
				"A program reached through one main is one flow. Its subcommands are steps of\n"+
				"that flow, shown by --show, and so is a handler written as a function literal,\n"+
				"which has no symbol of its own. A library has no entry and lists nothing.\n\n"+
				"A flow is a floor, not a measurement: the walk ends at any call whose target\n"+
				"is no symbol in the graph. The unresolved column counts those calls. Most of\n"+
				"them leave the repository, for the standard library or an imported package;\n"+
				"the rest are calls on a value whose type the extractor could not name.\n"+
				"Entries that reach the same symbols are folded into one flow, and a flow that\n"+
				"sits almost wholly inside a larger one says which.\n\n"+
				"The program's flows are listed first. A flow whose entry is an example, a\n"+
				"script, a migration or build tooling is marked side and listed after them,\n"+
				"however much it reaches.\n\n"+
				"Exit codes:\n"+
				"  0  the report was produced, including one that found no flows\n"+
				"  2  the command could not run, or --show or --through named nothing it\n"+
				"     could pick\n\nFlags:\n")
		fs.PrintDefaults()
		r.writeHelpFooter(fs.Output(), args, "flows")
	}
	if err := fs.Parse(args); err != nil {
		return usageError
	}
	rest := fs.Args()
	if *top < 1 || *top > 500 {
		say("--top %d is outside 1-500", *top)
		return usageError
	}
	if *show != "" && *through != "" {
		say("--show describes one flow and --through lists several; pass one")
		return usageError
	}
	if len(rest) > 1 {
		say("expected [repo_path|config_path], got %d arguments; quote a flow name that contains a space", len(rest))
		return usageError
	}
	for _, arg := range rest {
		if strings.HasPrefix(arg, "-") {
			say("flags go before the path; %q came after it", arg)
			return usageError
		}
	}
	target := ""
	if len(rest) > 0 {
		target = rest[0]
	}

	tgt := r.resolveTarget(target)
	say("%s", tgt.configNote)

	// Read-only, like impact and endpoint.
	tgt.engine.SetPersistCache(false)
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
			tgt.engine.SetDeferLinking(i < len(tgt.repoPaths)-1)
			if _, err := tgt.engine.GenerateSnapshot(ctx, repoPath, i > 0); err != nil {
				say("snapshot generation failed for %s: %v", repoPath, err)
				return usageError
			}
		}
	}

	store := tgt.engine.Resolver().Store
	if store == nil || store.Count() == 0 {
		say("the snapshot holds no facts")
		return usageError
	}
	analysis := flows.Analyze(store, flows.Options{})
	multiRepo := len(store.RepoLabels()) > 1

	var out strings.Builder
	switch {
	case *show != "":
		i, candidates := analysis.Find(*show)
		if i < 0 {
			if len(candidates) == 0 {
				say("no flow is named %q; run without --show to list them", *show)
			} else {
				say("%q names %d flows; pass one of them whole:\n  %s", *show, len(candidates), strings.Join(candidates, "\n  "))
			}
			return usageError
		}
		analysis.DescribeFlow(i)
		if *asJSON {
			if !writeFlowsJSON(&out, analysis.Flows[i]) {
				say("encoding the report failed")
				return usageError
			}
		} else {
			writeFlowDetail(&out, analysis.Flows[i])
		}
	case *through != "":
		indexes, known := analysis.Through(*through)
		if !known {
			say("no symbol is named %q", *through)
			return usageError
		}
		listed := pickFlows(analysis, indexes, *top)
		if *asJSON {
			if !writeFlowsJSON(&out, flowsReport{Through: *through, Total: len(indexes), Flows: listed}) {
				say("encoding the report failed")
				return usageError
			}
		} else {
			if len(indexes) == 0 {
				fmt.Fprintf(&out, "No flow reaches %s.\n", *through)
			} else {
				verb := "reach"
				if len(indexes) == 1 {
					verb = "reaches"
				}
				fmt.Fprintf(&out, "%s %s %s\n\n", plural(len(indexes), "flow"), verb, *through)
				writeFlowTable(&out, listed, multiRepo)
				writeFlowsCut(&out, len(listed), len(indexes))
			}
		}
	default:
		indexes := make([]int, len(analysis.Flows))
		for i := range indexes {
			indexes[i] = i
		}
		listed := pickFlows(analysis, indexes, *top)
		if *asJSON {
			if !writeFlowsJSON(&out, flowsReport{Roots: analysis.Roots, Leaves: &analysis.Leaves, Total: len(analysis.Flows), Flows: listed}) {
				say("encoding the report failed")
				return usageError
			}
		} else {
			writeFlowsHead(&out, analysis)
			if len(listed) > 0 {
				fmt.Fprintln(&out)
				writeFlowTable(&out, listed, multiRepo)
				writeFlowsCut(&out, len(listed), len(analysis.Flows))
				fmt.Fprint(&out, "\nreach: symbols the calls from the entry reach. unresolved: calls whose target\nis no symbol in the graph, most of them into the standard library or an\nimported package.\n")
			}
		}
	}
	_, _ = io.WriteString(stdout, out.String())
	return 0
}

// flowsReport is the JSON a listing emits. Roots and Leaves describe the whole
// repository, so a --through listing, which is a subset, leaves them out.
type flowsReport struct {
	Through string         `json:"through,omitempty"`
	Roots   map[string]int `json:"roots,omitempty"`
	Leaves  *int           `json:"leaves,omitempty"`
	// Total is how many flows the listing was cut from.
	Total int          `json:"total"`
	Flows []flows.Flow `json:"flows"`
}

// pickFlows describes and returns the first limit flows of indexes.
func pickFlows(a *flows.Analysis, indexes []int, limit int) []flows.Flow {
	if len(indexes) > limit {
		indexes = indexes[:limit]
	}
	out := make([]flows.Flow, 0, len(indexes))
	for _, i := range indexes {
		a.DescribeFlow(i)
		out = append(out, a.Flows[i])
	}
	return out
}

func writeFlowsJSON(w *strings.Builder, v any) bool {
	doc, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return false
	}
	w.Write(doc)
	w.WriteByte('\n')
	return true
}

// writeFlowsHead says how many entries were found and what became of them, so a
// short or empty ranking explains itself.
func writeFlowsHead(w *strings.Builder, a *flows.Analysis) {
	total := 0
	mechanisms := make([]string, 0, len(a.Roots))
	for m, n := range a.Roots {
		total += n
		mechanisms = append(mechanisms, m)
	}
	sort.Slice(mechanisms, func(i, j int) bool {
		if a.Roots[mechanisms[i]] != a.Roots[mechanisms[j]] {
			return a.Roots[mechanisms[i]] > a.Roots[mechanisms[j]]
		}
		return mechanisms[i] < mechanisms[j]
	})
	if total == 0 {
		fmt.Fprint(w, "No entries found: no route with a handler the graph names, no main, and no\nsymbol marked as invoked by a framework.\n")
		return
	}
	parts := make([]string, len(mechanisms))
	for i, m := range mechanisms {
		parts[i] = fmt.Sprintf("%d %s", a.Roots[m], m)
	}
	entries := "entries"
	if total == 1 {
		entries = "entry"
	}
	fmt.Fprintf(w, "%s from %d %s (%s)", plural(len(a.Flows), "flow"), total, entries, strings.Join(parts, ", "))
	if a.Leaves > 0 {
		fmt.Fprintf(w, "; %d reach nothing the graph follows", a.Leaves)
	}
	fmt.Fprintln(w)
}

func writeFlowTable(w *strings.Builder, listed []flows.Flow, multiRepo bool) {
	name := func(f flows.Flow) string {
		if multiRepo && f.Repo != "" {
			return f.Repo + ": " + f.Name
		}
		return f.Name
	}
	width := len("flow")
	for _, f := range listed {
		if n := len(name(f)); n > width {
			width = n
		}
	}
	fmt.Fprintf(w, "%4s  %-*s  %6s  %7s  %10s  %s\n", "#", width, "flow", "reach", "modules", "unresolved", "note")
	for _, f := range listed {
		reach := fmt.Sprint(f.Metrics.Reach)
		if f.Metrics.Truncated {
			reach += "+"
		}
		line := fmt.Sprintf("%4d  %-*s  %6s  %7d  %10d  %s", f.Rank, width, name(f), reach, f.Metrics.Modules, f.Metrics.UnresolvedCalls, flowNote(f))
		fmt.Fprintln(w, strings.TrimRight(line, " "))
	}
}

// flowNote is the last column of a listing: the entries folded into this flow,
// and the larger flow it sits inside.
func flowNote(f flows.Flow) string {
	var notes []string
	if f.Side {
		notes = append(notes, "side")
	}
	if n := len(f.SameReachAs); n > 0 {
		shown := f.SameReachAs
		more := ""
		if n > 2 {
			shown, more = shown[:2], fmt.Sprintf(" and %d more", n-2)
		}
		notes = append(notes, "also "+strings.Join(shown, ", ")+more)
	}
	if len(f.NestedIn) > 0 {
		notes = append(notes, "inside "+f.NestedIn[0])
	}
	return strings.Join(notes, "; ")
}

func writeFlowsCut(w *strings.Builder, shown, total int) {
	if shown < total {
		fmt.Fprintf(w, "\n%d of %d shown; raise --top to see more.\n", shown, total)
	}
}

func writeFlowDetail(w *strings.Builder, f flows.Flow) {
	fmt.Fprintln(w, f.Name)
	where := f.Entry.File
	if f.Entry.Line > 0 {
		where = fmt.Sprintf("%s:%d", f.Entry.File, f.Entry.Line)
	}
	row := func(label, format string, args ...any) {
		fmt.Fprintf(w, "  %-8s "+format+"\n", append([]any{label}, args...)...)
	}
	row("rank", "%d", f.Rank)
	if f.Side {
		row("side", "%s", "the entry is not the program: an example, a script, a migration or tooling")
	}
	row("entry", "%s  %s", f.Entry.Name, where)
	for _, route := range f.Routes[min(1, len(f.Routes)):] {
		row("also", "%s", strings.TrimSpace(route.Method+" "+route.Name))
	}
	reach := fmt.Sprintf("%s, %s, depth %d", plural(f.Metrics.Reach, "symbol"), plural(f.Metrics.Modules, "module"), f.Metrics.MaxDepth)
	if f.Metrics.Truncated {
		reach += " (the walk stopped at its bound; the flow is larger)"
	}
	row("reach", "%s", reach)
	if f.Metrics.PerformsIO > 0 {
		row("io", "%s perform I/O", plural(f.Metrics.PerformsIO, "symbol"))
	}
	blind := plural(f.Metrics.UnresolvedCalls, "call") + " not followed"
	if f.Metrics.ExternalCalls > 0 {
		blind += fmt.Sprintf(", %d of them into imported packages", f.Metrics.ExternalCalls)
	}
	row("blind", "%s", blind)
	if len(f.SameReachAs) > 0 {
		row("same", "%s", strings.Join(f.SameReachAs, ", "))
	}
	if len(f.NestedIn) > 0 {
		row("inside", "%s", strings.Join(f.NestedIn, ", "))
	}

	fmt.Fprint(w, "\nSummary\n")
	for _, line := range wrap(f.Summary, 76) {
		fmt.Fprintf(w, "  %s\n", line)
	}

	if len(f.Stages) > 0 {
		fmt.Fprint(w, "\nStages\n")
		width := 0
		labels := make([]string, len(f.Stages))
		for i, s := range f.Stages {
			labels[i] = strings.Repeat("  ", s.Depth-1) + flows.ShortName(s.Name)
			if n := len(labels[i]); n > width {
				width = n
			}
		}
		for i, s := range f.Stages {
			line := fmt.Sprintf("  %-*s  %5d  %s", width, labels[i], s.Reach, clip(s.Doc, 72))
			fmt.Fprintln(w, strings.TrimRight(line, " "))
		}
	}

	if len(f.Modules) > 0 {
		fmt.Fprint(w, "\nModules\n")
		shown := f.Modules
		if len(shown) > 8 {
			shown = shown[:8]
		}
		for _, m := range shown {
			fmt.Fprintf(w, "  %-40s %d\n", m.Name, m.Symbols)
		}
		if rest := f.Metrics.Modules - len(shown); rest > 0 {
			fmt.Fprintf(w, "  and %d more\n", rest)
		}
	}
}

// clip shortens a line of prose to fit a column, at a word.
func clip(text string, width int) string {
	if len(text) <= width {
		return text
	}
	cut := strings.LastIndexByte(text[:width], ' ')
	if cut <= 0 {
		return text
	}
	return text[:cut] + " ..."
}
