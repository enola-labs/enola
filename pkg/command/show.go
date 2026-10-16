package command

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/diff"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/pkg/history"
)

// Show is `enola show <rev>`: what one recorded revision did to the architecture.
//
// `enola log` says a revision added twelve facts; this says which twelve. It reconstructs
// the revision and the one before it from the stored history and runs the SAME comparator
// the live loop uses (diff.Compute), so a past change is described in exactly the words the
// change itself was described in at the time. Nothing here re-reads the repository — the
// commit it describes may be long gone from the working tree.
func (r *Runner) Show(args []string) {
	fs := flag.NewFlagSet("show", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	asJSON := fs.Bool("json", false, "emit the delta as JSON")
	focus := fs.String("focus", "", "narrow the delta to entries referencing this module, file or symbol")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr,
			"Usage: "+r.name()+" show [flags] [<revision>] [repo_path|config_path]\n\n"+
				"Show what one recorded revision did to the architecture.\n\n"+
				"A revision is a snapshot id or its prefix, a git commit, HEAD~N, @<seq>, a ref\n"+
				"name, or `latest` (the default).\n\nFlags:\n")
		fs.PrintDefaults()
		r.writeHelpFooter(fs.Output(), args, "show")
	}
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	rev, repoArg := splitRevAndRepo(fs.Args())
	entries, root := r.historyFor(repoArg)

	target, err := history.Resolve(entries, rev)
	if err != nil {
		r.showFatal("%v", err)
	}
	parent := parentOf(entries, target)

	current := r.reconstruct(root, target, "revision "+target.Short())

	header := fmt.Sprintf("revision %s · %s", target.Short(), shortDate(target.At))
	if dec := decorations(target); dec != "" {
		header += " · " + strings.TrimSpace(dec)
	}

	// The first recorded revision has nothing before it, and diffing it against nothing
	// describes the entire codebase as the work of whoever ran enola first — every finding
	// filed under "Regressions introduced", every module under "added". The numbers would
	// be arithmetically true and would answer a question nobody asked. Report what the
	// revision HELD instead, and say why.
	if parent == nil {
		r.emitInitial(header, target, current, *asJSON)
		return
	}

	before := r.reconstruct(root, *parent, "the revision before "+target.Short())
	d := diff.Compute(before, current)
	if *focus != "" {
		d = d.Focused(*focus)
	}
	r.emitDiff(header, d, *asJSON, "show")
}

// Diff is `enola diff <a>..<b>`: the architecture delta between any two recorded revisions.
//
// `show` answers "what did this one change?"; this answers "what happened between these
// two?", which is the question a week of work produces. Both reconstruct and then call the
// same comparator, so neither can disagree with the other or with the live diff_snapshot.
func (r *Runner) Diff(args []string) {
	fs := flag.NewFlagSet("diff", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	asJSON := fs.Bool("json", false, "emit the delta as JSON")
	focus := fs.String("focus", "", "narrow the delta to entries referencing this module, file or symbol")
	store := fs.String("store", "", "also resolve revisions from a shared history store (default: history.shared_dir)")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr,
			"Usage: "+r.name()+" diff [flags] <revA>..<revB> [repo_path|config_path]\n\n"+
				"Show the architecture delta between two recorded revisions.\n\n"+
				"Each side is a snapshot id or its prefix, a git commit, HEAD~N, @<seq>, a ref\n"+
				"name, or `latest`. An omitted side means the oldest (left) or newest (right).\n\n"+
				"With a shared store (--store, or history.shared_dir) both sides resolve over\n"+
				"the union of the local history and the store, and the header names where\n"+
				"each side came from.\n\n"+
				"Flags:\n")
		fs.PrintDefaults()
		r.writeHelpFooter(fs.Output(), args, "diff")
	}
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	spec, repoArg := splitRevAndRepo(fs.Args())
	if spec == "" {
		r.diffFatal("needs a revision range, e.g. `%s diff HEAD~5..HEAD`", r.name())
	}
	leftSel, rightSel, ok := strings.Cut(spec, "..")
	if !ok {
		r.diffFatal("%q is not a range — use <revA>..<revB> (either side may be empty)", spec)
	}

	local, root, sh := r.historyWithStore(repoArg, *store, r.diffFatal)
	repo, err := history.UnionRepo(local, sh, "")
	if err != nil {
		r.diffFatal("%v", err)
	}
	revs := history.BuildUnion(local, sh, repo)
	entries := history.UnionEntries(revs)
	if len(entries) == 0 {
		r.diffFatal("no revisions recorded")
	}
	if strings.TrimSpace(leftSel) == "" {
		leftSel = entries[0].ID
	}

	left, err := history.Resolve(entries, leftSel)
	if err != nil {
		r.diffFatal("left side: %v", err)
	}
	right, err := history.Resolve(entries, rightSel)
	if err != nil {
		r.diffFatal("right side: %v", err)
	}

	leftSnap, leftOrigin := r.reconstructUnion(root, sh, revs, left, "revision "+left.Short())
	rightSnap, rightOrigin := r.reconstructUnion(root, sh, revs, right, "revision "+right.Short())
	d := diff.Compute(leftSnap, rightSnap)
	if *focus != "" {
		d = d.Focused(*focus)
	}
	header := fmt.Sprintf("%s..%s · %s → %s",
		left.Short(), right.Short(), shortDate(left.At), shortDate(right.At))
	if sh != nil {
		header += fmt.Sprintf(" · left from %s, right from %s", leftOrigin, rightOrigin)
	}
	r.emitDiff(header, d, *asJSON, "diff")
}

func (r *Runner) reconstructUnion(root string, sh *history.Share, revs []history.UnionRevision, e history.Entry, what string) (*facts.Snapshot, string) {
	if sh == nil {
		return r.reconstruct(root, e, what), "local"
	}
	for _, u := range revs {
		if u.Entry.ID != e.ID || u.Entry.Seq != e.Seq {
			continue
		}
		if u.Local && u.Entry.Blob != nil {
			snap, err := history.Load(root, u.Entry)
			if err == nil {
				return snap, "local"
			}
			if !errors.Is(err, history.ErrThinned) {
				r.diffFatal("reconstructing %s: %v", what, err)
			}
		}
		if u.Record != nil {
			snap, err := sh.LoadSnapshot(*u.Record)
			if err != nil {
				r.diffFatal("reconstructing %s from the shared store: %v", what, err)
			}
			return snap, "store:" + u.Record.Source
		}
	}
	return r.reconstruct(root, e, what), "local"
}

// historyFor resolves the positional repo argument to the recorded entries and their root,
// failing with the same guidance `log` gives when there is nothing to read.
func (r *Runner) historyFor(repoArg string) ([]history.Entry, string) {
	repoPath, cfg := r.logTarget(repoArg)
	root, err := history.Root(repoPath, cfg.History.Dir)
	if err != nil {
		r.showFatal("cannot locate the history: %v", err)
	}
	entries, err := history.Read(root)
	if err != nil {
		if errors.Is(err, history.ErrNoHistory) {
			r.reportNoHistory(repoPath, root, cfg)
			os.Exit(0)
		}
		r.showFatal("%v", err)
	}
	// Timeline order, not write order: parentOf means "the revision before this one" and a
	// backfill appends old revisions after new ones.
	return history.SortedByTime(entries), root
}

// reconstruct loads a revision's stored contents, translating the one expected failure —
// contents dropped by retention — into an explanation rather than an error, since the
// revision is still in the timeline and its contents are re-derivable.
func (r *Runner) reconstruct(root string, e history.Entry, what string) *facts.Snapshot {
	snap, err := history.Load(root, e)
	if err != nil {
		if errors.Is(err, history.ErrThinned) {
			at := e.Commit()
			if at == "" {
				at = "the tree it was taken over"
			}
			r.showFatal("%s is no longer stored in full — only its summary line survives.\n"+
				"Older revisions keep their header and drop their contents; re-snapshot %s to replay it.",
				what, at)
		}
		r.showFatal("reconstructing %s: %v", what, err)
	}
	return snap
}

// parentOf returns the revision recorded immediately before e, or nil when e is the first.
//
// Position in the log, not git ancestry: the log is what was OBSERVED, and consecutive
// entries are the pair whose delta was actually measured. Using git ancestry here would
// pair revisions enola never compared and report a delta nobody's change produced.
func parentOf(entries []history.Entry, e history.Entry) *history.Entry {
	for i, cand := range entries {
		if cand.ID == e.ID && cand.Seq == e.Seq {
			if i == 0 {
				return nil
			}
			return &entries[i-1]
		}
	}
	return nil
}

// splitRevAndRepo separates an optional revision selector from an optional repo/config
// path. A bare argument naming a directory or an existing file is the target; otherwise it
// is the revision.
func splitRevAndRepo(args []string) (rev, repo string) {
	for _, a := range args {
		if isDirectory(a) || fileExists(a) {
			repo = a
			continue
		}
		if rev == "" {
			rev = a
		}
	}
	return rev, repo
}

// emitInitial reports what the first recorded revision held, rather than pretending its
// whole graph was introduced by it.
func (r *Runner) emitInitial(header string, e history.Entry, snap *facts.Snapshot, asJSON bool) {
	byKind := map[string]int{}
	for _, f := range snap.Facts {
		byKind[f.Kind]++
	}

	if asJSON {
		out, err := json.MarshalIndent(map[string]any{
			"revision":       e.ID,
			"at":             e.At,
			"initial":        true,
			"facts":          len(snap.Facts),
			"facts_by_kind":  byKind,
			"findings":       len(snap.Insights),
			"receipt":        snap.Meta.Receipt(),
			"no_predecessor": "this is the first recorded revision, so there is no delta",
		}, "", "  ")
		if err != nil {
			r.showFatal("failed to encode the revision: %v", err)
		}
		fmt.Println(string(out))
		return
	}

	fmt.Printf("# %s\n\n", header)
	fmt.Println("This is the FIRST recorded revision, so there is nothing before it to compare")
	fmt.Println("against. What follows is what the graph held at this point, not what changed.")
	fmt.Println()
	fmt.Printf("  facts     %d\n", len(snap.Facts))
	for _, kind := range sortedKeys(byKind) {
		fmt.Printf("    %-12s %d\n", kind, byKind[kind])
	}
	fmt.Printf("  findings  %d\n", len(snap.Insights))
	if snap.Meta.EnolaVersion != "" {
		fmt.Printf("  built by  enola %s · extractors %s\n",
			snap.Meta.EnolaVersion, strings.Join(snap.Meta.Extractors, ", "))
	}
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// emitDiff renders a computed delta the same way for both commands.
//
// RenderCompact rather than RenderSummary: someone who asked about one specific past
// revision has already narrowed the question, and the answer they want is which symbols and
// edges moved — the headline tally is what `log` already gave them.
func (r *Runner) emitDiff(header string, d *diff.SnapshotDiff, asJSON bool, cmd string) {
	if asJSON {
		out, err := json.MarshalIndent(d, "", "  ")
		if err != nil {
			r.cmdFatal(cmd, "failed to encode the delta: %v", err)
		}
		fmt.Println(string(out))
		return
	}
	fmt.Printf("# %s\n\n", header)
	if d.Empty() {
		fmt.Println("No architectural change.")
		return
	}
	fmt.Print(d.RenderCompact())
}

func (r *Runner) showFatal(format string, args ...any) { r.cmdFatal("show", format, args...) }
func (r *Runner) diffFatal(format string, args ...any) { r.cmdFatal("diff", format, args...) }
