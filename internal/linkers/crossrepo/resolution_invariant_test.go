package crossrepo

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/linkers/crossrepo/routeindex"
	httpsignal "github.com/enola-labs/enola/internal/linkers/crossrepo/signals/http"
	"github.com/enola-labs/enola/internal/linkers/vocab"
	"github.com/enola-labs/enola/pkg/plugin"
)

func invariantClient(repo, method, path string, props map[string]any) facts.Fact {
	p := map[string]any{"role": "client", "method": method, "source": facts.RouteSourceTSHTTPClient}
	for k, v := range props {
		p[k] = v
	}
	return facts.Fact{Kind: facts.KindRoute, Name: path, Repo: repo, Props: p}
}

func invariantServer(repo, method, path string) facts.Fact {
	return facts.Fact{Kind: facts.KindRoute, Name: path, Repo: repo, Props: map[string]any{"role": "server", "method": method}}
}

func coverageInts(svc facts.Fact) map[string]int {
	out := map[string]int{}
	entries, _ := svc.Props["edge_coverage"].([]map[string]any)
	for _, e := range entries {
		if e["edge_type"] != httpsignal.CoverageEdgeType {
			continue
		}
		for _, k := range []string{"detected", "resolved", "unresolved", "external", "declared"} {
			if n, ok := e[k].(int); ok {
				out[k] = n
			}
		}
	}
	return out
}

// TestResolution_CoverageAndReasonsAgreeOnEveryBranch runs the linker's coverage count
// and the unmatched pass over one fact set that reaches every branch of a client call's
// resolution, and holds them to one account per repository: calls carrying a reason
// other than attributed_by_intent number exactly `unresolved`, and those attributed by
// intent exactly `declared`.
//
// The committed goldens cannot prove this: none of them holds a single-segment path
// rejected for a substring hint, or a verb-less call in a repository with one declared
// seam, and both were cases where the two passes disagreed.
func TestResolution_CoverageAndReasonsAgreeOnEveryBranch(t *testing.T) {
	v := vocab.Default()
	v.ServiceAliases = map[string]string{"resource-api": "billing"}

	web := []facts.Fact{
		invariantClient("web", "GET", "/api/v1/widgets/list", nil),                            // resolved
		invariantClient("web", "GET", "/v3/charges/refund", map[string]any{"external": true}), // external
		invariantClient("web", "", "/api/v1/widgets/list", nil),                               // no_method
		invariantClient("web", "GET", "/health", nil),                                         // generic_path
		invariantClient("web", "POST", "/v1/catalog/imports", nil),                            // ambiguous_provider
		invariantClient("web", "POST", "/mcp", map[string]any{"target_hint": "back"}),         // single segment: ambiguous_provider
		invariantClient("web", "DELETE", "/api/v1/widgets/list", nil),                         // method_mismatch
		invariantClient("web", "GET", "/v9/nothing/here", nil),                                // path_unknown
	}
	sdk := []facts.Fact{
		// A declared client whose alias names a repository serving none of the matches.
		invariantClient("sdk", "GET", "/api/v1/widgets/list", map[string]any{
			"source": facts.RouteSourceConfiguredHTTPClient, facts.PropClientSpec: "sdk-http", "target_hint": "resource-api"}),
	}
	app := []facts.Fact{
		{Kind: facts.KindIntent, Repo: "app", Name: "consumes backend via http-client",
			Props: map[string]any{"intent_kind": "consumes", "target": "backend", "via": "http-client"}},
		invariantClient("app", "GET", "/api/v1/widgets/list", nil), // resolved
		invariantClient("app", "", "/api/v1/widgets/list", nil),    // verb-less, but the repo declares one seam
		invariantClient("app", "GET", "/v9/nothing/here", nil),     // unknown path, same seam
	}
	servers := []facts.Fact{
		invariantServer("backend", "GET", "/api/v1/widgets/list"),
		invariantServer("backend", "POST", "/v1/catalog/imports"),
		invariantServer("other", "POST", "/v1/catalog/imports"),
		invariantServer("backend", "POST", "/mcp"),
		invariantServer("other", "POST", "/mcp"),
		invariantServer("billing", "GET", "/v1/invoices/open"),
	}
	var all []facts.Fact
	for _, group := range [][]facts.Fact{web, sdk, app, servers} {
		all = append(all, group...)
	}

	linked := ComputeLinks(all, nil, []plugin.CrossRepoSignal{httpsignal.New(v)}, v)
	coverage := map[string]map[string]int{}
	for _, f := range linked {
		if f.Kind == facts.KindService {
			coverage[f.Repo] = coverageInts(f)
		}
	}
	reasons := httpsignal.UnmatchedClientRouteKeys(routeindex.New(v), all)

	stamped := map[string]map[string]int{}
	for _, f := range all {
		if f.Kind != facts.KindRoute || f.Props["role"] != facts.RoleClient {
			continue
		}
		reason, ok := reasons[routeindex.RouteIdentity(f)]
		if !ok {
			continue
		}
		if stamped[f.Repo] == nil {
			stamped[f.Repo] = map[string]int{}
		}
		if reason == httpsignal.ReasonDeclaredTarget {
			stamped[f.Repo]["declared"]++
		} else {
			stamped[f.Repo]["unresolved"]++
		}
	}

	for _, repo := range []string{"web", "sdk", "app"} {
		if got, want := stamped[repo]["unresolved"], coverage[repo]["unresolved"]; got != want {
			t.Errorf("%s: edge_coverage unresolved=%d, but %d call(s) carry a reason explaining it", repo, want, got)
		}
		if got, want := stamped[repo]["declared"], coverage[repo]["declared"]; got != want {
			t.Errorf("%s: edge_coverage declared=%d, but %d call(s) carry attributed_by_intent", repo, want, got)
		}
	}

	// The account itself, so an invariant satisfied by two equally wrong numbers fails too.
	want := map[string]map[string]int{
		"web": {"detected": 8, "resolved": 1, "external": 1, "declared": 0, "unresolved": 6},
		"sdk": {"detected": 1, "resolved": 0, "external": 0, "declared": 0, "unresolved": 1},
		"app": {"detected": 3, "resolved": 1, "external": 0, "declared": 2, "unresolved": 0},
	}
	for repo, counts := range want {
		for k, n := range counts {
			if coverage[repo][k] != n {
				t.Errorf("%s: edge_coverage %s = %d, want %d (all: %v)", repo, k, coverage[repo][k], n, coverage[repo])
			}
		}
	}

	expectReason := map[string]string{
		routeindex.RouteIdentity(web[2]): httpsignal.ReasonNoMethod,
		routeindex.RouteIdentity(web[3]): httpsignal.ReasonGenericPath,
		routeindex.RouteIdentity(web[4]): httpsignal.ReasonAmbiguousProvider,
		routeindex.RouteIdentity(web[5]): httpsignal.ReasonAmbiguousProvider,
		routeindex.RouteIdentity(web[6]): httpsignal.ReasonMethodMismatch,
		routeindex.RouteIdentity(web[7]): httpsignal.ReasonPathUnknown,
		routeindex.RouteIdentity(sdk[0]): httpsignal.ReasonAliasNotServing,
		routeindex.RouteIdentity(app[2]): httpsignal.ReasonDeclaredTarget,
		routeindex.RouteIdentity(app[3]): httpsignal.ReasonDeclaredTarget,
	}
	for id, wantReason := range expectReason {
		if got := reasons[id]; got != wantReason {
			t.Errorf("%q: unmatched_reason = %q, want %q", id, got, wantReason)
		}
	}
	for _, resolved := range []facts.Fact{web[0], web[1], app[1]} {
		if reason, ok := reasons[routeindex.RouteIdentity(resolved)]; ok {
			t.Errorf("%s %s is resolved or external but carries reason %q", resolved.Repo, resolved.Name, reason)
		}
	}
}
