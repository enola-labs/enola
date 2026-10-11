package command

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/enola-labs/enola/internal/config"
	inthistory "github.com/enola-labs/enola/internal/history"
	"github.com/enola-labs/enola/pkg/history"
)

func (r *Runner) History(args []string) {
	if len(args) == 0 {
		r.historyUsage()
		os.Exit(2)
	}
	switch strings.ToLower(args[0]) {
	case "push":
		r.historyPush(args[1:])
	case "pull":
		r.historyPull(args[1:])
	case "verify":
		r.historyVerify(args[1:])
	case "gc":
		r.historyGC(args[1:])
	default:
		r.historyUsage()
		os.Exit(2)
	}
}

func (r *Runner) historyUsage() {
	fmt.Fprint(os.Stderr,
		"Usage: "+r.name()+" history <push|pull|verify|gc> [store_dir] [flags] [repo_path|config_path]\n\n"+
			"Share the architecture history through a directory store — a git repository, a\n"+
			"shared mount, an S3-synced folder. Plain files, no daemon, no database.\n\n"+
			"  push    copy local revisions into the store, and record them on this\n"+
			"          machine's chain\n"+
			"  pull    import revisions other machines pushed into the local history\n"+
			"  verify  walk every chain and report gaps and tampering by name\n"+
			"  gc      apply a retention policy to the store; prints what it would remove\n"+
			"          and deletes nothing without --apply\n\n"+
			"The store directory is the first argument, or `history.shared_dir` in the config.\n")
}

func (r *Runner) historyTarget(rest []string) (string, string, *config.Config) {
	storeArg := ""
	repoArg := ""
	if len(rest) > 0 {
		storeArg = rest[0]
	}
	if len(rest) > 1 {
		repoArg = rest[1]
	}
	repoPath, cfg := r.logTarget(repoArg)
	if storeArg == "" {
		storeArg = cfg.History.SharedDir
		if storeArg != "" && !filepath.IsAbs(storeArg) {
			storeArg = filepath.Join(repoPath, storeArg)
		}
	}
	if storeArg == "" {
		r.historyFatal("no store directory given and history.shared_dir is not set — pass the store as the first argument, e.g. `%s history push /mnt/arch-history`", r.name())
	}
	abs, err := filepath.Abs(storeArg)
	if err != nil {
		r.historyFatal("resolving store path %q: %v", storeArg, err)
	}
	return abs, repoPath, cfg
}

func (r *Runner) localHistoryRoot(repoPath string, cfg *config.Config) string {
	root, err := history.Root(repoPath, cfg.History.Dir)
	if err != nil {
		r.historyFatal("cannot locate the local history: %v", err)
	}
	return root
}

func (r *Runner) historyPush(args []string) {
	fs := flag.NewFlagSet("history push", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	all := fs.Bool("all", false, "also push working revisions (snapshots of uncommitted trees)")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr,
			"Usage: "+r.name()+" history push [flags] [store_dir] [repo_path|config_path]\n\n"+
				"Copy this machine's recorded revisions into the shared store. Idempotent:\n"+
				"a revision already recorded on this machine's chain is not pushed again,\n"+
				"and an entry file already in the store is never rewritten — entries are\n"+
				"content-addressed, so the same snapshot is the same file everywhere.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(flagsFirst(fs, args)); err != nil {
		os.Exit(2)
	}
	store, repoPath, cfg := r.historyTarget(fs.Args())
	root := r.localHistoryRoot(repoPath, cfg)

	rep, err := inthistory.Push(root, store, inthistory.PushOptions{IncludeWorking: *all})
	if err != nil {
		r.historyFatal("%v", err)
	}
	fmt.Print(renderPush(rep))
}

func renderPush(rep inthistory.PushReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s (source %s)\n", rep.Store, rep.Source)
	fmt.Fprintf(&b, "  pushed        %d revision(s), %d payload file(s) new\n", rep.Pushed, rep.EntriesWritten)
	if rep.AlreadyThere > 0 {
		fmt.Fprintf(&b, "  already there %d\n", rep.AlreadyThere)
	}
	if rep.Working > 0 {
		fmt.Fprintf(&b, "  skipped       %d working revision(s) (uncommitted trees; --all pushes them)\n", rep.Working)
	}
	if n := len(rep.HeaderOnly); n > 0 {
		fmt.Fprintf(&b, "  skipped       %d header-only revision(s), contents no longer stored locally: %s\n",
			n, strings.Join(rep.HeaderOnly, ", "))
	}
	for _, u := range rep.Unavailable {
		fmt.Fprintf(&b, "  skipped       %s\n", u)
	}
	return b.String()
}

func (r *Runner) historyPull(args []string) {
	fs := flag.NewFlagSet("history pull", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	repo := fs.String("repo", "", "repository identity to pull (e.g. github.com/org/repo); default: the local history's own")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr,
			"Usage: "+r.name()+" history pull [flags] [store_dir] [repo_path|config_path]\n\n"+
				"Import revisions other machines pushed into this machine's local history.\n"+
				"Idempotent: a revision already recorded locally is never imported twice.\n"+
				"Imported revisions carry the machine that observed them as their origin.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(flagsFirst(fs, args)); err != nil {
		os.Exit(2)
	}
	store, repoPath, cfg := r.historyTarget(fs.Args())
	root := r.localHistoryRoot(repoPath, cfg)

	rep, err := inthistory.Pull(root, store, inthistory.PullOptions{Repo: *repo})
	if err != nil {
		r.historyFatal("%v", err)
	}
	fmt.Print(renderPull(rep))
}

func renderPull(rep inthistory.PullReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s (repo %s)\n", rep.Store, rep.Repo)
	fmt.Fprintf(&b, "  pulled        %d revision(s)\n", rep.Pulled)
	if rep.AlreadyLocal > 0 {
		fmt.Fprintf(&b, "  already local %d\n", rep.AlreadyLocal)
	}
	if n := len(rep.Pruned); n > 0 {
		fmt.Fprintf(&b, "  pruned        %d revision(s) removed from the store by retention (recorded, not lost silently): %s\n",
			n, strings.Join(rep.Pruned, ", "))
	}
	if n := len(rep.Gaps); n > 0 {
		fmt.Fprintf(&b, "  gaps          %d revision(s) the store claims but does not hold: %s — run `verify`\n",
			n, strings.Join(rep.Gaps, ", "))
	}
	return b.String()
}

func (r *Runner) historyVerify(args []string) {
	fs := flag.NewFlagSet("history verify", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr,
			"Usage: "+r.name()+" history verify [store_dir] [repo_path|config_path]\n\n"+
				"Walk every chain in the shared store and verify it end to end: each record\n"+
				"must name its predecessor's hash, each payload must match the digest its\n"+
				"record carries, and each payload's facts must match their recorded hash.\n"+
				"A missing payload covered by a prune record is retention; one that is not\n"+
				"is a gap. Exits 1 when anything is broken, naming each problem.\n")
	}
	if err := fs.Parse(flagsFirst(fs, args)); err != nil {
		os.Exit(2)
	}
	store, _, _ := r.historyTarget(fs.Args())

	rep, err := history.VerifyShare(store)
	if err != nil {
		r.historyFatal("%v", err)
	}
	fmt.Print(renderVerify(rep))
	os.Exit(verifyExitCode(rep))
}

func verifyExitCode(rep history.ShareVerifyReport) int {
	if rep.Clean() {
		return 0
	}
	return 1
}

func renderVerify(rep history.ShareVerifyReport) string {
	var b strings.Builder
	if !rep.Clean() {
		fmt.Fprintf(&b, "%d problem(s):\n", len(rep.Problems))
		for _, p := range rep.Problems {
			switch {
			case p.ID != "":
				fmt.Fprintf(&b, "  %-11s %s — %s\n", p.Kind, history.ShortID(p.ID), p.Detail)
			case p.Line > 0:
				fmt.Fprintf(&b, "  %-11s %s.jsonl:%d — %s\n", p.Kind, p.Source, p.Line, p.Detail)
			default:
				fmt.Fprintf(&b, "  %-11s %s\n", p.Kind, p.Detail)
			}
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "%s\n", rep.Dir)
	fmt.Fprintf(&b, "  sources    %d (%s)\n", len(rep.Sources), strings.Join(rep.Sources, ", "))
	fmt.Fprintf(&b, "  revisions  %d — %d verified, %d pruned by retention\n", rep.Revisions, rep.Verified, rep.Pruned)
	if rep.Clean() {
		b.WriteString("\nEvery chain verifies: no gaps, no tampering.\n")
	}
	return b.String()
}

func (r *Runner) historyGC(args []string) {
	fs := flag.NewFlagSet("history gc", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		keepLast  = fs.Int("keep-last", 0, "keep the newest N revisions per repository")
		keepSince = fs.String("keep-since", "", "keep revisions after this time (RFC3339, or an age like 90d)")
		apply     = fs.Bool("apply", false, "actually delete; without it the removal is only printed")
	)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr,
			"Usage: "+r.name()+" history gc [flags] [store_dir] [repo_path|config_path]\n\n"+
				"Apply a retention policy to the shared store. Shared entries are never\n"+
				"deleted silently: without --apply this only prints what would go, and an\n"+
				"applied prune is recorded in the chain — so \"no data\" and \"pruned data\"\n"+
				"stay distinguishable forever. Revisions satisfying ANY given keep policy\n"+
				"are kept.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(flagsFirst(fs, args)); err != nil {
		os.Exit(2)
	}
	store, _, _ := r.historyTarget(fs.Args())

	opts := inthistory.SharedGCOptions{KeepLast: *keepLast, Apply: *apply}
	if *keepSince != "" {
		since, err := parseKeepSince(*keepSince)
		if err != nil {
			r.historyFatal("%v", err)
		}
		opts.KeepSince = since
	}
	rep, err := inthistory.SharedGC(store, opts)
	if err != nil {
		r.historyFatal("%v", err)
	}
	fmt.Print(renderSharedGC(rep))
}

func parseKeepSince(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if d, err := parseAge(s); err == nil {
		return time.Now().Add(-d), nil
	}
	return time.Time{}, fmt.Errorf("--keep-since %q is neither a time (2026-08-01T00:00:00Z) nor an age (90d, 720h)", s)
}

func renderSharedGC(rep inthistory.SharedGCReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", rep.Store)
	fmt.Fprintf(&b, "  revisions  %d — %d kept under policy %s\n", rep.Revisions, rep.Keep, rep.Policy)
	if len(rep.Remove) == 0 {
		b.WriteString("\nNothing to remove.\n")
		return b.String()
	}
	verb := "Would remove"
	if rep.Applied {
		verb = "Removed"
	}
	fmt.Fprintf(&b, "\n%s %d revision(s) (%s):\n", verb, len(rep.Remove), humanBytes(rep.BytesFreed))
	for _, removal := range rep.Remove {
		fmt.Fprintf(&b, "  %-7s  %s  %s  (pushed by %s)\n",
			history.ShortID(removal.ID), shortDate(removal.At), removal.Repo, removal.Source)
	}
	if rep.Applied {
		b.WriteString("\nThe prune is recorded in the chain: readers see these as pruned, never as missing.\n")
	} else {
		b.WriteString("\nNothing was deleted. Re-run with --apply to prune, which records the removal in the chain.\n")
	}
	return b.String()
}

func (r *Runner) historyFatal(format string, args ...any) { r.cmdFatal("history", format, args...) }

// flagsFirst reorders args so every flag precedes every positional argument.
//
// The standard flag package stops at the first positional, so `history gc
// /mnt/store --keep-last 500` read --keep-last as a second positional and then
// refused the command for having no retention policy. That is the order the
// documentation shows and the natural one to type, with the store as the object of
// the verb, so both orders are accepted. A flag's value travels with it; everything
// after a bare "--" is positional, as usual.
func flagsFirst(fs *flag.FlagSet, args []string) []string {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if len(arg) < 2 || arg[0] != '-' {
			positional = append(positional, arg)
			continue
		}
		flags = append(flags, arg)
		name := strings.TrimLeft(arg, "-")
		if strings.Contains(name, "=") {
			continue
		}
		// A flag that is not boolean takes the next argument as its value. An
		// unknown flag is left for Parse to report.
		f := fs.Lookup(name)
		if f == nil {
			continue
		}
		if b, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && b.IsBoolFlag() {
			continue
		}
		if i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	if len(positional) == 0 {
		return flags
	}
	return append(append(flags, "--"), positional...)
}
