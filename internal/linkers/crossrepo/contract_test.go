package crossrepo

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
	httpsignal "github.com/enola-labs/enola/internal/linkers/crossrepo/signals/http"
	importsignal "github.com/enola-labs/enola/internal/linkers/crossrepo/signals/imports"
	kafkasignal "github.com/enola-labs/enola/internal/linkers/crossrepo/signals/kafka"
	sharedcodesignal "github.com/enola-labs/enola/internal/linkers/crossrepo/signals/sharedcode"
	"github.com/enola-labs/enola/internal/linkers/vocab"
	"github.com/enola-labs/enola/pkg/plugin"
)

// contractFixture exercises all four signals at once: an HTTP call, an import, a Kafka
// topic, and a shared type surface — including a pair whose only relationship is shared
// code, which is the case the symmetric phase has to get right.
func contractFixture() []facts.Fact {
	sym := func(repo, name, file string) facts.Fact {
		return facts.Fact{Kind: facts.KindSymbol, Name: name, Repo: repo, File: file,
			Props: map[string]any{"symbol_kind": facts.SymbolClass, "language": "ruby"}}
	}
	return []facts.Fact{
		// HTTP: web calls an endpoint api serves.
		{Kind: facts.KindRoute, Name: "/api/items/{id}", Repo: "web",
			Props: map[string]any{"method": "GET", facts.PropRole: facts.RoleClient,
				facts.PropSource: facts.RouteSourceTSHTTPClient}},
		{Kind: facts.KindRoute, Name: "/api/items/{id}", Repo: "api",
			Props: map[string]any{"method": "GET", facts.PropRole: facts.RoleServer}},
		// Import: web imports a module named after repo "shared".
		{Kind: facts.KindModule, Name: "src", Repo: "web"},
		{Kind: facts.KindDependency, Name: "web -> shared/utils", Repo: "web",
			Relations: []facts.Relation{{Kind: facts.RelImports, Target: "shared/utils"}}},
		{Kind: facts.KindModule, Name: "utils", Repo: "shared"},
		// Kafka: api consumes a topic owned by "orders".
		{Kind: facts.KindStorage, Name: "orders.placed", Repo: "api",
			Props: map[string]any{"storage_kind": facts.StorageKindTopic, "messaging": "kafka"}},
		{Kind: facts.KindModule, Name: "cmd", Repo: "orders"},
		// Shared code ON TOP of a directional edge: web already imports shared, and the
		// two also declare the same distinctive types. This pair is what makes the phase
		// split observable — the symmetric signal must see the import direction and
		// ANNOTATE that edge. Run it before the directional signals and it sees no
		// direction, and records a standalone coupling instead.
		sym("web", "InvoiceBuilder", "web/app/invoice_builder.rb"),
		sym("shared", "InvoiceBuilder", "shared/app/invoice_builder.rb"),
		sym("web", "LedgerPoster", "web/app/ledger_poster.rb"),
		sym("shared", "LedgerPoster", "shared/app/ledger_poster.rb"),
		sym("web", "DunningPolicy", "web/app/dunning_policy.rb"),
		sym("shared", "DunningPolicy", "shared/app/dunning_policy.rb"),
		// Shared code with NO direction anywhere: two otherwise-unrelated repos. This
		// pair covers the other branch — a coupling, never a fabricated edge.
		sym("alpha", "WidgetRegistry", "alpha/app/widget_registry.rb"),
		sym("beta", "WidgetRegistry", "beta/app/widget_registry.rb"),
		sym("alpha", "PaymentLedger", "alpha/app/payment_ledger.rb"),
		sym("beta", "PaymentLedger", "beta/app/payment_ledger.rb"),
		sym("alpha", "RetryPolicy", "alpha/app/retry_policy.rb"),
		sym("beta", "RetryPolicy", "beta/app/retry_policy.rb"),
	}
}

func runWith(order []plugin.CrossRepoSignal) []byte {
	out := ComputeLinks(contractFixture(), nil, order, vocab.Default(), 1)
	b, _ := json.Marshal(out)
	return b
}

// TestSignals_OutputIsIndependentOfRegistrationOrder is the load-bearing test for the
// CrossRepoSignal contract.
//
// Signals run as a plain loop over a registry, so if any two interacted the emitted
// facts would depend on registration order — and facts.jsonl is hashed into the
// snapshot ID, so an unchanged tree would produce a different snapshot depending on how
// the engine happened to be wired. Requiring byte-identical output across a reversed
// order is what makes "order within a phase carries no meaning" a checked property.
//
// It also covers the phase split: the symmetric signal reads the direction the
// directional ones established, so if phases were not ordered, reversing would move a
// shared-code annotation onto or off an edge.
func TestSignals_OutputIsIndependentOfRegistrationOrder(t *testing.T) {
	forward := []plugin.CrossRepoSignal{
		httpsignal.New(vocab.Default()), importsignal.New(), kafkasignal.New(vocab.Default()), sharedcodesignal.New(vocab.Default()),
	}
	reverse := []plugin.CrossRepoSignal{
		sharedcodesignal.New(vocab.Default()), kafkasignal.New(vocab.Default()), importsignal.New(), httpsignal.New(vocab.Default()),
	}

	a, b := runWith(forward), runWith(reverse)
	if !reflect.DeepEqual(a, b) {
		t.Errorf("signal output depends on registration order\n--- forward ---\n%s\n--- reverse ---\n%s", a, b)
	}
}

// TestComputeLinks_RepeatedRunsAreStable pins that linking is a pure function of its
// inputs: the engine recomputes it from scratch on every append, so two runs over the
// same facts must be identical.
func TestComputeLinks_RepeatedRunsAreStable(t *testing.T) {
	order := []plugin.CrossRepoSignal{
		httpsignal.New(vocab.Default()), importsignal.New(), kafkasignal.New(vocab.Default()), sharedcodesignal.New(vocab.Default()),
	}
	if a, b := runWith(order), runWith(order); !reflect.DeepEqual(a, b) {
		t.Errorf("two identical runs differ\n--- a ---\n%s\n--- b ---\n%s", a, b)
	}
}

// TestMaterialize_PropSurfaceIsStable pins the QUERYABLE SURFACE of a cross-repo fact.
//
// Materialization used to write each signal's props from a hardcoded block; it now
// writes whatever buckets the signals declared. That is the point of the refactor, and
// also its risk: a mistyped Bucket field would silently rename a prop, and
// query_facts(kind=dependency) would start returning something else. A compile error
// became a registration mistake, so this test replaces the compiler.
//
// The expected set is written out literally rather than derived from plugin.Bucket*,
// because deriving it from the same constants the code uses would assert nothing.
//
// The fixture covers a SUPERSET of what the golden corpus produces (which carries only
// confidence/endpoints/topics/via today), so the props no fixture repo happens to
// exercise — imports, shared symbols — are pinned here rather than nowhere.
// "name_match_count" is deliberately absent: it appears only when source verification
// narrows a shared-symbol set, and this fixture supplies no SourceReader.
func TestMaterialize_PropSurfaceIsStable(t *testing.T) {
	out := ComputeLinks(contractFixture(), nil, []plugin.CrossRepoSignal{
		httpsignal.New(vocab.Default()), importsignal.New(), kafkasignal.New(vocab.Default()), sharedcodesignal.New(vocab.Default()),
	}, vocab.Default(), 1)

	seen := map[string]map[string]bool{
		facts.KindService:    {},
		facts.KindDependency: {},
	}
	for _, f := range out {
		for k := range f.Props {
			seen[f.Kind][k] = true
		}
	}

	want := map[string][]string{
		facts.KindService: {"edge_coverage", "synthetic"},
		facts.KindDependency: {
			"confidence", "endpoint_count", "endpoints",
			"import_count", "import_samples",
			"repos", "symbol_count", "symbol_samples",
			"synthetic", "topic_count", "topic_samples",
			"type", "via",
		},
	}
	for kind, wantProps := range want {
		got := make([]string, 0, len(seen[kind]))
		for k := range seen[kind] {
			got = append(got, k)
		}
		sort.Strings(got)
		if !reflect.DeepEqual(got, wantProps) {
			t.Errorf("%s prop surface changed:\n got  %v\n want %v\n"+
				"This is the queryable contract — a rename here changes what "+
				"query_facts returns. Update the expectation only deliberately.", kind, got, wantProps)
		}
	}
}

// TestComputeLinks_SingleRepoIsNoOp pins the guard that keeps single-repo snapshots
// free of synthetic facts: with one repo there is nothing for an edge to run between,
// and emitting a lone service node would put a fact in the graph that means nothing.
func TestComputeLinks_SingleRepoIsNoOp(t *testing.T) {
	in := []facts.Fact{
		{Kind: facts.KindRoute, Name: "/api/items", Repo: "solo",
			Props: map[string]any{"method": "GET", facts.PropRole: facts.RoleClient}},
	}
	if out := ComputeLinks(in, nil, []plugin.CrossRepoSignal{httpsignal.New(vocab.Default())}, vocab.Default(), 1); len(out) != 0 {
		t.Errorf("single-repo snapshot produced %d synthetic facts, want 0: %+v", len(out), out)
	}
}

// TestVocabularyOverlay_SuppressesAFalseEdge is the user story the `linking:` block
// exists for, end to end.
//
// Two apps built on the same framework each declare that framework's boilerplate base
// classes. Nothing is shared between them — the names coincide because the framework
// generates them — but with the framework unknown to enola they look like shared code
// and get coupled. Before this overlay existed the only fix was a Go patch and a
// release, which is how every one of the built-in entries got there.
//
// The assertion is in two halves deliberately: the coupling must be present with the
// default vocabulary, and absent once the user names the convention. Only asserting the
// second half would pass against a fixture that never coupled at all.
func TestVocabularyOverlay_SuppressesAFalseEdge(t *testing.T) {
	sym := func(repo, name string) facts.Fact {
		return facts.Fact{Kind: facts.KindSymbol, Name: name, Repo: repo,
			File:  repo + "/app/" + name + ".ex",
			Props: map[string]any{"symbol_kind": facts.SymbolClass, "language": "elixir"}}
	}
	// Boilerplate every app of this (hypothetical) framework declares identically.
	boilerplate := []string{"AcmeWebEndpoint", "AcmeWebRouter", "AcmeWebTelemetry"}
	var in []facts.Fact
	for _, name := range boilerplate {
		in = append(in, sym("storefront", name), sym("admin", name))
	}

	signals := func(v *vocab.Set) []plugin.CrossRepoSignal {
		return []plugin.CrossRepoSignal{sharedcodesignal.New(v)}
	}

	// Half one: with the framework unknown, the two apps are coupled.
	before := sharedCodeFacts(ComputeLinks(in, nil, signals(vocab.Default()), vocab.Default(), 1))
	if len(before) != 1 {
		t.Fatalf("precondition: unknown framework boilerplate should couple the apps; got %d couplings", len(before))
	}

	// Half two: the user names the convention in enola.yaml. No code change, no release.
	tuned, err := vocab.Apply(&vocab.Overlay{
		FrameworkConventions: vocab.ListOverlay{Add: boilerplate},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	after := sharedCodeFacts(ComputeLinks(in, nil, signals(tuned), tuned, 1))
	if len(after) != 0 {
		t.Errorf("the overlay did not suppress the false coupling: %+v", after)
	}
}
