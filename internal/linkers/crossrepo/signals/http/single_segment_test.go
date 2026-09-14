package http

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/linkers/crossrepo/routeindex"
	"github.com/enola-labs/enola/internal/linkers/vocab"
)

// A single-segment path served by two repositories, with a hint that names one of them
// only by substring. The edge pass drops that provider: a one-segment path is too thin
// to pick by partial name. The unmatched pass must reach the same verdict and explain
// it, or the call counts unresolved with nothing on it saying why.
func TestSingleSegmentPath_RejectedProviderIsExplained(t *testing.T) {
	client := facts.Fact{Kind: facts.KindRoute, Name: "/mcp", Repo: "agent",
		Props: map[string]any{"role": "client", "method": "POST",
			"source": facts.RouteSourceTSHTTPClient, "target_hint": "back"}}
	all := []facts.Fact{client,
		{Kind: facts.KindRoute, Name: "/mcp", Repo: "backend", Props: map[string]any{"role": "server", "method": "POST"}},
		{Kind: facts.KindRoute, Name: "/mcp", Repo: "other", Props: map[string]any{"role": "server", "method": "POST"}},
	}

	if edges := runSignal(t, all...); len(edges) != 0 {
		t.Fatalf("a substring hint picked a provider for a single-segment path: %+v", edges)
	}
	reasons := UnmatchedClientRouteKeys(routeindex.New(vocab.Default()), all)
	if got := reasons[routeindex.RouteIdentity(client)]; got != ReasonAmbiguousProvider {
		t.Errorf("the rejected call's unmatched_reason = %q, want %q", got, ReasonAmbiguousProvider)
	}
}
