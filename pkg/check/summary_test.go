package check

import (
	"reflect"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/diff"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/pkg/plain"
)

func deadCode(name string, conf float64) facts.Insight {
	return facts.Insight{Source: "dead-code", Title: "Dead code candidate: function " + name, Confidence: conf,
		Evidence: []facts.Evidence{{File: "pkg/b/gamma.go", Line: 6}}}
}

func cycle(n string) facts.Insight {
	return facts.Insight{Source: "cycles", Title: "Cyclic dependency detected (" + n + " modules)", Confidence: 1}
}

func TestSummary_EverySectionIsFilled(t *testing.T) {
	for _, st := range []Status{StatusClean, StatusRegression, StatusIncomparable, StatusUsageError, StatusPartialClean} {
		s := Verdict{Status: st}.Summarize()
		sections := map[string][]plain.Item{
			"result": s.Result.Items, "changed": s.Changed, "worse": s.Worse, "better": s.Better,
			"neither": s.Neither, "could not see": s.CouldNotSee, "next": s.Next,
		}
		for name, items := range sections {
			if len(items) == 0 {
				t.Errorf("%s: section %q is empty", st, name)
			}
		}
		if s.Result.Outcome == "" {
			t.Errorf("%s: no outcome", st)
		}
	}
}

func TestSummary_IsDeterministic(t *testing.T) {
	v := Verdict{Status: StatusClean, Advisories: []facts.Insight{deadCode("pkg/b.A", 0.8), cycle("3")}, FactsAdded: 2}
	if a, b := v.Summarize(), v.Summarize(); !reflect.DeepEqual(a, b) {
		t.Errorf("same verdict, different summaries")
	}
}

func TestSummary_Result(t *testing.T) {
	enforcing := Policy{FailExplainers: []string{"cycles", "layers"}}
	cases := []struct {
		name    string
		v       Verdict
		outcome string
		says    string
	}{
		{"no change", Verdict{Status: StatusClean}, OutcomePasses, "did not alter the structure"},
		{"nothing enforced", Verdict{Status: StatusClean, FactsAdded: 1, Advisories: []facts.Insight{cycle("2")}}, OutcomePasses, "Nothing is enforced yet"},
		{"enforced and clean", Verdict{Status: StatusClean, Policy: enforcing, FactsAdded: 1}, OutcomePasses, "breaks what the build enforces (cycles and layers)"},
		{"warn-only", Verdict{Status: StatusClean, Policy: Policy{FailExplainers: []string{"cycles"}, WarnOnly: true}, Failures: []facts.Insight{cycle("2")}}, OutcomePasses, "only because --warn-only"},
		{"fails", Verdict{Status: StatusRegression, Policy: enforcing}, OutcomeFails, "Fails the build (cycles and layers)."},
		{"declined", Verdict{Status: StatusIncomparable, ComparabilityWarnings: []string{"extractor set differs", "config differs"}}, OutcomeNotGraded, "rather than your change"},
		{"could not run", Verdict{Status: StatusUsageError}, OutcomeCouldNotRun, "could not run"},
	}
	for _, c := range cases {
		r := c.v.Summarize().Result
		if r.Outcome != c.outcome || !strings.Contains(r.Items[0].Text, c.says) {
			t.Errorf("%s: got %q %+v, want %q and %q", c.name, r.Outcome, r.Items, c.outcome, c.says)
		}
	}

	declined := Verdict{Status: StatusIncomparable,
		BlockingKinds:         []diff.WarningKind{diff.WarnIgnoreGlobs, diff.WarnVersionMismatch},
		ComparabilityWarnings: []string{"ignore globs differ between baseline and current", "version differs"}}.Summarize().Result
	if !containsText(declined.Items, "Why: Your ignore settings changed, so a different set of files was read. 1 more reason is in the full verdict.") {
		t.Errorf("declined gives its first reason in plain words and counts the rest: %+v", declined.Items)
	}
	unknown := Verdict{Status: StatusIncomparable, BlockingKinds: []diff.WarningKind{diff.WarnUnclassified},
		ComparabilityWarnings: []string{"working tree moved since the snapshot"}}.Summarize().Result
	if !containsText(unknown.Items, "Why: Working tree moved since the snapshot.") {
		t.Errorf("an unknown kind is given in the verdict's words: %+v", unknown.Items)
	}
	breach := Verdict{Status: StatusRegression, Breaches: []Breach{
		{Measurement: Measurement{Label: "packages reached outside the declared scope", Count: 4}, Fatal: true},
		{Measurement: Measurement{Label: "warned", Count: 1}},
	}}.Summarize().Result
	if !containsText(breach.Items, "Over a limit the build enforces: 4 packages reached outside the declared scope.") || containsText(breach.Items, "warned") {
		t.Errorf("fatal breaches only: %+v", breach.Items)
	}
	partial := Verdict{Status: StatusPartialClean}.Summarize().Result
	if partial.Outcome != OutcomePasses || !containsText(partial.Items, "Only the code both snapshots could read") {
		t.Errorf("partial: %+v", partial)
	}
}

func TestSummary_WorseIsFailuresFirstThenMostCertain(t *testing.T) {
	v := Verdict{
		Status:     StatusRegression,
		Failures:   []facts.Insight{cycle("3")},
		Advisories: []facts.Insight{deadCode("pkg/b.Low", 0.5), deadCode("pkg/b.High", 0.8), deadCode("pkg/b.Mid", 0.7)},
	}
	s := v.Summarize()
	if !strings.HasPrefix(s.Worse[0].Text, "Fails the build. Creates a loop: 3 modules now depend on each other") {
		t.Errorf("first: %+v", s.Worse[0])
	}
	if s.Worse[1].Subject != "pkg/b.High" || s.Worse[2].Subject != "pkg/b.Mid" {
		t.Errorf("advisories most certain first: %+v", s.Worse)
	}
	if s.MoreWorse != 1 {
		t.Errorf("MoreWorse = %d, want 1", s.MoreWorse)
	}
	if s.Worse[1].Location != "pkg/b/gamma.go:6" || s.Worse[1].Certainty != plain.Likely {
		t.Errorf("location and certainty: %+v", s.Worse[1])
	}
}

func TestPlainFinding(t *testing.T) {
	cases := []struct {
		in      facts.Insight
		subject string
		says    string
	}{
		{facts.Insight{Source: "layers", Title: "Layer violation: domain -> adapter (declared)"}, "", `"domain" layer now depends on the "adapter" layer`},
		{facts.Insight{Source: "constraints", Title: "Constraint views-avoid-controllers violated: a -> b"}, "", `Breaks the project's rule "views-avoid-controllers".`},
		{facts.Insight{Source: "dead-code", Title: "Test-only code candidate: function pkg/a.helper"}, "pkg/a.helper", "Only tests use this function."},
		{facts.Insight{Source: "dead-code", Title: "Additional dead-code candidates: 94 more (see find_orphans)"}, "", "94 more pieces of code look unused."},
		{facts.Insight{Source: "god-class", Title: "High fan-in symbol: pkg/x.Base64 (18 dependents)"}, "pkg/x.Base64", "Now used from 18 places."},
		{facts.Insight{Source: "complexity-outliers", Title: "High cyclomatic complexity: pkg/x.ParseX509 (108)"}, "pkg/x.ParseX509", "Has 108 separate paths"},
		{facts.Insight{Source: "dependency-depth", Title: "Deep dependency chain: pkg/utf8 (depth 9)"}, "pkg/utf8", "a chain of 9 modules"},
		{facts.Insight{Source: "performance", Title: "Performance risk (call-in-loop, O(n)): cmd/enola.main"}, "cmd/enola.main", "Repeats work inside a loop."},
		{facts.Insight{Source: "hotspots", Title: "Call-graph hotspot: pkg/x.Sha256Hash (fan-in 30, fan-out 12)"}, "pkg/x.Sha256Hash", "ripples both ways"},
		{facts.Insight{Source: "exported-surface", Title: "Large public surface: pkg/oauth2 exports 40 of 44 symbols (91%)"}, "pkg/oauth2", "Exposes most of its code"},
		// An explainer the summary has no sentence for keeps its title.
		{facts.Insight{Source: "packwerk", Title: "Privacy violation: a uses b"}, "", "Privacy violation: a uses b"},
	}
	for _, c := range cases {
		it := plainFinding(c.in)
		if it.Subject != c.subject || !strings.Contains(it.Text, c.says) {
			t.Errorf("%s: got %+v, want subject %q and %q", c.in.Title, it, c.subject, c.says)
		}
	}
}

func TestSummary_BetterCountsWhatCleared(t *testing.T) {
	s := Verdict{Status: StatusClean, Resolved: []facts.Insight{cycle("2"), cycle("4"), deadCode("pkg/a.X", 0.8)}}.Summarize()
	want := []string{"Clears 2 dependency loops.", "Clears 1 piece of code that looked unused."}
	if len(s.Better) != 2 || s.Better[0].Text != want[0] || s.Better[1].Text != want[1] {
		t.Errorf("got %+v, want %v", s.Better, want)
	}
}

func TestSummary_NeitherKeepsWhatWasNotFixed(t *testing.T) {
	one := []facts.Insight{{Source: "constraints", Title: "x"}}
	v := Verdict{Status: StatusClean, Suppressed: one, Exempted: one, Silenced: one, Undeclared: one, Unattributed: one, Declared: one}
	got := v.Summarize().Neither
	for _, want := range []string{
		"excused in " + SuppressionsFileName, "exempted by the project's own rules",
		"moved out of what the rule covers. Nothing was fixed.", "the rule was changed or removed. The code is the same.",
		"could not be compared", "That is where the rule starts, not something you broke.",
	} {
		if !containsText(got, want) {
			t.Errorf("missing %q in %+v", want, got)
		}
	}
	// None of them is good news.
	if b := v.Summarize().Better; b[0].Text != "Nothing." {
		t.Errorf("hidden or excused findings must not read as better: %+v", b)
	}
}

func TestSummary_Changed(t *testing.T) {
	dep := func(name, source string) facts.Fact {
		return facts.Fact{Kind: facts.KindDependency, Name: name, Props: map[string]any{"source": source}}
	}
	v := Verdict{
		Status:      StatusClean,
		FactsAdded:  4,
		AddedByKind: map[string]int{facts.KindModule: 1, facts.KindSymbol: 2, facts.KindDependency: 1},
		Diff: &diff.SnapshotDiff{
			FactsAdded: []facts.Fact{
				{Kind: facts.KindModule, Name: "pkg/c"},
				dep("pkg/b -> example.com/app/pkg/c", "internal"),
				// TypeScript keeps an internal import's path as its target.
				dep("src/ui -> src/lib/api", "internal"),
				dep("pkg/b -> github.com/x/y", "external"),
				dep("pkg/b -> fmt", "stdlib"),
				// A dotless Go module path is classified stdlib, and still resolves.
				dep("storage -> layersgate/notify", "stdlib"),
			},
			FactsRemoved: []facts.Fact{dep("pkg/a -> example.com/app/pkg/d", "internal")},
			EdgesAdded: []diff.Edge{
				{Source: "pkg/b -> example.com/app/pkg/c", Kind: facts.RelImports, Target: "pkg/c"},
				// The same import from a second file of pkg/b is the same use.
				{Source: "pkg/b -> example.com/app/pkg/c", Kind: facts.RelImports, Target: "pkg/c"},
				{Source: "src/ui -> src/lib/api", Kind: facts.RelImports, Target: "src/lib/api"},
				{Source: "pkg/b -> github.com/x/y", Kind: facts.RelImports, Target: "github.com/x/y"},
				{Source: "pkg/b -> fmt", Kind: facts.RelImports, Target: "fmt"},
				{Source: "storage -> layersgate/notify", Kind: facts.RelImports, Target: "notify"},
				// An edge whose dependency fact is not in the diff is not guessed at.
				{Source: "pkg/e -> example.com/app/pkg/f", Kind: facts.RelImports, Target: "pkg/f"},
				{Source: "pkg/b.UsesGamma", Kind: facts.RelDeclares, Target: "pkg/b"},
			},
			EdgesRemoved: []diff.Edge{{Source: "pkg/a -> example.com/app/pkg/d", Kind: facts.RelImports, Target: "pkg/d"}},
		},
	}
	got := v.Summarize().Changed
	want := []string{
		"Added 1 folder of code and 2 functions and types (pkg/c).",
		"pkg/b now uses pkg/c.",
		"src/ui now uses src/lib/api.",
		"storage now uses notify.",
		"pkg/a no longer uses pkg/d.",
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i].Text != want[i] {
			t.Errorf("item %d: got %q, want %q", i, got[i].Text, want[i])
		}
	}
	if declined := (Verdict{Status: StatusIncomparable, FactsAdded: 9}).Summarize().Changed; !strings.HasPrefix(declined[0].Text, "Not shown") {
		t.Errorf("a declined verdict lists no changes: %+v", declined)
	}
}

func TestSummary_CouldNotSee(t *testing.T) {
	v := Verdict{Status: StatusClean, Census: &Census{Recorded: true, FilesExcludedByIgnore: 2, DeadExemptions: 1, UnusedSuppressions: 2}}
	got := v.Summarize().CouldNotSee
	for i, want := range []string{
		"2 files left out by your ignore settings, so a change there is not seen.",
		"1 exemption in your rules matches nothing, so it excuses nothing.",
		"2 excuses in " + SuppressionsFileName + " match no finding any more.",
	} {
		if got[i].Text != want {
			t.Errorf("item %d: got %q, want %q", i, got[i].Text, want)
		}
	}
	if last := got[len(got)-1]; !strings.Contains(last.Text, "does not run it or its tests") {
		t.Errorf("last: %+v", last)
	}
	stale := Verdict{Status: StatusClean, AdvisoryKinds: []diff.WarningKind{diff.WarnStaleBaseline}}.Summarize().CouldNotSee
	if !containsText(stale, "other people's work") {
		t.Errorf("a stale baseline is said: %+v", stale)
	}
	if old := (Verdict{Status: StatusClean, Census: &Census{}}).Summarize().CouldNotSee; !strings.HasPrefix(old[0].Text, "Not recorded") {
		t.Errorf("unrecorded census: %+v", old)
	}
}

func TestSummary_Next(t *testing.T) {
	unenforced := Verdict{Status: StatusClean, FactsAdded: 1, Advisories: []facts.Insight{deadCode("a", 0.8), cycle("2"), deadCode("b", 0.8)}}
	if n := unenforced.Summarize().Next; n[0].Subject != "enola check --fail-on cycles,dead-code" || n[1].Subject != "enola check" {
		t.Errorf("unenforced: %+v", n)
	}
	if n := (Verdict{Status: StatusIncomparable}).Summarize().Next; n[0].Subject != "enola baseline pin" {
		t.Errorf("declined: %+v", n)
	}
	if n := (Verdict{Status: StatusRegression}).Summarize().Next; len(n) != 1 || n[0].Subject != "enola check" {
		t.Errorf("fails: %+v", n)
	}
	enforced := Verdict{Status: StatusClean, Policy: Policy{FailExplainers: []string{"cycles"}}, Advisories: []facts.Insight{deadCode("a", 0.8)}}
	if n := enforced.Summarize().Next; len(n) != 1 {
		t.Errorf("a policy already set is not second-guessed: %+v", n)
	}
}

func TestSummary_GradesNothing(t *testing.T) {
	v := Verdict{Status: StatusRegression, Failures: []facts.Insight{cycle("2")}}
	before := v.ExitCode()
	v.Summarize()
	if v.ExitCode() != before {
		t.Error("summarizing changed the exit code")
	}
}

func containsText(items []plain.Item, s string) bool {
	for _, it := range items {
		if strings.Contains(it.Text, s) {
			return true
		}
	}
	return false
}

func TestSummaryRender_SectionsInFixedOrder(t *testing.T) {
	out := Verdict{Status: StatusClean, Diff: &diff.SnapshotDiff{CurrentRepo: "/repo/x"}}.Summarize().Render()
	last := -1
	for _, h := range []string{
		"Change summary: /repo/x", "Result: passes", "What you changed", "Got worse",
		"Got better", "Neither better nor worse", "Could not see", "Next",
	} {
		i := strings.Index(out, h)
		if i <= last {
			t.Fatalf("heading %q missing or out of order in:\n%s", h, out)
		}
		last = i
	}
}

func TestWrite_SummaryFormats(t *testing.T) {
	v := Verdict{Status: StatusClean}
	text, err := v.Write(Output{Format: FormatSummary})
	if err != nil || !strings.Contains(string(text), "Result: passes") {
		t.Errorf("summary: %v %s", err, text)
	}
	js, err := v.Write(Output{Format: FormatSummaryJSON})
	if err != nil || !strings.Contains(string(js), `"outcome": "passes"`) {
		t.Errorf("summary-json: %v %s", err, js)
	}
}

func TestSummary_ChainsAbsorbedByANewLoopAreNotBetter(t *testing.T) {
	depth := facts.Insight{Source: "dependency-depth", Title: "Deep dependency chain: pkg/a (depth 7)"}
	v := Verdict{Status: StatusRegression, Failures: []facts.Insight{cycle("12")}, Resolved: []facts.Insight{depth, depth}}
	b := v.Summarize().Better
	if !strings.Contains(b[0].Text, "the new loop absorbed them") || b[0].Certainty != plain.Likely {
		t.Errorf("got %+v", b)
	}
	if b := (Verdict{Status: StatusClean, Resolved: []facts.Insight{depth}}).Summarize().Better; b[0].Text != "Clears 1 long dependency chain." {
		t.Errorf("without a new loop it is an improvement: %+v", b)
	}
}

func TestPlainFinding_CycleNamesItsModules(t *testing.T) {
	in := cycle("12")
	in.Evidence = []facts.Evidence{{Fact: "a"}, {Fact: "b"}, {Fact: "a"}, {Fact: "c"}, {Fact: "d"}}
	if got := plainFinding(in).Text; !strings.HasSuffix(got, "They include a, b, c and 9 others.") {
		t.Errorf("got %q", got)
	}
}

func TestSummary_DeclinedGradesNothingAnywhere(t *testing.T) {
	s := Verdict{Status: StatusIncomparable,
		Failures: []facts.Insight{cycle("4")}, Advisories: []facts.Insight{deadCode("a", 0.8)},
		Resolved: []facts.Insight{cycle("2")}, Suppressed: []facts.Insight{cycle("3")}}.Summarize()
	for name, items := range map[string][]plain.Item{"worse": s.Worse, "better": s.Better, "neither": s.Neither} {
		if len(items) != 1 || items[0].Text != "Not graded." {
			t.Errorf("%s: %+v", name, items)
		}
	}
}

func TestSummary_AGrownLoopIsWorse(t *testing.T) {
	v := Verdict{Status: StatusClean, FindingsChanged: []diff.InsightChange{
		{Before: cycle("5"), After: cycle("8")},
		{Before: cycle("6"), After: cycle("4")},
		{Before: deadCode("a", 0.8), After: deadCode("a", 0.9)},
	}}
	s := v.Summarize()
	if len(s.Worse) != 1 || s.Worse[0].Text != "Makes an existing loop bigger: 8 modules now depend on each other, up from 5." {
		t.Errorf("worse: %+v", s.Worse)
	}
	if !containsText(s.Neither, "2 existing findings moved a little") {
		t.Errorf("the other altered findings are counted, not graded: %+v", s.Neither)
	}
}
