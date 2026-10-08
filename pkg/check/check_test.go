package check

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/diff"
	"github.com/enola-labs/enola/internal/facts"
)

func insight(source, title string, confidence float64) facts.Insight {
	return facts.Insight{Source: source, Title: title, Confidence: confidence}
}

// legacyDefault is the policy the gate once applied when the caller named none. It is
// spelled out here rather than assumed, because the empty Policy no longer means it —
// an unstated policy now enforces nothing (see DefaultFailExplainers). These tests are
// about how a NAMED explainer grades, so they name one; TestEvaluate_NoPolicyFailsNothing
// covers the unstated case.
func legacyDefault() Policy { return Policy{FailExplainers: []string{"cycles", "constraints"}} }

// TestEvaluate_ConfidenceAndExplainerBands is the policy table. The two cases that
// matter most are the ones that would break a naive "fail on confidence 1.0" gate:
// god-class clamps a statistical outlier to 1.0, and layers emits an informational
// "Architecture pattern" finding whose confidence is a match ratio that can reach 1.0.
// Neither is a structural defect, and neither may fail a build.
func TestEvaluate_ConfidenceAndExplainerBands(t *testing.T) {
	cases := []struct {
		name     string
		finding  facts.Insight
		wantFail bool
	}{
		{"cycle at 1.0 fails", insight("cycles", "Cyclic dependency detected (2 modules)", 1.0), true},
		{"coupled cluster at 0.4 does not fail", insight("cycles", "Highly coupled module cluster (9 modules)", 0.4), false},
		{"constraint violation at 1.0 fails", insight("constraints", "Constraint domain-stays-pure violated: a -> b via depends_on", 1.0), true},
		{"dead constraint selector at 0.4 does not fail", insight("constraints", "Constraint component ghost matches nothing", 0.4), false},
		{"god-class clamped to 1.0 does not fail", insight("god-class", "God class (400 dependents)", 1.0), false},
		{"architecture pattern at 1.0 does not fail", insight("layers", "Architecture pattern: hexagonal", 1.0), false},
		{"layer violation at 0.8 does not fail", insight("layers", "Layer violation: domain -> adapter", 0.8), false},
		{"hotspot at 0.7 does not fail", insight("hotspots", "Call-graph hotspot", 0.7), false},
		{"unused route at 0.6 does not fail", insight("unused-routes", "Unused routes", 0.6), false},
		{"unset source does not fail", insight("", "Something", 1.0), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := Evaluate(&diff.SnapshotDiff{
				Comparability: diff.Comparability{Comparable: true},
				FindingsNew:   []facts.Insight{tc.finding},
			}, legacyDefault())

			if tc.wantFail {
				if v.Status != StatusRegression {
					t.Errorf("status = %q, want %q", v.Status, StatusRegression)
				}
				if v.ExitCode() != 1 {
					t.Errorf("exit = %d, want 1", v.ExitCode())
				}
				if len(v.Failures) != 1 {
					t.Errorf("failures = %d, want 1", len(v.Failures))
				}
				return
			}
			if v.Status != StatusClean {
				t.Errorf("status = %q, want %q (a non-structural finding must not break a build)", v.Status, StatusClean)
			}
			if v.ExitCode() != 0 {
				t.Errorf("exit = %d, want 0", v.ExitCode())
			}
			// It must still be REPORTED — a clean exit may not be a silent one.
			if len(v.Advisories) != 1 {
				t.Errorf("advisories = %d, want 1: a finding below the policy must still be reported", len(v.Advisories))
			}
		})
	}
}

// TestEvaluate_StaleBaselineWarnsAndStillGrades pins the behaviour the whole gate
// hinges on. internal/diff treats staleness as deliberately-not-a-refusal; reading
// Comparability.Comparable (which staleness clears) would have turned every baseline
// older than three days into a hard refusal.
func TestEvaluate_StaleBaselineWarnsAndStillGrades(t *testing.T) {
	d := &diff.SnapshotDiff{}
	d.AddWarningKind(diff.WarnStaleBaseline, "baseline is 9 days older than the current snapshot — …")

	v := Evaluate(d, legacyDefault())

	if v.Status != StatusClean {
		t.Errorf("status = %q, want %q: a stale baseline must not block grading", v.Status, StatusClean)
	}
	if v.ExitCode() != 0 {
		t.Errorf("exit = %d, want 0", v.ExitCode())
	}
	if len(v.BlockingKinds) != 0 {
		t.Errorf("stale baseline was classified as blocking: %v", v.BlockingKinds)
	}
	if len(v.AdvisoryKinds) != 1 || v.AdvisoryKinds[0] != diff.WarnStaleBaseline {
		t.Errorf("advisory kinds = %v, want [%s]", v.AdvisoryKinds, diff.WarnStaleBaseline)
	}
	// The reason and its meaning must both reach the reader.
	out := v.Render()
	if !strings.Contains(out, "9 days older") {
		t.Errorf("render dropped the staleness reason:\n%s", out)
	}
	if !strings.Contains(out, "repository itself changed") {
		t.Errorf("render dropped what staleness MEANS for the delta:\n%s", out)
	}

	// And it must still grade: the same delta with a real cycle fails despite being stale.
	d.FindingsNew = []facts.Insight{insight("cycles", "Cyclic dependency detected (2 modules)", 1.0)}
	if got := Evaluate(d, legacyDefault()); got.Status != StatusRegression {
		t.Errorf("status = %q, want %q: staleness must not suppress a real regression", got.Status, StatusRegression)
	}
}

// TestEvaluate_BlockingDeclinesRatherThanFails guards the distinction the exit codes
// exist for: an untrustworthy comparison must never be reported as a bad change.
func TestEvaluate_BlockingDeclinesRatherThanFails(t *testing.T) {
	for _, kind := range []diff.WarningKind{
		diff.WarnDifferentRepo,
		// Same repository, incompatible fact labels — the gate must decline rather than
		// grade a delta in which nothing can match.
		diff.WarnRepoLabel,
		diff.WarnVersionMismatch,
		diff.WarnExtractorSet,
		diff.WarnIgnoreGlobs,
		diff.WarnUnclassified,
		// A union that lost a member: the diff layer declares the delta invalid, and
		// the gate graded it PASS because the kind sat in neither map.
		diff.WarnUnionMembership,
	} {
		t.Run(string(kind), func(t *testing.T) {
			d := &diff.SnapshotDiff{
				// A real regression is present: the point is that it does NOT decide the status.
				FindingsNew: []facts.Insight{insight("cycles", "Cyclic dependency detected", 1.0)},
			}
			d.AddWarningKind(kind, "something is not comparable")

			v := Evaluate(d, legacyDefault())
			if v.Status != StatusIncomparable {
				t.Errorf("status = %q, want %q", v.Status, StatusIncomparable)
			}
			if v.ExitCode() != 3 {
				t.Errorf("exit = %d, want 3 (must be distinguishable from 1 = regression)", v.ExitCode())
			}
			out := v.Render()
			if !strings.Contains(out, "NOT a statement about your change") {
				t.Errorf("a declined verdict must say it is not a judgement of the change:\n%s", out)
			}
			if strings.Contains(out, "(fail)") {
				t.Errorf("a declined verdict must not label findings as failing:\n%s", out)
			}
		})
	}
}

// TestEvaluate_AddWarningFailsClosed — a caveat contributed by a caller that this
// package cannot categorize must block, not be silently graded past.
func TestEvaluate_AddWarningFailsClosed(t *testing.T) {
	d := &diff.SnapshotDiff{}
	d.AddWarning("the working tree changed since the current snapshot was taken")

	v := Evaluate(d, legacyDefault())
	if v.Status != StatusIncomparable {
		t.Errorf("status = %q, want %q: an unclassified caveat must fail closed", v.Status, StatusIncomparable)
	}
}

// TestEvaluate_InvertedPairIsUsageError — the current snapshot predating the baseline
// has a concrete remedy, so it is exit 2 (fix your inputs), not exit 3.
func TestEvaluate_InvertedPairIsUsageError(t *testing.T) {
	d := &diff.SnapshotDiff{}
	d.AddWarningKind(diff.WarnInvertedPair, "the baseline is newer than the snapshot it is being compared against")

	v := Evaluate(d, legacyDefault())
	if v.Status != StatusUsageError {
		t.Errorf("status = %q, want %q", v.Status, StatusUsageError)
	}
	if v.ExitCode() != 2 {
		t.Errorf("exit = %d, want 2", v.ExitCode())
	}
}

// TestEvaluate_BlockingOutranksInvertedPair fixes the precedence, so a caller with
// both problems is not told to "re-run generate_snapshot" when the real issue is that
// they pointed at a different repository.
func TestEvaluate_BlockingOutranksInvertedPair(t *testing.T) {
	d := &diff.SnapshotDiff{}
	d.AddWarningKind(diff.WarnInvertedPair, "inverted")
	d.AddWarningKind(diff.WarnDifferentRepo, "different repo")

	if got := Evaluate(d, legacyDefault()).Status; got != StatusIncomparable {
		t.Errorf("status = %q, want %q", got, StatusIncomparable)
	}
}

// TestEvaluate_WarnOnly reports the regression but exits clean — and says so, rather
// than claiming there was none.
func TestEvaluate_WarnOnly(t *testing.T) {
	d := &diff.SnapshotDiff{
		Comparability: diff.Comparability{Comparable: true},
		FindingsNew:   []facts.Insight{insight("cycles", "Cyclic dependency detected", 1.0)},
	}
	v := Evaluate(d, Policy{FailExplainers: []string{"cycles"}, WarnOnly: true})

	if v.Status != StatusClean || v.ExitCode() != 0 {
		t.Errorf("status = %q exit = %d, want %q / 0", v.Status, v.ExitCode(), StatusClean)
	}
	if len(v.Failures) != 1 {
		t.Fatalf("failures = %d, want 1: --warn-only suppresses the exit code, not the finding", len(v.Failures))
	}
	out := v.Render()
	if !strings.Contains(out, "warn-only") {
		t.Errorf("render must attribute the clean exit to --warn-only:\n%s", out)
	}
	if strings.Contains(out, "no structural regression") {
		t.Errorf("render claimed there was no regression when one was reported:\n%s", out)
	}
}

// TestEvaluate_WarnOnlyDoesNotSuppressBlocking — --warn-only is a statement about how
// to treat findings, not permission to grade an untrustworthy comparison.
func TestEvaluate_WarnOnlyDoesNotSuppressBlocking(t *testing.T) {
	d := &diff.SnapshotDiff{}
	d.AddWarningKind(diff.WarnDifferentRepo, "different repo")

	if got := Evaluate(d, Policy{WarnOnly: true}).Status; got != StatusIncomparable {
		t.Errorf("status = %q, want %q", got, StatusIncomparable)
	}
}

// TestEvaluate_CustomPolicy — --fail-on/--min-confidence actually change the gate.
func TestEvaluate_CustomPolicy(t *testing.T) {
	d := &diff.SnapshotDiff{
		Comparability: diff.Comparability{Comparable: true},
		FindingsNew:   []facts.Insight{insight("layers", "Layer violation: domain -> adapter", 0.8)},
	}
	if got := Evaluate(d, legacyDefault()).Status; got != StatusClean {
		t.Fatalf("default policy status = %q, want %q", got, StatusClean)
	}
	strict := Policy{FailExplainers: []string{"layers"}, MinConfidence: 0.75}
	if got := Evaluate(d, strict).Status; got != StatusRegression {
		t.Errorf("strict policy status = %q, want %q", got, StatusRegression)
	}
}

// TestVerdict_JSONReportsEffectivePolicy — a CI consumer must see the policy that was
// ENFORCED, not the zero values the caller happened to leave unset.
func TestVerdict_JSONReportsEffectivePolicy(t *testing.T) {
	v := Evaluate(&diff.SnapshotDiff{Comparability: diff.Comparability{Comparable: true}}, legacyDefault())
	raw, err := v.JSON()
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Status string `json:"status"`
		Policy struct {
			FailExplainers []string `json:"fail_explainers"`
			MinConfidence  float64  `json:"min_confidence"`
		} `json:"policy"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status != string(StatusClean) {
		t.Errorf("status = %q, want %q", got.Status, StatusClean)
	}
	if len(got.Policy.FailExplainers) != 2 || got.Policy.FailExplainers[0] != "cycles" || got.Policy.FailExplainers[1] != "constraints" {
		t.Errorf("fail_explainers = %v, want [cycles constraints]", got.Policy.FailExplainers)
	}
	if got.Policy.MinConfidence != DefaultMinConfidence {
		t.Errorf("min_confidence = %v, want %v", got.Policy.MinConfidence, DefaultMinConfidence)
	}
}

// TestStatus_ExitCodesAreDistinct — the four codes are the CLI's contract with CI, so
// collapsing any two would silently change what a red build means.
func TestStatus_ExitCodesAreDistinct(t *testing.T) {
	want := map[Status]int{
		StatusClean:        0,
		StatusRegression:   1,
		StatusUsageError:   2,
		StatusIncomparable: 3,
	}
	seen := map[int]Status{}
	for status, code := range want {
		if got := status.ExitCode(); got != code {
			t.Errorf("%s.ExitCode() = %d, want %d", status, got, code)
		}
		if prev, dup := seen[code]; dup {
			t.Errorf("exit code %d used by both %s and %s", code, prev, status)
		}
		seen[code] = status
	}
	// An unknown status must not look clean.
	if got := Status("something-new").ExitCode(); got == 0 {
		t.Error("an unrecognized status must not exit 0")
	}
}

// TestEvaluate_NilDiffIsUsageError — no delta means the gate did not run.
func TestEvaluate_NilDiffIsUsageError(t *testing.T) {
	if got := Evaluate(nil, legacyDefault()); got.Status != StatusUsageError {
		t.Errorf("status = %q, want %q", got.Status, StatusUsageError)
	}
}

// TestRender_ShowsWhatChangedNotJustHowMuch — "+4/0 facts, +15/0 edges" is true and
// nearly useless: it says something moved without saying what, so the only way to find out
// was to re-run with --detail or go read the files, which is the work the gate replaces.
func TestRender_ShowsWhatChangedNotJustHowMuch(t *testing.T) {
	d := &diff.SnapshotDiff{
		Comparability: diff.Comparability{Comparable: true},
		FactsAdded: []facts.Fact{
			{Kind: facts.KindModule, Name: "pkg/check", File: "pkg/check"},
			{Kind: facts.KindSymbol, Name: "pkg/check.Evaluate", File: "pkg/check/check.go", Line: 180},
			{Kind: facts.KindDependency, Name: "pkg/check -> sort", File: "pkg/check/render.go"},
		},
		EdgesAdded: []diff.Edge{
			{Source: "cmd/enola.main", Kind: "calls", Target: "pkg/check.Evaluate"},
			{Source: "pkg/check.Evaluate", Kind: "declares", Target: "pkg/check"},
			{Source: "pkg/check -> sort", Kind: "imports", Target: "sort"},
		},
	}
	out := Evaluate(d, legacyDefault()).Render()

	// The names of what was added, not merely a count.
	for _, want := range []string{"pkg/check.Evaluate", "pkg/check/check.go:180", "cmd/enola.main"} {
		if !strings.Contains(out, want) {
			t.Errorf("render omitted %q:\n%s", want, out)
		}
	}
	// Kinds spelled correctly — appending "s" yields "dependencys".
	if !strings.Contains(out, "dependencies") || strings.Contains(out, "dependencys") {
		t.Errorf("fact kinds mis-pluralized:\n%s", out)
	}
	// The edge count is broken down, so a large number is readable rather than alarming.
	if !strings.Contains(out, "calls +1") || !strings.Contains(out, "declares +1") {
		t.Errorf("edge kinds not broken down:\n%s", out)
	}
	// A dependency fact's name embeds its target; it must not be printed twice.
	if strings.Contains(out, "pkg/check -> sort --imports--> sort") {
		t.Errorf("dependency edge rendered with a doubled target:\n%s", out)
	}
	// Meaningful relations outrank the mechanical symbol->module `declares` link.
	if i, j := strings.Index(out, "--calls-->"), strings.Index(out, "--declares-->"); i < 0 || j < 0 || i > j {
		t.Errorf("`declares` edges should sort last, being one-per-new-symbol noise:\n%s", out)
	}
}

// TestRender_AltersOnlyReportsVisibleChanges — a confidence that moved in the third
// decimal is a change the diff is right to record and this report has nothing to say
// about. Printing "confidence 0.77 -> 0.77" a dozen times teaches readers to skip the
// section that also carries the real ones.
func TestRender_AltersOnlyReportsVisibleChanges(t *testing.T) {
	same := func(c1, c2 float64) diff.InsightChange {
		return diff.InsightChange{
			Before: facts.Insight{Source: "god-class", Title: "High fan-in symbol: X (9 dependents)", Confidence: c1},
			After:  facts.Insight{Source: "god-class", Title: "High fan-in symbol: X (9 dependents)", Confidence: c2},
		}
	}
	d := &diff.SnapshotDiff{
		Comparability: diff.Comparability{Comparable: true},
		FindingsChanged: []diff.InsightChange{
			same(0.7712, 0.7734), // invisible at 2dp
			same(0.7701, 0.7749), // invisible at 2dp
			{
				Before: facts.Insight{Source: "complexity-outliers", Title: "High cyclomatic complexity: main (35)", Confidence: 0.7},
				After:  facts.Insight{Source: "complexity-outliers", Title: "High cyclomatic complexity: main (36)", Confidence: 0.7},
			},
		},
	}
	out := Evaluate(d, legacyDefault()).Render()

	if strings.Contains(out, "0.77 -> 0.77") {
		t.Errorf("rendered a confidence change invisible at the printed precision:\n%s", out)
	}
	if !strings.Contains(out, "(36)") || !strings.Contains(out, "was:") {
		t.Errorf("the finding that visibly moved was not shown:\n%s", out)
	}
	if !strings.Contains(out, "2 findings changed only in supporting evidence") {
		t.Errorf("evidence-only changes must be rolled up, not dropped:\n%s", out)
	}
}

// TestRender_IsDeterministic — the tool sells reproducibility, and map iteration order
// would quietly undermine it in the one artifact a human actually reads.
func TestRender_IsDeterministic(t *testing.T) {
	d := &diff.SnapshotDiff{
		Comparability: diff.Comparability{Comparable: true},
		FactsAdded: []facts.Fact{
			{Kind: facts.KindSymbol, Name: "a.B", File: "a.go", Line: 1},
			{Kind: facts.KindModule, Name: "a", File: "a"},
			{Kind: facts.KindRoute, Name: "GET /x", File: "r.go", Line: 2},
			{Kind: facts.KindDependency, Name: "a -> b", File: "a.go"},
		},
		EdgesAdded: []diff.Edge{
			{Source: "a.B", Kind: "declares", Target: "a"},
			{Source: "a", Kind: "imports", Target: "b"},
			{Source: "a.B", Kind: "calls", Target: "c.D"},
		},
	}
	first := Evaluate(d, legacyDefault()).Render()
	for i := 0; i < 20; i++ {
		if got := Evaluate(d, legacyDefault()).Render(); got != first {
			t.Fatalf("render is not deterministic across runs:\n--- first ---\n%s\n--- run %d ---\n%s", first, i, got)
		}
	}
}

// TestRender_DoesNotMutateTheDiff — the verdict reorders edges for display; doing that in
// place would reorder the delta that --json and --detail then emit.
func TestRender_DoesNotMutateTheDiff(t *testing.T) {
	d := &diff.SnapshotDiff{
		Comparability: diff.Comparability{Comparable: true},
		EdgesAdded: []diff.Edge{
			{Source: "z", Kind: "declares", Target: "m"},
			{Source: "a", Kind: "calls", Target: "b"},
		},
	}
	before := append([]diff.Edge(nil), d.EdgesAdded...)
	_ = Evaluate(d, legacyDefault()).Render()

	for i := range before {
		if d.EdgesAdded[i] != before[i] {
			t.Fatalf("Render reordered the underlying diff at %d: %+v != %+v", i, d.EdgesAdded[i], before[i])
		}
	}
}

// TestEvaluate_CleanIsQuiet — the no-change case must read as unambiguously clean.
func TestEvaluate_CleanIsQuiet(t *testing.T) {
	v := Evaluate(&diff.SnapshotDiff{Comparability: diff.Comparability{Comparable: true}}, legacyDefault())
	if v.Status != StatusClean {
		t.Fatalf("status = %q, want %q", v.Status, StatusClean)
	}
	if out := v.Render(); !strings.Contains(out, "PASS") {
		t.Errorf("render = %q, want a PASS headline", out)
	}
}

// TestEvaluate_NoPolicyFailsNothing is the default the gate now ships: a caller who
// named no explainer gets every finding REPORTED and an exit code of 0, including the
// ones enola proves. It replaced an implicit fail-on-cycles, which meant a Go or Rails
// repository was graded against a rule about cycles it had never asked for.
func TestEvaluate_NoPolicyFailsNothing(t *testing.T) {
	d := &diff.SnapshotDiff{
		Comparability: diff.Comparability{Comparable: true},
		FindingsNew: []facts.Insight{
			insight("cycles", "Cyclic dependency detected (2 modules)", 1.0),
			insight("layers", "Layer violation: storage -> api", 1.0),
		},
	}

	v := Evaluate(d, Policy{})

	if v.Status != StatusClean || v.ExitCode() != 0 {
		t.Errorf("status = %q exit = %d, want %q / 0: an unstated policy may not fail a build", v.Status, v.ExitCode(), StatusClean)
	}
	if len(v.Failures) != 0 {
		t.Errorf("failures = %d, want 0", len(v.Failures))
	}
	if len(v.Advisories) != 2 {
		t.Fatalf("advisories = %d, want 2: not failing is not the same as not reporting", len(v.Advisories))
	}
	if v.Policy.Enforcing() {
		t.Error("Enforcing() = true for a policy that names nothing")
	}

	// The exit code is the same as a genuinely clean run, so the TEXT has to carry the
	// difference — otherwise "PASS" reads as "enola looked and found nothing".
	out := v.Render()
	for _, want := range []string{"2 new findings reported", "no policy set", "--fail-on"} {
		if !strings.Contains(out, want) {
			t.Errorf("render must say what it did NOT do; missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "outside []") {
		t.Errorf("render printed an empty policy as a bracket pair:\n%s", out)
	}
	if strings.Contains(out, "PASS — no structural regression") {
		t.Errorf("render claimed a clean structure while listing two findings:\n%s", out)
	}
}

// TestPolicy_SpilloverAloneIsEnforcing — --max-spillover with no --fail-on is a real
// gate. It fails on scope and on no findings at all, so it must not be lumped in with
// "no policy set" and it must still be able to reach exit 1.
func TestPolicy_SpilloverAloneIsEnforcing(t *testing.T) {
	p := Policy{Thresholds: []Threshold{{Measurement: "spillover_packages", FailAt: 1}}}
	if !p.Enforcing() {
		t.Fatal("Enforcing() = false for a policy that gates spillover")
	}

	v := Evaluate(&diff.SnapshotDiff{Comparability: diff.Comparability{Comparable: true}}, p,
		Measurement{Name: "spillover_packages", Label: "package(s) reached outside the declared scope", Count: 1})

	if v.Status != StatusRegression || v.ExitCode() != 1 {
		t.Errorf("status = %q exit = %d, want %q / 1", v.Status, v.ExitCode(), StatusRegression)
	}
}

// TestUnenforcedAtFloor_KeepsProvenFindingsOnly is what the Stop hook speaks on. With
// no policy every finding lands in Advisories, and reporting all of them would hand an
// agent a re-ranked hotspot list every session; the floor is what keeps it to the ones
// enola measures exactly.
func TestUnenforcedAtFloor_KeepsProvenFindingsOnly(t *testing.T) {
	v := Evaluate(&diff.SnapshotDiff{
		Comparability: diff.Comparability{Comparable: true},
		FindingsNew: []facts.Insight{
			insight("layers", "Layer violation: storage -> api", 1.0),
			insight("hotspots", "Call-graph hotspot", 0.7),
		},
	}, Policy{})

	got := v.UnenforcedAtFloor()
	if len(got) != 1 || got[0].Source != "layers" {
		t.Fatalf("UnenforcedAtFloor() = %+v, want only the 1.00 layers finding", got)
	}

	// Once the explainer IS enforced the finding is a failure, not an unenforced one —
	// otherwise the hook would repeat what the gate already failed on.
	enforced := Evaluate(&diff.SnapshotDiff{
		Comparability: diff.Comparability{Comparable: true},
		FindingsNew:   []facts.Insight{insight("layers", "Layer violation: storage -> api", 1.0)},
	}, Policy{FailExplainers: []string{"layers"}})
	if len(enforced.UnenforcedAtFloor()) != 0 {
		t.Errorf("UnenforcedAtFloor() = %+v, want empty when the policy names the explainer", enforced.UnenforcedAtFloor())
	}
}

// TestEvaluate_DescriptiveFindingsAreNeverGraded is the trap this repository walked
// into by adopting its own advice: declare a layer order, and the `layers` explainer
// emits an EXACT finding announcing the declaration. Gating on `layers` would then fail
// the very pull request that added enola-intent.yaml — a build broken by a description
// of itself.
func TestEvaluate_DescriptiveFindingsAreNeverGraded(t *testing.T) {
	pattern := insight("layers", "Architecture pattern: declared (enola)", 1.0)
	pattern.Informational = true

	v := Evaluate(&diff.SnapshotDiff{
		Comparability: diff.Comparability{Comparable: true},
		FindingsNew:   []facts.Insight{pattern, insight("layers", "Layer violation: core -> surface", 1.0)},
	}, Policy{FailExplainers: []string{"layers"}})

	if v.Status != StatusRegression {
		t.Fatalf("status = %q, want %q: the real violation must still fail", v.Status, StatusRegression)
	}
	if len(v.Failures) != 1 || v.Failures[0].Title != "Layer violation: core -> surface" {
		t.Errorf("failures = %+v, want only the violation", v.Failures)
	}
	if len(v.Descriptive) != 1 || !v.Descriptive[0].Informational {
		t.Errorf("descriptive = %+v, want the pattern finding", v.Descriptive)
	}
	// Not silently dropped either: it is reported under its own heading, and the reason
	// it did not fail is that it describes rather than complains — not the floor, and
	// not the explainer list, which is what the advisory note would have claimed.
	if len(v.Advisories) != 0 {
		t.Errorf("advisories = %+v, want none: a description is not an advisory", v.Advisories)
	}
	out := v.Render()
	if !strings.Contains(out, "Descriptive (never graded)") || !strings.Contains(out, "declared (enola)") {
		t.Errorf("render dropped the descriptive finding:\n%s", out)
	}
}

// The same finding must not reach the Stop hook either — an agent asked to hand the
// user a decision about "Architecture pattern: declared" would be asking about nothing.
func TestUnenforcedAtFloor_SkipsDescriptiveFindings(t *testing.T) {
	pattern := insight("layers", "Architecture pattern: declared (enola)", 1.0)
	pattern.Informational = true

	v := Evaluate(&diff.SnapshotDiff{
		Comparability: diff.Comparability{Comparable: true},
		FindingsNew:   []facts.Insight{pattern},
	}, Policy{})

	if got := v.UnenforcedAtFloor(); len(got) != 0 {
		t.Errorf("UnenforcedAtFloor() = %+v, want empty", got)
	}
}

// TestEveryWarningKindIsClassified guards the two copies of "which warnings are
// fatal". A kind declared in internal/diff and registered in neither map here is
// printed and then graded as if it had not been raised: union_membership reached
// production that way, as PASS over a baseline that had lost a whole repository.
func TestEveryWarningKindIsClassified(t *testing.T) {
	for _, kind := range diff.WarningKinds() {
		// The gate reads an inverted pair as a usage error, not as either bucket.
		usage := kind == diff.WarnInvertedPair
		var homes int
		for _, in := range []bool{blockingKinds[kind], advisoryKinds[kind], usage} {
			if in {
				homes++
			}
		}
		if homes != 1 {
			t.Errorf("%s is classified %d times, want exactly once (blocking, advisory, or usage error)", kind, homes)
		}
		if _, ok := kindMeaning[kind]; !ok {
			t.Errorf("%s has no kindMeaning: the gate would name it without saying what it does to the delta", kind)
		}
		invalidates := diff.Comparability{Kinds: []diff.WarningKind{kind}}.InvalidatesDelta()
		if want := blockingKinds[kind] || usage; invalidates != want {
			t.Errorf("%s: InvalidatesDelta() = %v but the gate treats it as blocking-or-usage = %v", kind, invalidates, want)
		}
	}
}
