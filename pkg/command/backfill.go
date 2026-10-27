package command

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/enola-labs/enola/internal/config"
	"github.com/enola-labs/enola/internal/diff"
	"github.com/enola-labs/enola/internal/facts"
	inthistory "github.com/enola-labs/enola/internal/history"
	"github.com/enola-labs/enola/pkg/bootstrap"
	"github.com/enola-labs/enola/pkg/history"
)

// Backfill is `enola log --backfill`: build the timeline that would have existed if enola
// had been running all along.
//
// Without it the feature is worth nothing on the day somebody wants it. A history answers
// questions about the past, and the first time anyone asks one — "when did this coupling
// appear?" — the honest answer from a fresh install is "I started watching on Tuesday".
// Backfill is what makes `enola log` say something about a repository the moment it is
// pointed at one.
//
// It reads the repository and writes NOTHING into it. Each commit's tree is extracted with
// `git archive` into a temporary directory and snapshotted there, so a checkout that is
// read-only, shared, or somebody else's is a valid target. `git worktree` would have been
// the obvious mechanism and was rejected for exactly that: it records bookkeeping under
// .git/worktrees, which is a write.
func (r *Runner) runBackfill(args backfillArgs) {
	repoPath, cfg := r.logTarget(args.repoArg)
	if !isGitRepo(repoPath) {
		r.logFatal("%s is not a git repository, so there is no commit history to walk", repoPath)
	}
	root, err := history.Root(repoPath, cfg.History.Dir)
	if err != nil {
		r.logFatal("cannot locate the history: %v", err)
	}

	commits, err := selectCommits(repoPath, args)
	if err != nil {
		r.logFatal("%v", err)
	}
	if len(commits) == 0 {
		fmt.Fprintln(os.Stderr, "No commits match. Try widening --since, or --sample=all.")
		return
	}

	// Already-recorded commits are skipped, which is what makes a backfill resumable and
	// idempotent: an interrupted run is resumed by running it again, and a completed one
	// re-run costs a git walk and nothing else.
	done := recordedCommits(root)
	selected := commits
	var todo []commitInfo
	for _, c := range commits {
		if !done[c.SHA] {
			todo = append(todo, c)
		}
	}

	fmt.Fprintf(os.Stderr, "%d commit(s) selected, %d already recorded, %d to snapshot.\n",
		len(commits), len(commits)-len(todo), len(todo))
	if args.dryRun {
		for _, c := range todo {
			fmt.Fprintf(os.Stderr, "  %s  %s  %s\n", c.SHA[:12], c.When, firstLine(c.Subject))
		}
		return
	}
	if len(todo) == 0 {
		return
	}

	// ONE engine and ONE config for the whole run, deliberately: the config that governs
	// this repository NOW, never the one committed alongside each old commit. Reading each
	// commit's own config would change the ignore globs and plugin set as the walk moved,
	// so every historical config edit would open an epoch and the resulting timeline would
	// describe enola's settings rather than the code.
	eng, _, err := r.newEngine(bootstrap.Options{ConfigPath: configForRepo(repoPath)})
	if err != nil {
		r.logFatal("failed to create engine: %v", err)
	}
	// Nothing of enola's may be left in the extracted trees, and a cache keyed on a
	// throwaway path is worthless anyway.
	eng.SetPersistCache(false)

	repoIdentity := repoIdentityOf(repoPath)
	var prev *facts.Snapshot
	recorded, skipped, thin := 0, 0, 0
	start := time.Now()

	// Walk every SELECTED commit, not just the outstanding ones, stopping after the last
	// piece of work. A commit already recorded is not re-snapshotted — its stored graph is
	// loaded instead, purely to become the predecessor the next delta is measured against.
	//
	// Without that, a resumed backfill starts with no predecessor and marks its first
	// revision `initial`, which means "this is where the history begins". Resuming an
	// interrupted run therefore plants a false beginning in the middle of the timeline, and
	// every reader downstream — the log, `show`, the summary counts — repeats it. Seen for
	// real: backfilling cognee's newest 3 release tags and then the remaining 77 left
	// v1.2.1 declaring itself the start of a three-year history.
	work := selected[:lastTodoIndex(selected, done)+1]
	for i, c := range work {
		if done[c.SHA] {
			if snap := loadRecorded(root, c.SHA); snap != nil {
				prev = snap
			}
			continue
		}
		fmt.Fprintf(os.Stderr, "\r[%d/%d] %s %s", i+1, len(work), c.SHA[:12], strings.Repeat(" ", 20))

		snap, factLines, err := snapshotCommit(eng, repoPath, c.SHA)
		if err != nil {
			fmt.Fprintf(os.Stderr, "\r  %s: %v\n", c.SHA[:12], err)
			skipped++
			continue
		}

		// Provenance describes the REPOSITORY, not the scratch directory the tree was
		// extracted into — a receipt naming /tmp/enola-backfill-123 would be a true
		// statement about a path that never existed for anyone else.
		meta := snap.Meta
		meta.RepoPath = repoPath
		meta.GeneratedAt = c.When
		meta.Git = c.gitInfo()
		snap.Meta = meta

		if meta.ParseErrors > 0 || meta.FilesParsed == 0 {
			thin++
		}

		entry := history.Entry{
			ID:      meta.SnapshotID,
			Repo:    repoIdentity,
			At:      c.When,
			Epoch:   history.Epoch(meta),
			Git:     c.gitInfo(),
			Parents: c.Parents,
			Summary: summarizeAgainst(prev, snap),
		}
		insightLines, err := history.InsightLines(snap.Insights)
		if err != nil {
			r.logFatal("encoding findings for %s: %v", c.SHA[:12], err)
		}
		if _, err := inthistory.Append(root, entry, backfillOptions(cfg.History, &inthistory.Contents{
			FactLines:    factLines,
			InsightLines: insightLines,
			Receipt:      meta.Receipt(),
		})); err != nil {
			r.logFatal("recording %s: %v", c.SHA[:12], err)
		}
		recorded++
		prev = snap
	}

	fmt.Fprintf(os.Stderr, "\r%s\r", strings.Repeat(" ", 60))
	if fixed := r.reconcileInitial(root); fixed > 0 {
		fmt.Fprintf(os.Stderr, "Corrected %d revision(s) that had claimed to be the start of the history.\n", fixed)
	}
	fmt.Fprintf(os.Stderr, "Backfilled %d revision(s) in %s.\n", recorded, time.Since(start).Round(time.Second))
	if skipped > 0 {
		fmt.Fprintf(os.Stderr, "%d commit(s) could not be snapshotted and were left out.\n", skipped)
	}
	if thin > 0 {
		// Reported rather than hidden: an old commit that today's extractors parse poorly
		// produces a THIN graph, and a thin graph read as a complete one says the
		// architecture used to be smaller than it was.
		fmt.Fprintf(os.Stderr,
			"%d revision(s) extracted thinly (parse errors, or no files parsed). Their graphs are\n"+
				"smaller than the code was; treat deltas around them with suspicion.\n", thin)
	}
	fmt.Fprintf(os.Stderr, "Run `%s log` to read it.\n", r.name())
}

// backfillArgs are the parsed flags, kept separate so Log stays readable.
type backfillArgs struct {
	repoArg string
	since   string
	sample  string
	limit   int
	dryRun  bool
}

// commitInfo is one candidate commit.
type commitInfo struct {
	SHA     string
	Parents []string
	When    string // RFC3339 committer date — see the note in selectCommits
	Subject string
	Ref     string
}

func (c commitInfo) gitInfo() *facts.GitInfo {
	return &facts.GitInfo{Commit: c.SHA, Ref: c.Ref}
}

// selectCommits walks the repository and returns the commits to snapshot, oldest first.
//
// The recorded time is the COMMITTER DATE, not the moment the backfill ran. A timeline
// stamped with the run time collapses every revision into one instant, which orders
// nothing, filters under --since to all-or-nothing, and describes when somebody typed a
// command rather than when the architecture looked like that.
func selectCommits(repoPath string, args backfillArgs) ([]commitInfo, error) {
	// --first-parent: a merge's second parent re-walks a branch whose commits were already
	// described by the merge, so following it snapshots the same work twice and draws a
	// timeline that oscillates between two versions of the tree.
	argv := []string{"-C", repoPath, "rev-list", "--first-parent", "--reverse",
		"--format=%H%x00%P%x00%cI%x00%s", "--no-commit-header"}
	switch args.sample {
	case "", "all":
	case "merges":
		argv = append(argv, "--merges")
	case "tags", "daily":
		// Handled below by filtering; rev-list has no direct equivalent that also keeps
		// the ordering and formatting consistent.
	default:
		return nil, fmt.Errorf("unknown --sample %q: expected all, merges, tags or daily", args.sample)
	}
	if args.since != "" {
		argv = append(argv, "--since="+args.since)
	}
	argv = append(argv, "HEAD")

	out, err := exec.Command("git", argv...).Output()
	if err != nil {
		return nil, fmt.Errorf("listing commits: %w", err)
	}

	var commits []commitInfo
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.Split(line, "\x00")
		if len(parts) < 4 || parts[0] == "" {
			continue
		}
		c := commitInfo{SHA: parts[0], When: parts[2], Subject: parts[3], Ref: "backfill"}
		if parts[1] != "" {
			c.Parents = strings.Fields(parts[1])
		}
		commits = append(commits, c)
	}

	switch args.sample {
	case "daily":
		commits = onePerDay(commits)
	case "tags":
		commits = onlyTagged(repoPath, commits)
	}
	if args.limit > 0 && len(commits) > args.limit {
		// Say what was dropped. A cap that silently shortens the history produces a
		// timeline that looks complete and begins in the middle, and nothing downstream can
		// tell that from a repository that genuinely starts there.
		fmt.Fprintf(os.Stderr, "-n %d: keeping the newest %d of %d matching commits; the %d older ones are NOT backfilled.\n",
			args.limit, args.limit, len(commits), len(commits)-args.limit)
		commits = commits[len(commits)-args.limit:]
	}
	return commits, nil
}

// onePerDay keeps the LAST commit of each day — the state the day ended in, which is what
// somebody scanning a timeline by date is picturing.
func onePerDay(commits []commitInfo) []commitInfo {
	var out []commitInfo
	lastDay := ""
	for i, c := range commits {
		day := c.When
		if len(day) > 10 {
			day = day[:10]
		}
		if i > 0 && day != lastDay {
			out = append(out, commits[i-1])
		}
		lastDay = day
	}
	if len(commits) > 0 {
		out = append(out, commits[len(commits)-1])
	}
	return out
}

// onlyTagged keeps commits that carry a tag — releases, which is the coarsest useful
// sampling and the one whose dates mean something outside the repository.
func onlyTagged(repoPath string, commits []commitInfo) []commitInfo {
	out, err := exec.Command("git", "-C", repoPath, "show-ref", "--tags", "-d").Output()
	if err != nil {
		return nil
	}
	tagged := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		sha, ref, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		tagged[sha] = strings.TrimSuffix(strings.TrimPrefix(ref, "refs/tags/"), "^{}")
	}
	var kept []commitInfo
	for _, c := range commits {
		if tag, ok := tagged[c.SHA]; ok {
			c.Ref = tag
			kept = append(kept, c)
		}
	}
	return kept
}

// snapshotCommit extracts one commit's tree into a temporary directory and snapshots it.
//
// `git archive` rather than `git worktree add`: the repository is only ever READ, so a
// read-only or shared checkout is a valid target, and there is no worktree registration to
// leak if the process dies. The cost is that the extracted tree has no .git, which is why
// the caller supplies the git provenance rather than letting the engine detect it.
func snapshotCommit(eng *bootstrap.Engine, repoPath, sha string) (*facts.Snapshot, []string, error) {
	scratch, err := os.MkdirTemp("", "enola-backfill-")
	if err != nil {
		return nil, nil, fmt.Errorf("creating a scratch directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(scratch) }()

	dir := extractionDir(scratch, repoPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, fmt.Errorf("creating the extraction directory: %w", err)
	}

	archive := exec.Command("git", "-C", repoPath, "archive", "--format=tar", sha)
	extract := exec.Command("tar", "-x", "-C", dir)
	extract.Stdin, _ = archive.StdoutPipe()
	if err := extract.Start(); err != nil {
		return nil, nil, fmt.Errorf("extracting the tree: %w", err)
	}
	if err := archive.Run(); err != nil {
		_ = extract.Wait()
		return nil, nil, fmt.Errorf("reading the tree at %s: %w", sha[:12], err)
	}
	if err := extract.Wait(); err != nil {
		return nil, nil, fmt.Errorf("extracting the tree at %s: %w", sha[:12], err)
	}

	snap, err := eng.GenerateSnapshot(context.Background(), dir, false)
	if err != nil {
		return nil, nil, fmt.Errorf("snapshot: %w", err)
	}
	// The canonical bytes, straight from the engine — the same serialization facts.jsonl
	// would hold, without writing one.
	raw, err := eng.GetArtifact("facts.jsonl")
	if err != nil {
		return nil, nil, fmt.Errorf("reading the facts: %w", err)
	}
	return snap, splitFactLines(raw), nil
}

// extractionDir is where one commit's tree is unpacked: a directory named after the
// REPOSITORY, inside the scratch directory, rather than the scratch directory itself.
//
// Every fact carries a repo label, and the engine derives it from the base name of the path
// it walked — so extracting straight into enola-backfill-217483 stamps that name onto all
// 2,400 facts, a fresh one per revision.
//
// The consequence is total and silent. Fact identity includes the repo, so consecutive
// commits share not a single fact: every patch becomes a complete rewrite, the delta-ratio
// cut fires on every revision, each gets its own segment, and retention then discards nearly
// all of them. The first real backfill of this repository produced twenty revisions each
// reporting "+2509/-2457 facts" for days that changed a handful of files, and four surviving
// segments out of twenty-two — a plausible-looking timeline that was entirely an artifact of
// where the trees had been unpacked.
func extractionDir(scratch, repoPath string) string {
	return filepath.Join(scratch, filepath.Base(strings.TrimRight(repoPath, "/")))
}

func splitFactLines(b []byte) []string {
	s := strings.TrimSuffix(string(b), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// summarizeAgainst produces the entry summary for a backfilled revision, comparing it with
// the previously backfilled one — its predecessor along the walk, which is the pair whose
// delta the timeline is claiming to show.
func summarizeAgainst(prev, cur *facts.Snapshot) history.Summary {
	s := history.Summary{FactCount: len(cur.Facts), InsightCount: len(cur.Insights)}
	if prev == nil {
		s.Initial = true
		return s
	}
	d := diff.Compute(prev, cur)
	s.FactsAdded, s.FactsRemoved, s.FactsChanged = len(d.FactsAdded), len(d.FactsRemoved), len(d.FactsChanged)
	s.EdgesAdded, s.EdgesRemoved = len(d.EdgesAdded), len(d.EdgesRemoved)
	s.FindingsNew, s.FindingsResolved = len(d.FindingsNew), len(d.FindingsResolved)
	// Not simply !Comparable: elapsed time between two revisions is the normal shape of a
	// timeline, and flagging it would mark most of a release-sampled history as suspect.
	s.Incomparable = d.Comparability.InvalidatesDelta()

	byKind := map[string]int{}
	for k, n := range diff.KindCounts(d.FactsAdded) {
		byKind[k] += n
	}
	for k, n := range diff.KindCounts(d.FactsRemoved) {
		byKind[k] -= n
	}
	for k, n := range byKind {
		if n == 0 {
			delete(byKind, k)
		}
	}
	if len(byKind) > 0 {
		s.ByKind = byKind
	}
	return s
}

// reconcileInitial repairs revisions that claim to begin a history they no longer begin.
//
// `initial` is set when a revision has no predecessor, and a backfill that adds older
// revisions makes that false for whichever revision used to be oldest — a real sequence, not
// a corner: backfill six months, then backfill everything. The stale flag is not cosmetic,
// because an initial revision's counts are ABSOLUTE, so the row reports the repository's
// whole graph as one revision's work, in the middle of a timeline.
//
// Only revisions whose contents are still stored can be repaired; one whose blob has aged
// out keeps its flag, which is the honest outcome — there is nothing left to compute a delta
// from.
func (r *Runner) reconcileInitial(root string) int {
	entries, err := history.Read(root)
	if err != nil || len(entries) < 2 {
		return 0
	}
	ordered := history.SortedByTime(entries)

	fixed := 0
	// Both directions. A revision that is no longer the oldest must lose the claim, and the
	// one that IS oldest must carry it — the second case arises when a backfill inserts
	// revisions before the previous start, and, less obviously, when an earlier repair ran
	// against a mis-ordered timeline.
	for i, e := range ordered {
		wantInitial := i == 0
		if e.Summary.Initial == wantInitial {
			continue
		}
		cur, err := history.Load(root, e)
		if err != nil {
			continue // contents aged out; nothing left to recompute from
		}
		var prev *facts.Snapshot
		if !wantInitial {
			if prev, err = history.Load(root, ordered[i-1]); err != nil {
				continue
			}
		}
		if err := inthistory.RewriteSummary(root, e.ID, e.Seq, summarizeAgainst(prev, cur)); err != nil {
			continue
		}
		fixed++
	}
	return fixed
}

// lastTodoIndex is the position of the final commit still needing work, so the walk stops
// there rather than loading stored graphs nothing will be compared against.
func lastTodoIndex(selected []commitInfo, done map[string]bool) int {
	last := -1
	for i, c := range selected {
		if !done[c.SHA] {
			last = i
		}
	}
	return last
}

// loadRecorded returns the stored graph of an already-recorded commit, or nil when it can no
// longer be read (retention drops old contents). A missing predecessor costs one revision an
// accurate delta; it must not cost the run.
func loadRecorded(root, sha string) *facts.Snapshot {
	entries, err := history.Read(root)
	if err != nil {
		return nil
	}
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Commit() == sha {
			snap, err := history.Load(root, entries[i])
			if err != nil {
				return nil
			}
			return snap
		}
	}
	return nil
}

// recordedCommits is the set of commits the history already holds, so a re-run resumes
// rather than repeating.
func recordedCommits(root string) map[string]bool {
	done := map[string]bool{}
	entries, err := history.Read(root)
	if err != nil {
		return done
	}
	for _, e := range entries {
		if c := e.Commit(); c != "" {
			done[c] = true
		}
	}
	return done
}

// repoIdentityOf returns the portable identity of the repository being backfilled.
//
// Taken from the REPOSITORY rather than from each extracted tree: a scratch directory has
// no remote and a name like enola-backfill-317852, so identity derived there would differ
// per revision and per run — and identity is the field two machines' histories are
// reconciled on.
func repoIdentityOf(repoPath string) string {
	meta := facts.SnapshotMeta{RepoPath: repoPath}
	if remote, err := exec.Command("git", "-C", repoPath, "remote", "get-url", "origin").Output(); err == nil {
		meta.Git = &facts.GitInfo{Remote: facts.NormalizeRemote(strings.TrimSpace(string(remote)))}
	}
	return facts.RepoIdentity(meta)
}

func isGitRepo(path string) bool {
	return exec.Command("git", "-C", path, "rev-parse", "--git-dir").Run() == nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// backfillOptions appends under the retention a live snapshot is recorded with. The total
// cap was once left out here, so a backfill of more than 200 commits kept only the newest
// 200 while reporting every one of them as backfilled.
func backfillOptions(h config.HistoryConfig, c *inthistory.Contents) inthistory.Options {
	return inthistory.Options{
		WorkingKeep:  h.WorkingKeep,
		RevisionKeep: h.RevisionKeep,
		BlobKeep:     h.BlobKeep,
		Contents:     c,
	}
}
