package command

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/enola-labs/enola/internal/diff"
	"github.com/enola-labs/enola/internal/engine"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/filelock"
	"github.com/enola-labs/enola/internal/hookstate"
	"github.com/enola-labs/enola/internal/updatecheck"
	"github.com/enola-labs/enola/pkg/bootstrap"
	"github.com/enola-labs/enola/pkg/check"
)

// hookInput is the subset of the agent's hook payload enola reads. Unknown fields are
// ignored, so the payload growing does not break the hook.
type hookInput struct {
	CWD string `json:"cwd"`

	// StopHookActive is the harness's loop breaker, set on every Stop that is itself
	// the continuation of a previous Stop hook's output. Reading it is not optional:
	// see runStopHook for what ignoring it costs.
	StopHookActive bool `json:"stop_hook_active"`

	// SessionID scopes the once-per-report rule to a single session, so a suppression
	// cannot outlive the session it was decided in. See hookstate.Record.LastSession.
	SessionID string `json:"session_id"`
}

// stopHookOutput is the response shape for handing the model something to act on at the
// end of a turn.
//
// It does NOT hand it over passively. On Stop, `additionalContext` prevents the turn
// from ending: the harness feeds the text back to the model and stops again afterwards.
// This comment used to claim the opposite, as a stated fact that was never checked, and
// the whole defect in runStopHook grew out of believing it.
type stopHookOutput struct {
	HookSpecificOutput struct {
		HookEventName     string `json:"hookEventName"`
		AdditionalContext string `json:"additionalContext"`
	} `json:"hookSpecificOutput"`
}

// runHook dispatches `enola hook <event>`.
//
// ONE RULE GOVERNS EVERYTHING HERE: a hook must never break, delay or clutter a session.
// Every failure — no baseline, no snapshot, an unreadable payload, an incomparable
// baseline, a broken repo — exits 0 and says nothing. enola is a guest in someone else's
// session, and a guest that throws errors gets uninstalled.
//
// It also never exits 1 or 2. Exit 2 would block the agent, which is not this hook's
// job in advisory mode; exit 1 is a *non-blocking error* to the harness, so forwarding
// `enola check`'s regression code would surface as a hook failure rather than as the
// verdict it actually is.
func (r *Runner) Hook(ctx context.Context, args []string) {
	if len(args) == 0 {
		os.Exit(0)
	}
	switch args[0] {
	case "stop":
		r.runStopHook(ctx)
	case "session-start":
		r.runSessionStartHook(ctx, args[1:])
	case updateRefreshEvent:
		// The detached child SpawnUpdateRefresh starts. Nothing else: no engine, no
		// repository, no output. It is a hook event only to inherit this dispatcher's
		// silence and its unconditional exit 0.
		updatecheck.Refresh(ctx)
	default:
		// An event this build does not know about is not an error: a newer install may
		// have written config a older binary does not understand.
	}
	os.Exit(0)
}

// runStopHook grades the session's change and, only if it introduced something the user
// should see, hands the verdict back as context.
func (r *Runner) runStopHook(ctx context.Context) {
	in, err := readHookInput()
	if err != nil || in.CWD == "" {
		return
	}

	// The harness is already replaying a report this hook made, so this run says nothing.
	//
	// stop_hook_active is set on every Stop that is itself the continuation of a previous
	// Stop hook's output, and it exists because a Stop hook's output is not an annotation
	// on a finished turn: it PREVENTS the turn from ending. A hook that re-grades here
	// re-emits the identical verdict from an unchanged baseline, buys another model turn,
	// and repeats until the consecutive-block cap overrides it, so the session ends on the
	// harness's warning instead of on the report. Reported as issue #288.
	//
	// Checked BEFORE grading. A suppressed run must cost nothing, and grading first would
	// pay a full snapshot per loop iteration for output that is thrown away.
	if in.StopHookActive {
		hookstate.RecordSuppressed(r.outputDirFor(in.CWD), hookstate.EventStop)
		return
	}

	// Silence is the norm. The gate only has something to say when a baseline was pinned
	// during this session AND the change either regressed the architecture or introduced
	// a finding nothing enforced; every other path ends here, having printed nothing.
	verdict, outDir, ok := r.gradeQuietly(ctx, in.CWD)

	// Findings the run measured exactly and did not fail, because no policy named them.
	// The hook reports these: an agent that hears nothing after closing a layer violation
	// concludes the change was clean, and on a default install nothing fails at all.
	unenforced := []facts.Insight(nil)
	if ok && verdict.Status == check.StatusClean {
		unenforced = verdict.UnenforcedAtFloor()
	}

	// One switch decides both what to say and what identifies it. A non-empty key means
	// there is something to say, which is what lets the once-per-report rule below cover
	// every path rather than only the decline it started on.
	var key, context string
	switch {
	case ok && verdict.Status == check.StatusRegression:
		key = verdict.ReportKey()
		context = "enola graded the architectural change made in this session and found a structural " +
			"regression. This was not necessarily intended: review it before considering the task " +
			"finished, and either fix it or say why it is deliberate.\n\n" + verdict.Render()

	case len(unenforced) > 0:
		key = verdict.ReportKey()
		context = unenforcedReport(r.name(), verdict)

	case ok && verdict.Status == check.StatusIncomparable:
		// The gate could not grade at all, and saying nothing would be indistinguishable
		// from grading it clean. `enola check` spends a whole exit code (3) keeping those
		// apart so "I refuse to grade this" is never read as "your change is bad"; a hook
		// that stays silent collapses the same distinction in the other direction, and
		// leaves someone believing the loop is protecting them when it is not.
		key = verdict.ReportKey()
		context = "enola could NOT grade the architectural change made in this session: " +
			verdict.DeclineReason() + ".\n\n" +
			"This is NOT a statement about your change. The comparison itself was untrustworthy, " +
			"so no verdict was reached in either direction. Re-pin the baseline to restore grading " +
			fmt.Sprintf("(`%s baseline pin`, or the set_baseline tool), and `%s doctor` reports whether ", r.name(), r.name()) +
			"the hooks are grading again."
	}

	// Recorded on EVERY path, and the silent paths are the ones worth recording: a hook
	// that never fires and a hook that fires and finds nothing are indistinguishable in a
	// session, and only one of them is broken. See internal/hookstate.
	//
	// ShouldReport is asked BEFORE recording, because recording is what makes the next
	// identical report a repeat. Said once per distinct report per session, not once per
	// Stop: the whole cost of repeating it is in hookstate.ShouldReport.
	report := hookstate.Report{Key: key, Session: in.SessionID}
	say := hookstate.ShouldReport(outDir, hookstate.EventStop, report)
	hookstate.RecordFiredWithReport(outDir, hookstate.EventStop,
		stopOutcome(verdict, ok, len(unenforced) > 0), report)
	if !say {
		return
	}

	var out stopHookOutput
	out.HookSpecificOutput.HookEventName = "Stop"
	out.HookSpecificOutput.AdditionalContext = context

	encoded, err := json.Marshal(out)
	if err != nil {
		return
	}
	fmt.Println(string(encoded))
}

// detachedRunFlag marks the re-invocation that does the actual pinning. The hook the
// agent calls spawns a copy of itself carrying this flag, so the work happens in a
// process the agent does not own and does not wait for.
const detachedRunFlag = "--detached-run"

// autoPinMarker is written inside a baseline this hook pinned. Its presence is what
// distinguishes a baseline enola froze automatically from one a person or an agent chose
// deliberately — and only the former may be overwritten.
//
// Without it, a baseline pinned at the start of a multi-day refactor would be silently
// replaced at the next session start, destroying the very "before" it was recording.
const autoPinMarker = ".auto-pinned"

// runSessionStartHook freezes the architecture at the start of a session, so the change
// the session makes can be graded at the end of it.
//
// The snapshot NEVER runs in the hook itself. It costs 0.2 s on a small repository and
// over ten seconds on a large one, and a session start that stalls for ten seconds is a
// broken tool no matter how good the report at the other end is. So the hook spawns a
// detached copy of itself and returns immediately: session-start latency becomes the cost
// of one process spawn, independent of repository size. That is the only mitigation that
// is constant in the size of the repo rather than merely bounded — a timeout still pays
// the timeout.
func (r *Runner) runSessionStartHook(ctx context.Context, args []string) {
	if len(args) > 0 && args[0] == detachedRunFlag {
		// The detached child: the only place any work happens.
		if len(args) > 1 {
			r.pinBaselineSingleFlight(ctx, args[1])
		}
		return
	}

	in, err := readHookInput()
	if err != nil || in.CWD == "" || !isDirectory(in.CWD) {
		return
	}
	if !detachable {
		// Better to do nothing than to make every session start wait on a snapshot.
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}

	cmd := exec.Command(exe, "hook", "session-start", detachedRunFlag, in.CWD)
	// No stdio: the child must not write to the agent's streams, and an inherited pipe
	// would keep the hook's descriptors open after it returns.
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	detach(cmd)
	// Start, never Wait. Not reaping is deliberate: waiting is exactly what this must not
	// do, and the child is re-parented to init once this process exits.
	_ = cmd.Start()
}

// pinBaselineSingleFlight does the actual pin, in the detached child.
//
// Everything here is silent. It has no terminal, nobody is reading its output, and a hook
// that cannot fail loudly must not try.
func (r *Runner) pinBaselineSingleFlight(ctx context.Context, repoDir string) {
	// Refreshed here because this is already the one place enola does slow, unattended
	// work that nobody is waiting on — so the update check costs no extra process spawn
	// and cannot delay anything. It runs BEFORE the pin lock and every early return
	// below: skipping the pin is the common case (an unchanged tree, a deliberate
	// baseline, a sibling terminal holding the lock), and a check that only happened on
	// the rare path would go months without running. It is itself TTL-gated and
	// separately locked, so running it on every session start costs at most one request
	// per twelve hours per machine.
	updatecheck.Refresh(ctx)

	eng, cfg, err := r.newEngine(bootstrap.Options{ConfigPath: configForRepo(repoDir)})
	if err != nil {
		return
	}
	cfg.Repo, cfg.Repos = repoDir, nil
	repoPaths, err := cfg.RepoPaths()
	if err != nil || len(repoPaths) == 0 {
		return
	}
	anchor := repoPaths[0]
	outDir := eng.OutputDir(anchor)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return
	}

	// Recorded in the detached child rather than in the hook the agent calls, because
	// the parent returns before knowing anything and resolving the output dir there
	// would reintroduce the very work detaching exists to avoid. Doing nothing is still
	// a run: "skipped" and "never fired" are different states, and only one is a fault.
	outcome := hookstate.OutcomeSkipped
	defer func() { hookstate.RecordFired(outDir, hookstate.EventSessionStart, outcome) }()

	// Non-blocking: several agent terminals open on one repository is the documented
	// normal case, and queueing them would turn one redundant snapshot into a series of
	// them running back to back. Whoever gets the lock does the work; the rest do nothing.
	lock, ok, err := filelock.TryAcquire(filepath.Join(outDir, "session-pin"))
	if err != nil || !ok {
		return
	}
	defer lock.Release()

	baselineDir := engine.ResolveBaselineDir(outDir, "pinned")
	if !shouldAutoPin(baselineDir, anchor, cfg.Output.Dir, eng.CurrentMeta(anchor)) {
		return
	}

	for i, p := range repoPaths {
		if _, err := eng.GenerateSnapshot(ctx, p, i > 0); err != nil {
			return
		}
	}
	for _, p := range repoPaths {
		if err := eng.WriteArtifacts(p); err != nil {
			return
		}
	}
	if err := eng.SetBaseline(anchor); err != nil {
		return
	}
	// Stamp it as ours, so a later session may replace it and a deliberate pin may not be
	// replaced by us. Written after SetBaseline, which republishes the directory.
	_ = os.WriteFile(filepath.Join(baselineDir, autoPinMarker), nil, 0o644)
	outcome = hookstate.OutcomePinned
}

// shouldAutoPin decides whether to replace the existing baseline.
//
// Two rules, both about not destroying something the user meant to keep:
//
//   - A baseline without the auto-pin marker was pinned deliberately — by a person, or by
//     an agent following the server's prompt — and is left alone. Overwriting it would
//     discard the "before" of a refactor that may span days.
//   - An auto-pinned baseline is refreshed only when the tree has actually moved. If HEAD
//     matches and both sides are clean, re-snapshotting would burn seconds to produce a
//     byte-identical result.
//
// A dirty tree is never treated as current: "dirty" says the content is not identified by
// the commit, so two dirty trees at the same commit may differ arbitrarily.
//
// The third rule is about usefulness rather than freshness: an auto-pinned baseline that
// can no longer be COMPARED to a current snapshot — a different enola version, a changed
// extractor set or ignore globs — is not a baseline at all, and refreshing it costs one
// snapshot where leaving it costs the session's entire grading, silently. Tree movement
// alone missed this: a session starting on a clean unchanged tree after an upgrade
// graded against an unusable baseline and said nothing.
//
// Still only ever applied to baselines this hook created. A deliberate pin stays
// untouched even when unusable — replacing it would discard the "before" of a refactor
// that may span days, which is a worse outcome than a Stop hook that has to explain
// itself. That case is reported instead.
func shouldAutoPin(baselineDir, repoDir, outputDir string, current *facts.SnapshotMeta) bool {
	base, err := bootstrap.LoadSnapshotDir(baselineDir)
	if err != nil {
		return true // no baseline yet — this is exactly what the hook is for
	}
	if _, err := os.Stat(filepath.Join(baselineDir, autoPinMarker)); err != nil {
		return false // deliberately pinned; not ours to replace
	}
	if current != nil && baselineIsUnusable(base.Meta, *current) {
		return true
	}
	now := engine.GitState(repoDir, outputDir)
	if now == nil || base.Meta.Git == nil {
		return true // cannot prove it is current, so refresh
	}
	if now.Dirty || base.Meta.Git.Dirty {
		return true
	}
	return now.Commit != base.Meta.Git.Commit
}

// unenforcedReport is what an agent is handed when the session introduced a finding
// enola measures exactly and no policy enforced it.
//
// Reported, not failed — and the distinction has an owner. With no policy set, NOTHING
// has decided whether this change is acceptable: the gate had no grounds to, and an
// agent inferring a verdict from an unenforced finding is exactly the judgement enola
// declines to make on the user's behalf. So the instruction is to surface it and hand
// the decision over, rather than to fix it, revert it, or quietly accept it. Both silent
// outcomes are wrong for the same reason — each answers a question that was the user's.
func unenforcedReport(bin string, v check.Verdict) string {
	return "enola graded the architectural change made in this session. NOTHING FAILED: this " +
		"repository has no failure policy set, so the gate had no grounds to fail anything and the " +
		"run exited 0. The change did introduce findings enola measures exactly.\n\n" +
		"WHETHER THIS IS ACCEPTABLE IS THE USER'S DECISION, NOT YOURS. Before presenting this task " +
		"as finished: show them the findings below, say plainly that nothing was enforced and that " +
		"this is a report rather than a failed build, and ask them to decide — accept it as " +
		"intended, have you change it, or set a policy (`" + bin + " check --fail-on=…`) so that a " +
		"future run does fail on it. Do not revert or refactor on your own initiative over these, " +
		"and do not report the session as clean without mentioning them.\n\n" + v.Render()
}

// stopOutcome classifies a Stop-hook run for the heartbeat.
//
// The distinction that earns its keep is OutcomeDeclined: the hook is silent for an
// incomparable baseline exactly as it is silent for a clean change, and only a
// heartbeat can tell an operator which of the two has been happening all week.
//
// reported covers the other way the hook speaks without failing: a clean verdict
// carrying findings no policy enforces. A run that found those must not be filed as a
// silent clean one.
//
// It classifies what the run FOUND, not whether the words reached the agent. A repeat
// suppressed inside one session still found the finding, and filing it as clean would
// tell `doctor` the loop has gone quiet when the opposite is true.
func stopOutcome(v check.Verdict, ok, reported bool) hookstate.Outcome {
	if !ok {
		return hookstate.OutcomeUnavailable
	}
	switch {
	case v.Status == check.StatusRegression, reported:
		return hookstate.OutcomeReported
	case v.Status == check.StatusIncomparable:
		return hookstate.OutcomeDeclined
	default:
		return hookstate.OutcomeClean
	}
}

// baselineIsUnusable reports whether a BLOCKING comparability warning stands between
// these two snapshots — the same classification `enola check` uses to decline, so the
// hook refreshes exactly what the gate would have refused to grade against. Advisory
// warnings (a stale baseline) are deliberately not included: those still grade, and
// re-pinning on staleness would destroy the multi-day baseline the staleness warning
// exists to permit.
func baselineIsUnusable(base, current facts.SnapshotMeta) bool {
	return len(check.BlockingKinds(diff.CompareMeta(base, current))) > 0
}

// gradeQuietly runs the gate, returning ok=false for every reason a hook should stay
// silent rather than report a problem.
//
// It also returns the engine's output directory, which the caller needs to record the
// heartbeat. That is returned even on the failure paths wherever it is known, because
// "the hook ran and could not grade" is precisely the state worth having on record.
func (r *Runner) gradeQuietly(ctx context.Context, repoDir string) (check.Verdict, string, bool) {
	if !isDirectory(repoDir) {
		return check.Verdict{}, "", false
	}

	eng, cfg, err := r.newEngine(bootstrap.Options{ConfigPath: configForRepo(repoDir)})
	if err != nil {
		return check.Verdict{}, "", false
	}
	cfg.Repo, cfg.Repos = repoDir, nil
	repoPaths, err := cfg.RepoPaths()
	if err != nil || len(repoPaths) == 0 {
		return check.Verdict{}, "", false
	}
	anchor := repoPaths[0]
	outDir := eng.OutputDir(anchor)

	// Load the baseline BEFORE snapshotting. Without one there is nothing to grade, and
	// checking first means the common case costs nothing rather than paying for a full
	// snapshot to discover it was pointless.
	base, err := bootstrap.LoadSnapshotDir(engine.ResolveBaselineDir(outDir, "pinned"))
	if err != nil {
		return check.Verdict{}, outDir, false
	}

	// Read-only, exactly like `enola check`: the hook must not mutate the repository's
	// snapshot state as a side effect of observing it.
	eng.SetPersistCache(false)
	for i, p := range repoPaths {
		if _, err := eng.GenerateSnapshot(ctx, p, i > 0); err != nil {
			return check.Verdict{}, outDir, false
		}
	}
	snap := eng.Snapshot()
	if snap == nil || eng.Store().Count() == 0 {
		return check.Verdict{}, outDir, false
	}
	// FactsRef, not All: diff.Compute reads its inputs and the published bundle is
	// immutable. The hook runs on every agent edit, so a full fact-set copy here is
	// the one place the cost would be paid over and over.
	current := &facts.Snapshot{Meta: eng.MetaFor(repoDir), Facts: eng.Store().FactsRef(), Insights: snap.Insights}

	// The same ledger and strict semantics as `enola check`, so the two surfaces
	// cannot reach different verdicts from the same tree. The one divergence is
	// deliberate: a malformed ledger fails `check` loudly at exit 2, while the
	// hook — whose contract is to never disturb a session — proceeds without
	// suppressions and lets `check` deliver the actionable error.
	policy := check.Policy{}
	if suppressions, err := check.LoadSuppressions(anchor); err == nil {
		policy.Suppressions = suppressions
	}
	verdict := check.EvaluateCurrent(diff.Compute(base, current), policy, current.Insights)
	verdict = check.AttachCensus(verdict, current.Meta, policy, current.Insights)
	return check.AttachLedger(verdict, eng.Store(), policy, current.Insights, time.Now()), outDir, true
}

// outputDirFor resolves just the engine's output directory, for a path that must record
// a heartbeat without grading anything. Config resolution only: no snapshot, no store,
// nothing that scales with the size of the repository. Returns "" on every failure, and
// hookstate treats that as "nowhere to record" rather than as an error.
func (r *Runner) outputDirFor(repoDir string) string {
	if !isDirectory(repoDir) {
		return ""
	}
	eng, cfg, err := r.newEngine(bootstrap.Options{ConfigPath: configForRepo(repoDir)})
	if err != nil {
		return ""
	}
	cfg.Repo, cfg.Repos = repoDir, nil
	repoPaths, err := cfg.RepoPaths()
	if err != nil || len(repoPaths) == 0 {
		return ""
	}
	return eng.OutputDir(repoPaths[0])
}

// configForRepo prefers a config inside the repository, matching how `enola check`
// resolves a directory argument, so the hook and the CLI grade under identical settings.
// Differing ignore globs between them would make the diff decline as incomparable.
func configForRepo(repoDir string) string {
	if inner := repoDir + "/mcp-arch.yaml"; fileExists(inner) {
		return inner
	}
	return "mcp-arch.yaml"
}

func readHookInput() (hookInput, error) {
	var in hookInput
	// Bounded: a hook payload is small, and an unbounded read from a pipe that never
	// closes would hang the session until the harness's timeout fires.
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err != nil {
		return in, err
	}
	return in, json.Unmarshal(raw, &in)
}
