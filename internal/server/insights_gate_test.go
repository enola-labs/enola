package server

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/config"
	"github.com/enola-labs/enola/internal/explainers/deadmethods"
	"github.com/enola-labs/enola/internal/explainers/entrypoints"
	"github.com/enola-labs/enola/internal/explainers/finalinherit"
	"github.com/enola-labs/enola/internal/explainers/importclosure"
	"github.com/enola-labs/enola/internal/explainers/messagingcoverage"
	"github.com/enola-labs/enola/internal/explainers/packwerk"
	"github.com/enola-labs/enola/internal/explainers/queryloops"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/pkg/plugin"
)

// inputGatedExplainers is every explainer whose no-match answer can name its missing
// input, with the real explainer to check that answer against.
var inputGatedExplainers = map[string]plugin.Explainer{
	"import-closure":     importclosure.New(),
	"dead-methods":       deadmethods.New(),
	"query-loops":        queryloops.New(),
	"entry-points":       entrypoints.New(),
	"messaging-coverage": messagingcoverage.New(),
	"final-inheritance":  finalinherit.New(),
	"packwerk":           packwerk.New(),
}

// withoutInputs holds code none of the input-gated explainers reads: a TypeScript
// class and a module with no packwerk declaration.
func withoutInputs() *facts.Store {
	store := facts.NewStore()
	store.Add(
		facts.Fact{Kind: facts.KindModule, Name: "web", File: "web"},
		facts.Fact{Kind: facts.KindSymbol, Name: "web.Page", File: "web/page.ts",
			Props: map[string]any{"language": "typescript", "symbol_kind": facts.SymbolClass}},
	)
	return store
}

// withInput holds the one input name's explainer reads, and nothing it would report.
func withInput(name string) *facts.Store {
	store := withoutInputs()
	switch name {
	case "import-closure":
		store.Add(
			facts.Fact{Kind: facts.KindModule, Name: "app", File: "app/__init__.py"},
			facts.Fact{Kind: facts.KindModule, Name: "app.util", File: "app/util.py"},
			facts.Fact{Kind: facts.KindDependency, Name: "app -> app.util", File: "app/__init__.py",
				Props:     map[string]any{"language": "python", facts.PropSource: facts.DepSourceInternal},
				Relations: []facts.Relation{{Kind: facts.RelImports, Target: "app/util"}}},
		)
	case "dead-methods":
		store.Add(facts.Fact{Kind: facts.KindSymbol, Name: "User#name", File: "app/models/user.rb",
			Props: map[string]any{"language": "ruby", "symbol_kind": "method"}})
	case "query-loops":
		store.Add(facts.Fact{Kind: facts.KindSymbol, Name: "svc.Run", File: "svc/run.go",
			Props: map[string]any{"language": "go", "symbol_kind": facts.SymbolFunc}})
	case "entry-points":
		store.Add(facts.Fact{Kind: facts.KindSymbol, Name: "Jobs.Run", File: "jobs/run.go",
			Props: map[string]any{"language": "go", facts.PropFrameworkRoot: "job"}})
	case "messaging-coverage":
		store.Add(facts.Fact{Kind: facts.KindStorage, Name: "orders", File: "asyncapi.yaml",
			Props: map[string]any{facts.PropMessagingImplementationStatus: "implemented"}})
	case "final-inheritance":
		store.Add(facts.Fact{Kind: facts.KindSymbol, Name: "Base", File: "Base.kt",
			Props: map[string]any{"final": true, "symbol_kind": facts.SymbolClass}})
	case "packwerk":
		store.Add(facts.Fact{Kind: facts.KindModule, Name: "packs/orders", File: "packs/orders",
			Props: map[string]any{"packwerk": true}})
	}
	return store
}

var otherInsight = &facts.Snapshot{Insights: []facts.Insight{{Title: "gc", Source: "god-class", Confidence: 1}}}

// An explainer whose input the snapshot lacks did not look at anything, and the
// answer must say which input is missing instead of reading like a filter mismatch.
func TestNoMatchInsightsMessage_InputMissing(t *testing.T) {
	for name := range inputGatedExplainers {
		msg := noMatchInsightsMessage(name, "", 0, otherInsight, withoutInputs())
		if strings.Contains(msg, "No insights matched") || !strings.Contains(msg, "had nothing to analyze") {
			t.Errorf("%s with no input: want a had-nothing-to-analyze answer, got %q", name, msg)
		}
	}
}

// With its input present the explainer ran over it, so an empty answer is a genuine
// no-match.
func TestNoMatchInsightsMessage_InputPresent(t *testing.T) {
	for name := range inputGatedExplainers {
		msg := noMatchInsightsMessage(name, "", 0, otherInsight, withInput(name))
		if !strings.Contains(msg, "No insights matched") {
			t.Errorf("%s with its input present: want a genuine no-match, got %q", name, msg)
		}
	}
}

// An explainer that produced findings, all filtered out here, had input whatever the
// gate says: the answer stays a filter mismatch.
func TestNoMatchInsightsMessage_ProducedThenFiltered(t *testing.T) {
	snap := &facts.Snapshot{Insights: []facts.Insight{{Title: "ic", Source: "import-closure", Confidence: 0.4}}}
	msg := noMatchInsightsMessage("import-closure", "", 0.9, snap, withoutInputs())
	if !strings.Contains(msg, "No insights matched") {
		t.Errorf("findings exist below the bar: want a filter mismatch, got %q", msg)
	}
}

// The gate is a necessary condition of each explainer, never a stricter one: on the
// store the gate calls empty the real explainer reports nothing, and on the store it
// calls non-empty the gate agrees. Every gated name is one this build ships.
func TestExplainerInputGatesAgreeWithExplainers(t *testing.T) {
	if len(explainerInputs) != len(inputGatedExplainers) {
		t.Fatalf("explainerInputs has %d entries, this test checks %d", len(explainerInputs), len(inputGatedExplainers))
	}
	for name, e := range inputGatedExplainers {
		if !slices.Contains(config.KnownExplainers, name) {
			t.Errorf("%q is gated but config.KnownExplainers does not ship it", name)
		}
		if e.Name() != name {
			t.Errorf("explainer for %q is named %q", name, e.Name())
		}
		gate, ok := explainerInputs[name]
		if !ok {
			t.Errorf("%q has no entry in explainerInputs", name)
			continue
		}
		if gate.has(withoutInputs()) {
			t.Errorf("%s gate finds input in a store that has none", name)
		}
		if !gate.has(withInput(name)) {
			t.Errorf("%s gate misses the input its explainer reads", name)
		}
		got, err := e.Explain(context.Background(), withoutInputs())
		if err != nil || len(got) != 0 {
			t.Errorf("%s reported %d insight(s), err %v, on a store its gate calls empty", name, len(got), err)
		}
	}
}
