package command

import (
	"flag"
	"fmt"
	"os"
	"time"

	inthistory "github.com/enola-labs/enola/internal/history"
	"github.com/enola-labs/enola/pkg/history"
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
				"Flags:\n")
		fs.PrintDefaults()
		r.writeHelpFooter(fs.Output(), args, "gc")
	}
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	var arg string
	if rest := fs.Args(); len(rest) > 0 {
		arg = rest[0]
	}
	repoPath, cfg := r.logTarget(arg)
	root, err := history.Root(repoPath, cfg.History.Dir)
	if err != nil {
		r.gcFatal("cannot locate the history: %v", err)
	}

	opts := inthistory.GCOptions{DryRun: *dryRun, PruneWorking: *pruneWorking}
	if *olderThan != "" {
		d, err := parseAge(*olderThan)
		if err != nil {
			r.gcFatal("%v", err)
		}
		opts.ThinOlderThan = d
	}

	rep, err := inthistory.GC(root, opts)
	if err != nil {
		r.gcFatal("%v", err)
	}
	fmt.Print(renderGC(rep, *dryRun, root))
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
func renderGC(rep inthistory.GCReport, dryRun bool, root string) string {
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
		add("  %d working revision(s) from the log\n", rep.PrunedEntries)
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
