package command

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/enola-labs/enola/internal/config"
	"github.com/enola-labs/enola/internal/conformance"
	"github.com/enola-labs/enola/internal/diff"
	"github.com/enola-labs/enola/internal/engine"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/workspace"
	"github.com/enola-labs/enola/pkg/bootstrap"
	"github.com/enola-labs/enola/pkg/check"
	"github.com/enola-labs/enola/pkg/cli"
	pkghistory "github.com/enola-labs/enola/pkg/history"
)

// target is the repository the gate operates on, plus how it was resolved — reported to
// the user so which repo and which config were used is never a guess.
type target struct {
	engine     *bootstrap.Engine
	repoPaths  []string
	configNote string
	cfgPath    string
	historyDir string
}

// resolveTarget turns the single positional argument into an engine pointed at the right
// repository. The argument is a REPOSITORY when it names a directory and a CONFIG FILE
// when it names a file — the same disambiguation --explain uses, so `enola check
// /path/to/repo` means what a reader expects.
//
// Without this, a directory argument fell through to config.Load, which cannot read a
// directory: it warned and silently used built-in defaults, whose `repo: .` is the WORKING
// DIRECTORY. So `enola check /path/to/other/repo` analysed whichever repo you happened to
// be standing in and graded it against that repo's baseline — the wrong answer, presented
// as a real verdict.
//
// When the argument is a directory, a mcp-arch.yaml INSIDE it is picked up if present.
// That keeps `baseline pin` and `check` resolving identically, which matters more than it
// looks: the config determines the ignore globs, and a pin and a check that disagreed on
// them would differ in ignore-glob hash and make every diff decline as incomparable.
func (r *Runner) resolveTarget(arg string) target {
	cfgPath := "mcp-arch.yaml"
	repoOverride := ""

	switch {
	case arg == "":
	case isDirectory(arg):
		abs, err := filepath.Abs(arg)
		if err != nil {
			r.checkFatal("resolving repo path %q: %v", arg, err)
		}
		repoOverride = abs
		if inner := filepath.Join(abs, "mcp-arch.yaml"); fileExists(inner) {
			cfgPath = inner
		}
	default:
		cfgPath = arg
	}

	eng, cfg, err := r.newEngine(bootstrap.Options{ConfigPath: cfgPath})
	if err != nil {
		r.checkFatal("failed to create engine: %v", err)
	}

	// The note names the config that was LOADED, not the one that was looked for.
	// The two differ whenever the lookup falls back — to built-in defaults, or to a
	// config sitting beside the binary — and a note that reports the intent as
	// though it were the outcome is worse than none: it is a confirmation of
	// something nobody checked.
	note := "built-in default config"
	if cfg.SourcePath != "" {
		note = "config " + cfg.SourcePath
	}
	if repoOverride != "" {
		note = "repo " + repoOverride + " (" + note + ")"
	}
	if repoOverride != "" {
		// Repos would otherwise win in RepoPaths and silently ignore the directory the
		// caller named.
		cfg.Repo = repoOverride
		cfg.Repos = nil
	}
	repoPaths, err := cfg.RepoPaths()
	if err != nil {
		r.checkFatal("failed to resolve repo path: %v", err)
	}
	// A folder of several git repositories resolves to one repository here, as it does
	// everywhere. Say so on stderr, which leaves --json output a clean document.
	if len(cfg.Repos) == 0 && len(repoPaths) == 1 {
		if folded := workspace.Folded(repoPaths[0], cfg.SourcePath); len(folded) > 0 {
			fmt.Fprint(os.Stderr, workspace.FoldWarning(r.name(), repoPaths[0], folded))
		}
	}
	return target{engine: eng, repoPaths: repoPaths, configNote: note, cfgPath: cfgPath, historyDir: cfg.History.Dir}
}

// parseFailOn splits a --fail-on spec into the explainer names that exist and the ones
// that do not, so the caller can refuse rather than enforce a policy it could not read.
//
// An unrecognised name used to be accepted and enforce nothing, which is the one failure
// this flag must not have: `--fail-on=cyles` exited 0 on a tree where `--fail-on=cycles`
// exited 1, so a typo in a CI config was indistinguishable from a passing build for as
// long as nobody read the policy line.
//
// Matching is EXACT. `CYCLES` is not `cycles`, because a case-insensitive match would be
// a guess about which explainer was meant, and this flag exists to remove guesses about
// what fails. A spec mixing good and bad names reports the bad ones rather than quietly
// enforcing the good half — half a policy is the same defect wearing a smaller number.
func parseFailOn(spec string) (named, unknown []string) {
	for _, raw := range strings.Split(spec, ",") {
		n := strings.TrimSpace(raw)
		if n == "" {
			continue
		}
		if slices.Contains(config.KnownExplainers, n) {
			named = append(named, n)
			continue
		}
		unknown = append(unknown, n)
	}
	return named, unknown
}

// runCheck is the `enola check` gate: snapshot the repo, diff it against a baseline,
// grade the delta, and exit with a code CI can act on.
//
// It never returns — it exits with the verdict's code. Exit codes are the contract:
// 0 clean, 1 regression, 2 usage/operational error, 3 declined (not comparable).
func (r *Runner) Check(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		baseline      = fs.String("baseline", "pinned", "what to compare against: pinned, previous, or a path to a directory holding facts.jsonl")
		failOn        = fs.String("fail-on", "", "comma-separated explainer names whose new findings fail (default: none — the run reports and exits 0)")
		minConfidence = fs.Float64("min-confidence", 0, "confidence floor within --fail-on explainers (default: 1.00)")
		warnOnly      = fs.Bool("warn-only", false, "downgrade a --fail-on / --max-spillover policy to warnings (blocking/usage errors still apply)")
		asJSON        = fs.Bool("json", false, "emit the verdict as JSON instead of text (an alias of -format json)")
		format        = fs.String("format", "text", "how to write the verdict: text, json, sarif, or annotations")
		host          = fs.String("host", "", "with -format annotations, the CI that shows them: buildkite or github")
		link          = fs.String("link", "", "with -host buildkite, the pull request's files view to link each line into")
		focus         = fs.String("focus", "", "narrow the delta to entries referencing this module/file/symbol")
		detail        = fs.Bool("detail", false, "print the full delta (changed edges and facts) under the verdict")
		write         = fs.Bool("write", false, "persist snapshot artifacts to .enola/ (default: read-only, nothing is written)")
		target        = fs.String("target", "", "the symbol, type or package you INTENDED to change; packages reached outside its predicted blast radius are reported as spillover")
		expected      = fs.String("expected", "", "comma-separated packages you expected to touch, in addition to the predicted radius")
		maxSpillover  = fs.Int("max-spillover", -1, "fail when more than N packages are reached outside the declared scope (default: report only, never fail)")
		reviewers     = fs.Bool("reviewers", false, "report who owns the modules this change touched, and who should review it (reads git author names)")
		reviewWindow  = fs.Int("reviewer-window", 500, "with --reviewers, how many recent commits authorship is measured over")
		author        = fs.String("author", "", "with --reviewers, whose change this is (default: git config user.name)")
	)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "Usage: "+r.name()+" check [flags] [config_path]\n\n"+
			"Grade what a change did to the architecture, against a pinned baseline.\n\n"+
			"Nothing fails until you say what should: with no --fail-on and no --max-spillover\n"+
			"every finding is reported and the run exits 0. The names --fail-on accepts are\n"+
			// Printed from the same list the validation checks, so the two cannot
			// disagree — which they did, for four explainers, until v0.4.0.
			"exactly these, and are case-sensitive:\n  "+strings.Join(config.KnownExplainers, ", ")+"\n\n"+
			"Exit codes:\n"+
			"  0  clean      nothing the policy enforces\n"+
			"  1  regression the policy was violated\n"+
			"  2  error      the gate could not run (no baseline, bad flag, inverted pair)\n"+
			"  3  declined   the baseline is not comparable; refusing to grade\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		os.Exit(check.StatusUsageError.ExitCode())
	}
	if *asJSON {
		*format = string(check.FormatJSON)
	}
	outFormat, err := check.ParseFormat(*format)
	if err != nil {
		r.checkFatal("%v", err)
	}
	outHost, err := check.ParseHost(*host)
	if err != nil {
		r.checkFatal("%v", err)
	}
	if outFormat == check.FormatAnnotations && outHost == check.HostNone {
		r.checkFatal("-format annotations needs -host buildkite or -host github: the host is named, never detected")
	}

	var arg string
	if rest := fs.Args(); len(rest) > 0 {
		arg = rest[0]
	}

	policy := check.Policy{
		MinConfidence: *minConfidence,
		WarnOnly:      *warnOnly,
	}
	// Spillover is REPORTED by default and fails only when a bound is given. A scope
	// check that broke builds the moment someone first passed --target would teach
	// people not to pass it.
	if *maxSpillover >= 0 {
		policy.Thresholds = append(policy.Thresholds, check.Threshold{
			Measurement: "spillover_packages",
			FailAt:      *maxSpillover + 1, // "allow up to N" — so N+1 is the failure
		})
	}
	if *failOn != "" {
		named, unknown := parseFailOn(*failOn)
		policy.FailExplainers = append(policy.FailExplainers, named...)
		if len(unknown) > 0 {
			noun := "no explainer is called"
			if len(unknown) > 1 {
				noun = "no explainers are called"
			}
			fmt.Fprintf(os.Stderr, "%s check: --fail-on names what %s: %s\n",
				r.name(), noun, strings.Join(unknown, ", "))
			fmt.Fprintf(os.Stderr, "The %d names it accepts are: %s\n",
				len(config.KnownExplainers), strings.Join(config.KnownExplainers, ", "))
			fmt.Fprintf(os.Stderr, "Refusing to run: a policy that enforces nothing must not look like a passing one.\n")
			os.Exit(check.StatusUsageError.ExitCode())
		}
	}

	tgt := r.resolveTarget(arg)
	eng, repoPaths := tgt.engine, tgt.repoPaths
	fmt.Fprintf(os.Stderr, r.name()+" check: %s\n", tgt.configNote)

	// Read-only unless --write. Skipping WriteArtifacts is what makes it read-only:
	// that is where the snapshot is persisted AND where the previous/ rotation happens,
	// so a bare `enola check` leaves both the pinned baseline and previous/ untouched
	// and can be run repeatedly against the same baseline. SetPersistCache(false)
	// additionally keeps the extractor cache off disk, matching --explain.
	if !*write {
		eng.SetPersistCache(false)
	}
	for i, repoPath := range repoPaths {
		if _, err := eng.GenerateSnapshot(ctx, repoPath, i > 0); err != nil {
			r.checkFatal("snapshot generation failed for %s: %v", repoPath, err)
		}
	}
	if *write {
		for _, repoPath := range repoPaths {
			if err := eng.WriteArtifacts(repoPath); err != nil {
				r.checkFatal("failed to write artifacts for %s: %v", repoPath, err)
			}
		}
	}

	snap := eng.Snapshot()
	if snap == nil || eng.Store().Count() == 0 {
		r.checkFatal("snapshot produced no facts for %s", strings.Join(repoPaths, ", "))
	}
	// Build current from the store so it reflects the whole (possibly multi-repo) graph
	// rather than only the last repo indexed — the same construction diff_snapshot uses,
	// FactsRef included: diff.Compute reads its inputs and the published bundle is
	// immutable, so copying the fact set here would buy nothing.
	// The baseline is anchored on the FIRST repo: that is the one whose snapshot reset
	// the graph, and it is where `enola baseline pin` writes. Its meta carries that
	// member's provenance, so the current side is read for the same member.
	anchor := repoPaths[0]
	current := &facts.Snapshot{Meta: eng.MetaFor(anchor), Facts: eng.Store().FactsRef(), Insights: snap.Insights}
	baseDir := engine.ResolveBaselineDir(eng.OutputDir(anchor), *baseline)
	base, err := bootstrap.LoadSnapshotDir(baseDir)
	if err != nil {
		r.checkFatal("%s", r.baselineHelp(*baseline, baseDir, anchor, err))
	}

	// The committed suppression ledger joins the policy before grading. A ledger
	// that cannot be parsed is an operational failure, never a silent skip: half
	// a ledger would silence findings nobody signed off, or fail ones somebody did.
	suppressions, err := check.LoadSuppressions(anchor)
	if err != nil {
		r.checkFatal("%v", err)
	}
	policy.Suppressions = suppressions

	d := diff.ComputeChanged(base, current, changedFilesBetween(anchor, base.Meta.Git, current.Meta.Git))
	if *focus != "" {
		d = d.Focused(*focus)
	}

	// Conformance: did the change stay inside what the caller declared? Computed only
	// when something WAS declared — a gate that graded scope nobody stated would be
	// grading its own guess.
	var measurements []check.Measurement
	var conf *conformance.Report
	if *target != "" || *expected != "" {
		baseStore := facts.NewStore()
		baseStore.Add(base.Facts...)
		rep := conformance.Compute(baseStore, eng.Store(), d, conformance.Options{
			Target:           *target,
			ExpectedPackages: splitList(*expected),
		})
		conf = &rep
		// Reported as a MEASUREMENT, not graded here. Whether spillover fails a build is
		// policy, and policy lives in one place — otherwise this gate and any other
		// surface computing the same number would come to disagree about it.
		measurements = append(measurements, check.Measurement{
			Name:  "spillover_packages",
			Label: "package(s) reached outside the declared scope",
			Count: len(rep.Spillover),
		})
	}

	// EvaluateCurrent, not Evaluate: strict-mode constraint violations grade
	// against the CURRENT snapshot, baselined or not — the one deliberate
	// exception to delta scoping.
	verdict := check.EvaluateCurrent(d, policy, current.Insights, measurements...)
	blame := check.NewBlameReader(anchor, eng.OutputDir(anchor))
	verdict = check.ApplyTime(verdict, base, r.revisionAt(anchor, tgt.historyDir), blame.Age)
	if err := blame.Save(); err != nil {
		fmt.Fprintf(os.Stderr, "enola: blame cache not written: %v\n", err)
	}
	verdict = check.RegradeIntersection(verdict, base, current, policy,
		check.OwnershipFromExtractors(eng.Extractors()), current.Insights, *focus, measurements...)
	verdict = check.AttachGuidance(verdict, eng.Store())
	// Opt-in, and gated here rather than inside AttachReviewers, so that without the
	// flag no git author name is read, computed, or printed at all.
	if *reviewers {
		verdict = check.AttachReviewers(verdict, eng.Store(),
			check.ReadAuthorship(anchor, *reviewWindow, eng.Store()),
			check.Actor(anchor, *author))
	}
	verdict = check.AttachCensus(verdict, current.Meta, policy, current.Insights)
	verdict = check.AttachLedger(verdict, eng.Store(), policy, current.Insights, time.Now())

	switch outFormat {
	case check.FormatText:
		if conf != nil {
			fmt.Print(conf.Render())
		}
		fmt.Print(verdict.Render())
		if *detail {
			fmt.Printf("\n%s\n", verdict.Detail())
		}
	default:
		out, err := verdict.Write(check.Output{
			Format: outFormat,
			Host:   outHost,
			Link:   *link,
			// A SARIF document is uploaded and attributed, so it names the
			// binary that actually graded the change. See check.Tool.
			Tool: check.Tool{Name: r.name(), Version: r.buildVersion()},
		})
		if err != nil {
			r.checkFatal("failed to encode verdict: %v", err)
		}
		fmt.Println(strings.TrimRight(string(out), "\n"))
	}
	// After the verdict and on STDERR, in both output modes. Stderr because `--json`
	// promises stdout is a verdict document and nothing else, and after because a
	// housekeeping note must never be the first thing read when the gate just failed.
	// Only when --write actually persisted a snapshot: without it the dashboard
	// would read whatever a PRIOR --generate left behind, which is not what this
	// run graded and would be a misleading thing to point someone at.
	if *write && cli.ShowDashboardHint(os.Stderr) {
		fmt.Fprint(os.Stderr, cli.DashboardHint(r.name(), arg))
	}
	r.updateNotice(os.Stderr)
	os.Exit(verdict.ExitCode())
}

// splitList parses a comma-separated flag into a trimmed, non-empty list.
func splitList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// baselineHelp turns a failed baseline load into an actionable message rather than a
// bare stat error — the most common way a first run of `enola check` fails is simply
// that no baseline was ever pinned.
// repoHint is the repository the caller named, echoed back into the suggested commands so
// they can be pasted as-is rather than adapted.
func (r *Runner) baselineHelp(selector, dir, repoHint string, err error) string {
	switch strings.ToLower(strings.TrimSpace(selector)) {
	case "", "pinned":
		return fmt.Sprintf("no pinned baseline at %s\n\n"+
			"The gate needs a \"before\" to compare against, pinned BEFORE you edit:\n"+
			"    "+r.name()+" baseline pin %s\n"+
			"    …make your change…\n"+
			"    "+r.name()+" check %s\n\n"+
			"Or compare against the immediately-preceding snapshot with --baseline=previous.",
			dir, repoHint, repoHint)
	case "previous":
		return fmt.Sprintf("no previous snapshot at %s — that requires at least two snapshots "+
			"written with `"+r.name()+" --generate` (note `"+r.name()+" check` is read-only by default and does "+
			"not rotate it).\n\nFor a stable baseline across several rounds of edits, prefer:\n"+
			"    "+r.name()+" baseline pin %s", dir, repoHint)
	default:
		return fmt.Sprintf("could not load baseline from %q: %v", dir, err)
	}
}

// checkFatal reports an operational failure and exits 2 — distinct from a regression,
// because the gate did not run at all.
func (r *Runner) checkFatal(format string, args ...any) { r.cmdFatal("check", format, args...) }

// cmdFatal reports an operational failure and exits 2 — distinct from a regression,
// because the command did not run at all. The command name is a parameter so a shared
// helper cannot misattribute an error to the wrong subcommand.
func (r *Runner) cmdFatal(cmd, format string, args ...any) {
	r.writeFatal(os.Stderr, cmd, format, args...)
	os.Exit(check.StatusUsageError.ExitCode())
}

// writeFatal renders the failure. Split from cmdFatal only because cmdFatal ends in
// os.Exit, which no test can call and return from.
//
// THE UPDATE NOTICE BELONGS ON THIS PATH, not only on the successful one. An operational
// failure is the case where being behind the release stream is most likely to be the
// CAUSE rather than a footnote: when the extractors have moved, an old build detects no
// language where a current one detects several, and the run dies here with "snapshot
// produced no facts" and a wall of "extractor X: not detected". Withholding the one line
// that explains that — because the command failed — leaves someone debugging a
// repository that is fine.
//
// After the error, never before it: what failed is what they need first.
func (r *Runner) writeFatal(w io.Writer, cmd, format string, args ...any) {
	_, _ = fmt.Fprintf(w, r.name()+" "+cmd+": "+format+"\n", args...)
	r.updateNotice(w)
}

// runBaseline is `enola baseline pin|show|clear` — the CLI half of the loop, so the
// baseline can be managed without an agent.
func (r *Runner) Baseline(args []string) {
	fs := flag.NewFlagSet("baseline", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "Usage: "+r.name()+" baseline <pin|show|clear> [config_path]\n\n"+
			"  pin    freeze the current .enola snapshot as the diff baseline\n"+
			"  show   report whether a baseline exists, and what it describes\n"+
			"  clear  remove the pinned baseline\n")
	}
	if err := fs.Parse(args); err != nil {
		os.Exit(check.StatusUsageError.ExitCode())
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fs.Usage()
		os.Exit(check.StatusUsageError.ExitCode())
	}
	action := strings.ToLower(rest[0])

	var arg string
	if len(rest) > 1 {
		arg = rest[1]
	}
	tgt := r.resolveTarget(arg)
	eng := tgt.engine
	anchor := tgt.repoPaths[0]
	outDir := eng.OutputDir(anchor)
	baseDir := engine.ResolveBaselineDir(outDir, "pinned")

	switch action {
	case "pin":
		// Snapshot first, then pin. "Pin a baseline of this repo" is one intent, and
		// requiring a separate --generate made the first useful thing a new user does a
		// two-command ritual whose failure mode ("no snapshot to pin") explained the
		// mechanism rather than the goal.
		//
		// Pinning whatever happened to be on disk could freeze a snapshot from days ago
		// as "the state before my change", so the reuse below is gated on the on-disk
		// snapshot proving it describes today's tree under today's build and config.
		fmt.Fprintf(os.Stderr, r.name()+" baseline: %s\n", tgt.configNote)
		// A snapshot that already describes every working tree under this build and
		// config is the snapshot a regenerate would produce, byte for byte, so it is
		// pinned as it stands. Anything less (a moved file, another extractor version,
		// another config, a member with no snapshot) regenerates, with linking and the
		// explainers deferred to the cluster's last turn the way --generate defers them.
		if generatedAt, stale := snapshotIsCurrent(eng, tgt.repoPaths); stale == "" {
			fmt.Fprintf(os.Stderr, r.name()+" baseline: the snapshot written %s matches every working tree under this build and config; pinned without regenerating\n", generatedAt)
		} else {
			fmt.Fprintf(os.Stderr, r.name()+" baseline: regenerating, %s\n", stale)
			for i, repoPath := range tgt.repoPaths {
				eng.SetDeferLinking(i < len(tgt.repoPaths)-1)
				if _, err := eng.GenerateSnapshot(context.Background(), repoPath, i > 0); err != nil {
					r.checkFatal("snapshot generation failed for %s: %v", repoPath, err)
				}
			}
			for _, repoPath := range tgt.repoPaths {
				if err := eng.WriteArtifacts(repoPath); err != nil {
					r.checkFatal("failed to write artifacts for %s: %v", repoPath, err)
				}
			}
		}
		if err := eng.SetBaseline(anchor); err != nil {
			r.checkFatal("could not pin baseline: %v", err)
		}
		fmt.Printf("Baseline pinned for %s\n", anchor)
		if snap, err := bootstrap.LoadSnapshotDir(baseDir); err == nil {
			describeBaseline(snap)
		}
		fmt.Printf("\nNow make your changes, then run:\n    %s check %s\n", r.name(), anchor)
	case "show":
		snap, err := bootstrap.LoadSnapshotDir(baseDir)
		if err != nil {
			fmt.Printf("No baseline pinned (%s does not hold a snapshot).\nRun `%s --generate` then `%s baseline pin`.\n", baseDir, r.name(), r.name())
			os.Exit(check.StatusUsageError.ExitCode())
		}
		fmt.Printf("Baseline at %s\n", baseDir)
		describeBaseline(snap)
	case "clear":
		if err := os.RemoveAll(baseDir); err != nil {
			r.checkFatal("could not clear baseline: %v", err)
		}
		fmt.Printf("Baseline cleared (%s removed).\n", baseDir)
	default:
		fmt.Fprintf(os.Stderr, r.name()+" baseline: unknown action %q\n", action)
		fs.Usage()
		os.Exit(check.StatusUsageError.ExitCode())
	}
}

// describeBaseline prints what a pinned baseline actually describes, so `show` answers
// "is this still the right thing to compare against?" rather than only "it exists".
func describeBaseline(snap *facts.Snapshot) {
	m := snap.Meta
	fmt.Printf("  Generated: %s\n", orUnknown(m.GeneratedAt))
	fmt.Printf("  Repo:      %s\n", orUnknown(m.RepoPath))
	if r := m.Receipt(); r.Git != nil {
		state := "clean"
		if r.Git.Dirty {
			state = "dirty (uncommitted changes)"
		}
		fmt.Printf("  Git:       %s @ %s — %s\n", orUnknown(r.Git.Ref), shortCommit(r.Git.Commit), state)
		// The repository identity a restored baseline is matched on. Shown because
		// "which repo does this baseline describe?" is the question `show` exists to
		// answer, and after an import the answer is no longer obvious from the path.
		if r.Git.Remote != "" {
			fmt.Printf("  Remote:    %s\n", r.Git.Remote)
		}
	}
	fmt.Printf("  Facts:     %d · Insights: %d\n", len(snap.Facts), len(snap.Insights))
	fmt.Printf("  Snapshot:  %s\n", orUnknown(m.Receipt().SnapshotID))
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(unknown)"
	}
	return s
}

func shortCommit(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return orUnknown(s)
}

// revisionAt reads the architecture history for a dated rule: the newest
// revision at or before the date, reconstructed from its blob, and the first
// revision's date for a rule that predates the record.
func (r *Runner) revisionAt(repoPath, historyDir string) check.RevisionAt {
	return func(date time.Time) (*facts.Snapshot, string, string, bool) {
		root, err := pkghistory.Root(repoPath, historyDir)
		if err != nil {
			return nil, "", "", false
		}
		entries, err := pkghistory.Read(root)
		if err != nil || len(entries) == 0 {
			return nil, "", "", false
		}
		first := ""
		var chosen *pkghistory.Entry
		for i := range entries {
			e := entries[i]
			at, err := time.Parse(time.RFC3339, e.At)
			if err != nil || e.Blob == nil {
				continue
			}
			if first == "" || at.Format("2006-01-02") < first {
				first = at.Format("2006-01-02")
			}
			if !at.After(date.Add(24*time.Hour-time.Nanosecond)) && (chosen == nil || e.At > chosen.At) {
				chosen = &entries[i]
			}
		}
		if chosen == nil {
			return nil, "", first, false
		}
		snap, err := pkghistory.Load(root, *chosen)
		if err != nil {
			return nil, "", first, false
		}
		return snap, chosen.At[:10], first, true
	}
}

// changedFilesBetween asks git which files the change touched between the
// two snapshots' commits, plus the working tree's own changes when the
// current snapshot was taken dirty. Nil when either side has no commit or
// git cannot answer, which hands the diff its fact-based fallback.
func changedFilesBetween(repoPath string, base, current *facts.GitInfo) []string {
	if base == nil || current == nil || base.Commit == "" || current.Commit == "" {
		return nil
	}
	args := []string{"-C", repoPath, "diff", "--name-only", base.Commit, current.Commit}
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		return nil
	}
	files := strings.Fields(string(out))
	if current.Dirty {
		if dirty, err := exec.Command("git", "-C", repoPath, "diff", "--name-only", "HEAD").Output(); err == nil {
			files = append(files, strings.Fields(string(dirty))...)
		}
		if untracked, err := exec.Command("git", "-C", repoPath, "ls-files", "--others", "--exclude-standard").Output(); err == nil {
			files = append(files, strings.Fields(string(untracked))...)
		}
	}
	if files == nil {
		files = []string{}
	}
	return files
}
