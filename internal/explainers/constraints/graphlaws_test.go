package constraints

import (
	"context"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func storage(model, file, table string) facts.Fact {
	return facts.Fact{Kind: facts.KindStorage, Name: model, File: file, Repo: "app", Props: map[string]any{"storage_kind": "model", "table": table, "language": "ruby"}}
}

func lawIntent(id string, props map[string]any) facts.Fact {
	base := map[string]any{"intent_kind": "rule", "rule": id, "because": "stated on the page", "source": "wiki/p.md"}
	for k, v := range props {
		base[k] = v
	}
	return facts.Fact{Kind: facts.KindIntent, Name: "rule: " + id, File: "wiki/p.md", Props: base}
}

func titlesOf(t *testing.T, store *facts.Store, needle string) []string {
	t.Helper()
	got, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, i := range got {
		if strings.Contains(i.Title, needle) {
			titles = append(titles, i.Title)
		}
	}
	return titles
}

// Billing reaches its own invoices table and the orders table another part
// owns; only the second is a breach, and the cut names the owner's public
// member that already reaches the model.
func TestStorageStaysHome_NamesTheTableOutsideThePart(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		componentIntent("billing", "app/billing/**"),
		facts.Fact{Kind: facts.KindIntent, Name: "component: orders", File: "wiki/p.md",
			Props: map[string]any{"intent_kind": "component", "component": "orders", "match": "app/orders/**", "public": "app/orders/public/**", "source": "wiki/p.md"}},
		lawIntent("billing-keeps-to-its-tables", map[string]any{"storage_stays_home": "billing"}),
		storage("Invoice", "app/billing/invoice.rb", "invoices"),
		storage("Order", "app/orders/order.rb", "orders"),
		facts.Fact{Kind: facts.KindSymbol, Name: "Orders::Public::Fulfilment#ship", File: "app/orders/public/fulfilment.rb", Repo: "app",
			Props:     map[string]any{"symbol_kind": "method", "language": "ruby"},
			Relations: []facts.Relation{{Kind: facts.RelCalls, Target: "Order.find"}}},
		facts.Fact{Kind: facts.KindSymbol, Name: "Billing::Charge#run", File: "app/billing/charge.rb", Repo: "app",
			Props:     map[string]any{"symbol_kind": "method", "language": "ruby"},
			Relations: []facts.Relation{{Kind: facts.RelCalls, Target: "Invoice.create"}, {Kind: facts.RelCalls, Target: "Order.update"}}},
	)
	got, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	var hits []facts.Insight
	for _, i := range got {
		if strings.Contains(i.Title, "billing-keeps-to-its-tables violated") {
			hits = append(hits, i)
		}
	}
	if len(hits) != 1 || !strings.Contains(hits[0].Title, "reaches table orders through Order") {
		t.Fatalf("want the one reach outside the part named with its table, got %+v", hits)
	}
	if !strings.Contains(hits[0].Actions[0], "Orders::Public::Fulfilment#ship") {
		t.Fatalf("the cut must name the owner's public member that already reaches the model, got %q", hits[0].Actions[0])
	}
}

func TestCapRuntime_RefusesWithoutACapture(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		componentIntent("controllers", "app/controllers/**"),
		lawIntent("frames-keep-their-budget", map[string]any{"cap_runtime": "controllers", "metric": "queries", "max": 20}),
		method("UsersController#index", "app/controllers/users_controller.rb"),
	)
	titles := titlesOf(t, store, "frames-keep-their-budget")
	if len(titles) != 1 || !strings.Contains(titles[0], "cannot be evaluated: no runtime capture") {
		t.Fatalf("want the named refusal and no verdict, got %v", titles)
	}
	store.Add(facts.Fact{Kind: facts.KindRoute, Name: "runtime-queries: app/controllers/users_controller.rb:index", File: "app/controllers/users_controller.rb", Repo: "app",
		Props: map[string]any{"resolution_level": "runtime-observed", "observed_via": "activesupport-notifications", "frame_label": "index", "queries": 31}})
	titles = titlesOf(t, store, "frames-keep-their-budget")
	if len(titles) != 1 || !strings.Contains(titles[0], "index issues 31 queries against a budget of 20") {
		t.Fatalf("with a capture the frame over budget is named, got %v", titles)
	}
}

// enola's spec runner keys every frame by the spec file, so a capture from it
// holds frames and none of them in application code. The budget used to skip
// every frame outside its members and emit nothing, which read as kept.
func TestCapRuntime_RefusesACaptureThatMissesThePart(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		componentIntent("services", "app/services/**"),
		lawIntent("services-keep-their-budget", map[string]any{"cap_runtime": "services", "metric": "queries", "max": 20}),
		method("ReportBuilder#call", "app/services/report_builder.rb"),
		facts.Fact{Kind: facts.KindDependency, Name: "runtime-queries: spec/services/report_builder_spec.rb:ReportBuilder sums every account", File: "spec/services/report_builder_spec.rb", Repo: "app",
			Props: map[string]any{"resolution_level": "runtime-observed", "observed_via": "query-counter", "frame_label": "ReportBuilder sums every account", "queries": 45}},
		facts.Fact{Kind: facts.KindDependency, Name: "runtime-queries: spec/models/account_spec.rb:Account is valid", File: "spec/models/account_spec.rb", Repo: "app",
			Props: map[string]any{"resolution_level": "runtime-observed", "observed_via": "query-counter", "frame_label": "Account is valid", "queries": 2}},
	)
	got, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	var hits []facts.Insight
	for _, i := range got {
		if strings.Contains(i.Title, "services-keep-their-budget") {
			hits = append(hits, i)
		}
	}
	if len(hits) != 1 || !strings.Contains(hits[0].Title, "cannot be evaluated: no frame in its members") {
		t.Fatalf("want the second named refusal and no verdict, got %+v", hits)
	}
	if !strings.Contains(hits[0].Evidence[0].Detail, "cause "+CauseNoFrameInMembers) {
		t.Fatalf("the refusal names its cause, got %q", hits[0].Evidence[0].Detail)
	}
	for _, want := range []string{"2 frame(s)", "spec/models/account_spec.rb", "spec/services/report_builder_spec.rb"} {
		if !strings.Contains(hits[0].Description, want) {
			t.Fatalf("the refusal names the frame count and the frames' files (%q), got %q", want, hits[0].Description)
		}
	}

	store.Add(facts.Fact{Kind: facts.KindDependency, Name: "runtime-queries: app/services/report_builder.rb:call", File: "app/services/report_builder.rb", Repo: "app",
		Props: map[string]any{"resolution_level": "runtime-observed", "observed_via": "query-counter", "frame_label": "call", "queries": 12}})
	if titles := titlesOf(t, store, "services-keep-their-budget"); len(titles) != 0 {
		t.Fatalf("a capture that reaches the part and keeps the budget reads as today, no verdict, got %v", titles)
	}
}

func TestRequireConsumer_RefusesWithoutACounterparty(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		facts.Fact{Kind: facts.KindIntent, Name: "component: api", File: "wiki/p.md",
			Props: map[string]any{"intent_kind": "component", "component": "api", "kind": "route", "match": "config/**", "source": "wiki/p.md"}},
		lawIntent("every-route-has-a-client", map[string]any{"require_consumer": "api"}),
		facts.Fact{Kind: facts.KindRoute, Name: "/api/v1/orders", File: "config/routes.rb", Repo: "app", Props: map[string]any{"method": "GET", "unmatched_by_clients": true}},
	)
	titles := titlesOf(t, store, "every-route-has-a-client")
	if len(titles) != 1 || !strings.Contains(titles[0], "no counterparty") {
		t.Fatalf("one repository cannot answer who consumes, got %v", titles)
	}
	store.Add(facts.Fact{Kind: facts.KindSymbol, Name: "fetchOrders", File: "src/api.ts", Repo: "web", Props: map[string]any{"symbol_kind": "function", "language": "typescript"}})
	titles = titlesOf(t, store, "every-route-has-a-client")
	if len(titles) != 1 || !strings.Contains(titles[0], "GET /api/v1/orders has no consumer") {
		t.Fatalf("with a client repository loaded the unmatched route is named, got %v", titles)
	}
}

func TestUniqueAcross_NamesBothOwners(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		facts.Fact{Kind: facts.KindIntent, Name: "component: tables", File: "wiki/p.md",
			Props: map[string]any{"intent_kind": "component", "component": "tables", "kind": "storage", "match": "app/models/**", "source": "wiki/p.md"}},
		lawIntent("one-owner-per-table", map[string]any{"unique_across": "tables", "by": "table"}),
		facts.Fact{Kind: facts.KindStorage, Name: "Candidate", File: "app/models/candidate.rb", Repo: "ats", Props: map[string]any{"table": "candidates"}},
		facts.Fact{Kind: facts.KindStorage, Name: "Person", File: "app/models/person.rb", Repo: "crm", Props: map[string]any{"table": "candidates"}},
		facts.Fact{Kind: facts.KindStorage, Name: "Deal", File: "app/models/deal.rb", Repo: "crm", Props: map[string]any{"table": "deals"}},
	)
	titles := titlesOf(t, store, "one-owner-per-table")
	if len(titles) != 1 || !strings.Contains(titles[0], "table candidates is owned by ats and crm") {
		t.Fatalf("want the shared table named with both owners and deals left alone, got %v", titles)
	}
}

func TestRequireGoverned_NamesTheUnanchoredFile(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		componentIntent("billing", "app/billing/**"),
		lawIntent("billing-is-governed", map[string]any{"require_governed": "billing"}),
		method("Billing::Charge#run", "app/billing/charge.rb"),
		method("Billing::Refund#run", "app/billing/refund.rb"),
	)
	titles := titlesOf(t, store, "billing-is-governed")
	if len(titles) != 1 || !strings.Contains(titles[0], "no compiled pages") {
		t.Fatalf("without pages the rule refuses by name, got %v", titles)
	}
	store.Add(
		facts.Fact{Kind: facts.KindIntent, Name: "page: wiki/app/adrs/charges.md", File: "wiki/app/adrs/charges.md", Repo: "docs", Props: map[string]any{"intent_kind": "page", "source": "wiki/app/adrs/charges.md", "status": "accepted"}},
		facts.Fact{Kind: facts.KindIntent, Name: "anchor: app app/billing/charge.rb", File: "wiki/app/adrs/charges.md", Repo: "docs",
			Props:     map[string]any{"intent_kind": "anchor", "source": "wiki/app/adrs/charges.md", "path": "app/billing/charge.rb"},
			Relations: []facts.Relation{{Kind: facts.RelDependsOn, Target: "app/app/billing/charge.rb"}}},
	)
	titles = titlesOf(t, store, "billing-is-governed")
	if len(titles) != 1 || !strings.Contains(titles[0], "app/billing/refund.rb has no governing page") {
		t.Fatalf("the anchored file passes and the other is named, got %v", titles)
	}
}

// governed_by selects the code a page anchors, and with status:superseded the
// code of the decisions a newer page replaced.
func TestGovernedBy_SelectsThePagesCode(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		facts.Fact{Kind: facts.KindIntent, Name: "component: old-way", File: "wiki/p.md",
			Props: map[string]any{"intent_kind": "component", "component": "old-way", "governed_by": "wiki/app/adrs/*.md status:superseded", "source": "wiki/p.md"}},
		facts.Fact{Kind: facts.KindIntent, Name: "component: new-way", File: "wiki/p.md",
			Props: map[string]any{"intent_kind": "component", "component": "new-way", "governed_by": "wiki/app/adrs/sync-v2.md", "source": "wiki/p.md"}},
		lawIntent("new-never-reaches-old", map[string]any{"forbid": "new-way", "to": "old-way", "via": "calls"}),
		facts.Fact{Kind: facts.KindIntent, Name: "page: wiki/app/adrs/sync-v1.md", Props: map[string]any{"intent_kind": "page", "source": "wiki/app/adrs/sync-v1.md", "status": "superseded"}},
		facts.Fact{Kind: facts.KindIntent, Name: "page: wiki/app/adrs/sync-v2.md", Props: map[string]any{"intent_kind": "page", "source": "wiki/app/adrs/sync-v2.md", "status": "accepted"}},
		facts.Fact{Kind: facts.KindIntent, Name: "anchor: app app/sync/legacy.rb", Props: map[string]any{"intent_kind": "anchor", "source": "wiki/app/adrs/sync-v1.md"}, Relations: []facts.Relation{{Kind: facts.RelDependsOn, Target: "app/app/sync/legacy.rb"}}},
		facts.Fact{Kind: facts.KindIntent, Name: "anchor: app app/sync/engine.rb", Props: map[string]any{"intent_kind": "anchor", "source": "wiki/app/adrs/sync-v2.md"}, Relations: []facts.Relation{{Kind: facts.RelDependsOn, Target: "app/app/sync/engine.rb"}}},
		method("Sync::Legacy#pull", "app/sync/legacy.rb"),
		facts.Fact{Kind: facts.KindSymbol, Name: "Sync::Engine#run", File: "app/sync/engine.rb", Repo: "app",
			Props:     map[string]any{"symbol_kind": "method", "language": "ruby"},
			Relations: []facts.Relation{{Kind: facts.RelCalls, Target: "Sync::Legacy#pull"}}},
	)
	titles := titlesOf(t, store, "new-never-reaches-old violated")
	if len(titles) != 1 || !strings.Contains(titles[0], "Sync::Engine#run -> Sync::Legacy#pull") {
		t.Fatalf("the superseding decision's code reaching the superseded one is the breach, got %v", titles)
	}
}

func TestHandles_SelectsTheCodeBehindMutatingRoutes(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		facts.Fact{Kind: facts.KindIntent, Name: "component: mutating-actions", File: "wiki/p.md",
			Props: map[string]any{"intent_kind": "component", "component": "mutating-actions", "match": "app/controllers/**", "handles": "DELETE PATCH POST PUT", "source": "wiki/p.md"}},
		componentIntent("policies", "app/policies/**"),
		lawIntent("mutations-are-authorized", map[string]any{"require_edge": "mutating-actions", "to": "policies", "via": "calls", "direction": "outbound"}),
		facts.Fact{Kind: facts.KindRoute, Name: "/orders", File: "config/routes.rb", Repo: "app", Props: map[string]any{"method": "POST"}, Relations: []facts.Relation{{Kind: facts.RelHandledBy, Target: "OrdersController#create"}}},
		facts.Fact{Kind: facts.KindRoute, Name: "/orders", File: "config/routes.rb", Repo: "app", Props: map[string]any{"method": "GET"}, Relations: []facts.Relation{{Kind: facts.RelHandledBy, Target: "OrdersController#index"}}},
		method("OrdersController#index", "app/controllers/orders_controller.rb"),
		method("OrdersController#create", "app/controllers/orders_controller.rb"),
		facts.Fact{Kind: facts.KindSymbol, Name: "OrderPolicy#create?", File: "app/policies/order_policy.rb", Repo: "app", Props: map[string]any{"symbol_kind": "method", "language": "ruby"}},
	)
	members, _ := resolveMembership(store, decodeComponent(store.ByName("component: mutating-actions")[0]))
	if !members["OrdersController#create"] || members["OrdersController#index"] {
		t.Fatalf("handles admits the POST handler and not the GET one, got %v", members)
	}
}
