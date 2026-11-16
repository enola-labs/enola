package command

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/enola-labs/enola/internal/engine"
	inthistory "github.com/enola-labs/enola/internal/history"
)

// GC is `enola gc`: report what a history holds and remove what it no longer needs.
//
// It exists because the alternative turned out to be a hand-written script. Repairing a
// history twice while building this — once after a backfill wrote a false timeline, once
// after a retention bug stranded most of it — meant rewriting log.jsonl and deleting
// directories by hand, against a format whose invariants are exactly what a hand edit gets
// wrong. A tool that knows a segment is a chain is not a convenience here.
//
// With no flags it removes only GARBAGE: segment directories no revision refers to, which
// an interrupted write or an earlier hand edit can leave behind. Anything that loses
// something a reader could still reach needs an explicit flag, because "reclaim some space"
// and "discard the last six months" should not be the same command typed the same way.
func (r *Runner) GC(args []string) {
	fs := flag.NewFlagSet("gc", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		dryRun       = fs.Bool("dry-run", false, "report what would be removed and change nothing")
		olderThan    = fs.String("thin-older-than", "", "drop the stored contents of revisions older than this (e.g. 90d, 720h); their summary lines stay")
		pruneWorking = fs.Bool("prune-working", false, "remove working revisions (snapshots of uncommitted trees) from the log entirely")
		only         = fs.String("repo", "", "in a cluster, collect only this member (its folder name)")
		pruneCopies  = fs.Bool("prune-cluster-copies", false, "remove the copies of a cluster's graph older enola wrote into members: from their output dirs (a pinned one moves to the cluster's own directory) and from their histories")
	)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr,
			"Usage: "+r.name()+" gc [flags] [repo_path|config_path]\n\n"+
				"Report what this repository's architecture history holds, and remove what it no\n"+
				"longer needs.\n\n"+
				"With no flags it removes only garbage — segment directories no revision refers\n"+
				"to. Thinning and pruning discard things a reader could still reach, so each\n"+
				"needs asking for.\n\n"+
				"Revisions whose contents are dropped keep their summary line: the timeline stays\n"+
				"complete, and only replay is lost. `log --backfill` can rebuild them.\n\n"+
				"A cluster config, or a folder of repositories (the default when run from one),\n"+
				"collects the cluster's own history and every member's; --repo picks one member.\n\n"+
				"Flags:\n")
		fs.PrintDefaults()
		r.writeHelpFooter(fs.Output(), args, "gc")
	}
	if err := fs.Parse(flagsFirst(fs, args)); err != nil {
		os.Exit(2)
	}

	var arg string
	if rest := fs.Args(); len(rest) > 0 {
		arg = rest[0]
	}
	scope := r.historyTargetOf("gc", arg)
	roots, err := scope.roots()
	if err != nil {
		r.gcFatal("cannot locate the history: %v", err)
	}
	if *only != "" {
		if scope.cluster == "" {
			r.gcFatal("--repo picks a member of a cluster, and %s is one repository", scope.repos[0])
		}
		roots = rootsFor(roots, *only)
		if len(roots) == 0 {
			r.gcFatal("no repository %q in %s; members: %s", *only, scope.cluster, strings.Join(memberLabels(scope.repos), ", "))
		}
	}

	r.gcClusterCopies(scope, *only, *pruneCopies, *dryRun)

	opts := inthistory.GCOptions{DryRun: *dryRun, PruneWorking: *pruneWorking}
	if *olderThan != "" {
		d, err := parseAge(*olderThan)
		if err != nil {
			r.gcFatal("%v", err)
		}
		opts.ThinOlderThan = d
	}

	collect := func(hr historyRoot) (inthistory.GCReport, error) {
		o := opts
		// Every revision of a cluster's own history is multi-repo; only a repository's
		// own history holds copies.
		o.PruneClusterCopies = *pruneCopies && !hr.cluster
		return inthistory.GC(hr.root, o)
	}

	if scope.cluster == "" || *only != "" {
		rep, err := collect(roots[0])
		if err != nil {
			r.gcFatal("%v", err)
		}
		fmt.Print(renderGC(rep, *dryRun, roots[0].root, false))
		return
	}

	// A cluster has its own history, of snapshots of the whole cluster, and each member
	// keeps its own of snapshots of it alone; all of them are collected. One that fails
	// does not stop the rest: they are independent directories.
	fmt.Printf("Cluster %s: %d repositor%s\n", scope.cluster, len(scope.repos), pluralY(len(scope.repos)))
	var total inthistory.GCReport
	failed := 0
	for _, hr := range roots {
		name := strings.Join(hr.labels, ", ")
		if hr.cluster {
			name = "cluster"
		}
		fmt.Printf("\n== %s\n", name)
		rep, err := collect(hr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s gc: %s: %v\n", r.name(), name, err)
			failed++
			continue
		}
		fmt.Print(renderGC(rep, *dryRun, hr.root, hr.cluster))
		total.Revisions += rep.Revisions
		total.BytesBefore += rep.BytesBefore
		total.BytesFreed += rep.BytesFreed
	}
	verb := "reclaimed"
	if *dryRun {
		verb = "would be reclaimed"
	}
	fmt.Printf("\nTotal: %d revision%s, %s stored, %s %s\n",
		total.Revisions, pluralS(total.Revisions), humanBytes(total.BytesBefore), humanBytes(total.BytesFreed), verb)
	if failed > 0 {
		r.gcFatal("%d of %d histories could not be collected", failed, len(roots))
	}
}

// parseAge accepts a Go duration or a plain day count ("90d"), because "older than ninety
// days" is the way anybody actually says this and 2160h is not.
func parseAge(s string) (time.Duration, error) {
	if len(s) > 1 && s[len(s)-1] == 'd' {
		var days int
		if _, err := fmt.Sscanf(s[:len(s)-1], "%d", &days); err == nil && days >= 0 {
			return time.Duration(days) * 24 * time.Hour, nil
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("--thin-older-than %q is neither a day count (90d) nor a duration (720h)", s)
	}
	return d, nil
}

// renderGC states what is there before what changed, because the report is useful on its
// own — "how much history do I have and how much of it can I still replay" has no other
// answer.
func renderGC(rep inthistory.GCReport, dryRun bool, root string, cluster bool) string {
	var b []byte
	add := func(format string, args ...any) { b = append(b, fmt.Sprintf(format, args...)...) }

	add("%s\n\n", root)
	if rep.Revisions == 0 {
		add("No revisions recorded.\n")
		return string(b)
	}

	add("  %-14s %d\n", "revisions", rep.Revisions)
	add("    %-12s %d  (replayable)\n", "with contents", rep.Replayable)
	if rep.Thinned > 0 {
		add("    %-12s %d  (contents dropped; `log --backfill` can rebuild them)\n", "header only", rep.Thinned)
	}
	if rep.Working > 0 {
		add("    %-12s %d  (snapshots of uncommitted trees)\n", "working", rep.Working)
	}
	if rep.MultiRepo > 0 && !cluster {
		add("    %-12s %d  (copies of cluster snapshots; --prune-cluster-copies removes them)\n", "cluster", rep.MultiRepo)
	}
	add("  %-14s %d\n", "segments", rep.Segments)
	add("  %-14s %s\n", "stored", humanBytes(rep.BytesBefore))

	if len(rep.OrphanSegments) == 0 && len(rep.ThinnedSegments) == 0 && rep.PrunedEntries == 0 {
		add("\nNothing to remove.\n")
		return string(b)
	}

	verb := "Removed"
	if dryRun {
		verb = "Would remove"
	}
	add("\n%s:\n", verb)
	if n := len(rep.OrphanSegments); n > 0 {
		add("  %d orphaned segment(s) — on disk, referred to by no revision\n", n)
	}
	if n := len(rep.ThinnedSegments); n > 0 {
		add("  %d segment(s) past the age cutoff; those revisions keep their summary line\n", n)
	}
	if rep.PrunedEntries > 0 {
		add("  %d revision(s) from the log\n", rep.PrunedEntries)
	}
	add("  %s reclaimed\n", humanBytes(rep.BytesFreed))
	return string(b)
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func (r *Runner) gcFatal(format string, args ...any) { r.cmdFatal("gc", format, args...) }

func pluralY(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

// rootsFor keeps the history that the member labelled label records into.
func rootsFor(roots []historyRoot, label string) []historyRoot {
	for _, hr := range roots {
		if hr.cluster {
			continue
		}
		for _, l := range hr.labels {
			if l == label {
				return []historyRoot{hr}
			}
		}
	}
	return nil
}

func memberLabels(repos []string) []string {
	out := make([]string, len(repos))
	for i, r := range repos {
		out[i] = filepath.Base(r)
	}
	return out
}

// gcClusterCopies reports, and with prune removes, the copies of a cluster's graph that
// enola before the cluster had a directory of its own wrote into every member's output
// dir. A copy pinned as a baseline is the cluster's pin: on a cluster it moves to the
// cluster's directory, and on a single repository, where the cluster is not known, it
// is kept and named.
func (r *Runner) gcClusterCopies(scope historyScope, only string, prune, dryRun bool) {
	repos := scope.repos
	if only != "" {
		repos = nil
		for _, repo := range scope.repos {
			if filepath.Base(repo) == only {
				repos = append(repos, repo)
			}
		}
	}
	copies := engine.FindClusterCopies(scope.cfg, repos)
	home := ""
	if scope.cluster != "" {
		home = scope.cfg.GraphOutputDir(scope.repos)
	}
	var kept []engine.ClusterCopy
	if home == "" {
		var rest []engine.ClusterCopy
		for _, c := range copies {
			if c.Where == "baseline" {
				kept = append(kept, c)
			} else {
				rest = append(rest, c)
			}
		}
		copies = rest
	}
	if len(copies) == 0 && len(kept) == 0 {
		if prune {
			fmt.Printf("No copies of a cluster's graph in the output dirs.\n\n")
		}
		return
	}

	members := map[string]bool{}
	for _, c := range copies {
		members[c.Member] = true
	}
	size := engine.MeasureClusterCopies(copies)
	fmt.Printf("Copies of a cluster's graph in %d output dir%s: %s as files, %s on disk\n",
		len(members), pluralS(len(members)), humanBytes(size.Apparent), humanBytes(size.Freed))
	for _, c := range kept {
		fmt.Printf("  kept %s: a cluster's pinned baseline; run gc on the cluster (its config or folder) to move it to the cluster's directory\n", c.Dir)
	}
	switch {
	case len(copies) == 0:
	case !prune:
		fmt.Printf("  --prune-cluster-copies removes them\n")
	case dryRun:
		fmt.Printf("  Would remove %d cop%s, %s reclaimed\n", len(copies), pluralY(len(copies)), humanBytes(size.Freed))
		if moves := movableBaseline(copies, home); moves != nil {
			fmt.Printf("  Would move the cluster's baseline from %s to %s\n", moves.Dir, filepath.Join(home, engine.BaselineSubdir))
		}
	default:
		moved, err := engine.PruneClusterCopies(copies, home)
		if moved != nil {
			fmt.Printf("  Moved the cluster's baseline from %s to %s\n", moved.Dir, filepath.Join(home, engine.BaselineSubdir))
		}
		if err != nil {
			r.gcFatal("removing cluster copies: %v", err)
		}
		fmt.Printf("  Removed %d cop%s, %s reclaimed\n", len(copies), pluralY(len(copies)), humanBytes(size.Freed))
	}
	fmt.Println()
}

// movableBaseline is the copy PruneClusterCopies would move to home: the newest
// pinned one, when home has no baseline of its own.
func movableBaseline(copies []engine.ClusterCopy, home string) *engine.ClusterCopy {
	if home == "" {
		return nil
	}
	if _, err := os.Stat(filepath.Join(home, engine.BaselineSubdir, "facts.jsonl")); err == nil {
		return nil
	}
	var best *engine.ClusterCopy
	for i := range copies {
		if c := &copies[i]; c.Where == "baseline" && (best == nil || c.GeneratedAt > best.GeneratedAt) {
			best = c
		}
	}
	return best
}
