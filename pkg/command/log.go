package command

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/enola-labs/enola/internal/config"
	"github.com/enola-labs/enola/pkg/history"
)

// Log is `enola log`: the architecture history of a repository, newest last.
//
// It is a pure READER. It never snapshots, never writes, and never touches the repository
// — which is what makes it safe to run against a checkout you do not own, and what keeps
// it honest about the one thing it reports: what enola has actually observed. A `log`
// that quietly took a snapshot to fill in a gap would be inventing the history it claims
// to be reading.
//
// EXPERIMENTAL. Recording is on by default (see config.HistoryConfig for why a feature
// like this cannot usefully be opt-in), so the common outcome on a repository enola has
// snapshotted before is a timeline, and on one it has not is a line saying to snapshot it.
func (r *Runner) Log(args []string) {
	fs := flag.NewFlagSet("log", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		oneline = fs.Bool("oneline", true, "one line per revision")
		graph   = fs.Bool("graph", false, "draw the branch topology beside the revisions")
		stat    = fs.Bool("stat", false, "break each revision's delta down by fact kind")
		since   = fs.String("since", "", "only revisions taken after this time (RFC3339, or a duration like 72h)")
		limit   = fs.Int("n", 20, "show at most this many revisions (0 for all)")
		asJSON  = fs.Bool("json", false, "emit the entries as JSON")
		all     = fs.Bool("all", false, "include working revisions (uncommitted trees)")

		backfill = fs.Bool("backfill", false, "build the timeline from the repository's own commit history")
		sample   = fs.String("sample", "all", "which commits to backfill: all, merges, tags or daily")
		dryRun   = fs.Bool("dry-run", false, "with --backfill, list what would be snapshotted and stop")
	)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr,
			"Usage: "+r.name()+" log [flags] [repo_path|config_path]\n\n"+
				"Show what this repository's architecture has done over time: one line per\n"+
				"snapshot enola recorded, with what changed since the one before it.\n\n"+
				"Read-only. It reports what was observed and never snapshots to fill a gap.\n\n"+
				"Oldest first, which is the opposite of `git log`: a changelog answers \"what\n"+
				"landed recently\", and this answers \"how did it get like this\", which runs\n"+
				"forward. With --graph, lines therefore diverge downward at a branch and\n"+
				"converge downward at a merge.\n\n"+
				"With --backfill it instead BUILDS that timeline from the repository's own\n"+
				"commit history, snapshotting past commits so a repo enola has never seen still\n"+
				"has a past to read. It only reads the repository — trees are extracted to a\n"+
				"temp dir — and re-running resumes rather than repeating.\n\n"+
				"EXPERIMENTAL. Every snapshot is recorded as a revision; turn that off with\n"+
				"`history:` / `enabled: false` in your config.\n\n"+
				"Flags:\n")
		fs.PrintDefaults()
		r.writeHelpFooter(fs.Output(), args, "log")
	}
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	_ = oneline // the only format in this phase; the flag exists so scripts can be explicit

	var arg string
	if rest := fs.Args(); len(rest) > 0 {
		arg = rest[0]
	}

	// Backfill WRITES, and everything else here reads. It is a flag on `log` rather than a
	// command of its own because it answers the same question — "what has this
	// architecture done over time" — for a repository that has no answer yet.
	if *backfill {
		// -n is a DISPLAY default for reading a log, and inheriting it here silently caps
		// how much history gets BUILT: the first backfill of a 108-commit repository
		// produced 20 revisions and said nothing about the 88 it dropped. A limit on what
		// you look at and a limit on what you record are different promises, so this one is
		// honoured only when it was actually typed.
		explicitLimit := 0
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "n" {
				explicitLimit = *limit
			}
		})
		r.runBackfill(backfillArgs{
			repoArg: arg, since: *since, sample: *sample, limit: explicitLimit, dryRun: *dryRun,
		})
		return
	}

	repoPath, cfg := r.logTarget(arg)

	root, err := history.Root(repoPath, cfg.History.Dir)
	if err != nil {
		r.logFatal("cannot locate the history: %v", err)
	}
	entries, err := history.Read(root)
	if err != nil {
		if errors.Is(err, history.ErrNoHistory) {
			r.reportNoHistory(repoPath, root, cfg)
			return
		}
		r.logFatal("%v", err)
	}

	// Presented as a timeline, so ordered by when each revision describes rather than when
	// it was written — the two diverge after a backfill. See history.SortedByTime.
	entries = history.SortedByTime(entries)

	recorded := len(entries)
	if !*all {
		entries = onlyCommitted(entries)
	}
	if *since != "" {
		cutoff, err := parseSince(*since)
		if err != nil {
			r.logFatal("%v", err)
		}
		entries = takenAfter(entries, cutoff)
	}
	// Narrowing happens BEFORE the topology is derived, so the shape describes what is
	// actually on screen: edges collapse across whatever was filtered out, exactly as they
	// collapse across commits enola never observed. Deriving first and filtering after
	// would leave a picture referring to rows that are not there.
	if *limit > 0 && len(entries) > *limit {
		entries = entries[len(entries)-*limit:]
	}

	if *asJSON {
		out, err := json.MarshalIndent(entries, "", "  ")
		if err != nil {
			r.logFatal("failed to encode the history: %v", err)
		}
		fmt.Println(string(out))
		return
	}
	if len(entries) == 0 {
		// Say how much was filtered out. "Nothing here" and "nothing here that you asked
		// to see" are different situations, and on a dirty tree — which is where anybody
		// running an agent loop spends their time — the second is the common one.
		fmt.Fprintf(os.Stderr,
			"No committed revisions recorded yet (%d revision%s hidden by the filters — try --all).\n",
			recorded, pluralS(recorded))
		return
	}

	_ = oneline // the default and only text format; the flag exists so scripts can be explicit
	if *graph {
		if note := multiRepoNote(entries); note != "" {
			fmt.Fprintln(os.Stderr, note)
		}
		topo := history.BuildTopology(entries, repoPath)
		if note := topologyNote(topo.Source); note != "" {
			fmt.Fprintln(os.Stderr, note)
		}
		if note := observationOrderNote(topo, r.name()); note != "" {
			fmt.Fprintln(os.Stderr, note)
		}
		fmt.Print(renderGraphLog(topo, *stat))
		return
	}
	fmt.Print(renderOneline(entries, *stat))
}

// logTarget resolves the positional argument to a repository and the config that governs
// it, WITHOUT constructing an engine.
//
// resolveTarget (used by check/coverage) builds one, because those commands snapshot. A
// reader that built an engine would register every extractor and load every grammar to
// print twenty lines of text — and, worse, would make `enola log` fail on a repository
// whose config enola cannot fully honour, which is exactly a repository whose history
// somebody might want to look at.
func (r *Runner) logTarget(arg string) (string, *config.Config) {
	cfgPath := "mcp-arch.yaml"
	repoOverride := ""

	switch {
	case arg == "":
	case isDirectory(arg):
		abs, err := filepath.Abs(arg)
		if err != nil {
			r.logFatal("resolving repo path %q: %v", arg, err)
		}
		repoOverride = abs
		if inner := filepath.Join(abs, "mcp-arch.yaml"); fileExists(inner) {
			cfgPath = inner
		}
	default:
		cfgPath = arg
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		// A missing config is the ordinary case (built-in defaults); anything else is
		// worth saying out loud, but is still not a reason to refuse to read a history.
		cfg = config.Default()
	}
	if repoOverride != "" {
		cfg.Repo = repoOverride
		cfg.Repos = nil
	}
	if err := cfg.Normalize(); err != nil {
		r.logFatal("invalid config: %v", err)
	}

	repoPaths, err := cfg.RepoPaths()
	if err != nil || len(repoPaths) == 0 {
		r.logFatal("failed to resolve repo path: %v", err)
	}
	// The FIRST repo of a cluster owns the history: it is the one a multi-repo snapshot
	// is generated from, so it is where the graph's revisions were recorded.
	return repoPaths[0], cfg
}

// reportNoHistory explains an empty history in terms of what to do about it, and
// distinguishes the two ways of getting here. Recording is on by default, so the usual
// case is simply that this repository has not been snapshotted since — the remedy is a
// snapshot, not a setting. Being told to enable something that is already enabled is how
// a diagnostic becomes noise.
func (r *Runner) reportNoHistory(repoPath, root string, cfg *config.Config) {
	fmt.Fprintf(os.Stderr, "No architecture history for %s.\n", repoPath)
	if !cfg.HistoryEnabled() {
		fmt.Fprintf(os.Stderr,
			"\nRecording is turned off in %s (history.enabled: false).\n"+
				"Remove that line, or set it to true, and every snapshot after it becomes a revision.\n",
			configToEdit(repoPath, cfg))
		return
	}
	fmt.Fprintf(os.Stderr,
		"\nRecording is on — nothing has been snapshotted yet.\n"+
			"Run `%s --generate` and the first revision lands in\n%s\n", r.name(), root)
}

// configToEdit names the file the user should turn recording on in.
//
// The loaded config is the obvious candidate and is the WRONG answer in a case that is
// easy to hit: `enola log /some/other/repo` run from a directory that has its own
// mcp-arch.yaml loads THAT config (the same fallback `check` uses deliberately), so the
// advice would tell somebody to enable history in a config belonging to a repository they
// were not asking about — and doing as they were told would record a different repo's
// history. When the config that was loaded does not live inside the repository being
// reported on, name the file inside it instead.
func configToEdit(repoPath string, cfg *config.Config) string {
	if cfg.SourcePath != "" {
		if abs, err := filepath.Abs(cfg.SourcePath); err == nil {
			if rel, err := filepath.Rel(repoPath, abs); err == nil && !strings.HasPrefix(rel, "..") {
				return cfg.SourcePath
			}
		}
	}
	return filepath.Join(repoPath, "mcp-arch.yaml")
}

// onlyCommitted drops unanchored revisions — the working-tree snapshots an agent loop
// produces between commits. They are the majority of entries during a session and the
// minority of what anybody wants to read afterwards, which is why they are opt-in rather
// than filtered out by eye.
func onlyCommitted(entries []history.Entry) []history.Entry {
	out := make([]history.Entry, 0, len(entries))
	for _, e := range entries {
		if !e.Working() {
			out = append(out, e)
		}
	}
	return out
}

// renderOneline prints one revision per line, oldest first — the same direction as
// `git log --reverse`, because a history read forward is a story and read backward is a
// changelog, and this is the one that answers "how did it get like this?".
//
// Columns: short id · date · decorations · headline. An epoch change gets its own line
// above the revision that opens it, since a delta across that seam is rebuild noise and a
// reader who is not told that will read it as somebody's change.
func renderOneline(entries []history.Entry, stat bool) string {
	var b strings.Builder
	prevEpoch := ""
	for i, e := range entries {
		if i > 0 && e.Epoch != prevEpoch {
			b.WriteString("  " + epochSeam(prevEpoch, e.Epoch) + "\n")
		}
		prevEpoch = e.Epoch

		fmt.Fprintf(&b, "%-7s  %s  %s%s\n", e.Short(), shortDate(e.At), decorations(e), e.Summary.Headline())
		if stat {
			b.WriteString(statLines(e, "         "))
		}
	}
	return b.String()
}

// renderGraphLog prints the same lines with the branch topology drawn beside them.
//
// Every row is padded to the widest graph column so the text stays aligned: a graph that
// makes the dates ragged has traded the thing people actually read for the thing they
// glance at.
func renderGraphLog(topo history.Topology, stat bool) string {
	rows := history.RenderGraph(topo)
	width := history.GraphWidth(rows)

	var b strings.Builder
	prevEpoch := ""
	for i, row := range rows {
		e := row.Entry
		if row.Before != "" {
			fmt.Fprintf(&b, "%s\n", row.Before)
		}
		if i > 0 && e.Epoch != prevEpoch {
			fmt.Fprintf(&b, "%-*s%s\n", width, verticalRule(width), epochSeam(prevEpoch, e.Epoch))
		}
		prevEpoch = e.Epoch

		fmt.Fprintf(&b, "%-*s%-7s  %s  %s%s\n",
			width, row.Prefix, e.Short(), shortDate(e.At), decorations(e), e.Summary.Headline())
		if stat {
			b.WriteString(statLines(e, strings.Repeat(" ", width)+"         "))
		}
		if row.After != "" {
			fmt.Fprintf(&b, "%s\n", row.After)
		}
	}
	return b.String()
}

// verticalRule keeps the graph's lines unbroken through an interleaved note, so a seam
// drawn between two revisions does not look like the branch ended there.
func verticalRule(width int) string {
	if width < 2 {
		return "| "
	}
	return strings.Repeat("| ", width/2)
}

// epochSeam is the note marking where enola itself changed between two revisions.
func epochSeam(before, after string) string {
	return fmt.Sprintf("══ epoch changed (%s → %s) — the delta below is rebuild noise, not a change to the code",
		shortEpoch(before), shortEpoch(after))
}

// statLines breaks a revision's delta down by fact kind, indented under its line.
//
// Net per kind, not added-and-removed: the question `--stat` answers is "what KIND of thing
// changed here" — did this revision move modules, or symbols, or routes — and a pair of
// numbers per kind buries that under arithmetic the headline already gave.
func statLines(e history.Entry, indent string) string {
	if len(e.Summary.ByKind) == 0 {
		return ""
	}
	var b strings.Builder
	for _, kind := range sortedKeys(e.Summary.ByKind) {
		n := e.Summary.ByKind[kind]
		sign := "+"
		if n < 0 {
			sign, n = "-", -n
		}
		fmt.Fprintf(&b, "%s%-12s %s%d\n", indent, kind, sign, n)
	}
	return b.String()
}

// topologyNote discloses a shape that was not derived from the repository. A picture
// assembled from a fallback must not be presented as though it came from the commit graph —
// the reader cannot tell by looking, and the two support different conclusions.
func topologyNote(source history.TopologySource) string {
	switch source {
	case history.SourceRecordedParents:
		return "note: the repository could not be read, so the shape below uses the parent\n" +
			"commits each snapshot recorded. Revisions whose parent was never observed appear\n" +
			"unconnected even where the repository would join them."
	case history.SourceTime:
		return "note: no commit ancestry is available, so the revisions below are strung together\n" +
			"in the order they were taken. This is a timeline, not a history: it says what\n" +
			"happened next, never which revision descends out of which."
	default:
		return ""
	}
}

// observationOrderNote warns when the drawn shape and the printed numbers answer different
// questions — which happens exactly when somebody switches branches.
//
// A revision's summary is the delta from the snapshot taken BEFORE it, because that is the
// only pair enola had in hand at the time. The graph shows ANCESTRY. Usually they coincide,
// and the numbers read as "what this revision did". Snapshot a branch, switch back to main
// and snapshot again, and the second summary reports the branch's work as removed — true of
// the pair that was compared, and not at all what the row's position in the graph implies.
//
// The honest fix is not to recompute (the same revision would then show different numbers
// in different views, which is worse) but to say so, and to point at the command that
// answers the ancestry question directly.
func observationOrderNote(topo history.Topology, bin string) string {
	for i, row := range topo.Rows {
		if i == 0 || len(row.Parents) == 0 {
			continue
		}
		// The common, quiet case: this revision follows the one printed above it.
		if len(row.Parents) == 1 && row.Parents[0] == i-1 {
			continue
		}
		return fmt.Sprintf(
			"note: some revisions below do not follow the one printed above them — work moved\n"+
				"between branches. Each summary is the delta from the PREVIOUS SNAPSHOT, not from\n"+
				"the revision it descends from in the graph, so those rows report what changed\n"+
				"since enola last looked. For the ancestry delta use `%s diff <a>..<b>`.", bin)
	}
	return ""
}

// multiRepoNote warns that a drawn shape describes only the primary repository.
//
// A multi-repo graph has no single commit — it has a VECTOR of them, one per repository, and
// a snapshot advances some of them and not others. There is no DAG over that, and inventing
// one by drawing the primary repository's commits would state a relationship between
// revisions that the other repositories may flatly contradict. So the shape is drawn from
// the primary repository and the reader is told that is what it is; the per-repository
// positions are on each row's decoration, where they can be compared without being
// fabricated into edges.
func multiRepoNote(entries []history.Entry) string {
	for _, e := range entries {
		if len(e.Repos) > 1 {
			return fmt.Sprintf("note: this history covers %d repositories. A multi-repo graph has one commit\n"+
				"per repository rather than one overall, so the shape below follows the primary\n"+
				"repository only; each row lists where the others stood.", len(e.Repos))
		}
	}
	return ""
}

// parseSince accepts either an RFC3339 instant or a duration back from now, because both
// are things people mean by "since": a date they remember, or "the last few days".
func parseSince(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		return time.Now().Add(-d), nil
	}
	return time.Time{}, fmt.Errorf("--since %q is neither a time (2026-08-01T00:00:00Z) nor a duration (72h)", s)
}

// takenAfter narrows entries to those recorded after cutoff. An unparseable timestamp is
// KEPT: dropping a revision because its clock reading is unreadable would silently shorten
// the history for a reason that has nothing to do with what was asked.
func takenAfter(entries []history.Entry, cutoff time.Time) []history.Entry {
	out := make([]history.Entry, 0, len(entries))
	for _, e := range entries {
		at, err := time.Parse(time.RFC3339, e.At)
		if err != nil || !at.Before(cutoff) {
			out = append(out, e)
		}
	}
	return out
}

// decorations is the bracketed context before the headline: the commit, the branch, any
// refs, and whether the tree was dirty. Empty when there is nothing to say.
func decorations(e history.Entry) string {
	var parts []string
	if c := e.Commit(); c != "" {
		parts = append(parts, shortCommit(c))
	}
	if ref := e.Ref(); ref != "" {
		parts = append(parts, ref)
	}
	parts = append(parts, e.Refs...)
	if e.Working() && e.Commit() != "" {
		parts = append(parts, "dirty")
	}
	if e.Summary.Incomparable {
		parts = append(parts, "incomparable")
	}
	if e.Origin != "" {
		parts = append(parts, "from "+e.Origin)
	}
	// In a multi-repo graph the other repositories' positions belong on the row, because
	// the drawn shape cannot express them (see multiRepoNote).
	for _, r := range e.Repos {
		if r.Commit != "" {
			parts = append(parts, fmt.Sprintf("%s@%s", r.Label, shortCommit(r.Commit)))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "(" + strings.Join(parts, ", ") + ")  "
}

func shortEpoch(epoch string) string {
	if len(epoch) > 6 {
		return epoch[:6]
	}
	return epoch
}

// shortDate renders an RFC3339 timestamp as "2006-01-02 15:04", falling back to the raw
// string so an unparseable timestamp is shown rather than blanked.
func shortDate(ts string) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ts
	}
	return t.Local().Format("2006-01-02 15:04")
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func (r *Runner) logFatal(format string, args ...any) { r.cmdFatal("log", format, args...) }
