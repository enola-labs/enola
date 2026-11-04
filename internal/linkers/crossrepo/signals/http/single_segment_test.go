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

// A thin client path ("{}/prepare", a word after an unread base) that only longer server
// paths serve is declined as thin_path. The verb matches, so method_mismatch, which it
// was stamped before, sent a reader looking for a wrong verb. The slash-led id form
// ("/{}/captions") is not thin and resolves, with no reason at all.
func TestThinPath_DeclinedWithItsOwnReason(t *testing.T) {
	thin := facts.Fact{Kind: facts.KindRoute, Name: "{}/prepare", Repo: "mobile",
		Props: map[string]any{"role": "client", "method": "POST"}}
	idPath := facts.Fact{Kind: facts.KindRoute, Name: "/{}/captions", Repo: "player",
		Props: map[string]any{"role": "client", "method": "GET",
			"source": facts.RouteSourceTSHTTPClient, "derived": "template-tail"}}
	all := []facts.Fact{thin, idPath,
		{Kind: facts.KindRoute, Name: "/admin/v1/stores/requests/{id}/prepare", Repo: "stores",
			Props: map[string]any{"role": "server", "method": "POST"}},
		{Kind: facts.KindRoute, Name: "/api/v1/videos/{videoId}/captions", Repo: "media",
			Props: map[string]any{"role": "server", "method": "GET"}},
	}

	reasons := UnmatchedClientRouteKeys(routeindex.New(vocab.Default()), all)
	if got := reasons[routeindex.RouteIdentity(thin)]; got != ReasonThinPath {
		t.Errorf("{}/prepare: unmatched_reason = %q, want %q", got, ReasonThinPath)
	}
	if got, ok := reasons[routeindex.RouteIdentity(idPath)]; ok {
		t.Errorf("/{}/captions must resolve, got unmatched_reason %q", got)
	}
}

// outside_snapshot says a call goes to a service the snapshot does not hold. Only a hint
// that names a service can say so: one read off a variable or class field ("base",
// "favorite") left calls the snapshot does serve, under a prefix the linker did not
// compose, labelled outside, and they dropped out of the unresolved count. A generic word
// names no service either, whatever it was read from.
func TestOutsideSnapshot_NeedsAHintThatNamesAService(t *testing.T) {
	call := func(path, hint, from string) facts.Fact {
		props := map[string]any{"role": "client", "method": "GET", "target_hint": hint}
		if from != "" {
			props[facts.PropTargetHintFrom] = from
		}
		return facts.Fact{Kind: facts.KindRoute, Name: path, Repo: "web", File: "src/" + hint + ".ts", Props: props}
	}
	envLedger := call("/v1/entries/{id}", "ledger", facts.HintFromEnv)
	fieldHint := call("/favorite/type/list", "favorite", "")
	camelBase := call("/api/organization/list", "base", "")
	genericAPI := call("/api/tenant/list", "api", facts.HintFromConstant)
	all := []facts.Fact{envLedger, fieldHint, camelBase, genericAPI,
		{Kind: facts.KindRoute, Name: "/organization", Repo: "backend", Props: map[string]any{"role": "server", "method": "GET"}},
	}
	reasons := UnmatchedClientCallReasons(routeindex.New(vocab.Default()), all)
	if got := reasons[ClientCallKey(envLedger)]; got != ReasonOutsideSnapshot {
		t.Errorf("env-named service: reason = %q, want %q", got, ReasonOutsideSnapshot)
	}
	for _, f := range []facts.Fact{fieldHint, camelBase, genericAPI} {
		if got := reasons[ClientCallKey(f)]; got != ReasonPathUnknown {
			t.Errorf("%s (hint %q): reason = %q, want %q", f.Name, f.PropString("target_hint"), got, ReasonPathUnknown)
		}
	}
}

// Two call sites of one verb and path are two calls. One whose hint names a service
// outside the snapshot does not make its twin with no hint outside too: reasons are kept
// per call site, as coverage counts them.
func TestUnmatchedReasons_ArePerCallSite(t *testing.T) {
	named := facts.Fact{Kind: facts.KindRoute, Name: "/v1/entries/list", Repo: "web", File: "src/a.ts", Line: 3,
		Props: map[string]any{"role": "client", "method": "GET", "target_hint": "ledger", facts.PropTargetHintFrom: facts.HintFromEnv}}
	bare := facts.Fact{Kind: facts.KindRoute, Name: "/v1/entries/list", Repo: "web", File: "src/b.ts", Line: 9,
		Props: map[string]any{"role": "client", "method": "GET"}}
	all := []facts.Fact{named, bare,
		{Kind: facts.KindRoute, Name: "/health/live", Repo: "backend", Props: map[string]any{"role": "server", "method": "GET"}},
	}
	reasons := UnmatchedClientCallReasons(routeindex.New(vocab.Default()), all)
	if got := reasons[ClientCallKey(named)]; got != ReasonOutsideSnapshot {
		t.Errorf("named call: reason = %q, want %q", got, ReasonOutsideSnapshot)
	}
	if got := reasons[ClientCallKey(bare)]; got != ReasonPathUnknown {
		t.Errorf("its twin with no hint: reason = %q, want %q", got, ReasonPathUnknown)
	}
}
