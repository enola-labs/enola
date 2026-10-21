package plan

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/explainers/constraints"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/intent"
)

const testDeclaration = `components:
  - name: models
    match: [app/models/**]
  - name: billing
    match: [app/billing/**]
rules:
  - id: models-never-bill
    forbid: models
    to: billing
    via: calls
    because: billing is reached through the ledger service, never directly
`

func declaredStore(t *testing.T, declaration string, measured ...facts.Fact) *facts.Store {
	t.Helper()
	store := facts.NewStore()
	store.Add(measured...)
	if declaration != "" {
		decl, err := intent.Parse([]byte(declaration))
		if err != nil {
			t.Fatalf("intent.Parse: %v", err)
		}
		decl.Source = intent.RepoFileName
		store.Add(intent.CompileFacts(decl)...)
	}
	return store
}

func measuredFixture() []facts.Fact {
	return []facts.Fact{
		{Kind: facts.KindSymbol, Name: "User", File: "app/models/user.rb", Relations: []facts.Relation{{Kind: facts.RelCalls, Target: "Billing"}}},
		{Kind: facts.KindSymbol, Name: "Billing", File: "app/billing/billing.rb"},
		{Kind: facts.KindSymbol, Name: "AdminController", File: "app/controllers/admin.rb", Relations: []facts.Relation{{Kind: facts.RelCalls, Target: "User"}}},
	}
}

func computeOrFail(t *testing.T, req Request, deps Deps) *Report {
	t.Helper()
	report, err := Compute(context.Background(), req, deps)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	return report
}

func TestCompute_GoverningRulesForPath(t *testing.T) {
	deps := Deps{RepoLabel: "shop", Store: declaredStore(t, testDeclaration, measuredFixture()...)}
	report := computeOrFail(t, Request{Paths: []string{"app/models/user.rb"}}, deps)

	if !report.ConstraintsDeclared {
		t.Fatal("ConstraintsDeclared = false with a declared rule set")
	}
	if len(report.Targets) != 1 {
		t.Fatalf("targets = %d, want 1", len(report.Targets))
	}
	tr := report.Targets[0]
	if tr.NoRuleGoverns {
		t.Error("NoRuleGoverns = true for a governed path")
	}
	if len(tr.Components) != 1 || tr.Components[0].Component != "models" {
		t.Fatalf("components = %+v, want the models component", tr.Components)
	}
	rules := tr.Components[0].Rules
	if len(rules) != 1 || rules[0].Rule != "models-never-bill" {
		t.Fatalf("rules = %+v, want models-never-bill", rules)
	}
	if rules[0].Because == "" || rules[0].Mode != "ratchet" {
		t.Errorf("rule binding lost because/mode: %+v", rules[0])
	}
	if !tr.Measured || tr.BlastRadius == nil {
		t.Fatalf("path with measured facts reported measured=%v blast=%v", tr.Measured, tr.BlastRadius)
	}
	if tr.BlastRadius.FanOut != 1 || tr.BlastRadius.Out[0] != "Billing" {
		t.Errorf("fan-out = %+v, want [Billing]", tr.BlastRadius.Out)
	}
	if tr.BlastRadius.FanIn != 1 || tr.BlastRadius.In[0] != "AdminController" {
		t.Errorf("fan-in = %+v, want [AdminController]", tr.BlastRadius.In)
	}
}

func TestCompute_PreEditPathIsStillGoverned(t *testing.T) {
	deps := Deps{RepoLabel: "shop", Store: declaredStore(t, testDeclaration, measuredFixture()...)}
	report := computeOrFail(t, Request{Paths: []string{"app/models/not_written_yet.rb"}}, deps)

	tr := report.Targets[0]
	if tr.Measured {
		t.Error("an unwritten path reported measured=true")
	}
	if tr.NoRuleGoverns || len(tr.Components) != 1 {
		t.Errorf("an unwritten path under app/models/** must still be governed: %+v", tr)
	}
	if tr.BlastRadius != nil {
		t.Error("an unmeasured path carries a blast radius")
	}
}

func TestCompute_NoRuleGovernsIsExplicit(t *testing.T) {
	deps := Deps{RepoLabel: "shop", Store: declaredStore(t, testDeclaration, measuredFixture()...)}
	report := computeOrFail(t, Request{Paths: []string{"lib/unrelated.rb"}}, deps)
	if !report.Targets[0].NoRuleGoverns {
		t.Error("NoRuleGoverns = false for an ungoverned path")
	}
	if !strings.Contains(report.Render(), "No rule governs this target.") {
		t.Errorf("render does not say no rule governs:\n%s", report.Render())
	}
}

func TestCompute_NoConstraintsDeclaredIsExplicit(t *testing.T) {
	deps := Deps{RepoLabel: "shop", Store: declaredStore(t, "", measuredFixture()...)}
	report := computeOrFail(t, Request{Paths: []string{"app/models/user.rb"}}, deps)
	if report.ConstraintsDeclared {
		t.Error("ConstraintsDeclared = true with nothing declared")
	}
	if !strings.Contains(report.Render(), "no rule governs these targets") {
		t.Errorf("render does not state the absence of declared constraints:\n%s", report.Render())
	}
}

func TestCompute_SymbolTargets(t *testing.T) {
	deps := Deps{RepoLabel: "shop", Store: declaredStore(t, testDeclaration, measuredFixture()...)}
	report := computeOrFail(t, Request{Symbols: []string{"User", "Ghost"}}, deps)

	byName := map[string]TargetReport{}
	for _, tr := range report.Targets {
		byName[tr.Target] = tr
	}
	user := byName["User"]
	if !user.Measured || user.BlastRadius == nil {
		t.Fatalf("User not measured: %+v", user)
	}
	if user.NoRuleGoverns || len(user.Components) != 1 || user.Components[0].Component != "models" {
		t.Errorf("User is a member of models and must be governed: %+v", user.Components)
	}
	ghost := byName["Ghost"]
	if ghost.Measured || ghost.BlastRadius != nil {
		t.Errorf("Ghost is unmeasured and must say so: %+v", ghost)
	}
	if !strings.Contains(report.Render(), "Nothing measured carries this name") {
		t.Errorf("render does not state the unmeasured symbol:\n%s", report.Render())
	}
}

func TestCompute_BlastRadiusCapsSamplesKeepsCounts(t *testing.T) {
	measured := []facts.Fact{{Kind: facts.KindSymbol, Name: "Hub", File: "app/models/hub.rb"}}
	for i := 0; i < BlastSampleCap+5; i++ {
		measured = append(measured, facts.Fact{
			Kind: facts.KindSymbol, Name: fmt.Sprintf("Caller%02d", i), File: "app/controllers/c.rb",
			Relations: []facts.Relation{{Kind: facts.RelCalls, Target: "Hub"}},
		})
	}
	deps := Deps{RepoLabel: "shop", Store: declaredStore(t, testDeclaration, measured...)}
	report := computeOrFail(t, Request{Symbols: []string{"Hub"}}, deps)
	br := report.Targets[0].BlastRadius
	if br.FanIn != BlastSampleCap+5 {
		t.Errorf("FanIn = %d, want %d", br.FanIn, BlastSampleCap+5)
	}
	if len(br.In) != BlastSampleCap || !br.Truncated {
		t.Errorf("sample len = %d truncated = %v, want %d and true", len(br.In), br.Truncated, BlastSampleCap)
	}
}

func TestCompute_DeterministicAcrossInputOrder(t *testing.T) {
	store := declaredStore(t, testDeclaration, measuredFixture()...)
	depsA := Deps{RepoLabel: "shop", Store: store}
	a := computeOrFail(t, Request{Paths: []string{"app/models/user.rb", "app/billing/billing.rb"}, Symbols: []string{"User", "Billing"}}, depsA)
	b := computeOrFail(t, Request{Paths: []string{"app/billing/billing.rb", "app/models/user.rb"}, Symbols: []string{"Billing", "User"}}, depsA)
	aJSON, err := a.JSON()
	if err != nil {
		t.Fatal(err)
	}
	bJSON, err := b.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if string(aJSON) != string(bJSON) {
		t.Errorf("reports differ across input order:\n%s\n---\n%s", aJSON, bJSON)
	}
	if a.Render() != b.Render() {
		t.Error("text renders differ across input order")
	}
}

func TestCompute_EmptyRequestIsANamedError(t *testing.T) {
	_, err := Compute(context.Background(), Request{}, Deps{Store: facts.NewStore()})
	if err == nil || !strings.Contains(err.Error(), "nothing to plan") {
		t.Errorf("err = %v, want a nothing-to-plan error", err)
	}
}

func TestCompute_PatchWithoutFactoryIsANamedError(t *testing.T) {
	deps := Deps{RepoLabel: "shop", Store: declaredStore(t, testDeclaration)}
	diff := "--- a/app/models/user.rb\n+++ b/app/models/user.rb\n@@ -1,1 +1,1 @@\n-a\n+b\n"
	_, err := Compute(context.Background(), Request{Patch: []byte(diff)}, deps)
	if err == nil || !strings.Contains(err.Error(), "engine factory") {
		t.Errorf("err = %v, want an engine-factory error", err)
	}
}

func TestCompute_PatchOutsideScopeIsANamedError(t *testing.T) {
	deps := Deps{RepoLabel: "shop", Store: declaredStore(t, testDeclaration), OutputDirName: ".enola"}
	diff := "--- a/.enola/facts.jsonl\n+++ b/.enola/facts.jsonl\n@@ -1,1 +1,1 @@\n-a\n+b\n"
	_, err := Compute(context.Background(), Request{Patch: []byte(diff)}, deps)
	if err == nil || !strings.Contains(err.Error(), "outside the snapshot's scope") {
		t.Errorf("err = %v, want an outside-the-scope error", err)
	}
}

func TestContractStore_CompilesWorkingTreeDeclarations(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, intent.RepoFileName), []byte(testDeclaration), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := ContractStore(repo, measuredFixture(), nil)
	if err != nil {
		t.Fatalf("ContractStore: %v", err)
	}
	if got := len(store.ByKind(facts.KindIntent)); got != 3 {
		t.Errorf("compiled %d intent facts, want 3 (two components, one rule)", got)
	}
	if got := len(store.ByKind(facts.KindSymbol)); got != 3 {
		t.Errorf("measured symbols = %d, want 3", got)
	}
}

func TestContractStore_InvalidDeclarationIsANamedError(t *testing.T) {
	repo := t.TempDir()
	invalid := "rules:\n  - id: broken\n    forbid: ghost\n    to: nowhere\n    via: calls\n    because: x\n"
	if err := os.WriteFile(filepath.Join(repo, intent.RepoFileName), []byte(invalid), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := ContractStore(repo, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "constraints lint") {
		t.Errorf("err = %v, want a declaration-invalid error naming constraints lint", err)
	}
}

func TestRuleAndBecauseExtraction(t *testing.T) {
	cases := []struct {
		title, wantRule string
	}{
		{"Constraint models-never-bill violated: User -> Billing via calls", "models-never-bill"},
		{"Advisory constraint soft-rule violated: x", "soft-rule"},
		{"Strict constraint hard-rule violated: y", "hard-rule"},
		{"forbid_reach rule far-rule skipped: component too large for bounded traversal", "far-rule"},
		{"Guidance for models: prior-art", "prior-art"},
		{"Constraint component models matches nothing", ""},
	}
	for _, tc := range cases {
		if got := ruleOf(tc.title); got != tc.wantRule {
			t.Errorf("ruleOf(%q) = %q, want %q", tc.title, got, tc.wantRule)
		}
	}
	desc := "Something. The rule is declared. Because: billing is reached through the ledger"
	if got := becauseOf(desc); got != "billing is reached through the ledger" {
		t.Errorf("becauseOf = %q", got)
	}
	if got := becauseOf("no rationale here"); got != "" {
		t.Errorf("becauseOf on absent marker = %q, want empty", got)
	}
}

const guideDeclaration = `components:
  - name: models
    match: [app/models/**]
rules:
  - id: prior-art
    guide: models
    message: "Follow the concern pattern the exemplars show"
    exemplars:
      - app/models/user.rb
      - app/models/gone.rb
    because: fat models are where every past incident started
`

func TestRender_ExemplarPresenceIsTriState(t *testing.T) {
	measured := Deps{RepoLabel: "shop", Store: declaredStore(t, guideDeclaration, measuredFixture()...)}
	report := computeOrFail(t, Request{Paths: []string{"app/models/new_model.rb"}}, measured)
	rendered := report.Render()
	if !strings.Contains(rendered, "exemplar app/models/user.rb (present)") {
		t.Errorf("a measured exemplar must render present:\n%s", rendered)
	}
	if !strings.Contains(rendered, "exemplar app/models/gone.rb (absent)") {
		t.Errorf("an unresolvable exemplar must render absent when a snapshot exists:\n%s", rendered)
	}

	declarationsOnly := Deps{RepoLabel: "shop", Store: declaredStore(t, guideDeclaration)}
	report = computeOrFail(t, Request{Paths: []string{"app/models/new_model.rb"}}, declarationsOnly)
	rendered = report.Render()
	if !strings.Contains(rendered, "exemplar app/models/user.rb (unmeasured — no snapshot)") ||
		!strings.Contains(rendered, "exemplar app/models/gone.rb (unmeasured — no snapshot)") {
		t.Errorf("with no snapshot every exemplar must render unmeasured, never absent:\n%s", rendered)
	}
	if strings.Contains(rendered, "(absent)") || strings.Contains(rendered, "(present)") {
		t.Errorf("declarations-only mode must not claim presence either way:\n%s", rendered)
	}

	var decoded struct {
		Targets []struct {
			Components []struct {
				Guidance []struct {
					Exemplars []struct {
						Presence string `json:"presence"`
					} `json:"exemplars"`
				} `json:"guidance"`
			} `json:"components"`
		} `json:"targets"`
	}
	out, err := report.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatal(err)
	}
	exemplars := decoded.Targets[0].Components[0].Guidance[0].Exemplars
	if len(exemplars) != 2 || exemplars[0].Presence != "unmeasured" || exemplars[1].Presence != "unmeasured" {
		t.Errorf("JSON exemplars = %+v, want the same tri-state the text renders", exemplars)
	}
}

// A path target carries the verdicts that would change if the file left every
// part: the model's breach vanishes with it, and the radius is absent when
// skipped or when a patch supplies the counterfactual instead.
func TestCompute_PathRadiusNamesVanishingVerdicts(t *testing.T) {
	deps := Deps{RepoLabel: "shop", Store: declaredStore(t, testDeclaration, measuredFixture()...)}
	report := computeOrFail(t, Request{Paths: []string{"app/models/user.rb"}}, deps)
	radius := report.Targets[0].Radius
	if radius == nil || len(radius.Vanish) != 1 || radius.Vanish[0].Rule != "models-never-bill" || len(radius.Appear) != 0 {
		t.Fatalf("radius = %+v", radius)
	}
	if !strings.Contains(report.Render(), "would stop being checked:") {
		t.Fatal("the rendered report carries the radius section")
	}
	deps.SkipRadius = true
	if computeOrFail(t, Request{Paths: []string{"app/models/user.rb"}}, deps).Targets[0].Radius != nil {
		t.Fatal("--no-radius leaves the section out")
	}
}

func TestRender_MarksARuleTheSnapshotCannotCheck(t *testing.T) {
	report := &Report{Repo: "r", ConstraintsDeclared: true, Targets: []TargetReport{{
		Target: "analytics/report.go", Kind: TargetPath,
		Components: []constraints.ComponentBinding{{Component: "stats", Rules: []constraints.RuleBinding{
			{Rule: "stats-runtime", Mode: "ratchet", Statement: "each frame of stats must stay within 5 queries", NotChecked: "no runtime capture in this snapshot"},
			{Rule: "checked", Mode: "ratchet", Statement: "stats stays small"},
		}}},
	}}}
	out := report.Render()
	if !strings.Contains(out, "within 5 queries (not checked in this snapshot: no runtime capture)") {
		t.Errorf("the unchecked rule is not marked:\n%s", out)
	}
	if strings.Contains(out, "stats stays small (not checked") {
		t.Errorf("a checked rule is marked:\n%s", out)
	}
}
