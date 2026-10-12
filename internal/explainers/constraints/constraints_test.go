package constraints

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func componentIntent(name, match string) facts.Fact {
	return facts.Fact{Kind: facts.KindIntent, Name: "component: " + name, File: "wiki/p.md",
		Props: map[string]any{"intent_kind": "component", "component": name, "match": match, "source": "wiki/p.md"}}
}

func ruleIntent(id, forbid, to, via, because string) facts.Fact {
	return facts.Fact{Kind: facts.KindIntent, Name: "rule: " + id, File: "wiki/p.md",
		Props: map[string]any{"intent_kind": "rule", "rule": id, "forbid": forbid, "to": to,
			"via": via, "because": because, "source": "wiki/p.md"}}
}

func TestExplain_ForbiddenEdgeIsAProofClassViolation(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		componentIntent("domain", "app/domain/**"),
		componentIntent("adapters", "app/adapters/**"),
		ruleIntent("domain-stays-pure", "domain", "adapters", "depends_on", "the domain must not know its delivery mechanisms"),
		facts.Fact{Kind: facts.KindModule, Name: "app/domain/billing", File: "app/domain/billing",
			Relations: []facts.Relation{{Kind: facts.RelDependsOn, Target: "app/adapters/http"}}},
		facts.Fact{Kind: facts.KindModule, Name: "app/adapters/http", File: "app/adapters/http"},
	)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if len(insights) != 1 {
		t.Fatalf("insights = %d, want exactly 1: %+v", len(insights), insights)
	}
	got := insights[0]
	want := "Constraint domain-stays-pure violated: app/domain/billing -> app/adapters/http via depends_on"
	if got.Title != want {
		t.Errorf("title = %q, want %q", got.Title, want)
	}
	if got.Confidence != 1.0 {
		t.Errorf("confidence = %v, want 1.0: a declared rule's breach is decided, not estimated", got.Confidence)
	}
	if !strings.Contains(got.Description, "Because: the domain must not know its delivery mechanisms") {
		t.Errorf("description must surface the rule's rationale, got: %q", got.Description)
	}
	if len(got.Evidence) != 1 || got.Evidence[0].File != "app/domain/billing" ||
		got.Evidence[0].Symbol != "app/domain/billing" || got.Evidence[0].Fact != "app/adapters/http" {
		t.Errorf("evidence = %+v, want the source file/name and target name", got.Evidence)
	}
}

// TestExplain_RollupRestatingADirectBreachIsNotASecondBreach reproduces a
// Python import: the file-level edge grounds onto payments/checkout.py and is
// cited at its line, while the module-edges binder also derives
// orders -> payments from the call behind it. Reporting both counted one import
// as two breaches, the second with no line to point at. A rollup no direct
// breach explains is still reported: it is the only edge a language without
// import statements has.
func TestExplain_RollupRestatingADirectBreachIsNotASecondBreach(t *testing.T) {
	rollup := facts.Fact{Kind: facts.KindDependency, Name: "module-edge: orders -> payments", File: "orders",
		Props:     map[string]any{facts.PropCouplingKind: facts.CouplingSymbolRollup},
		Relations: []facts.Relation{{Kind: facts.RelImports, Target: "payments"}}}
	direct := facts.Fact{Kind: facts.KindDependency, Name: "orders/service -> payments.checkout", File: "orders/service.py", Line: 2,
		Relations: []facts.Relation{{Kind: facts.RelImports, Target: "payments/checkout"}}}
	base := []facts.Fact{
		componentIntent("orders", "orders/**"),
		componentIntent("payments", "payments/**"),
		ruleIntent("orders-not-payments", "orders", "payments", "imports", "payments depends on orders"),
		{Kind: facts.KindModule, Name: "orders", File: "orders"},
		{Kind: facts.KindModule, Name: "payments", File: "payments"},
		{Kind: facts.KindSymbol, Name: "payments/checkout.charge", File: "payments/checkout.py", Line: 3},
	}
	violations := func(extra ...facts.Fact) []facts.Insight {
		store := facts.NewStore()
		store.Add(append(append([]facts.Fact(nil), base...), extra...)...)
		var out []facts.Insight
		for _, in := range explain(t, store) {
			if strings.Contains(in.Title, " violated: ") {
				out = append(out, in)
			}
		}
		return out
	}

	got := violations(rollup, direct)
	if len(got) != 1 {
		t.Fatalf("violations = %+v, want only the direct breach", got)
	}
	if ev := got[0].Evidence[0]; ev.File != "orders/service.py" || ev.Line != 2 || ev.Fact != "payments/checkout" {
		t.Errorf("evidence = %+v, want orders/service.py:2 naming payments/checkout", ev)
	}
	if want := "orders/service -> payments/checkout via imports"; !strings.HasSuffix(got[0].Title, want) {
		t.Errorf("title = %q, want suffix %q", got[0].Title, want)
	}

	if got := violations(rollup); len(got) != 1 || !strings.HasSuffix(got[0].Title, "module-edge: orders -> payments via imports") {
		t.Errorf("rollup-only violations = %+v, want the rollup kept", got)
	}
}

// TestEdgeTitle_DropsOnlyARestatedTarget: the imported half of a dependency
// name is dropped when it merely restates the target, and kept when it says
// something the target does not. Dropping it for Rust merged every `use` of a
// crate from one directory into a single finding, measured on codex: 90
// breaches became 59, and the text gate printed one line of each merged group.
func TestEdgeTitle_DropsOnlyARestatedTarget(t *testing.T) {
	dep := func(name string) facts.Fact { return facts.Fact{Kind: facts.KindDependency, Name: name} }
	for _, tt := range []struct {
		fact   facts.Fact
		target string
		want   string
	}{
		{dep("module-edge: orders -> payments"), "payments", "module-edge: orders -> payments via imports"},
		{dep("orders/service -> payments.checkout"), "payments/checkout", "orders/service -> payments/checkout via imports"},
		{dep("codex-rs/analytics/src -> codex_git_utils::collect_git_info"), "codex-rs/git-utils/src",
			"codex-rs/analytics/src -> codex_git_utils::collect_git_info -> codex-rs/git-utils/src via imports"},
		{facts.Fact{Kind: facts.KindSymbol, Name: "a -> b"}, "b", "a -> b -> b via imports"},
	} {
		if got := edgeTitle(tt.fact, tt.target, "imports"); got != tt.want {
			t.Errorf("edgeTitle(%q, %q) = %q, want %q", tt.fact.Name, tt.target, got, tt.want)
		}
	}
}

func TestExplain_SameDependencyFromTwoCarriersIsOneVerdict(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		componentIntent("engine", "gin.go context.go"),
		componentIntent("render", "render/**"),
		ruleIntent("engine-avoids-render", "engine", "render", "imports", "the engine must not depend on rendering"),
		facts.Fact{Kind: facts.KindDependency, Name: ". -> example.com/gin/render", File: "context.go",
			Relations: []facts.Relation{{Kind: facts.RelImports, Target: "render"}}},
		facts.Fact{Kind: facts.KindDependency, Name: ". -> example.com/gin/render", File: "gin.go",
			Relations: []facts.Relation{{Kind: facts.RelImports, Target: "render"}}},
		facts.Fact{Kind: facts.KindModule, Name: "render", File: "render"},
	)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	var verdicts []facts.Insight
	for _, insight := range insights {
		if strings.Contains(insight.Title, "engine-avoids-render violated") {
			verdicts = append(verdicts, insight)
		}
	}
	if len(verdicts) != 1 {
		t.Fatalf("verdicts = %d, want exactly 1 — two carriers naming the same dependency is one breach: %+v", len(verdicts), verdicts)
	}
	evidence := verdicts[0].Evidence
	if len(evidence) != 2 {
		t.Fatalf("evidence = %+v, want both witnessing files merged onto the one verdict", evidence)
	}
	if evidence[0].File != "context.go" || evidence[1].File != "gin.go" {
		t.Errorf("evidence order = [%s, %s], want deterministic file order [context.go, gin.go]", evidence[0].File, evidence[1].File)
	}
}

func TestExplain_NonViolatingEdgeIsSilence(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		componentIntent("domain", "app/domain/**"),
		componentIntent("adapters", "app/adapters/**"),
		ruleIntent("domain-stays-pure", "domain", "adapters", "depends_on", "the domain must not know its delivery mechanisms"),
		// The allowed direction: an adapter reaching into the domain.
		facts.Fact{Kind: facts.KindModule, Name: "app/adapters/http", File: "app/adapters/http",
			Relations: []facts.Relation{{Kind: facts.RelDependsOn, Target: "app/domain/billing"}}},
		facts.Fact{Kind: facts.KindModule, Name: "app/domain/billing", File: "app/domain/billing"},
	)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if len(insights) != 0 {
		t.Fatalf("insights = %+v, want none: an agreeing verdict is silence", insights)
	}
}

func TestExplain_EmptyComponentGetsTheDeadSelectorAdvisory(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		componentIntent("ghost", "app/ghost/**"),
		facts.Fact{Kind: facts.KindModule, Name: "app/domain/billing", File: "app/domain/billing"},
	)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if len(insights) != 1 {
		t.Fatalf("insights = %d, want exactly 1: %+v", len(insights), insights)
	}
	if got := insights[0].Title; got != "Constraint component ghost matches nothing" {
		t.Errorf("title = %q", got)
	}
	if got := insights[0].Confidence; got != emptyComponentConfidence {
		t.Errorf("confidence = %v, want %v: a dead selector is an advisory, never a breach", got, emptyComponentConfidence)
	}
}

func TestExplain_TargetResolutionFailsClosed(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		componentIntent("domain", "app/domain/**"),
		componentIntent("adapters", "app/adapters/**"),
		ruleIntent("domain-stays-pure", "domain", "adapters", "depends_on", "the domain must not know its delivery mechanisms"),
		// The target string does not exactly name the adapter fact, so no
		// membership can be proven — and an unprovable match is no violation.
		facts.Fact{Kind: facts.KindModule, Name: "app/domain/billing", File: "app/domain/billing",
			Relations: []facts.Relation{{Kind: facts.RelDependsOn, Target: "http"}}},
		facts.Fact{Kind: facts.KindModule, Name: "app/adapters/http", File: "app/adapters/http"},
	)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if len(insights) != 0 {
		t.Fatalf("insights = %+v, want none: an unresolvable target must never be guessed into a breach", insights)
	}
}

func TestExplain_OutputIsDeterministic(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		componentIntent("domain", "app/domain/**"),
		componentIntent("adapters", "app/adapters/**"),
		componentIntent("ghost", "app/ghost/**"),
		ruleIntent("domain-stays-pure", "domain", "adapters", "depends_on", "the domain must not know its delivery mechanisms"),
		ruleIntent("no-domain-calls", "domain", "adapters", "calls", "runtime coupling counts too"),
		facts.Fact{Kind: facts.KindModule, Name: "app/domain/billing", File: "app/domain/billing",
			Relations: []facts.Relation{{Kind: facts.RelDependsOn, Target: "app/adapters/http"}}},
		facts.Fact{Kind: facts.KindModule, Name: "app/domain/pricing", File: "app/domain/pricing",
			Relations: []facts.Relation{{Kind: facts.RelDependsOn, Target: "app/adapters/http"}}},
		facts.Fact{Kind: facts.KindSymbol, Name: "app/domain/billing.Invoice.Send", File: "app/domain/billing/invoice.go",
			Relations: []facts.Relation{{Kind: facts.RelCalls, Target: "app/adapters/http.Client.Post"}}},
		facts.Fact{Kind: facts.KindSymbol, Name: "app/adapters/http.Client.Post", File: "app/adapters/http/client.go"},
		facts.Fact{Kind: facts.KindModule, Name: "app/adapters/http", File: "app/adapters/http"},
	)
	first, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	second, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 4 {
		t.Fatalf("insights = %d, want 3 violations + 1 advisory: %+v", len(first), first)
	}
	if !reflect.DeepEqual(first, second) {
		t.Errorf("two runs over one store diverged:\n%+v\n%+v", first, second)
	}
	if !sortedByTitle(first) {
		t.Errorf("insights are not title-sorted: %+v", first)
	}
}

func ruleIntentProps(id string, props map[string]any) facts.Fact {
	merged := map[string]any{"intent_kind": "rule", "rule": id, "source": "wiki/p.md"}
	for k, v := range props {
		merged[k] = v
	}
	return facts.Fact{Kind: facts.KindIntent, Name: "rule: " + id, File: "wiki/p.md", Props: merged}
}

func TestExplain_AllowOnlyEdgeOutsideOnlyIsAViolation(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		componentIntent("domain", "app/domain/**"),
		componentIntent("contracts", "app/contracts/**"),
		ruleIntentProps("domain-reaches-contracts-only", map[string]any{
			"allow": "domain", "only": "contracts", "via": "depends_on",
			"because": "the domain speaks to the world through its contracts"}),
		facts.Fact{Kind: facts.KindModule, Name: "app/domain/billing", File: "app/domain/billing",
			Relations: []facts.Relation{
				{Kind: facts.RelDependsOn, Target: "app/contracts/invoicing"},
				{Kind: facts.RelDependsOn, Target: "app/adapters/http"},
			}},
		facts.Fact{Kind: facts.KindModule, Name: "app/contracts/invoicing", File: "app/contracts/invoicing"},
		facts.Fact{Kind: facts.KindModule, Name: "app/adapters/http", File: "app/adapters/http"},
	)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if len(insights) != 1 {
		t.Fatalf("insights = %d, want exactly 1: %+v", len(insights), insights)
	}
	got := insights[0]
	want := "Constraint domain-reaches-contracts-only violated: app/domain/billing -> app/adapters/http via depends_on"
	if got.Title != want {
		t.Errorf("title = %q, want %q", got.Title, want)
	}
	if got.Confidence != 1.0 {
		t.Errorf("confidence = %v, want 1.0", got.Confidence)
	}
	if !strings.Contains(got.Description, "Because: the domain speaks to the world through its contracts") {
		t.Errorf("description must surface the rationale, got: %q", got.Description)
	}
}

func TestExplain_AllowOnlySelfAllowedAndUnresolvableSkipped(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		componentIntent("domain", "app/domain/**"),
		componentIntent("contracts", "app/contracts/**"),
		ruleIntentProps("domain-reaches-contracts-only", map[string]any{
			"allow": "domain", "only": "contracts", "via": "depends_on",
			"because": "the domain speaks to the world through its contracts"}),
		// Internal structure, an allowed landing, and a target nothing measures:
		// none of the three may become a breach.
		facts.Fact{Kind: facts.KindModule, Name: "app/domain/billing", File: "app/domain/billing",
			Relations: []facts.Relation{
				{Kind: facts.RelDependsOn, Target: "app/domain/pricing"},
				{Kind: facts.RelDependsOn, Target: "app/contracts/invoicing"},
				{Kind: facts.RelDependsOn, Target: "github.com/elsewhere/pkg"},
			}},
		facts.Fact{Kind: facts.KindModule, Name: "app/domain/pricing", File: "app/domain/pricing"},
		facts.Fact{Kind: facts.KindModule, Name: "app/contracts/invoicing", File: "app/contracts/invoicing"},
	)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if len(insights) != 0 {
		t.Fatalf("insights = %+v, want none: self-edges are internal structure, allowed landings agree, and an unresolvable target is never guessed into a breach", insights)
	}
}

func TestExplain_ForbidFactMembersAreViolations(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		componentIntent("legacy", "app/legacy/**"),
		ruleIntentProps("legacy-stays-empty", map[string]any{
			"forbid_fact": "legacy", "because": "the legacy tree was retired in the platform decision"}),
		facts.Fact{Kind: facts.KindModule, Name: "app/legacy/billing", File: "app/legacy/billing"},
		facts.Fact{Kind: facts.KindModule, Name: "app/legacy/auth", File: "app/legacy/auth"},
	)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if len(insights) != 2 {
		t.Fatalf("insights = %d, want one per member: %+v", len(insights), insights)
	}
	if got, want := insights[0].Title, "Constraint legacy-stays-empty violated: app/legacy/auth is measured in legacy"; got != want {
		t.Errorf("title = %q, want %q", got, want)
	}
	if insights[0].Confidence != 1.0 || insights[1].Confidence != 1.0 {
		t.Errorf("a forbidden fact is a decided-rule breach: %+v", insights)
	}
	if insights[0].Evidence[0].File != "app/legacy/auth" {
		t.Errorf("evidence = %+v, want the member's file", insights[0].Evidence)
	}
}

func TestExplain_CapOverflowIsOneViolationNamingTheOverflow(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		componentIntent("public-api", "app/api/**"),
		ruleIntentProps("api-stays-small", map[string]any{
			"cap": "public-api", "max_members": 2, "because": "every exposed surface is a support contract"}),
		facts.Fact{Kind: facts.KindModule, Name: "app/api/users", File: "app/api/users"},
		facts.Fact{Kind: facts.KindModule, Name: "app/api/billing", File: "app/api/billing"},
		facts.Fact{Kind: facts.KindModule, Name: "app/api/exports", File: "app/api/exports"},
	)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if len(insights) != 1 {
		t.Fatalf("insights = %d, want exactly 1 — the breach is the count, not each member: %+v", len(insights), insights)
	}
	got := insights[0]
	if want := "Constraint api-stays-small violated: public-api has 3 members over a cap of 2"; got.Title != want {
		t.Errorf("title = %q, want %q", got.Title, want)
	}
	// Name order puts billing and exports under the cap of 2; users overflows.
	if !strings.Contains(got.Description, "in name order: app/api/users") {
		t.Errorf("description must name the sorted overflow, got: %q", got.Description)
	}
	if len(got.Evidence) != 1 || got.Evidence[0].Symbol != "app/api/users" {
		t.Errorf("evidence = %+v, want one entry per overflow member", got.Evidence)
	}
}

func TestExplain_CapUnderTheLimitIsSilence(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		componentIntent("public-api", "app/api/**"),
		ruleIntentProps("api-stays-small", map[string]any{
			"cap": "public-api", "max_members": 2, "because": "every exposed surface is a support contract"}),
		facts.Fact{Kind: facts.KindModule, Name: "app/api/users", File: "app/api/users"},
		facts.Fact{Kind: facts.KindModule, Name: "app/api/billing", File: "app/api/billing"},
	)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if len(insights) != 0 {
		t.Fatalf("insights = %+v, want none: a membership at the cap agrees with the rule", insights)
	}
}

func TestExplain_AdvisoryModeReportsBelowTheGate(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		componentIntent("domain", "app/domain/**"),
		componentIntent("adapters", "app/adapters/**"),
		ruleIntentProps("domain-stays-pure", map[string]any{
			"forbid": "domain", "to": "adapters", "via": "depends_on", "mode": "advisory",
			"because": "the domain must not know its delivery mechanisms"}),
		facts.Fact{Kind: facts.KindModule, Name: "app/domain/billing", File: "app/domain/billing",
			Relations: []facts.Relation{{Kind: facts.RelDependsOn, Target: "app/adapters/http"}}},
		facts.Fact{Kind: facts.KindModule, Name: "app/adapters/http", File: "app/adapters/http"},
	)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if len(insights) != 1 {
		t.Fatalf("insights = %d, want exactly 1: %+v", len(insights), insights)
	}
	got := insights[0]
	if !strings.HasPrefix(got.Title, "Advisory constraint domain-stays-pure violated:") {
		t.Errorf("an advisory breach must announce itself in the title, got %q", got.Title)
	}
	if got.Confidence != advisoryConfidence {
		t.Errorf("confidence = %v, want %v — below the check gate's floor, so it reports and never fails", got.Confidence, advisoryConfidence)
	}
}

// TestExplain_StrictModeStampsTheGateRecognizedTitle: a strict rule's breach
// carries the "Strict constraint" prefix at full confidence — the marker
// pkg/check keys on to fail the violation regardless of baseline presence.
func TestExplain_StrictModeStampsTheGateRecognizedTitle(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		componentIntent("domain", "app/domain/**"),
		componentIntent("adapters", "app/adapters/**"),
		ruleIntentProps("domain-stays-pure", map[string]any{
			"forbid": "domain", "to": "adapters", "via": "depends_on", "mode": "strict",
			"because": "the domain must not know its delivery mechanisms"}),
		facts.Fact{Kind: facts.KindModule, Name: "app/domain/billing", File: "app/domain/billing",
			Relations: []facts.Relation{{Kind: facts.RelDependsOn, Target: "app/adapters/http"}}},
		facts.Fact{Kind: facts.KindModule, Name: "app/adapters/http", File: "app/adapters/http"},
	)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if len(insights) != 1 {
		t.Fatalf("insights = %d, want exactly 1: %+v", len(insights), insights)
	}
	got := insights[0]
	if !strings.HasPrefix(got.Title, "Strict constraint domain-stays-pure violated:") {
		t.Errorf("a strict breach must announce itself in the title, got %q", got.Title)
	}
	if got.Confidence != 1.0 {
		t.Errorf("confidence = %v, want 1.0 — strict changes enforcement scope, never certainty", got.Confidence)
	}
}

func TestExplain_ImportsCarrierResolvesSourceMembership(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		componentIntent("domain", "app/domain/**"),
		componentIntent("adapters", "app/adapters/**"),
		ruleIntent("domain-imports-nothing-delivered", "domain", "adapters", "imports", "the domain must not know its delivery mechanisms"),
		facts.Fact{Kind: facts.KindModule, Name: "app/domain/billing", File: "app/domain/billing"},
		facts.Fact{Kind: facts.KindModule, Name: "app/adapters/http", File: "app/adapters/http"},
		// The imports edge does not ride the module fact: extractors carry it on
		// a dependency fact whose File is the importing file.
		facts.Fact{Kind: facts.KindDependency, Name: "app/domain -> app/adapters/http", File: "app/domain/billing.go",
			Relations: []facts.Relation{{Kind: facts.RelImports, Target: "app/adapters/http"}}},
	)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if len(insights) != 1 {
		t.Fatalf("insights = %d, want exactly 1 — the carrier's file joins the component exactly: %+v", len(insights), insights)
	}
	got := insights[0]
	// The source name is the carrier's own canonical name — extractors name a
	// dependency fact "pkg -> import", so the title reads source-name -> target.
	want := "Constraint domain-imports-nothing-delivered violated: app/domain -> app/adapters/http via imports"
	if got.Title != want {
		t.Errorf("title = %q, want %q", got.Title, want)
	}
	if got.Evidence[0].File != "app/domain/billing.go" {
		t.Errorf("evidence = %+v, want the importing file", got.Evidence)
	}
}

func TestExplain_CarrierSkippedForNameNarrowedComponent(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		facts.Fact{Kind: facts.KindIntent, Name: "component: billing", File: "wiki/p.md",
			Props: map[string]any{"intent_kind": "component", "component": "billing",
				"match": "app/domain/**", "name_pattern": "app/domain/billing", "source": "wiki/p.md"}},
		componentIntent("adapters", "app/adapters/**"),
		ruleIntent("billing-imports-no-adapters", "billing", "adapters", "imports", "billing is delivery-agnostic"),
		facts.Fact{Kind: facts.KindModule, Name: "app/domain/billing", File: "app/domain/billing"},
		facts.Fact{Kind: facts.KindModule, Name: "app/adapters/http", File: "app/adapters/http"},
		// A file-level carrier cannot prove the single named fact made this
		// import, so it must not be attributed to it.
		facts.Fact{Kind: facts.KindDependency, Name: "app/domain -> app/adapters/http", File: "app/domain/pricing.go",
			Relations: []facts.Relation{{Kind: facts.RelImports, Target: "app/adapters/http"}}},
	)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if len(insights) != 0 {
		t.Fatalf("insights = %+v, want none: a name-narrowed component gets no file-level carriers", insights)
	}
}

func storageComponentIntent(name, match string) facts.Fact {
	return facts.Fact{Kind: facts.KindIntent, Name: "component: " + name, File: "wiki/p.md",
		Props: map[string]any{"intent_kind": "component", "component": name, "match": match,
			"kind": "storage", "source": "wiki/p.md"}}
}

func TestExplain_ProtectUnownedTouchIsAViolation(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		storageComponentIntent("invoice-tables", "db/**"),
		componentIntent("billing", "app/billing/**"),
		ruleIntentProps("only-billing-touches-invoices", map[string]any{
			"protect": "invoice-tables", "owners": "billing", "via": "depends_on",
			"because": "invoice writes must stay auditable through one owner"}),
		facts.Fact{Kind: facts.KindStorage, Name: "invoices", File: "db/schema.rb"},
		facts.Fact{Kind: facts.KindModule, Name: "app/billing/ledger", File: "app/billing/ledger",
			Relations: []facts.Relation{{Kind: facts.RelDependsOn, Target: "invoices"}}},
		facts.Fact{Kind: facts.KindModule, Name: "app/reports/monthly", File: "app/reports/monthly",
			Relations: []facts.Relation{{Kind: facts.RelDependsOn, Target: "invoices"}}},
	)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if len(insights) != 1 {
		t.Fatalf("insights = %d, want exactly 1 — the owner's touch is silence: %+v", len(insights), insights)
	}
	got := insights[0]
	want := "Constraint only-billing-touches-invoices violated: app/reports/monthly -> invoices via depends_on"
	if got.Title != want {
		t.Errorf("title = %q, want %q", got.Title, want)
	}
	if got.Confidence != 1.0 {
		t.Errorf("confidence = %v, want 1.0", got.Confidence)
	}
	if !strings.Contains(got.Description, "Because: invoice writes must stay auditable through one owner") {
		t.Errorf("description must surface the rationale, got: %q", got.Description)
	}
	if got.Evidence[0].File != "app/reports/monthly" || got.Evidence[0].Fact != "invoices" {
		t.Errorf("evidence = %+v, want the unowned source and the protected target", got.Evidence)
	}
}

func TestExplain_ProtectSelfAndOwnerCarrierAreSilence(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		storageComponentIntent("invoice-tables", "db/**"),
		componentIntent("billing", "app/billing/**"),
		ruleIntentProps("only-billing-touches-invoices", map[string]any{
			"protect": "invoice-tables", "owners": "billing", "via": "imports",
			"because": "invoice writes must stay auditable through one owner"}),
		facts.Fact{Kind: facts.KindStorage, Name: "invoices", File: "db/schema.rb"},
		facts.Fact{Kind: facts.KindStorage, Name: "invoice-lines", File: "db/schema.rb",
			Relations: []facts.Relation{{Kind: facts.RelImports, Target: "invoices"}}},
		facts.Fact{Kind: facts.KindModule, Name: "app/billing/ledger", File: "app/billing/ledger"},
		// The owner's edge rides a dependency carrier — file-joined, exactly.
		facts.Fact{Kind: facts.KindDependency, Name: "app/billing -> invoices", File: "app/billing/ledger.rb",
			Relations: []facts.Relation{{Kind: facts.RelImports, Target: "invoices"}}},
	)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if len(insights) != 0 {
		t.Fatalf("insights = %+v, want none: internal structure and an owner's carrier both agree with the rule", insights)
	}
}

func TestExplain_ProtectCarrierFromUnownedFileIsAViolation(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		storageComponentIntent("invoice-tables", "db/**"),
		componentIntent("billing", "app/billing/**"),
		ruleIntentProps("only-billing-touches-invoices", map[string]any{
			"protect": "invoice-tables", "owners": "billing", "via": "imports",
			"because": "invoice writes must stay auditable through one owner"}),
		facts.Fact{Kind: facts.KindStorage, Name: "invoices", File: "db/schema.rb"},
		facts.Fact{Kind: facts.KindModule, Name: "app/billing/ledger", File: "app/billing/ledger"},
		facts.Fact{Kind: facts.KindDependency, Name: "app/reports -> invoices", File: "app/reports/monthly.rb",
			Relations: []facts.Relation{{Kind: facts.RelImports, Target: "invoices"}}},
	)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if len(insights) != 1 {
		t.Fatalf("insights = %d, want exactly 1: %+v", len(insights), insights)
	}
	if got := insights[0].Evidence[0].File; got != "app/reports/monthly.rb" {
		t.Errorf("evidence file = %q, want the unowned importing file", got)
	}
}

func TestExplain_NewFormsAreDeterministic(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		componentIntent("domain", "app/domain/**"),
		componentIntent("contracts", "app/contracts/**"),
		componentIntent("legacy", "app/legacy/**"),
		componentIntent("public-api", "app/api/**"),
		ruleIntentProps("domain-reaches-contracts-only", map[string]any{
			"allow": "domain", "only": "contracts", "via": "depends_on", "because": "contracts are the boundary"}),
		ruleIntentProps("legacy-stays-empty", map[string]any{
			"forbid_fact": "legacy", "because": "retired"}),
		ruleIntentProps("api-stays-small", map[string]any{
			"cap": "public-api", "max_members": 1, "because": "surface discipline"}),
		facts.Fact{Kind: facts.KindModule, Name: "app/domain/billing", File: "app/domain/billing",
			Relations: []facts.Relation{{Kind: facts.RelDependsOn, Target: "app/legacy/auth"}}},
		facts.Fact{Kind: facts.KindModule, Name: "app/contracts/invoicing", File: "app/contracts/invoicing"},
		facts.Fact{Kind: facts.KindModule, Name: "app/legacy/auth", File: "app/legacy/auth"},
		facts.Fact{Kind: facts.KindModule, Name: "app/api/users", File: "app/api/users"},
		facts.Fact{Kind: facts.KindModule, Name: "app/api/billing", File: "app/api/billing"},
	)
	first, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	second, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	// One allow-only breach, one forbidden member, one cap overflow.
	if len(first) != 3 {
		t.Fatalf("insights = %d, want 3: %+v", len(first), first)
	}
	if !reflect.DeepEqual(first, second) {
		t.Errorf("two runs over one store diverged:\n%+v\n%+v", first, second)
	}
	if !sortedByTitle(first) {
		t.Errorf("insights are not title-sorted: %+v", first)
	}
}

func sortedByTitle(insights []facts.Insight) bool {
	for i := 1; i < len(insights); i++ {
		if insights[i-1].Title > insights[i].Title {
			return false
		}
	}
	return true
}

// The require form's target rule: every storage member whose columns contain
// company_id must carry the companies foreign key. One compliant table, one
// violating, one out of the when clause's scope — and one whose column merely
// embeds the token, which whole-member containment must not match.
func TestExplain_RequireRuleVerdictsPropContainment(t *testing.T) {
	store := facts.NewStore()
	table := func(name, columns, fks string) facts.Fact {
		props := map[string]any{"storage_kind": "table", "table": name, "columns": columns}
		if fks != "" {
			props["fk_constraints"] = fks
		}
		return facts.Fact{Kind: facts.KindStorage, Name: name, File: "db/structure.sql", Props: props}
	}
	store.Add(
		storageComponentIntent("tables", "db/**"),
		ruleIntentProps("company-fk", map[string]any{
			"require": "tables", "when_prop": "columns", "when_value": "company_id",
			"must_prop": "fk_constraints", "must_value": "company_id->companies",
			"mode": "ratchet", "because": "tenant isolation joins through companies"}),
		table("employments", "company_id id title", "company_id->companies"),
		table("audit_rows", "company_id id", ""),
		table("companies", "created_at id name", ""),
		table("imports", "id parent_company_id", ""),
	)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	violations := withTitlePrefix(insights, "Constraint company-fk violated")
	if len(violations) != 1 {
		t.Fatalf("violations = %+v, want exactly the audit_rows breach", insights)
	}
	v := violations[0]
	if v.Title != "Constraint company-fk violated: audit_rows must have fk_constraints containing company_id->companies" {
		t.Errorf("title = %q", v.Title)
	}
	if v.Confidence != 1.0 {
		t.Errorf("confidence = %v, want the decided-rule 1.0", v.Confidence)
	}
	if !strings.Contains(v.Description, "Because: tenant isolation joins through companies") {
		t.Errorf("description must carry the rationale: %q", v.Description)
	}
	if len(v.Evidence) != 1 || v.Evidence[0].File != "db/structure.sql" || v.Evidence[0].Symbol != "audit_rows" {
		t.Errorf("evidence = %+v", v.Evidence)
	}
}

// Without a when clause, require binds every member.
func TestExplain_RequireRuleWithoutWhenBindsEveryMember(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		storageComponentIntent("tables", "db/**"),
		ruleIntentProps("always-columns", map[string]any{
			"require":   "tables",
			"must_prop": "columns", "must_value": "id",
			"mode": "ratchet", "because": "every table gets a surrogate key"}),
		facts.Fact{Kind: facts.KindStorage, Name: "keyed", File: "db/structure.sql",
			Props: map[string]any{"columns": "id name"}},
		facts.Fact{Kind: facts.KindStorage, Name: "keyless", File: "db/structure.sql",
			Props: map[string]any{"columns": "name"}},
	)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	violations := withTitlePrefix(insights, "Constraint always-columns violated")
	if len(violations) != 1 || !strings.Contains(violations[0].Title, "keyless") {
		t.Fatalf("violations = %+v, want exactly the keyless breach", insights)
	}
}

func withTitlePrefix(insights []facts.Insight, prefix string) []facts.Insight {
	var out []facts.Insight
	for _, in := range insights {
		if strings.HasPrefix(in.Title, prefix) {
			out = append(out, in)
		}
	}
	return out
}

func serviceComponentIntent(name, service, match string) facts.Fact {
	f := componentIntent(name, match)
	f.Props["service"] = service
	return f
}

// crossRepoStore models a two-repo append-mode snapshot the way the crossrepo
// linker leaves one: repo-labeled member facts with repo-prefixed files, one
// KindService node per repo carrying the service-to-service depends_on edges,
// and the synthetic cross_repo dependency fact per pair.
func crossRepoStore() *facts.Store {
	store := facts.NewStore()
	store.Add(
		facts.Fact{Kind: facts.KindSymbol, Name: "web.Checkout", File: "frontend/app/checkout.go", Repo: "frontend",
			Relations: []facts.Relation{
				{Kind: facts.RelCalls, Target: "billing.internal.Charge"},
				{Kind: facts.RelCalls, Target: "web.internal.Helper"},
			}},
		facts.Fact{Kind: facts.KindSymbol, Name: "web.internal.Helper", File: "frontend/internal/helper.go", Repo: "frontend"},
		facts.Fact{Kind: facts.KindSymbol, Name: "billing.internal.Charge", File: "billing/internal/charge.go", Repo: "billing"},
		facts.Fact{Kind: facts.KindSymbol, Name: "billing.api.Invoice", File: "billing/api/invoice.go", Repo: "billing"},
		facts.Fact{Kind: facts.KindService, Name: "frontend", Repo: "frontend",
			Props:     map[string]any{"synthetic": "crossrepo"},
			Relations: []facts.Relation{{Kind: facts.RelDependsOn, Target: "billing"}}},
		facts.Fact{Kind: facts.KindService, Name: "billing", Repo: "billing",
			Props: map[string]any{"synthetic": "crossrepo"}},
		facts.Fact{Kind: facts.KindDependency, Name: "frontend -> billing", Repo: "frontend",
			Props: map[string]any{"type": "cross_repo", "synthetic": "crossrepo", "via": []string{"http"}}},
	)
	return store
}

func TestExplain_CrossRepoForbiddenEdgeFires(t *testing.T) {
	store := crossRepoStore()
	store.Add(
		serviceComponentIntent("frontend", "frontend", ""),
		serviceComponentIntent("billing-internal", "billing", "internal/**"),
		ruleIntent("no-internal-reach", "frontend", "billing-internal", "calls", "internal surfaces are not a contract"),
	)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if len(insights) != 1 {
		t.Fatalf("insights = %+v, want exactly the cross-repo breach", insights)
	}
	got := insights[0]
	want := "Constraint no-internal-reach violated: web.Checkout -> billing.internal.Charge via calls"
	if got.Title != want {
		t.Errorf("title = %q, want %q", got.Title, want)
	}
	if got.Confidence != 1.0 {
		t.Errorf("confidence = %v, want the decided-rule 1.0", got.Confidence)
	}
	if len(got.Evidence) != 1 || got.Evidence[0].File != "frontend/app/checkout.go" ||
		got.Evidence[0].Fact != "billing.internal.Charge" {
		t.Errorf("evidence = %+v", got.Evidence)
	}
}

// The frontend's own internal/ subtree matches billing-internal's patterns
// once the repo prefix is trimmed, so without the service AND the helper call
// above would breach. Exactly one violation in the fixture proves service
// scoping excludes same-path facts of the other repo.
func TestExplain_ServiceScopeANDsWithMatchPatterns(t *testing.T) {
	store := crossRepoStore()
	store.Add(
		serviceComponentIntent("frontend", "frontend", ""),
		serviceComponentIntent("billing-internal", "", "internal/**"),
		ruleIntent("no-internal-reach", "frontend", "billing-internal", "calls", "internal surfaces are not a contract"),
	)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(withTitlePrefix(insights, "Constraint no-internal-reach violated")); got != 2 {
		t.Fatalf("a serviceless internal/** selector must catch both repos' internal trees, got %+v", insights)
	}
}

func TestExplain_ServiceToServiceForbiddenEdgeFires(t *testing.T) {
	store := crossRepoStore()
	store.Add(
		serviceComponentIntent("frontend", "frontend", ""),
		serviceComponentIntent("billing", "billing", ""),
		ruleIntent("frontend-off-billing", "frontend", "billing", "depends_on", "billing is reached through the gateway"),
	)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if len(insights) != 1 {
		t.Fatalf("insights = %+v, want exactly the service-to-service breach", insights)
	}
	want := "Constraint frontend-off-billing violated: frontend -> billing via depends_on"
	if insights[0].Title != want {
		t.Errorf("title = %q, want %q", insights[0].Title, want)
	}
	if insights[0].Confidence != 1.0 {
		t.Errorf("confidence = %v, want the decided-rule 1.0", insights[0].Confidence)
	}
}

func TestExplain_CrossRepoAllowedDirectionIsSilence(t *testing.T) {
	store := crossRepoStore()
	store.Add(
		serviceComponentIntent("frontend", "frontend", ""),
		serviceComponentIntent("billing", "billing", ""),
		ruleIntent("billing-off-frontend", "billing", "frontend", "depends_on", "billing must not know its consumers"),
	)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if len(insights) != 0 {
		t.Fatalf("insights = %+v, want none: the measured edge runs the allowed direction", insights)
	}
}

func TestExplain_AbsentServiceRuleIsUnaskedWithAdvisory(t *testing.T) {
	store := crossRepoStore()
	store.Add(
		serviceComponentIntent("frontend", "frontend", ""),
		serviceComponentIntent("payments", "payments", ""),
		ruleIntent("frontend-off-payments", "frontend", "payments", "depends_on", "payments is reached through the gateway"),
	)
	store.Add(facts.Fact{Kind: facts.KindService, Name: "gateway", Repo: "frontend",
		Relations: []facts.Relation{{Kind: facts.RelDependsOn, Target: "payments"}}})
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if len(insights) != 1 {
		t.Fatalf("insights = %+v, want only the absent-service advisory", insights)
	}
	got := insights[0]
	want := "Constraint component payments names service payments not present in this snapshot"
	if got.Title != want {
		t.Errorf("title = %q, want %q", got.Title, want)
	}
	if got.Confidence != absentServiceConfidence {
		t.Errorf("confidence = %v, want %v: unasked is an advisory, never a breach", got.Confidence, absentServiceConfidence)
	}
	if len(withTitlePrefix(insights, "Constraint frontend-off-payments violated")) != 0 {
		t.Error("a rule naming an absent service must emit no verdicts")
	}
	if len(withTitlePrefix(insights, "Constraint component payments matches nothing")) != 0 {
		t.Error("an unasked component must not double-report as a dead selector")
	}
}

func TestExplain_CrossRepoDeterminism(t *testing.T) {
	build := func() *facts.Store {
		store := crossRepoStore()
		store.Add(
			serviceComponentIntent("frontend", "frontend", ""),
			serviceComponentIntent("billing", "billing", ""),
			serviceComponentIntent("billing-internal", "billing", "internal/**"),
			serviceComponentIntent("payments", "payments", ""),
			ruleIntent("no-internal-reach", "frontend", "billing-internal", "calls", "internal surfaces are not a contract"),
			ruleIntent("frontend-off-billing", "frontend", "billing", "depends_on", "billing is reached through the gateway"),
			ruleIntent("frontend-off-payments", "frontend", "payments", "depends_on", "payments is reached through the gateway"),
		)
		return store
	}
	first, err := New().Explain(context.Background(), build())
	if err != nil {
		t.Fatal(err)
	}
	second, err := New().Explain(context.Background(), build())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Errorf("two runs over the same store must agree exactly:\n%+v\n%+v", first, second)
	}
}

func privateStore(rule facts.Fact) *facts.Store {
	store := facts.NewStore()
	store.Add(
		componentIntent("billing", "app/billing/**"),
		componentIntent("web", "app/web/**"),
		componentIntent("support", "app/support/**"),
		rule,
		facts.Fact{Kind: facts.KindSymbol, Name: "Billing::Charge#authorize", File: "app/billing/charge.rb",
			Props: map[string]any{"exported": false}},
		facts.Fact{Kind: facts.KindSymbol, Name: "Billing::Charge#create", File: "app/billing/charge.rb",
			Props: map[string]any{"exported": true}},
		facts.Fact{Kind: facts.KindSymbol, Name: "Support::Refund#issue", File: "app/support/refund.rb",
			Props:     map[string]any{"exported": true},
			Relations: []facts.Relation{{Kind: facts.RelCalls, Target: "Billing::Charge#authorize"}}},
		facts.Fact{Kind: facts.KindSymbol, Name: "Web::Home#index", File: "app/web/home.rb",
			Props: map[string]any{"exported": true}},
	)
	return store
}

func TestExplain_PrivateReachFromOutsideIsAViolation(t *testing.T) {
	store := privateStore(ruleIntentProps("pack-internals", map[string]any{
		"private": "billing", "because": "only the pack's public surface is a contract"}))
	store.Add(
		facts.Fact{Kind: facts.KindSymbol, Name: "Web::CheckoutController#pay", File: "app/web/checkout.rb",
			Props:     map[string]any{"exported": true},
			Relations: []facts.Relation{{Kind: facts.RelCalls, Target: "Billing::Charge#authorize"}}},
	)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if len(insights) != 2 {
		t.Fatalf("insights = %d, want the web and support reaches: %+v", len(insights), insights)
	}
	got := insights[0]
	want := "Constraint pack-internals violated: Support::Refund#issue -> Billing::Charge#authorize via calls"
	if got.Title != want {
		t.Errorf("title = %q, want %q", got.Title, want)
	}
	if got.Confidence != 1.0 {
		t.Errorf("confidence = %v, want 1.0: a declared rule's breach is decided, not estimated", got.Confidence)
	}
	if !strings.Contains(got.Description, "Because: only the pack's public surface is a contract") {
		t.Errorf("description must surface the rule's rationale, got: %q", got.Description)
	}
	if !strings.Contains(insights[1].Title, "Web::CheckoutController#pay -> Billing::Charge#authorize") {
		t.Errorf("second title = %q, want the web reach", insights[1].Title)
	}
}

func TestExplain_PrivateInsideAndExportedReachesAreSilence(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		componentIntent("billing", "app/billing/**"),
		componentIntent("web", "app/web/**"),
		ruleIntentProps("pack-internals", map[string]any{
			"private": "billing", "because": "only the pack's public surface is a contract"}),
		facts.Fact{Kind: facts.KindSymbol, Name: "Billing::Charge#authorize", File: "app/billing/charge.rb",
			Props: map[string]any{"exported": false}},
		facts.Fact{Kind: facts.KindSymbol, Name: "Billing::Charge#create", File: "app/billing/charge.rb",
			Props:     map[string]any{"exported": true},
			Relations: []facts.Relation{{Kind: facts.RelCalls, Target: "Billing::Charge#authorize"}}},
		facts.Fact{Kind: facts.KindSymbol, Name: "Web::CheckoutController#pay", File: "app/web/checkout.rb",
			Props:     map[string]any{"exported": true},
			Relations: []facts.Relation{{Kind: facts.RelCalls, Target: "Billing::Charge#create"}}},
	)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if len(insights) != 0 {
		t.Fatalf("insights = %+v, want none: an inside reach and an exported reach both comply", insights)
	}
}

func TestExplain_PrivateExceptComponentMayReach(t *testing.T) {
	store := privateStore(ruleIntentProps("pack-internals", map[string]any{
		"private": "billing", "except": "support", "because": "support is a blessed collaborator"}))
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if len(insights) != 0 {
		t.Fatalf("insights = %+v, want none: an except component's reach is declared allowed", insights)
	}
}

func TestExplain_PrivateUnmeasuredVisibilityIsOutOfScope(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		componentIntent("billing", "app/billing/**"),
		componentIntent("web", "app/web/**"),
		ruleIntentProps("pack-internals", map[string]any{
			"private": "billing", "because": "only the pack's public surface is a contract"}),
		// No exported prop at all: the extractor recorded no visibility, so the
		// member is out of the rule's scope, not in breach of it.
		facts.Fact{Kind: facts.KindSymbol, Name: "Billing::Charge#legacy", File: "app/billing/charge.rb"},
		// Two facts disagree about the same name's visibility: exactness is
		// gone, so the name is out of scope too.
		facts.Fact{Kind: facts.KindSymbol, Name: "Billing::Charge#settle", File: "app/billing/charge.rb",
			Props: map[string]any{"exported": true}},
		facts.Fact{Kind: facts.KindSymbol, Name: "Billing::Charge#settle", File: "app/billing/charge_ext.rb",
			Props: map[string]any{"exported": false}},
		facts.Fact{Kind: facts.KindSymbol, Name: "Web::CheckoutController#pay", File: "app/web/checkout.rb",
			Props: map[string]any{"exported": true},
			Relations: []facts.Relation{
				{Kind: facts.RelCalls, Target: "Billing::Charge#legacy"},
				{Kind: facts.RelCalls, Target: "Billing::Charge#settle"}}},
	)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if len(insights) != 0 {
		t.Fatalf("insights = %+v, want none: unmeasured or contradictory visibility fails closed", insights)
	}
}

func TestExplain_PrivateIsDeterministic(t *testing.T) {
	build := func() *facts.Store {
		store := privateStore(ruleIntentProps("pack-internals", map[string]any{
			"private": "billing", "because": "only the pack's public surface is a contract"}))
		store.Add(
			facts.Fact{Kind: facts.KindSymbol, Name: "Web::CheckoutController#pay", File: "app/web/checkout.rb",
				Props:     map[string]any{"exported": true},
				Relations: []facts.Relation{{Kind: facts.RelCalls, Target: "Billing::Charge#authorize"}}},
			facts.Fact{Kind: facts.KindDependency, Name: "app/web/cart.rb -> app/billing/charge.rb", File: "app/web/cart.rb",
				Relations: []facts.Relation{{Kind: facts.RelImports, Target: "Billing::Charge#authorize"}}},
		)
		return store
	}
	first, err := New().Explain(context.Background(), build())
	if err != nil {
		t.Fatal(err)
	}
	second, err := New().Explain(context.Background(), build())
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 3 {
		t.Fatalf("insights = %d, want the two symbol reaches and the import carrier's: %+v", len(first), first)
	}
	if !reflect.DeepEqual(first, second) {
		t.Errorf("two runs over one store diverged:\n%+v\n%+v", first, second)
	}
	if !sortedByTitle(first) {
		t.Errorf("insights are not title-sorted: %+v", first)
	}
}

func classSymbol(name, file string) facts.Fact {
	return facts.Fact{Kind: facts.KindSymbol, Name: name, File: file,
		Props: map[string]any{"symbol_kind": facts.SymbolClass}}
}

func methodSymbol(name, file string) facts.Fact {
	return facts.Fact{Kind: facts.KindSymbol, Name: name, File: file,
		Props: map[string]any{"symbol_kind": facts.SymbolMethod}}
}

func TestExplain_RequireDefinesMissingMethodIsAViolation(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		componentIntent("jobs", "app/jobs/**"),
		ruleIntentProps("jobs-perform", map[string]any{
			"require_defines": "jobs", "method": "perform", "because": "the queue calls perform on every job"}),
		classSymbol("SyncJob", "app/jobs/sync_job.rb"),
		classSymbol("ReportJob", "app/jobs/report_job.rb"),
		methodSymbol("ReportJob#perform", "app/jobs/report_job.rb"),
	)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if len(insights) != 1 {
		t.Fatalf("insights = %d, want exactly the missing definition: %+v", len(insights), insights)
	}
	got := insights[0]
	if want := "Constraint jobs-perform violated: SyncJob does not define perform"; got.Title != want {
		t.Errorf("title = %q, want %q", got.Title, want)
	}
	if got.Confidence != 1.0 {
		t.Errorf("confidence = %v, want 1.0: a declared rule's breach is decided, not estimated", got.Confidence)
	}
	if !strings.Contains(got.Description, "Because: the queue calls perform on every job") {
		t.Errorf("description must surface the rule's rationale, got: %q", got.Description)
	}
	if len(got.Evidence) != 1 || got.Evidence[0].File != "app/jobs/sync_job.rb" || got.Evidence[0].Symbol != "SyncJob" {
		t.Errorf("evidence = %+v, want the class's file and name", got.Evidence)
	}
}

func TestExplain_RequireDefinesBothQualifiedShapesComply(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		componentIntent("jobs", "app/jobs/**"),
		ruleIntentProps("jobs-perform", map[string]any{
			"require_defines": "jobs", "method": "perform", "because": "the queue calls perform on every job"}),
		classSymbol("InstanceJob", "app/jobs/instance_job.rb"),
		methodSymbol("InstanceJob#perform", "app/jobs/instance_job.rb"),
		classSymbol("ClassLevelJob", "app/jobs/class_level_job.rb"),
		methodSymbol("ClassLevelJob.perform", "app/jobs/class_level_job.rb"),
	)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if len(insights) != 0 {
		t.Fatalf("insights = %+v, want none: both the instance and class-level shapes define the method", insights)
	}
}

func TestExplain_RequireDefinesComposingClassesAreOutOfScope(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		componentIntent("jobs", "app/jobs/**"),
		ruleIntentProps("jobs-perform", map[string]any{
			"require_defines": "jobs", "method": "perform", "because": "the queue calls perform on every job"}),
		// Inherits: the definition could live on the superclass.
		facts.Fact{Kind: facts.KindSymbol, Name: "ChildJob", File: "app/jobs/child_job.rb",
			Props:     map[string]any{"symbol_kind": facts.SymbolClass},
			Relations: []facts.Relation{{Kind: facts.RelImplements, Target: "ApplicationJob"}}},
		// Includes a concern: the definition could ride the mixin.
		classSymbol("MixinJob", "app/jobs/mixin_job.rb"),
		facts.Fact{Kind: facts.KindDependency, Name: "MixinJob -> Performable", File: "app/jobs/mixin_job.rb",
			Props:     map[string]any{"mixin_kind": "include"},
			Relations: []facts.Relation{{Kind: facts.RelImplements, Target: "Performable"}}},
		// Not a class at all: the form ranges over class members only.
		methodSymbol("helper#run", "app/jobs/helper.rb"),
	)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if len(insights) != 0 {
		t.Fatalf("insights = %+v, want none: composition the check cannot see through fails closed", insights)
	}
}

// TestExplain_RequireDefinesVerdictsStructKinds pins the kinds this form ranges
// over. Go, Rust, C++ and C# name the owner of a method "struct", and while the
// gate admitted only "class" the form resolved its members on those repositories,
// verdicted nothing and said nothing — a rule holding because it looked at
// nothing, which is the one outcome this vocabulary must never produce.
func TestExplain_RequireDefinesVerdictsStructKinds(t *testing.T) {
	structSymbol := func(name, file string) facts.Fact {
		return facts.Fact{Kind: facts.KindSymbol, Name: name, File: file,
			Props: map[string]any{"symbol_kind": facts.SymbolStruct}}
	}
	store := facts.NewStore()
	store.Add(
		componentIntent("stores", "customers/**"),
		ruleIntentProps("stores-erase", map[string]any{
			"require_defines": "stores", "method": "Erase", "because": "a subject may demand erasure"}),
		structSymbol("customers.ProfileStore", "customers/store.go"),
		methodSymbol("customers.ProfileStore.Erase", "customers/store.go"),
		structSymbol("customers.AuditStore", "customers/audit.go"),
		methodSymbol("customers.AuditStore.Record", "customers/audit.go"),
		// Embedding is Go's composition, and the extractor writes it as an
		// implements relation: out of scope, the same as an inheriting class.
		facts.Fact{Kind: facts.KindSymbol, Name: "customers.EmbeddingStore", File: "customers/embed.go",
			Props:     map[string]any{"symbol_kind": facts.SymbolStruct},
			Relations: []facts.Relation{{Kind: facts.RelImplements, Target: "customers.Base"}}},
		// An interface declares signatures rather than defining them.
		facts.Fact{Kind: facts.KindSymbol, Name: "customers.Eraser", File: "customers/iface.go",
			Props: map[string]any{"symbol_kind": facts.SymbolInterface}},
	)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if len(insights) != 1 {
		t.Fatalf("insights = %d, want exactly the struct with no Erase: %+v", len(insights), insights)
	}
	if want := "Constraint stores-erase violated: customers.AuditStore does not define Erase"; insights[0].Title != want {
		t.Errorf("title = %q, want %q", insights[0].Title, want)
	}
	if insights[0].Confidence != 1.0 {
		t.Errorf("confidence = %v, want 1.0", insights[0].Confidence)
	}
}

func TestExplain_RequireDefinesIsDeterministic(t *testing.T) {
	build := func() *facts.Store {
		store := facts.NewStore()
		store.Add(
			componentIntent("jobs", "app/jobs/**"),
			ruleIntentProps("jobs-perform", map[string]any{
				"require_defines": "jobs", "method": "perform", "because": "the queue calls perform on every job"}),
			classSymbol("BravoJob", "app/jobs/bravo_job.rb"),
			classSymbol("AlphaJob", "app/jobs/alpha_job.rb"),
			classSymbol("DoneJob", "app/jobs/done_job.rb"),
			methodSymbol("DoneJob#perform", "app/jobs/done_job.rb"),
		)
		return store
	}
	first, err := New().Explain(context.Background(), build())
	if err != nil {
		t.Fatal(err)
	}
	second, err := New().Explain(context.Background(), build())
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 {
		t.Fatalf("insights = %d, want the two undefined classes: %+v", len(first), first)
	}
	if !reflect.DeepEqual(first, second) {
		t.Errorf("two runs over one store diverged:\n%+v\n%+v", first, second)
	}
	if !sortedByTitle(first) {
		t.Errorf("insights are not title-sorted: %+v", first)
	}
}

func TestExplain_RequireNameOutsideConventionIsAViolation(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		componentIntent("jobs", "app/jobs/**"),
		ruleIntentProps("jobs-named-job", map[string]any{
			"require_name": "jobs", "pattern": "*Job",
			"because": "the scheduler discovers jobs by their suffix"}),
		classSymbol("SyncJob", "app/jobs/sync_job.rb"),
		classSymbol("Reporter", "app/jobs/reporter.rb"),
	)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if len(insights) != 1 {
		t.Fatalf("insights = %d, want exactly the misnamed member: %+v", len(insights), insights)
	}
	got := insights[0]
	if want := "Constraint jobs-named-job violated: Reporter does not match *Job"; got.Title != want {
		t.Errorf("title = %q, want %q", got.Title, want)
	}
	if got.Confidence != 1.0 {
		t.Errorf("confidence = %v, want 1.0: a declared rule's breach is decided, not estimated", got.Confidence)
	}
	if !strings.Contains(got.Description, "Because: the scheduler discovers jobs by their suffix") {
		t.Errorf("description must surface the rule's rationale, got: %q", got.Description)
	}
}

func TestExplain_RequireNameBoundedDialectMatches(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		componentIntent("jobs", "app/jobs/**"),
		componentIntent("admin", "app/admin/**"),
		componentIntent("config", "app/config/**"),
		ruleIntentProps("jobs-suffix", map[string]any{
			"require_name": "jobs", "pattern": "*Job", "because": "suffix discovery"}),
		ruleIntentProps("admin-prefix", map[string]any{
			"require_name": "admin", "pattern": "Admin*", "because": "prefix namespacing"}),
		ruleIntentProps("config-exact", map[string]any{
			"require_name": "config", "pattern": "Settings", "because": "one well-known name"}),
		classSymbol("SyncJob", "app/jobs/sync_job.rb"),
		classSymbol("AdminUsers", "app/admin/users.rb"),
		classSymbol("Settings", "app/config/settings.rb"),
		// Outside every component: a convention rule binds members only.
		classSymbol("Anything", "app/other/anything.rb"),
	)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if len(insights) != 0 {
		t.Fatalf("insights = %+v, want none: every member matches its convention and non-members are out of scope", insights)
	}
}

func TestExplain_RequireNameIsDeterministic(t *testing.T) {
	build := func() *facts.Store {
		store := facts.NewStore()
		store.Add(
			componentIntent("jobs", "app/jobs/**"),
			ruleIntentProps("jobs-named-job", map[string]any{
				"require_name": "jobs", "pattern": "*Job", "because": "suffix discovery"}),
			classSymbol("Reporter", "app/jobs/reporter.rb"),
			classSymbol("Cleaner", "app/jobs/cleaner.rb"),
			classSymbol("SyncJob", "app/jobs/sync_job.rb"),
		)
		return store
	}
	first, err := New().Explain(context.Background(), build())
	if err != nil {
		t.Fatal(err)
	}
	second, err := New().Explain(context.Background(), build())
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 {
		t.Fatalf("insights = %d, want the two misnamed members: %+v", len(first), first)
	}
	if !reflect.DeepEqual(first, second) {
		t.Errorf("two runs over one store diverged:\n%+v\n%+v", first, second)
	}
	if !sortedByTitle(first) {
		t.Errorf("insights are not title-sorted: %+v", first)
	}
}

func mixinDependency(source, mixin, file string) facts.Fact {
	return facts.Fact{Kind: facts.KindDependency, Name: source + " -> " + mixin, File: file,
		Props:     map[string]any{"mixin_kind": "include"},
		Relations: []facts.Relation{{Kind: facts.RelImplements, Target: mixin}}}
}

func TestExplain_ForbidViaImplementsSeesAnIncludeEdge(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		componentIntent("services", "app/services/**"),
		componentIntent("model-concerns", "lib/concerns/**"),
		ruleIntentProps("no-model-concerns-in-services", map[string]any{
			"forbid": "services", "to": "model-concerns", "via": "implements",
			"because": "model concerns carry persistence assumptions services must not inherit"}),
		facts.Fact{Kind: facts.KindSymbol, Name: "Auditable", File: "lib/concerns/auditable.rb",
			Props: map[string]any{"symbol_kind": "module"}},
		classSymbol("SyncService", "app/services/sync_service.rb"),
		mixinDependency("SyncService", "Auditable", "app/services/sync_service.rb"),
	)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if len(insights) != 1 {
		t.Fatalf("insights = %d, want exactly the include edge: %+v", len(insights), insights)
	}
	got := insights[0]
	// The source name is the mixin carrier's own canonical name — the Ruby
	// extractor names an include's dependency fact "includer -> mixin", so the
	// title reads source-name -> target, same as the imports carriers.
	if want := "Constraint no-model-concerns-in-services violated: SyncService -> Auditable via implements"; got.Title != want {
		t.Errorf("title = %q, want %q", got.Title, want)
	}
	if got.Confidence != 1.0 {
		t.Errorf("confidence = %v, want 1.0: a declared rule's breach is decided, not estimated", got.Confidence)
	}
}

func TestExplain_ProtectViaImplementsScopesWhoMayInclude(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		componentIntent("model-concerns", "lib/concerns/**"),
		componentIntent("models", "app/models/**"),
		componentIntent("services", "app/services/**"),
		ruleIntentProps("only-models-include", map[string]any{
			"protect": "model-concerns", "owners": "models", "via": "implements",
			"because": "model concerns assume an ActiveRecord includer"}),
		facts.Fact{Kind: facts.KindSymbol, Name: "Auditable", File: "lib/concerns/auditable.rb",
			Props: map[string]any{"symbol_kind": "module"}},
		classSymbol("User", "app/models/user.rb"),
		mixinDependency("User", "Auditable", "app/models/user.rb"),
		classSymbol("SyncService", "app/services/sync_service.rb"),
		mixinDependency("SyncService", "Auditable", "app/services/sync_service.rb"),
	)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if len(insights) != 1 {
		t.Fatalf("insights = %d, want only the service's include: %+v", len(insights), insights)
	}
	if want := "Constraint only-models-include violated: SyncService -> Auditable via implements"; insights[0].Title != want {
		t.Errorf("title = %q, want %q", insights[0].Title, want)
	}
}

func TestExplain_ImplementsTargetResolutionFailsClosed(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		componentIntent("services", "app/services/**"),
		componentIntent("model-concerns", "lib/concerns/**"),
		ruleIntentProps("no-model-concerns-in-services", map[string]any{
			"forbid": "services", "to": "model-concerns", "via": "implements",
			"because": "model concerns carry persistence assumptions services must not inherit"}),
		facts.Fact{Kind: facts.KindSymbol, Name: "Auditable", File: "lib/concerns/auditable.rb",
			Props: map[string]any{"symbol_kind": "module"}},
		classSymbol("SyncService", "app/services/sync_service.rb"),
		// The include names a constant no measured fact carries: membership in
		// the to component cannot be proven, so there is no violation.
		mixinDependency("SyncService", "Elsewhere::Auditable", "app/services/sync_service.rb"),
	)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if len(insights) != 0 {
		t.Fatalf("insights = %+v, want none: an unresolvable mixin target must never be guessed into a breach", insights)
	}
}

func TestExplain_ImplementsRulesAreDeterministic(t *testing.T) {
	build := func() *facts.Store {
		store := facts.NewStore()
		store.Add(
			componentIntent("services", "app/services/**"),
			componentIntent("model-concerns", "lib/concerns/**"),
			ruleIntentProps("no-model-concerns-in-services", map[string]any{
				"forbid": "services", "to": "model-concerns", "via": "implements",
				"because": "model concerns carry persistence assumptions services must not inherit"}),
			facts.Fact{Kind: facts.KindSymbol, Name: "Auditable", File: "lib/concerns/auditable.rb",
				Props: map[string]any{"symbol_kind": "module"}},
			facts.Fact{Kind: facts.KindSymbol, Name: "Searchable", File: "lib/concerns/searchable.rb",
				Props: map[string]any{"symbol_kind": "module"}},
			classSymbol("SyncService", "app/services/sync_service.rb"),
			mixinDependency("SyncService", "Auditable", "app/services/sync_service.rb"),
			mixinDependency("SyncService", "Searchable", "app/services/sync_service.rb"),
		)
		return store
	}
	first, err := New().Explain(context.Background(), build())
	if err != nil {
		t.Fatal(err)
	}
	second, err := New().Explain(context.Background(), build())
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 {
		t.Fatalf("insights = %d, want both include edges: %+v", len(first), first)
	}
	if !reflect.DeepEqual(first, second) {
		t.Errorf("two runs over one store diverged:\n%+v\n%+v", first, second)
	}
	if !sortedByTitle(first) {
		t.Errorf("insights are not title-sorted: %+v", first)
	}
}

// TestExplain_GuidanceNotifyEmitsNothing: notify is the quiet channel — the
// guidance exists only in the pre-edit contract, and a populated component
// with a notify guidance rule produces no finding at all.
func TestExplain_GuidanceNotifyEmitsNothing(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		componentIntent("components", "app/components/**"),
		ruleIntentProps("getters-cached", map[string]any{
			"guide": "components", "mode": "notify",
			"message":   "Expensive derived getters here use @cached — consider it (see exemplars)",
			"exemplars": "app/components/sortable-table.js",
			"because":   "recomputing derived state on every render is the recurring perf bug here"}),
		facts.Fact{Kind: facts.KindModule, Name: "app/components/avatar-stack", File: "app/components/avatar-stack.js"},
		facts.Fact{Kind: facts.KindModule, Name: "app/components/sortable-table", File: "app/components/sortable-table.js"},
	)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if len(insights) != 0 {
		t.Fatalf("insights = %+v, want none: notify guidance never emits a finding", insights)
	}
}

// TestExplain_GuidanceAdvisoryIsOneFindingPerComponent: advisory guidance is
// one 0.9 finding per guided component — never one per member, because
// guidance is not a violation census — carrying the message and the sorted
// exemplars, below the gate's floor so it can fail nothing.
func TestExplain_GuidanceAdvisoryIsOneFindingPerComponent(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		componentIntent("components", "app/components/**"),
		ruleIntentProps("getters-cached", map[string]any{
			"guide": "components", "mode": "advisory",
			"message":   "Expensive derived getters here use @cached — consider it (see exemplars)",
			"exemplars": "app/components/sortable-table.js app/components/avatar-stack.js",
			"because":   "recomputing derived state on every render is the recurring perf bug here"}),
		facts.Fact{Kind: facts.KindModule, Name: "app/components/avatar-stack", File: "app/components/avatar-stack.js"},
		facts.Fact{Kind: facts.KindModule, Name: "app/components/sortable-table", File: "app/components/sortable-table.js"},
		facts.Fact{Kind: facts.KindModule, Name: "app/components/filter-bar", File: "app/components/filter-bar.js"},
	)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if len(insights) != 1 {
		t.Fatalf("insights = %d, want exactly 1 for the component, never one per member: %+v", len(insights), insights)
	}
	got := insights[0]
	if got.Title != "Guidance for components: getters-cached" {
		t.Errorf("title = %q", got.Title)
	}
	if got.Confidence != advisoryConfidence {
		t.Errorf("confidence = %v, want %v — guidance rides the report and fails nothing", got.Confidence, advisoryConfidence)
	}
	if !strings.Contains(got.Description, "Expensive derived getters here use @cached") {
		t.Errorf("description must carry the message, got %q", got.Description)
	}
	if !strings.Contains(got.Description, "app/components/avatar-stack.js, app/components/sortable-table.js") {
		t.Errorf("description must list the exemplars sorted, got %q", got.Description)
	}
}

// TestExplain_GuidanceIsDeterministic: two runs over one store agree byte for
// byte, and the guidance finding sorts into the listing like any other.
func TestExplain_GuidanceIsDeterministic(t *testing.T) {
	build := func() *facts.Store {
		store := facts.NewStore()
		store.Add(
			componentIntent("components", "app/components/**"),
			componentIntent("ghost", "app/ghost/**"),
			ruleIntentProps("getters-cached", map[string]any{
				"guide": "components", "mode": "advisory",
				"message":   "Expensive derived getters here use @cached",
				"exemplars": "app/components/sortable-table.js app/components/avatar-stack.js",
				"because":   "the recurring perf bug"}),
			facts.Fact{Kind: facts.KindModule, Name: "app/components/sortable-table", File: "app/components/sortable-table.js"},
		)
		return store
	}
	first, err := New().Explain(context.Background(), build())
	if err != nil {
		t.Fatal(err)
	}
	second, err := New().Explain(context.Background(), build())
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 {
		t.Fatalf("insights = %d, want the guidance finding + the dead-selector advisory: %+v", len(first), first)
	}
	if !reflect.DeepEqual(first, second) {
		t.Errorf("two runs over one store diverged:\n%+v\n%+v", first, second)
	}
	if !sortedByTitle(first) {
		t.Errorf("insights are not title-sorted: %+v", first)
	}
}

// violationTitles keeps a test's assertion on the verdicts a rule reached,
// separately from the advisories that report what it could not reach.
func violationTitles(insights []facts.Insight) []string {
	var out []string
	for _, in := range insights {
		if strings.Contains(in.Title, " violated: ") {
			out = append(out, in.Title)
		}
	}
	return out
}
