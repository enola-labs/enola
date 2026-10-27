package command

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/enola-labs/enola/internal/config"
	"github.com/enola-labs/enola/pkg/history"
)

// Blame is `enola blame <pattern>`: when did this appear, and when did it go away?
//
// It is the question a snapshot cannot answer however good the snapshot is. "When did
// internal/server start importing internal/extractors" is about the past, and a snapshot
// has none — which is why the timeline stores verbatim canonical lines: the search is a
// substring match over what the graph actually held at each point.
//
// Deliberately ONE command rather than the `blame` + `bisect` pair the plan sketched.
// `git bisect` names a search procedure — an interactive walk narrowing an unknown
// boundary — and here the whole history is on disk, so finding the first appearance is a
// lookup, not a search. Borrowing the word would have promised a procedure that does not
// happen; `--first` says what it does.
func (r *Runner) Blame(args []string) {
	fs := flag.NewFlagSet("blame", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		findings = fs.Bool("findings", false, "search the recorded findings instead of the facts")
		first    = fs.Bool("first", false, "stop at the first appearance")
		asJSON   = fs.Bool("json", false, "emit the events as JSON")
		full     = fs.Bool("full", false, "print the whole matching line rather than its head")
		all      = fs.Bool("all", false, "include working revisions (uncommitted trees)")
		store    = fs.String("store", "", "also search a shared history store (default: history.shared_dir)")
	)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr,
			"Usage: "+r.name()+" blame [flags] <pattern> [repo_path|config_path]\n\n"+
				"Show when something entered the architecture and when it left.\n\n"+
				"The pattern is matched, case-insensitively, against the recorded facts — a\n"+
				"module or symbol name, a file path, or both endpoints of an edge. With\n"+
				"--findings it is matched against recorded findings instead.\n\n"+
				"With a shared store (--store, or history.shared_dir) the search runs over the\n"+
				"union of the local history and the store, and every event names where its\n"+
				"revision came from.\n\n"+
				"Only revisions whose contents are still stored can be searched; older ones\n"+
				"keep their summary line and are reported as unread rather than as absent.\n\n"+
				"Flags:\n")
		fs.PrintDefaults()
		r.writeHelpFooter(fs.Output(), args, "blame")
	}
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	pattern, repoArg := splitPatternAndRepo(fs.Args())
	if pattern == "" {
		r.blameFatal("needs something to look for, e.g. `%s blame internal/server`", r.name())
	}

	entries, root, sh := r.historyWithStore(repoArg, *store, r.blameFatal)
	if !*all {
		entries = onlyCommitted(entries)
	}

	repo, err := history.UnionRepo(entries, sh, "")
	if err != nil {
		r.blameFatal("%v", err)
	}
	revs := history.BuildUnion(entries, sh, repo)
	if !*all {
		revs = onlyCommittedUnion(revs)
	}

	b, err := history.BlameUnion(root, revs, sh, pattern, history.BlameOptions{
		Findings:  *findings,
		FirstOnly: *first,
	})
	if err != nil {
		r.blameFatal("%v", err)
	}

	if *asJSON {
		out, err := json.MarshalIndent(b, "", "  ")
		if err != nil {
			r.blameFatal("failed to encode the result: %v", err)
		}
		fmt.Println(string(out))
		return
	}
	fmt.Print(renderBlame(b, *full, sh != nil))
}

func (r *Runner) historyWithStore(repoArg, storeFlag string, fatal func(string, ...any)) ([]history.Entry, string, *history.Share) {
	repoPath, cfg := r.logTarget(repoArg)
	root, err := history.Root(repoPath, cfg.History.Dir)
	if err != nil {
		fatal("cannot locate the history: %v", err)
	}
	storeDir := resolveStoreDir(storeFlag, repoPath, cfg)

	entries, err := history.Read(root)
	if err != nil {
		if !errors.Is(err, history.ErrNoHistory) {
			fatal("%v", err)
		}
		if storeDir == "" {
			r.reportNoHistory(repoPath, root, cfg)
			os.Exit(0)
		}
	}
	entries = history.SortedByTime(entries)

	var sh *history.Share
	if storeDir != "" {
		sh, err = history.OpenShare(storeDir)
		if err != nil {
			fatal("%v", err)
		}
	}
	return entries, root, sh
}

func resolveStoreDir(storeFlag, repoPath string, cfg *config.Config) string {
	dir := storeFlag
	if dir == "" {
		dir = cfg.History.SharedDir
	}
	if dir == "" {
		return ""
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(repoPath, dir)
	}
	return dir
}

func onlyCommittedUnion(revs []history.UnionRevision) []history.UnionRevision {
	out := make([]history.UnionRevision, 0, len(revs))
	for _, u := range revs {
		if !u.Entry.Working() {
			out = append(out, u)
		}
	}
	return out
}

// splitPatternAndRepo takes the pattern as the FIRST positional and an optional
// repo/config path as the second.
//
// Strictly positional, unlike `show` and `diff`, which decide by looking at the argument —
// and the difference is forced by what the arguments look like. A revision selector is a
// hex prefix or `HEAD~2` and never names a directory, so those commands can tell a lone
// argument apart by inspecting it. A blame pattern is very often a module path:
// `enola blame internal/history` was the first command run against this feature, and the
// path-shaped test swallowed the pattern as the repository, leaving nothing to search for.
func splitPatternAndRepo(args []string) (pattern, repo string) {
	if len(args) > 0 {
		pattern = args[0]
	}
	if len(args) > 1 {
		repo = args[1]
	}
	return pattern, repo
}

// renderBlame prints the events oldest first, each under the revision that produced it.
func renderBlame(b *history.Blame, full, withOrigins bool) string {
	var out strings.Builder

	if len(b.Events) == 0 {
		fmt.Fprintf(&out, "Nothing matching %q in %d searched revision%s.\n",
			b.Pattern, b.Scanned, pluralS(b.Scanned))
		out.WriteString(unreadNote(b))
		return out.String()
	}

	for _, ev := range b.Events {
		e := ev.Entry
		origins := ""
		if withOrigins {
			origins = "  [" + strings.Join(ev.Origins, ", ") + "]"
		}
		fmt.Fprintf(&out, "%-7s  %s  %s%s\n", e.Short(), shortDate(e.At), strings.TrimSpace(decorations(e)), origins)
		for _, l := range ev.Added {
			fmt.Fprintf(&out, "    + %s\n", blameLine(l, full))
		}
		for _, l := range ev.Removed {
			fmt.Fprintf(&out, "    - %s\n", blameLine(l, full))
		}
		for _, p := range ev.Changed {
			fmt.Fprintf(&out, "    ~ %-9s  %s\n", p.Kind, blamePair(p, full))
		}
	}

	fmt.Fprintf(&out, "\n%d event%s across %d searched revision%s.\n",
		len(b.Events), pluralS(len(b.Events)), b.Scanned, pluralS(b.Scanned))
	out.WriteString(unreadNote(b))
	return out.String()
}

// unreadNote says how much of the history could NOT be searched.
//
// Without it "nothing matching" is indistinguishable from "nothing matching in the part I
// could read", and those support opposite conclusions — the first says it never existed,
// the second says look further back.
func unreadNote(b *history.Blame) string {
	var out strings.Builder
	if b.Skipped > 0 {
		fmt.Fprintf(&out, "%d older revision%s could not be searched: their contents are no longer stored,\n"+
			"so anything that appeared and vanished within them is invisible here.\n",
			b.Skipped, pluralS(b.Skipped))
	}
	if b.Pruned > 0 {
		fmt.Fprintf(&out, "%d revision%s were pruned from the shared store by retention — removed on record,\n"+
			"not missing.\n", b.Pruned, pluralS(b.Pruned))
	}
	return out.String()
}

// blameLine renders one canonical fact or finding line.
//
// Abbreviated by default to the fields that identify it — kind, name, file — because a
// fact line carries its full props and relations, and a blame answering "when" should not
// bury the answer under a screenful of JSON. --full prints the line as stored.
func blameLine(line string, full bool) string {
	if full {
		return line
	}
	var f struct {
		Kind  string `json:"kind"`
		Name  string `json:"name"`
		File  string `json:"file"`
		Line  int    `json:"line"`
		Title string `json:"title"`
	}
	if err := json.Unmarshal([]byte(line), &f); err != nil {
		return truncateLine(line, 120)
	}
	if f.Title != "" { // a finding
		return f.Title
	}
	where := f.File
	if where != "" && f.Line > 0 {
		where = fmt.Sprintf("%s:%d", f.File, f.Line)
	}
	if where == "" {
		return fmt.Sprintf("%-11s %s", f.Kind, f.Name)
	}
	return fmt.Sprintf("%-11s %s  (%s)", f.Kind, f.Name, where)
}

// blamePair renders a changed symbol as its new line and where it was before, which is
// the part of the old line a reader needs once the pair says it is the same symbol.
func blamePair(p history.BlamePair, full bool) string {
	if full {
		return p.To + "\n                 was " + p.From
	}
	var from struct {
		File string `json:"file"`
		Line int    `json:"line"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(p.From), &from); err != nil {
		return blameLine(p.To, false)
	}
	was := fmt.Sprintf("%s:%d", from.File, from.Line)
	if p.Kind == history.PairMoved {
		was = from.Name + "  (" + was + ")"
	}
	return blameLine(p.To, false) + "  was " + was
}

func truncateLine(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func (r *Runner) blameFatal(format string, args ...any) { r.cmdFatal("blame", format, args...) }
