package constraints

import (
	"context"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func contractStore() *facts.Store {
	store := facts.NewStore()
	store.Add(
		componentIntent("domain", "app/domain/**"),
		componentIntent("adapters", "app/adapters/**"),
		ruleIntent("domain-stays-pure", "domain", "adapters", "depends_on", "the domain must not know its delivery mechanisms"),
		ruleIntentProps("api-stays-small", map[string]any{
			"cap": "adapters", "max_members": 2, "because": "surface discipline"}),
		facts.Fact{Kind: facts.KindModule, Name: "app/domain/billing", File: "app/domain/billing"},
		facts.Fact{Kind: facts.KindModule, Name: "app/adapters/http", File: "app/adapters/http"},
	)
	return store
}

func TestContractFor_FactNameBindsItsComponentAndRules(t *testing.T) {
	bindings, declared := ContractFor(contractStore(), "app/domain/billing")
	if !declared {
		t.Fatal("components are declared, the contract was asked")
	}
	if len(bindings) != 1 || bindings[0].Component != "domain" {
		t.Fatalf("bindings = %+v, want exactly the domain component", bindings)
	}
	rules := bindings[0].Rules
	if len(rules) != 1 || rules[0].Rule != "domain-stays-pure" {
		t.Fatalf("rules = %+v, want the one rule naming domain", rules)
	}
	if rules[0].Statement != "domain must not reach adapters via depends_on" {
		t.Errorf("statement = %q", rules[0].Statement)
	}
	if rules[0].Because != "the domain must not know its delivery mechanisms" {
		t.Errorf("because = %q", rules[0].Because)
	}
}

func TestContractFor_UnwrittenPathBindsByPattern(t *testing.T) {
	bindings, _ := ContractFor(contractStore(), "app/adapters/grpc/server.go")
	if len(bindings) != 1 || bindings[0].Component != "adapters" {
		t.Fatalf("bindings = %+v, want the adapters component by pattern alone", bindings)
	}
	// Both sides of a rule bind: the target component's contract names the
	// forbid rule pointing at it, and the cap on its own membership.
	if len(bindings[0].Rules) != 2 {
		t.Fatalf("rules = %+v, want the forbid naming adapters and the cap on it", bindings[0].Rules)
	}
	// Rules bind in id order: api-stays-small precedes domain-stays-pure.
	if got := bindings[0].Rules[0].Statement; got != "adapters must not exceed 2 members" {
		t.Errorf("cap statement = %q", got)
	}
}

func TestContractFor_UnboundAndUndeclaredAreDistinct(t *testing.T) {
	bindings, declared := ContractFor(contractStore(), "elsewhere/free.go")
	if !declared || len(bindings) != 0 {
		t.Fatalf("an unbound target must answer declared-but-empty, got %+v, %v", bindings, declared)
	}
	if _, declared := ContractFor(facts.NewStore(), "app/domain/billing"); declared {
		t.Fatal("a store with no components must answer not-declared")
	}
}

func TestViolationsReferencing_ExactEvidenceOnly(t *testing.T) {
	insights := []facts.Insight{
		{Title: "Constraint x violated: a -> b", Source: "constraints",
			Evidence: []facts.Evidence{{File: "app/domain/billing", Symbol: "app/domain/billing", Fact: "app/adapters/http"}}},
		{Title: "Some cycle", Source: "cycles",
			Evidence: []facts.Evidence{{File: "app/domain/billing"}}},
		{Title: "Constraint y violated: c -> d", Source: "constraints",
			Evidence: []facts.Evidence{{File: "app/domain/billing/deep.go"}}},
	}
	got := ViolationsReferencing(insights, "app/domain/billing")
	if len(got) != 1 || got[0].Title != "Constraint x violated: a -> b" {
		t.Fatalf("got %+v, want only the constraint whose evidence names the target exactly", got)
	}
	if landed := ViolationsReferencing(insights, "app/adapters/http"); len(landed) != 1 {
		t.Fatalf("an edge's landing fact is evidence about the target too, got %+v", landed)
	}
}

// TestMemberCounts_ResolvesEveryDeclaredComponent: counts come from exactly
// the membership the explainer verdicts with, in name order, with a matched
// component counted by distinct fact name and a dead selector reported as 0.
func TestMemberCounts_ResolvesEveryDeclaredComponent(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		componentIntent("domain", "app/domain/**"),
		componentIntent("ghost", "app/ghost/**"),
		facts.Fact{Kind: facts.KindModule, Name: "app/domain/billing", File: "app/domain/billing"},
		facts.Fact{Kind: facts.KindSymbol, Name: "Billing", File: "app/domain/billing/invoice.rb"},
		facts.Fact{Kind: facts.KindSymbol, Name: "Billing", File: "app/domain/billing/refund.rb"},
	)
	got := MemberCounts(store)
	want := []ComponentCount{
		{Component: "domain", Members: 2, Selector: "match app/domain/**"},
		{Component: "ghost", Members: 0, Selector: "match app/ghost/**"},
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("MemberCounts = %+v, want %+v (distinct names, name order, dead selector at 0)", got, want)
	}
}

// MemberCounts is what `constraints lint` prints, and it resolves through the
// same resolveMembership the explainer verdicts with — so a service-scoped
// selector must count only the named repo's facts, plus the service node for a
// whole-service component, and a same-path fact of another repo never leaks in.
func TestMemberCounts_ServiceScopedComponents(t *testing.T) {
	store := crossRepoStore()
	store.Add(
		serviceComponentIntent("frontend", "frontend", ""),
		serviceComponentIntent("billing-internal", "billing", "internal/**"),
		serviceComponentIntent("payments", "payments", ""),
	)
	got := MemberCounts(store)
	want := []ComponentCount{
		{Component: "billing-internal", Members: 1, Selector: "service billing, match internal/**"},
		{Component: "frontend", Members: 3, Selector: "service frontend"},
		{Component: "payments", Members: 0, Selector: "service payments"},
	}
	if len(got) != len(want) {
		t.Fatalf("MemberCounts = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("MemberCounts[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestContractFor_ServiceScopedPathAndFactTargets(t *testing.T) {
	store := crossRepoStore()
	store.Add(
		serviceComponentIntent("billing-internal", "billing", "internal/**"),
		ruleIntentProps("no-internal-reach", map[string]any{
			"forbid": "frontend", "to": "billing-internal", "via": "calls",
			"mode": "ratchet", "because": "internal surfaces are not a contract"}),
	)
	for target, want := range map[string]bool{
		"billing/internal/new_file.go":  true,
		"billing.internal.Charge":       true,
		"frontend/internal/helper.go":   false,
		"internal/unattributed_path.go": false,
	} {
		bindings, declared := ContractFor(store, target)
		if !declared {
			t.Fatal("components are declared, ContractFor must say so")
		}
		if got := len(bindings) == 1; got != want {
			t.Errorf("ContractFor(%q) bound = %v, want %v: %+v", target, got, want, bindings)
		}
	}
}

// TestContractFor_GuidanceEntriesAnnotateExemplars: guidance binds through
// the contract's own steering list — separate from the law under Rules — and
// each exemplar is annotated against the snapshot: present when a measured
// fact carries it as a file path or an exact name, absent otherwise (fail
// closed), sorted so the contract renders the same on every run.
func TestContractFor_GuidanceEntriesAnnotateExemplars(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		componentIntent("components", "app/components/**"),
		ruleIntentProps("getters-cached", map[string]any{
			"guide": "components", "mode": "notify",
			"message":   "Expensive derived getters here use @cached — consider it (see exemplars)",
			"exemplars": "app/components/sortable-table.js SortableTable app/components/gone.js",
			"because":   "the recurring perf bug"}),
		facts.Fact{Kind: facts.KindModule, Name: "app/components/sortable-table", File: "app/components/sortable-table.js"},
		facts.Fact{Kind: facts.KindSymbol, Name: "SortableTable", File: "app/components/sortable-table.js"},
	)
	bindings, declared := ContractFor(store, "app/components/new-widget.js")
	if !declared || len(bindings) != 1 || bindings[0].Component != "components" {
		t.Fatalf("bindings = %+v, want the guided component for a not-yet-written path", bindings)
	}
	if len(bindings[0].Rules) != 0 {
		t.Fatalf("guidance must not masquerade as law, got rules %+v", bindings[0].Rules)
	}
	guidance := bindings[0].Guidance
	if len(guidance) != 1 || guidance[0].Rule != "getters-cached" {
		t.Fatalf("guidance = %+v, want the one guidance rule", guidance)
	}
	if guidance[0].Mode != "notify" {
		t.Errorf("mode = %q", guidance[0].Mode)
	}
	if guidance[0].Message != "Expensive derived getters here use @cached — consider it (see exemplars)" {
		t.Errorf("message = %q", guidance[0].Message)
	}
	want := []ExemplarStatus{
		{Exemplar: "SortableTable", Presence: PresencePresent},
		{Exemplar: "app/components/gone.js", Presence: PresenceAbsent},
		{Exemplar: "app/components/sortable-table.js", Presence: PresencePresent},
	}
	if len(guidance[0].Exemplars) != len(want) {
		t.Fatalf("exemplars = %+v, want %+v", guidance[0].Exemplars, want)
	}
	for i := range want {
		if guidance[0].Exemplars[i] != want[i] {
			t.Errorf("exemplars[%d] = %+v, want %+v (sorted, present by file or fact name, absent fail-closed)", i, guidance[0].Exemplars[i], want[i])
		}
	}
}

// TestAbsentExemplars_ReportsOnlyTheUnresolvable: lint's note lists exactly
// the exemplars the snapshot cannot resolve, in rule-id then exemplar order,
// through the same existence check the contract annotates with.
func TestAbsentExemplars_ReportsOnlyTheUnresolvable(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		componentIntent("components", "app/components/**"),
		ruleIntentProps("getters-cached", map[string]any{
			"guide": "components", "mode": "notify",
			"message":   "consider @cached",
			"exemplars": "app/components/sortable-table.js app/components/gone.js",
			"because":   "x"}),
		ruleIntentProps("actions-named", map[string]any{
			"guide": "components", "mode": "advisory",
			"message":   "actions here are verbs",
			"exemplars": "also/gone.js",
			"because":   "x"}),
		facts.Fact{Kind: facts.KindModule, Name: "app/components/sortable-table", File: "app/components/sortable-table.js"},
	)
	got := AbsentExemplars(store)
	want := []ExemplarNote{
		{Rule: "actions-named", Exemplar: "also/gone.js"},
		{Rule: "getters-cached", Exemplar: "app/components/gone.js"},
	}
	if len(got) != len(want) {
		t.Fatalf("AbsentExemplars = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("AbsentExemplars[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func guidanceStore(extra ...facts.Fact) *facts.Store {
	store := facts.NewStore()
	store.Add(
		componentIntent("components", "app/components/**"),
		componentIntent("models", "app/models/**"),
		ruleIntentProps("getters-cached", map[string]any{
			"guide": "components", "mode": "notify",
			"message":   "Expensive derived getters here use @cached — consider it",
			"exemplars": "app/components/sortable-table.js app/components/gone.js",
			"because":   "the recurring perf bug"}),
		ruleIntentProps("models-thin", map[string]any{
			"guide": "models", "mode": "advisory",
			"message": "Keep persistence models free of workflow logic",
			"because": "workflow in models is where every past cycle started"}),
	)
	store.Add(extra...)
	return store
}

func TestContractFor_DeclarationsOnlyStoreAnnotatesExemplarsUnmeasured(t *testing.T) {
	bindings, declared := ContractFor(guidanceStore(), "app/components/new-widget.js")
	if !declared || len(bindings) != 1 {
		t.Fatalf("bindings = %+v, declared = %v", bindings, declared)
	}
	guidance := bindings[0].Guidance
	if len(guidance) != 1 || len(guidance[0].Exemplars) != 2 {
		t.Fatalf("guidance = %+v, want one rule with both exemplars", guidance)
	}
	for i, ex := range guidance[0].Exemplars {
		if ex.Presence != PresenceUnmeasured {
			t.Errorf("exemplars[%d].Presence = %q, want %q: with no snapshot, absent and unmeasured must never look the same", i, ex.Presence, PresenceUnmeasured)
		}
		if ex.Label() != "unmeasured — no snapshot" {
			t.Errorf("exemplars[%d].Label() = %q", i, ex.Label())
		}
	}
}

func TestExemplarStatus_LabelsAllThreeStates(t *testing.T) {
	cases := map[string]string{
		PresencePresent:    "present",
		PresenceAbsent:     "absent",
		PresenceUnmeasured: "unmeasured — no snapshot",
	}
	for presence, want := range cases {
		if got := (ExemplarStatus{Presence: presence}).Label(); got != want {
			t.Errorf("Label(%q) = %q, want %q", presence, got, want)
		}
	}
}

func TestGuidanceForFiles_MatchesOnlyGuidedComponentsTheFilesTouch(t *testing.T) {
	store := guidanceStore(
		facts.Fact{Kind: facts.KindModule, Name: "app/components/sortable-table", File: "app/components/sortable-table.js"},
	)
	got := GuidanceForFiles(store, []ChangedFile{
		{Path: "app/components/table.js"},
		{Path: "app/components/avatar.js"},
		{Path: "app/services/billing.rb"},
	})
	if len(got) != 1 || got[0].Rule != "getters-cached" {
		t.Fatalf("got %+v, want exactly the components guidance", got)
	}
	if got[0].Component != "components" || got[0].Mode != "notify" || got[0].Because != "the recurring perf bug" {
		t.Errorf("match = %+v", got[0])
	}
	wantFiles := []string{"app/components/avatar.js", "app/components/table.js"}
	if len(got[0].MatchedFiles) != len(wantFiles) || got[0].MatchedFiles[0] != wantFiles[0] || got[0].MatchedFiles[1] != wantFiles[1] {
		t.Errorf("matched files = %+v, want %+v sorted", got[0].MatchedFiles, wantFiles)
	}
	wantExemplars := []ExemplarStatus{
		{Exemplar: "app/components/gone.js", Presence: PresenceAbsent},
		{Exemplar: "app/components/sortable-table.js", Presence: PresencePresent},
	}
	for i := range wantExemplars {
		if got[0].Exemplars[i] != wantExemplars[i] {
			t.Errorf("exemplars[%d] = %+v, want %+v", i, got[0].Exemplars[i], wantExemplars[i])
		}
	}
}

func TestGuidanceForFiles_UntouchedComponentsStaySilent(t *testing.T) {
	if got := GuidanceForFiles(guidanceStore(), []ChangedFile{{Path: "app/services/billing.rb"}}); len(got) != 0 {
		t.Fatalf("got %+v, want nothing: guidance for files the delta never touched must not ride the verdict", got)
	}
	if got := GuidanceForFiles(guidanceStore(), nil); len(got) != 0 {
		t.Fatalf("got %+v, want nothing for an empty delta", got)
	}
	if got := GuidanceForFiles(facts.NewStore(), []ChangedFile{{Path: "app/components/table.js"}}); len(got) != 0 {
		t.Fatalf("got %+v, want nothing when no components are declared", got)
	}
}

func TestGuidanceForFiles_SortedByRuleAcrossComponents(t *testing.T) {
	got := GuidanceForFiles(guidanceStore(), []ChangedFile{
		{Path: "app/models/user.rb"},
		{Path: "app/components/table.js"},
	})
	if len(got) != 2 || got[0].Rule != "getters-cached" || got[1].Rule != "models-thin" {
		t.Fatalf("got %+v, want both matches in rule-id order", got)
	}
}

func TestGuidanceForFiles_ServiceScopedComponentAttributesByLabelFailClosed(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		facts.Fact{Kind: facts.KindIntent, Name: "component: billing-app", File: "wiki/p.md",
			Props: map[string]any{"intent_kind": "component", "component": "billing-app",
				"service": "billing", "match": "app/models/**", "source": "wiki/p.md"}},
		ruleIntentProps("billing-models", map[string]any{
			"guide": "billing-app", "mode": "notify",
			"message": "billing models version their money columns",
			"because": "currency drift"}),
	)
	if got := GuidanceForFiles(store, []ChangedFile{{Path: "app/models/invoice.rb", Repo: "storefront"}}); len(got) != 0 {
		t.Fatalf("got %+v, want nothing: another service's file must not select billing guidance", got)
	}
	if got := GuidanceForFiles(store, []ChangedFile{{Path: "app/models/invoice.rb"}}); len(got) != 0 {
		t.Fatalf("got %+v, want nothing: an unlabeled unprefixed path cannot be attributed to a service", got)
	}
	labeled := GuidanceForFiles(store, []ChangedFile{{Path: "app/models/invoice.rb", Repo: "billing"}})
	prefixed := GuidanceForFiles(store, []ChangedFile{{Path: "billing/app/models/invoice.rb"}})
	if len(labeled) != 1 || len(prefixed) != 1 {
		t.Fatalf("labeled = %+v, prefixed = %+v, want the billing file matched in both forms", labeled, prefixed)
	}
}

// Finding 0057: a part bound by a name rule or a cycle rule is told so before
// an edit, and a forbid over literals says the literals it forbids.
func TestContractFor_NameAndCycleRulesBindAndLiteralsAreSaid(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		componentIntent("queries", "app/queries/**"),
		componentIntent("models", "app/models/**"),
		componentIntent("jobs", "app/jobs/**"),
		ruleIntentProps("queries-never-write", map[string]any{"forbid": "queries", "to_name": "save update!", "via": "calls", "because": "a query never writes"}),
		ruleIntentProps("no-render", map[string]any{"forbid": "models", "to_name": "render", "receiver": "none", "via": "calls", "because": "request only"}),
		ruleIntentProps("no-get-prefix", map[string]any{"forbid_name": "queries", "pattern": "get_*", "because": "a reader is a noun"}),
		ruleIntentProps("no-circle", map[string]any{"forbid_cycles": "models", "among": "jobs", "because": "one unit in practice"}),
		facts.Fact{Kind: facts.KindModule, Name: "app/queries/open", File: "app/queries/open.rb"},
		facts.Fact{Kind: facts.KindModule, Name: "app/jobs/sweep", File: "app/jobs/sweep.rb"},
	)
	said := func(path string) map[string]string {
		bindings, _ := ContractFor(store, path)
		out := map[string]string{}
		for _, b := range bindings {
			for _, r := range b.Rules {
				out[r.Rule] = r.Statement
			}
		}
		return out
	}
	q := said("app/queries/open.rb")
	if q["queries-never-write"] != "queries must not reach save or update! via calls" {
		t.Errorf("a forbid over literals names them: %q", q["queries-never-write"])
	}
	if q["no-get-prefix"] != "members of queries must not be named like get_*" {
		t.Errorf("a name rule binds its part: %+v", q)
	}
	m := said("app/models/invoice.rb")
	if m["no-render"] != "models must not reach render via calls, called with no receiver" {
		t.Errorf("a receiver narrowing is said: %+v", m)
	}
	if m["no-circle"] != "models, jobs must not depend on each other in a circle" {
		t.Errorf("a cycle rule binds its first part: %+v", m)
	}
	if j := said("app/jobs/sweep.rb"); j["no-circle"] == "" {
		t.Errorf("a cycle rule binds every part among it: %+v", j)
	}
}

func policyStore(extra ...facts.Fact) *facts.Store {
	store := facts.NewStore()
	store.Add(
		componentIntent("stats", "analytics/**"),
		componentIntent("shop", "checkout/**"),
		ruleIntentProps("stats-runtime", map[string]any{
			"cap_runtime": "stats", "metric": "queries", "max": 5, "because": "frames of the report stay cheap"}),
		ruleIntentProps("shop-no-debug-names", map[string]any{
			"forbid_name": "shop", "pattern": "Debug*", "surface": "exported", "because": "no debug helpers in the public surface"}),
		ruleIntentProps("shop-no-debug-anywhere", map[string]any{
			"forbid_name": "shop", "pattern": "Debug*", "because": "no debug helpers at all"}),
		facts.Fact{Kind: facts.KindSymbol, Name: "Report", File: "analytics/report.go"},
		facts.Fact{Kind: facts.KindSymbol, Name: "Checkout", File: "checkout/checkout.go"},
	)
	store.Add(extra...)
	return store
}

func bindingFor(t *testing.T, store *facts.Store, target, rule string) RuleBinding {
	t.Helper()
	bindings, _ := ContractFor(store, target)
	for _, b := range bindings {
		for _, r := range b.Rules {
			if r.Rule == rule {
				return r
			}
		}
	}
	t.Fatalf("no binding for rule %s at %s: %+v", rule, target, bindings)
	return RuleBinding{}
}

func TestContractFor_ForbidNameSaysExportedWhenTheSurfaceNarrowsIt(t *testing.T) {
	store := policyStore()
	if got := bindingFor(t, store, "checkout/checkout.go", "shop-no-debug-names").Statement; got != "exported members of shop must not be named like Debug*" {
		t.Errorf("statement = %q", got)
	}
	if got := bindingFor(t, store, "checkout/checkout.go", "shop-no-debug-anywhere").Statement; got != "members of shop must not be named like Debug*" {
		t.Errorf("unnarrowed statement = %q", got)
	}
}

func TestContractFor_AnUnevaluableRuleCarriesTheEvaluatorsReason(t *testing.T) {
	store := policyStore()
	unchecked := bindingFor(t, store, "analytics/report.go", "stats-runtime")
	if unchecked.NotChecked != "no runtime capture in this snapshot" {
		t.Errorf("NotChecked = %q", unchecked.NotChecked)
	}
	if got := bindingFor(t, store, "checkout/checkout.go", "shop-no-debug-names").NotChecked; got != "" {
		t.Errorf("a rule the snapshot checks is marked %q", got)
	}

	var finding string
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range insights {
		if strings.Contains(in.Title, "stats-runtime") {
			finding = in.Title
		}
	}
	if !strings.HasSuffix(finding, "cannot be evaluated: "+unchecked.NotChecked) {
		t.Errorf("the evaluator says %q, the contract says %q", finding, unchecked.NotChecked)
	}
}

func TestContractFor_WithoutMeasuredFactsNothingIsMarked(t *testing.T) {
	store := facts.NewStore()
	store.Add(componentIntent("stats", "analytics/**"),
		ruleIntentProps("stats-runtime", map[string]any{
			"cap_runtime": "stats", "metric": "queries", "max": 5, "because": "cheap"}))
	if got := bindingFor(t, store, "analytics/report.go", "stats-runtime").NotChecked; got != "" {
		t.Errorf("declarations alone were marked %q", got)
	}
}

func TestContractFor_EveryFormTheEvaluatorRefusesIsMarked(t *testing.T) {
	store := policyStore(
		ruleIntentProps("shop-independent", map[string]any{"independent": "shop", "because": "b"}),
		ruleIntentProps("shop-consumed", map[string]any{"require_consumer": "shop", "because": "b"}),
		ruleIntentProps("shop-unique", map[string]any{"unique_across": "shop", "by": "name", "because": "b"}),
	)
	want := map[string]string{
		"shop-independent": "no resolved ancestry in this snapshot",
		"shop-consumed":    "no counterparty in this snapshot",
		"shop-unique":      "no counterparty in this snapshot",
	}
	for rule, reason := range want {
		if got := bindingFor(t, store, "checkout/checkout.go", rule).NotChecked; got != reason {
			t.Errorf("%s: NotChecked = %q, want %q", rule, got, reason)
		}
	}
}
