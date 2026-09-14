package coverage

import (
	"context"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func clientAccount(repo, spec string, receivers, calls int) facts.Fact {
	return facts.Fact{Kind: facts.KindExtraction, Name: "typescript:client:" + spec, Repo: repo, File: repo,
		Props: map[string]any{facts.PropClientSpec: spec, "receivers": receivers,
			"edge_coverage": []map[string]any{{"edge_type": "configured_http_call", "detected": calls, "resolved": calls}}}}
}

func silentInsights(t *testing.T, ff ...facts.Fact) []facts.Insight {
	t.Helper()
	store := facts.NewStore()
	store.Add(ff...)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	var out []facts.Insight
	for _, in := range insights {
		if strings.HasPrefix(in.Title, "Declared client ") {
			out = append(out, in)
		}
	}
	return out
}

// Nothing found in every loaded repository, and no member of the type anywhere: the
// receiver types are what to check.
func TestExplain_SilentClientPointsAtReceiverTypes(t *testing.T) {
	got := silentInsights(t, clientAccount("gateway", "sdk-http", 0, 0), clientAccount("sdk", "sdk-http", 0, 0))
	if len(got) != 1 || got[0].Title != "Declared client sdk-http matched no call site" {
		t.Fatalf("want one silent-client insight, got %+v", got)
	}
	if !strings.Contains(got[0].Description, "receiver types") || !strings.Contains(got[0].Actions[0], "receiver_types") {
		t.Errorf("insight does not point at receiver_types: %+v", got[0])
	}
	if len(got[0].Evidence) != 2 {
		t.Errorf("want one evidence line per repository, got %+v", got[0].Evidence)
	}
}

// Members of the type exist but no declared method is called: the method names are what
// to check.
func TestExplain_SilentClientPointsAtMethodNames(t *testing.T) {
	got := silentInsights(t, clientAccount("sdk", "sdk-http", 2, 0))
	if len(got) != 1 || !strings.Contains(got[0].Description, "declared methods") ||
		!strings.Contains(got[0].Actions[0], "method names") {
		t.Fatalf("insight does not point at method names: %+v", got)
	}
}

// Finding nothing in one repository is ordinary: a server calls no client. Only a client
// silent in every loaded repository is reported.
func TestExplain_ClientFoundAnywhereIsNotSilent(t *testing.T) {
	if got := silentInsights(t, clientAccount("gateway", "sdk-http", 0, 0), clientAccount("sdk", "sdk-http", 1, 3)); len(got) != 0 {
		t.Fatalf("a client with call sites in one repository was reported silent: %+v", got)
	}
}
