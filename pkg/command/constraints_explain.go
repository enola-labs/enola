package command

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/enola-labs/enola/internal/explainers/constraints"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/intent"
	"github.com/enola-labs/enola/pkg/bootstrap"
	"github.com/enola-labs/enola/pkg/check"
)

// ConstraintsExplain is `enola constraints explain <path|part> [repo_path]`:
// why a file belongs where it belongs, who reaches it, and which verdicts
// would change if it left every part; or, for a declared part, its members,
// public face, edges by part and the laws naming it. Everything is read off
// the same membership the evaluator verdicts on.
func (r *Runner) ConstraintsExplain(args []string) {
	fs := flag.NewFlagSet("constraints explain", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	asJSON := fs.Bool("json", false, "print the explanation as JSON")
	noRadius := fs.Bool("no-radius", false, "skip the re-evaluation that names the verdicts a move would change")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "Usage: "+r.name()+" constraints explain [--json] [--no-radius] <path|part> [repo_path]\n\n"+
			"For a file: the components its facts belong to and the selector that admitted\n"+
			"each, the edges the file makes, the edges landing on it by kind and part, and\n"+
			"the verdicts that would appear or vanish if the file left every part.\n"+
			"For a declared part: its members by file, public face, edges in and out by\n"+
			"part, and the laws naming it. Read from the current snapshot, never a gate.\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil || fs.NArg() < 1 {
		fs.Usage()
		os.Exit(check.StatusUsageError.ExitCode())
	}
	target := fs.Arg(0)
	repoPath := "."
	if fs.NArg() > 1 {
		repoPath = fs.Arg(1)
	}
	repoPath, _ = filepath.Abs(repoPath)
	rel := target
	if filepath.IsAbs(target) {
		if p, err := filepath.Rel(repoPath, target); err == nil {
			rel = p
		}
	}
	tgt := r.resolveTarget(repoPath)
	tgt.engine.SetPersistCache(false)
	snap, err := bootstrap.LoadSnapshotDir(tgt.engine.OutputDir(repoPath))
	if err != nil {
		fmt.Fprintf(os.Stderr, "constraints explain: no snapshot for %s; generate one first\n", repoPath)
		os.Exit(check.StatusUsageError.ExitCode())
	}
	decl, err := intent.LoadRepoFile(repoPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "constraints explain: %v\n", err)
		os.Exit(check.StatusUsageError.ExitCode())
	}
	store := facts.NewStore()
	for _, f := range snap.Facts {
		if f.Kind != facts.KindIntent {
			store.Add(f)
		}
	}
	label := filepath.Base(repoPath)
	if decl != nil {
		for _, f := range intent.CompileFacts(decl) {
			if f.Repo == "" {
				f.Repo = label
			}
			store.Add(f)
		}
	}
	origin := &constraints.Origin{GeneratedAt: snap.Meta.GeneratedAt}
	if snap.Meta.Git != nil {
		origin.Commit, origin.Dirty = snap.Meta.Git.Commit, snap.Meta.Git.Dirty
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")

	if part, ok := constraints.ExplainPart(store, rel); ok && !fileExists(filepath.Join(repoPath, rel)) {
		part.Origin = origin
		if *asJSON {
			_ = enc.Encode(part)
			return
		}
		fmt.Print(RenderPartExplanation(part))
		return
	}

	explanation := constraints.ExplainFile(store, rel)
	explanation.Origin = origin
	if !*noRadius && len(explanation.Facts) > 0 {
		radius := constraints.BlastRadiusAgainst(store, []string{rel}, snap.Insights)
		explanation.Radius = &radius
	}
	if *asJSON {
		_ = enc.Encode(explanation)
		return
	}
	fmt.Print(RenderFileExplanation(explanation))
}

// RenderFileExplanation is the text form, one fact per line, one component
// per line with its selector, one edge per line with its line number, one
// incoming group per line, and the radius as two lists.
func RenderFileExplanation(e constraints.FileExplanation) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", e.File)
	renderOrigin(&b, e.Origin)
	b.WriteString("\n")
	if len(e.Facts) == 0 {
		b.WriteString("  no measured fact lives in this file\n")
		return b.String()
	}
	b.WriteString("  measured facts:\n")
	for _, f := range e.Facts {
		fmt.Fprintf(&b, "    %s\n", f)
	}
	if len(e.Memberships) == 0 {
		b.WriteString("  components: none; no declared selector admits a fact in this file\n")
	} else {
		b.WriteString("  components:\n")
		for _, m := range e.Memberships {
			fmt.Fprintf(&b, "    %s: %s (declared in %s)\n", m.Component, m.Selector, m.Source)
			for _, member := range m.Members {
				fmt.Fprintf(&b, "      %s\n", member)
			}
		}
	}
	if len(e.Outgoing) > 0 {
		b.WriteString("  outgoing edges:\n")
		for _, edge := range e.Outgoing {
			fmt.Fprintf(&b, "    %4d  %s --%s--> %s\n", edge.Line, edge.From, edge.Kind, edge.Target)
		}
	}
	if len(e.Incoming) == 0 {
		b.WriteString("  incoming edges: none\n")
	} else {
		b.WriteString("  incoming edges (by kind and the part the source belongs to):\n")
		for _, g := range e.Incoming {
			fmt.Fprintf(&b, "    %-12s from %-24s %6d  e.g. %s\n", g.Kind, g.Component, g.Count, strings.Join(g.Sources, ", "))
		}
	}
	if e.Radius != nil {
		renderRadius(&b, *e.Radius)
	}
	return b.String()
}

// RenderPartExplanation is the text form of a part: files and members, the
// public face, fan-in and fan-out by part, and the laws.
func RenderPartExplanation(p constraints.PartExplanation) string {
	var b strings.Builder
	fmt.Fprintf(&b, "part %s: %s (declared in %s)\n", p.Part, p.Selector, p.Source)
	renderOrigin(&b, p.Origin)
	b.WriteString("\n")
	if len(p.Files) == 0 {
		b.WriteString("  members: none; the selector admits no measured fact\n")
	} else {
		fmt.Fprintf(&b, "  members in %d file(s):\n", len(p.Files))
		for _, f := range p.Files {
			fmt.Fprintf(&b, "    %s\n", f.File)
			for _, m := range f.Members {
				fmt.Fprintf(&b, "      %s\n", m)
			}
		}
	}
	if len(p.PublicFace) == 0 {
		b.WriteString("  public face: none declared\n")
	} else {
		b.WriteString("  public face:\n")
		for _, m := range p.PublicFace {
			fmt.Fprintf(&b, "    %s\n", m)
		}
	}
	renderPartEdges(&b, "fan-in (edges landing on the part, by source part)", p.FanIn)
	renderPartEdges(&b, "fan-out (edges the part makes, by target part)", p.FanOut)
	if len(p.Laws) == 0 {
		b.WriteString("  laws naming the part: none\n")
	} else {
		b.WriteString("  laws naming the part:\n")
		for _, l := range p.Laws {
			fmt.Fprintf(&b, "    %s [%s]: %s\n      because: %s\n", l.Rule, l.Mode, l.Statement, l.Because)
		}
	}
	return b.String()
}

func renderPartEdges(b *strings.Builder, heading string, edges []constraints.PartEdges) {
	if len(edges) == 0 {
		fmt.Fprintf(b, "  %s: none\n", heading)
		return
	}
	fmt.Fprintf(b, "  %s:\n", heading)
	for _, e := range edges {
		fmt.Fprintf(b, "    %-24s %6d\n", e.Component, e.Count)
	}
}

func renderOrigin(b *strings.Builder, o *constraints.Origin) {
	if o == nil {
		return
	}
	commit := o.Commit
	if len(commit) > 12 {
		commit = commit[:12]
	}
	switch {
	case commit == "" && o.GeneratedAt == "":
		b.WriteString("  read from: the current snapshot (no receipt)\n")
	case commit == "":
		fmt.Fprintf(b, "  read from: the snapshot generated %s\n", o.GeneratedAt)
	case o.Dirty:
		fmt.Fprintf(b, "  read from: the snapshot at %s with uncommitted changes, generated %s\n", commit, o.GeneratedAt)
	default:
		fmt.Fprintf(b, "  read from: the snapshot at %s, generated %s\n", commit, o.GeneratedAt)
	}
}

func renderRadius(b *strings.Builder, r constraints.BlastRadius) {
	fmt.Fprintf(b, "  if %s left every part (%d rule(s) re-run over the loaded snapshot):\n", strings.Join(r.Files, ", "), r.RulesRun)
	renderRadiusList(b, "would start failing", r.Appear)
	renderRadiusList(b, "would stop being checked", r.Vanish)
	for _, nc := range r.NotComputed {
		fmt.Fprintf(b, "    not computed: rule %s (%s)\n", nc.Rule, nc.Cause)
	}
}

func renderRadiusList(b *strings.Builder, heading string, verdicts []constraints.RadiusVerdict) {
	if len(verdicts) == 0 {
		fmt.Fprintf(b, "    %s: nothing\n", heading)
		return
	}
	fmt.Fprintf(b, "    %s:\n", heading)
	for _, v := range verdicts {
		fmt.Fprintf(b, "      %s\n", v.Title)
		if v.Because != "" {
			fmt.Fprintf(b, "        because: %s\n", v.Because)
		}
		if v.Cut != "" {
			fmt.Fprintf(b, "        cut: %s\n", v.Cut)
		}
	}
}
