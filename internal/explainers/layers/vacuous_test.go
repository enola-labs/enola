package layers

import (
	"context"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// The regression tests for issue #242's second half: a declaration that governs
// nothing must not report the same way as one in force.
//
// The first half — the host separator that CAUSED the empty classification — is
// pinned in internal/facts' path contract and by the Windows CI job. What is
// pinned here is the part that made it undiagnosable: whatever the reason a
// declaration selects nothing, the snapshot says so.

func findInsight(insights []facts.Insight, substr string) *facts.Insight {
	for i := range insights {
		if strings.Contains(insights[i].Title, substr) {
			return &insights[i]
		}
	}
	return nil
}

func explain(t *testing.T, ff ...facts.Fact) []facts.Insight {
	t.Helper()
	store := facts.NewStore()
	store.Add(ff...)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	return insights
}

// A layer order whose paths match no module gets its own finding. Before this, the
// only report was the digit `0` inside a finding titled "Architecture pattern:
// declared" at confidence 1.00 — which reads as a declaration in force.
func TestDeclaredLayers_OrderMatchingNothingIsReported(t *testing.T) {
	insights := explain(t,
		layerIntent("svc", "handlers", 0, "app/handlers/**"),
		layerIntent("svc", "domain", 1, "app/domain/**"),
		// The modules live under src/, not app/: nothing the declaration names exists.
		facts.Fact{Kind: facts.KindModule, Repo: "svc", Name: "src/handlers"},
		facts.Fact{Kind: facts.KindModule, Repo: "svc", Name: "src/domain"},
	)

	got := findInsight(insights, "classifies no modules")
	if got == nil {
		t.Fatalf("a declaration matching nothing must say so: %+v", insights)
	}
	if got.Informational {
		t.Error("the advisory is a finding, not a description: an informational one is routed to the never-graded section, which is where the silence came from")
	}
	// Reported but not gating. `--fail-on=layers` gates at a 1.00 floor, and the
	// snapshot that first declares a layer order is exactly when a mismatched path
	// shows up — failing there would fail the pull request that wrote the declaration.
	if got.Confidence >= 1.0 {
		t.Errorf("confidence %v would fail --fail-on=layers and break the PR that declares the order", got.Confidence)
	}
	if got.Confidence == 0 {
		t.Error("confidence 0 reads as no signal at all")
	}
	// The measured module paths are named, so the mismatch is visible without
	// going and querying the fact store by hand.
	var namesModule bool
	for _, e := range got.Evidence {
		if e.Fact == "src/handlers" {
			namesModule = true
		}
	}
	if !namesModule {
		t.Errorf("the advisory should show what WAS measured beside what was declared: %+v", got.Evidence)
	}
}

// One empty layer among populated ones is a different mistake — usually a directory
// that moved — and gets its own advisory naming that layer.
func TestDeclaredLayers_EmptyLayerAmongPopulatedOnesIsReported(t *testing.T) {
	insights := explain(t,
		layerIntent("svc", "handlers", 0, "app/handlers/**"),
		layerIntent("svc", "domain", 1, "app/domain/**"),
		layerIntent("svc", "legacy", 2, "app/legacy/**"),
		facts.Fact{Kind: facts.KindModule, Repo: "svc", Name: "app/handlers"},
		facts.Fact{Kind: facts.KindModule, Repo: "svc", Name: "app/domain"},
	)

	got := findInsight(insights, `"legacy" classifies no modules`)
	if got == nil {
		t.Fatalf("an empty layer beside populated ones must be named: %+v", insights)
	}
	if got.Confidence >= 1.0 {
		t.Errorf("confidence %v would gate", got.Confidence)
	}
	// The whole-order advisory must NOT also fire: the declaration is being read.
	if other := findInsight(insights, "layer order declared (svc) classifies no modules"); other != nil {
		t.Error("a populated declaration must not also report itself as classifying nothing")
	}
}

// The healthy case: no advisory at all. A check that fires on correct declarations
// is one people learn to skip, which is how the original silence went unnoticed.
func TestDeclaredLayers_FullyPopulatedOrderRaisesNoAdvisory(t *testing.T) {
	insights := explain(t,
		layerIntent("svc", "handlers", 0, "app/handlers/**"),
		layerIntent("svc", "domain", 1, "app/domain/**"),
		facts.Fact{Kind: facts.KindModule, Repo: "svc", Name: "app/handlers"},
		facts.Fact{Kind: facts.KindModule, Repo: "svc", Name: "app/domain"},
	)
	if got := findInsight(insights, "classifies no modules"); got != nil {
		t.Fatalf("every layer is populated; nothing to advise: %+v", got)
	}
}

// A declaration written on Windows carries backslashes, and it has to select the
// same code as the same file read on Linux. This is the reporter's workaround in
// issue #242 — they generated exact backslash paths to get anything to classify —
// working as it should, rather than by accident of which host reads the file.
func TestDeclaredLayers_BackslashDeclarationClassifies(t *testing.T) {
	insights := explain(t,
		layerIntent("svc", "handlers", 0, `app\handlers\**`),
		layerIntent("svc", "domain", 1, `app\domain`),
		facts.Fact{Kind: facts.KindModule, Repo: "svc", Name: "app/handlers"},
		facts.Fact{Kind: facts.KindModule, Repo: "svc", Name: "app/domain"},
		facts.Fact{Kind: facts.KindDependency, Repo: "svc", Name: "domain-imports-handlers",
			File:      "app/domain/thing.go",
			Relations: []facts.Relation{{Kind: facts.RelImports, Target: "app/handlers"}}},
	)

	pattern := findInsight(insights, "Architecture pattern: declared (svc)")
	if pattern == nil {
		t.Fatalf("no declared pattern: %+v", insights)
	}
	if !strings.Contains(pattern.Description, "2 classified modules") {
		t.Errorf("a backslash declaration must classify exactly as its forward-slash twin: %s", pattern.Description)
	}
	if got := findInsight(insights, "classifies no modules"); got != nil {
		t.Errorf("nothing is vacuous here: %+v", got)
	}
	// And the point of classifying at all: the gate has something to grade.
	if v := findInsight(insights, "Layer violation"); v == nil || v.Confidence != 1.0 {
		t.Fatalf("inner-imports-outer must be a proof-class violation: %+v", insights)
	}
}

// MemberCounts is what `constraints lint` prints, so it must agree with the
// explainer about the same declaration — including which layer is empty, and the
// order the author wrote them in.
func TestDeclaredLayers_MemberCountsMatchTheExplainer(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		layerIntent("svc", "handlers", 0, "app/handlers/**"),
		layerIntent("svc", "domain", 1, "app/domain/**"),
		layerIntent("svc", "legacy", 2, "app/legacy/**"),
		facts.Fact{Kind: facts.KindModule, Repo: "svc", Name: "app/handlers"},
		facts.Fact{Kind: facts.KindModule, Repo: "svc", Name: "app/domain"},
	)

	counts := MemberCounts(store)
	if len(counts) != 3 {
		t.Fatalf("want one row per declared layer, got %+v", counts)
	}
	// Outermost first — the order the declaration is written in.
	want := []struct {
		layer   string
		members int
	}{{"handlers", 1}, {"domain", 1}, {"legacy", 0}}
	for i, w := range want {
		if counts[i].Layer != w.layer || counts[i].Members != w.members {
			t.Errorf("row %d = %s/%d, want %s/%d", i, counts[i].Layer, counts[i].Members, w.layer, w.members)
		}
	}
	if got := ModuleNames(store, "svc"); len(got) != 2 || got[0] != "app/domain" {
		t.Errorf("ModuleNames = %v, want the measured module paths sorted", got)
	}
}
