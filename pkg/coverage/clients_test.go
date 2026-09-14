package coverage

import (
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func clientAccountFact(repo, spec string, receivers, calls, routes int, skipped string) facts.Fact {
	props := map[string]any{
		facts.PropClientSpec: spec,
		"receivers":          receivers,
		"edge_coverage": []map[string]any{{
			"edge_type": "configured_http_call", "detected": calls, "resolved": routes, "unresolved": calls - routes,
		}},
	}
	if skipped != "" {
		props["skipped"] = skipped
	}
	return facts.Fact{Kind: facts.KindExtraction, Name: "typescript:client:" + spec, Repo: repo, File: repo, Props: props}
}

func TestBuildClients_SumsAcrossRepositories(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		clientAccountFact("sdk", "sdk-http", 2, 5, 2, "dynamic_path=2,missing_path_argument=1"),
		clientAccountFact("gateway", "sdk-http", 0, 0, 0, ""),
		clientAccountFact("web", "sdk-http", 1, 1, 0, "dynamic_path=1"),
		clientAccountFact("sdk", "legacy-http", 0, 0, 0, ""),
		// A JSON round trip turns ints into float64 and the entries into []any.
		facts.Fact{Kind: facts.KindExtraction, Name: "typescript:client:sdk-http", Repo: "api", File: "api",
			Props: map[string]any{facts.PropClientSpec: "sdk-http", "receivers": float64(1),
				"edge_coverage": []any{map[string]any{"detected": float64(3), "resolved": float64(3)}}}},
		// An extraction fact from another pass carries no client_spec and is not a client.
		facts.Fact{Kind: facts.KindExtraction, Name: "typescript:angular-di", File: "sdk",
			Props: map[string]any{"edge_coverage": []map[string]any{{"detected": 9, "resolved": 9}}}},
	)

	clients := BuildClients(store)
	if len(clients) != 2 || clients[0].Spec != "legacy-http" || clients[1].Spec != "sdk-http" {
		t.Fatalf("want [legacy-http sdk-http], got %+v", clients)
	}
	sdk := clients[1]
	if sdk.Receivers != 4 || sdk.CallSites != 9 || sdk.Routes != 5 {
		t.Errorf("sums wrong: %+v", sdk)
	}
	if sdk.Skipped["dynamic_path"] != 3 || sdk.Skipped["missing_path_argument"] != 1 || sdk.SkippedTotal() != 4 {
		t.Errorf("skipped causes wrong: %v", sdk.Skipped)
	}
	if clients[0].Skipped != nil {
		t.Errorf("a client that skipped nothing must omit skipped, got %v", clients[0].Skipped)
	}
}

func TestRenderClients_NamesTheSilentClientAndItsLikelyCause(t *testing.T) {
	if RenderClientsText(nil) != "" || RenderClientsMarkdown(nil) != "" {
		t.Error("no declared clients must render nothing")
	}
	clients := []Client{
		{Spec: "legacy-http"},
		{Spec: "sdk-http", Receivers: 2, CallSites: 5, Routes: 2, Skipped: map[string]int{"dynamic_path": 3}},
		{Spec: "unused-http", Receivers: 3},
	}
	text := RenderClientsText(clients)
	for _, want := range []string{
		"legacy-http matched no call site: no class declares a member of its receiver_types",
		"unused-http matched no call site: 3 members of its receiver types exist",
		"sdk-http skipped 3 calls: dynamic_path ×3",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text omits %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "sdk-http matched no call site") {
		t.Errorf("a client with call sites was reported silent:\n%s", text)
	}
	md := RenderClientsMarkdown(clients)
	if !strings.Contains(md, "| sdk-http | 2 | 5 | 2 | 3 (dynamic_path ×3) |") {
		t.Errorf("markdown row wrong:\n%s", md)
	}
}
