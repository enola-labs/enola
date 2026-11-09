package coverage

import (
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// svc builds a service node with one http_client edge-coverage entry.
func svc(name string, dependsOn []string, detected, resolved, unresolved int) facts.Fact {
	f := facts.Fact{Kind: facts.KindService, Name: name, Repo: name,
		Props: map[string]any{"edge_coverage": []map[string]any{
			{"edge_type": "http_client", "detected": detected, "resolved": resolved, "unresolved": unresolved},
		}}}
	for _, t := range dependsOn {
		f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelDependsOn, Target: t})
	}
	return f
}

func build(t *testing.T, ff ...facts.Fact) Report {
	t.Helper()
	s := facts.NewStore()
	s.Add(ff...)
	return Build(s, "")
}

// TestRenderText_AlwaysShowsWhatDidNotResolve is the reason this report is worth
// publishing at all.
//
// Cross-repo resolution is the claim hardest to verify from outside, so a report
// listing only successes would be marketing, and would deserve to be distrusted. The
// misses are what make the resolved count mean something — and they are the actionable
// half, since each one is either a repository the reader forgot to load or a genuine
// blind spot in enola.
func TestRenderText_AlwaysShowsWhatDidNotResolve(t *testing.T) {
	out := build(t, svc("web", []string{"api"}, 4, 1, 3)).RenderText()

	if !strings.Contains(out, "Unresolved outbound call sites") {
		t.Errorf("the unresolved section is missing:\n%s", out)
	}
	if !strings.Contains(out, "http_client ×3") {
		t.Errorf("the unresolved edge type and count are missing:\n%s", out)
	}
	// And it must say what an unresolved edge actually means, or the number is just a
	// number the reader cannot act on.
	if !strings.Contains(out, "have not snapshotted") {
		t.Errorf("no explanation of what an unresolved call site is:\n%s", out)
	}
}

// TestRenderText_SaysSoWhenEverythingResolved — silence would read as "the section is
// broken"; a clean result deserves to be stated.
func TestRenderText_SaysSoWhenEverythingResolved(t *testing.T) {
	out := build(t, svc("web", []string{"api"}, 3, 3, 0)).RenderText()
	if !strings.Contains(out, "Every detected outbound call site resolved") {
		t.Errorf("a fully-resolved graph should say so:\n%s", out)
	}
}

// TestRenderText_CallsOutCoverageGaps — the distinction the whole report exists for.
// A coverage gap looks exactly like an isolated service in the graph, and reading one
// as the other is how someone concludes a service has no dependencies when enola simply
// could not follow them.
func TestRenderText_CallsOutCoverageGaps(t *testing.T) {
	out := build(t, svc("orphan", nil, 5, 0, 5)).RenderText()

	if !strings.Contains(out, facts.ServiceCoverageGap) {
		t.Errorf("a coverage gap was not classified as one:\n%s", out)
	}
	if !strings.Contains(out, "Do not read these as isolated") {
		t.Errorf("the gap warning is missing — the report's whole purpose:\n%s", out)
	}
}

// TestRenderText_EmptyReportExplainsItself — running this against a single repository
// is the most likely first attempt, and a blank table there reads as a broken tool
// rather than as a missing second repository.
func TestRenderText_EmptyReportExplainsItself(t *testing.T) {
	out := Report{}.RenderText()

	for _, want := range []string{"No services", "at least two", "repos:"} {
		if !strings.Contains(out, want) {
			t.Errorf("empty report must explain itself and how to fix it; missing %q:\n%s", want, out)
		}
	}
}

// TestBuild_IsDeterministic — two runs over one graph must render identically, or the
// report cannot be diffed or trusted as evidence.
func TestBuild_IsDeterministic(t *testing.T) {
	facts := []facts.Fact{
		svc("zulu", nil, 1, 0, 1),
		svc("alpha", []string{"zulu"}, 2, 2, 0),
		svc("mike", []string{"alpha"}, 3, 1, 2),
	}
	first := build(t, facts...).RenderText()
	for i := 0; i < 10; i++ {
		if got := build(t, facts...).RenderText(); got != first {
			t.Fatalf("render differs between runs:\n--- first ---\n%s\n--- run %d ---\n%s", first, i, got)
		}
	}
	// Sorted by service name, so the order does not depend on store iteration.
	r := build(t, facts...)
	if r[0].Service != "alpha" || r[2].Service != "zulu" {
		t.Errorf("services not sorted by name: %v", []string{r[0].Service, r[1].Service, r[2].Service})
	}
}

// TestRenderMarkdownAndText_AgreeOnTheNumbers — the two surfaces exist so an agent and
// a person get the SAME answer. Different totals would make the CLI useless as evidence
// for what the agent was told.
func TestRenderMarkdownAndText_AgreeOnTheNumbers(t *testing.T) {
	r := build(t, svc("web", []string{"api"}, 7, 4, 3), svc("api", nil, 0, 0, 0))

	md, txt := r.RenderMarkdown(), r.RenderText()
	for _, want := range []string{"web", "api"} {
		if !strings.Contains(md, want) || !strings.Contains(txt, want) {
			t.Errorf("%q missing from one of the two renderings", want)
		}
	}
	if r.Gaps() != 0 {
		t.Errorf("Gaps() = %d, want 0", r.Gaps())
	}
	if got := r[1].Detected(); got != 7 { // sorted: api, web
		t.Errorf("web detected = %d, want 7", got)
	}
	if got := r[1].Resolved(); got != 4 {
		t.Errorf("web resolved = %d, want 4", got)
	}
}

// allOutside is a client whose three calls all name a service the snapshot does not
// hold: unresolved 0, outside 3.
func allOutside() facts.Fact {
	return facts.Fact{Kind: facts.KindService, Name: "widget-client", Repo: "widget-client",
		Props: map[string]any{"edge_coverage": []map[string]any{
			{"edge_type": "http_client", "detected": 3, "resolved": 0, "unresolved": 0, "outside": 3},
		}}}
}

// TestRenderMarkdown_GapWarningNamesOutsideCalls: the warning above the table must be
// true of the row it counts. An all-outside row reads Unresolved 0, so the warning
// may not say it has unresolved call sites.
func TestRenderMarkdown_GapWarningNamesOutsideCalls(t *testing.T) {
	r := build(t, allOutside())
	if r[0].Classification != facts.ServiceCoverageGap {
		t.Fatalf("classification = %q, want %q", r[0].Classification, facts.ServiceCoverageGap)
	}
	out := r.RenderMarkdown()
	warn := ""
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "⚠️") {
			warn = line
		}
	}
	if warn == "" {
		t.Fatalf("no gap warning:\n%s", out)
	}
	if strings.Contains(warn, "unresolved") {
		t.Errorf("warning calls outside-the-snapshot calls unresolved: %q", warn)
	}
	if !strings.Contains(warn, "not loaded") {
		t.Errorf("warning does not say the target is not loaded: %q", warn)
	}
}

// TestRenderText_AllOutsideIsNotEverythingResolved: a client whose calls all name
// an unloaded service resolved nothing, and the text view may not say otherwise.
func TestRenderText_AllOutsideIsNotEverythingResolved(t *testing.T) {
	out := build(t, allOutside()).RenderText()
	if strings.Contains(out, "Every detected outbound call site resolved") {
		t.Errorf("text view claims everything resolved for an all-outside client:\n%s", out)
	}
	if !strings.Contains(out, "not loaded") {
		t.Errorf("text view does not name the calls to an unloaded service:\n%s", out)
	}
}

// TestRenderMarkdown_IsolatedFooterHoldsForExternalOnlyRow: the footer defines
// `isolated` directly under the rows it classifies, so an isolated row whose calls
// are all third-party must satisfy the printed definition.
func TestRenderMarkdown_IsolatedFooterHoldsForExternalOnlyRow(t *testing.T) {
	api := facts.Fact{Kind: facts.KindService, Name: "api", Repo: "api",
		Props: map[string]any{"edge_coverage": []map[string]any{
			{"edge_type": "http_client", "detected": 3, "resolved": 0, "unresolved": 0, "external": 3},
		}}}
	r := build(t, api)
	if r[0].Classification != facts.ServiceIsolated {
		t.Fatalf("classification = %q, want %q (external-only is expected, not a gap)", r[0].Classification, facts.ServiceIsolated)
	}
	out := r.RenderMarkdown()
	footer := ""
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "`isolated` means") {
			footer = line
		}
	}
	if footer == "" {
		t.Fatalf("no isolated footer:\n%s", out)
	}
	if strings.Contains(footer, "no detected call sites") {
		t.Errorf("footer excludes detected call sites, which the row above it has: %q", footer)
	}
	if !strings.Contains(footer, "External") {
		t.Errorf("footer does not name the expected bucket the row's calls are in: %q", footer)
	}
}
