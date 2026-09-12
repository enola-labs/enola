package command

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/diff"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/hookstate"
	"github.com/enola-labs/enola/pkg/check"
)

// declined builds a verdict in the state the gate reaches when it refuses to grade.
func declined(kinds ...diff.WarningKind) check.Verdict {
	d := &diff.SnapshotDiff{}
	for _, k := range kinds {
		d.AddWarningKind(k, "warning text for "+string(k))
	}
	return check.Evaluate(d, check.Policy{})
}

// writeBaselineMeta writes a baseline directory carrying an arbitrary meta, which the
// shared writeBaseline helper does not allow — the comparability rules being exercised
// here are all about fields it hard-codes.
func writeBaselineMeta(t *testing.T, meta facts.SnapshotMeta, auto bool) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "baseline")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "facts.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "snapshot.meta.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if auto {
		if err := os.WriteFile(filepath.Join(dir, autoPinMarker), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestStopOutcome_ClassifiesEveryPath(t *testing.T) {
	for _, tt := range []struct {
		name     string
		verdict  check.Verdict
		ok       bool
		reported bool
		want     hookstate.Outcome
	}{
		{"graded, nothing wrong", check.Evaluate(&diff.SnapshotDiff{}, check.Policy{}), true, false, hookstate.OutcomeClean},
		{"could not grade", declined(diff.WarnVersionMismatch), true, false, hookstate.OutcomeDeclined},
		{"nothing to grade against", check.Verdict{}, false, false, hookstate.OutcomeUnavailable},
		// Clean exit, and the hook still spoke: findings no policy enforces. Filing this
		// as OutcomeClean would tell `doctor` the hook has been silent all week.
		{"clean but reported", check.Evaluate(&diff.SnapshotDiff{}, check.Policy{}), true, true, hookstate.OutcomeReported},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := stopOutcome(tt.verdict, tt.ok, tt.reported); got != tt.want {
				t.Errorf("stopOutcome = %q, want %q", got, tt.want)
			}
		})
	}
}

// The reason an agent is shown must name the cause and the remedy. Anything vaguer
// leaves the reader unable to act, which is barely better than the silence this
// replaced.
func TestDeclineReason_NamesTheCauseAndIsEmptyOtherwise(t *testing.T) {
	v := declined(diff.WarnVersionMismatch)
	if v.Status != check.StatusIncomparable {
		t.Fatalf("expected an incomparable verdict, got %q", v.Status)
	}
	reason := v.DeclineReason()
	if reason == "" {
		t.Fatal("a declined verdict must produce a reason")
	}
	if !contains(reason, "version") {
		t.Errorf("reason should name the cause, got %q", reason)
	}

	clean := check.Evaluate(&diff.SnapshotDiff{}, check.Policy{})
	if clean.DeclineReason() != "" {
		t.Errorf("a graded verdict must have no decline reason, got %q", clean.DeclineReason())
	}
}

// DeclineKey drives the once-per-reason rule, so it has to distinguish DIFFERENT
// problems while treating the same problem as the same problem.
func TestDeclineKey_IdentifiesTheProblemNotTheProse(t *testing.T) {
	a := declined(diff.WarnVersionMismatch).DeclineKey()
	b := declined(diff.WarnVersionMismatch).DeclineKey()
	c := declined(diff.WarnDifferentRepo).DeclineKey()

	if a == "" {
		t.Fatal("a declined verdict needs a key")
	}
	if a != b {
		t.Errorf("the same decline must produce the same key: %q vs %q", a, b)
	}
	if a == c {
		t.Errorf("different declines must produce different keys, both %q", a)
	}
	// Order of the underlying set must not change the key.
	if declined(diff.WarnVersionMismatch, diff.WarnDifferentRepo).DeclineKey() !=
		declined(diff.WarnDifferentRepo, diff.WarnVersionMismatch).DeclineKey() {
		t.Error("the key must be order-independent")
	}
	if check.Evaluate(&diff.SnapshotDiff{}, check.Policy{}).DeclineKey() != "" {
		t.Error("a graded verdict has no key")
	}
}

// The whole point of the once-per-report rule: say it once, say it again when it is a
// NEW problem, say it again when a fixed problem comes back, and say it again in a new
// session.
func TestShouldReport_OncePerReportPerSession(t *testing.T) {
	dir := t.TempDir()
	rep := hookstate.Report{Key: declined(diff.WarnVersionMismatch).DeclineKey(), Session: "s1"}

	if !hookstate.ShouldReport(dir, hookstate.EventStop, rep) {
		t.Fatal("a first report must be made")
	}
	hookstate.RecordFiredWithReport(dir, hookstate.EventStop, hookstate.OutcomeDeclined, rep)

	if hookstate.ShouldReport(dir, hookstate.EventStop, rep) {
		t.Error("the same report repeating in one session must stay quiet: on Stop it prevents the turn from ending")
	}

	other := hookstate.Report{Key: declined(diff.WarnDifferentRepo).DeclineKey(), Session: "s1"}
	if !hookstate.ShouldReport(dir, hookstate.EventStop, other) {
		t.Error("a different problem is a new report and must be made")
	}

	// A new session hears it again. Without this, a deliberately pinned baseline (which
	// the session-start hook never replaces) would be reported once and then never, and
	// a regression going permanently quiet is worse than one repeated across sessions.
	if !hookstate.ShouldReport(dir, hookstate.EventStop, hookstate.Report{Key: rep.Key, Session: "s2"}) {
		t.Error("a standing problem must be reported again in a new session")
	}

	// A successful grade clears the key, so a recurrence is heard again even inside the
	// same session: without this, a problem fixed and then reintroduced would be
	// suppressed forever.
	hookstate.RecordFiredWithReport(dir, hookstate.EventStop, hookstate.OutcomeClean, hookstate.Report{Session: "s1"})
	if !hookstate.ShouldReport(dir, hookstate.EventStop, rep) {
		t.Error("a problem recurring after a clean grade must be reported again")
	}

	if hookstate.ShouldReport(dir, hookstate.EventStop, hookstate.Report{Session: "s1"}) {
		t.Error("an empty key is nothing to say and must never be reported")
	}
}

// ReportKey drives that rule on all three paths the hook speaks on, not just the decline
// it started on, so it has to distinguish DIFFERENT reports while treating the same
// report as the same report.
func TestReportKey_IdentifiesTheReportNotTheProse(t *testing.T) {
	layer := facts.Insight{Source: "layers", Title: "Layer violation: storage -> delivery", Confidence: 1.0}
	cycle := facts.Insight{Source: "cycles", Title: "Cyclic dependency detected: a -> b -> a", Confidence: 1.0}
	advisory := func(ins ...facts.Insight) check.Verdict {
		return check.Evaluate(&diff.SnapshotDiff{
			Comparability: diff.Comparability{Comparable: true},
			FindingsNew:   ins,
		}, check.Policy{})
	}

	one, again := advisory(layer), advisory(layer)
	if one.ReportKey() == "" {
		t.Fatal("a verdict carrying an unenforced finding needs a key, or its report repeats at every Stop")
	}
	if one.ReportKey() != again.ReportKey() {
		t.Errorf("the same finding must produce the same key: %q vs %q", one.ReportKey(), again.ReportKey())
	}
	if one.ReportKey() == advisory(cycle).ReportKey() {
		t.Errorf("different findings must produce different keys, both %q", one.ReportKey())
	}
	// A re-ordered findings list is the same report; treating it as new would reinstate
	// the repeat this key exists to stop.
	if advisory(layer, cycle).ReportKey() != advisory(cycle, layer).ReportKey() {
		t.Error("the key must be order-independent")
	}
	// Nothing to say means no key, which is what clears a previously reported one.
	if advisory().ReportKey() != "" {
		t.Error("a verdict with nothing to report must have no key")
	}
	// The decline path keeps exactly the identity it already had.
	d := declined(diff.WarnVersionMismatch)
	if d.ReportKey() != d.DeclineKey() {
		t.Errorf("a declined verdict's report key must be its decline key: %q vs %q", d.ReportKey(), d.DeclineKey())
	}
}

// The payload fields the hook depends on, read through the real stdin path.
//
// stop_hook_active was never parsed at all, and that is what looped a turn until the
// harness overrode the hook at its consecutive-block cap. A struct-tag typo would
// reintroduce it in the quietest possible way: the field would simply always be false.
func TestReadHookInput_ReadsTheLoopBreakerAndTheSession(t *testing.T) {
	in := readHookInputFrom(t, `{"session_id":"abc123","transcript_path":"/tmp/t.jsonl",`+
		`"cwd":"/repo","hook_event_name":"Stop","stop_hook_active":true,"added_later":{"x":1}}`)

	if in.CWD != "/repo" {
		t.Errorf("cwd = %q, want /repo", in.CWD)
	}
	if !in.StopHookActive {
		t.Error("stop_hook_active must be read: ignoring it is what re-fired the identical report until the block cap")
	}
	if in.SessionID != "abc123" {
		t.Errorf("session_id = %q, want abc123", in.SessionID)
	}

	// Absent on the first Stop of a turn, and absent entirely on a harness that predates
	// the flag. Neither may read as active, or the hook would go permanently silent.
	if readHookInputFrom(t, `{"cwd":"/repo"}`).StopHookActive {
		t.Error("a payload without the flag must not read as active")
	}
}

// readHookInputFrom feeds a payload through readHookInput's own stdin rather than around
// it, so the struct tags are what is under test.
func readHookInputFrom(t *testing.T, payload string) hookInput {
	t.Helper()
	path := filepath.Join(t.TempDir(), "payload.json")
	if err := os.WriteFile(path, []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	saved := os.Stdin
	os.Stdin = f
	defer func() { os.Stdin = saved }()

	in, err := readHookInput()
	if err != nil {
		t.Fatalf("readHookInput: %v", err)
	}
	return in
}

// shouldAutoPin's new rule: refresh a baseline that can no longer be compared, but
// only one this hook created. A deliberate pin is the "before" of a refactor that may
// span days, and replacing it silently would destroy exactly what it was recording.
func TestShouldAutoPin_RefreshesUnusableAutoPinButNeverADeliberateOne(t *testing.T) {
	base := facts.SnapshotMeta{
		RepoPath: "/repo", EnolaVersion: "v1",
		Extractors: []string{"go"}, IgnoreGlobHash: "sha256:aaa",
	}
	current := base
	current.EnolaVersion = "v2" // blocking: different versions extract differently

	if !baselineIsUnusable(base, current) {
		t.Fatal("a version mismatch must count as unusable")
	}
	same := base
	if baselineIsUnusable(base, same) {
		t.Error("identical metadata must be usable")
	}

	// Auto-pinned and unusable → refreshed.
	autoDir := writeBaselineMeta(t, base, true)
	if !shouldAutoPin(autoDir, t.TempDir(), ".enola", &current) {
		t.Error("an auto-pinned baseline that can no longer be compared must be refreshed")
	}
	// Deliberate and unusable → left alone.
	deliberateDir := writeBaselineMeta(t, base, false)
	if shouldAutoPin(deliberateDir, t.TempDir(), ".enola", &current) {
		t.Error("a deliberately pinned baseline must never be replaced, even when unusable")
	}
}

func contains(hay, needle string) bool {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// The verdict an agent is handed for an UNENFORCED finding has one job beyond reporting:
// it must leave the judgement with the user. Nothing failed, so nothing has decided
// whether the change is acceptable — an agent that decides for itself either "fixes"
// architecture the user chose, or files the session as clean over a real finding.
func TestUnenforcedReport_HandsTheDecisionToTheUser(t *testing.T) {
	v := check.Evaluate(&diff.SnapshotDiff{
		Comparability: diff.Comparability{Comparable: true},
		FindingsNew: []facts.Insight{
			{Source: "layers", Title: "Layer violation: storage -> delivery", Confidence: 1.0},
		},
	}, check.Policy{})

	report := unenforcedReport("enola", v)

	for _, want := range []string{
		"NOTHING FAILED",
		"USER'S DECISION",
		"enola check --fail-on=…",
		"Layer violation: storage -> delivery",
	} {
		if !contains(report, want) {
			t.Errorf("report is missing %q:\n%s", want, report)
		}
	}
	// The two ways an agent could wrongly settle it on its own.
	for _, want := range []string{"rather than a failed build", "Do not revert or refactor on your own initiative"} {
		if !contains(report, want) {
			t.Errorf("report must explicitly rule out settling it alone; missing %q:\n%s", want, report)
		}
	}
}
