package routeindex

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/linkers/vocab"
)

func paramMatcher(on bool) *Matcher {
	v := vocab.Default()
	v.MatchLiteralAgainstParams = on
	return New(v)
}

func serves(repo, path, method string) facts.Fact {
	return facts.Fact{Kind: facts.KindRoute, Name: path, Repo: repo,
		Props: map[string]any{"role": "server", "method": method}}
}

func lookup(m *Matcher, clientPath, method string, servers ...facts.Fact) ([]RouteRef, bool) {
	index := m.IndexServerRoutes(servers)
	refs, _, viaParam := m.LookupClientMatchesDetailed(index, CanonicalLeadingSlash(m.NormalizePath(clientPath)), method)
	return refs, viaParam
}

func repos(refs []RouteRef) map[string]bool {
	out := map[string]bool{}
	for _, r := range refs {
		out[r.Repo] = true
	}
	return out
}

func TestParamMatch_OffByDefault(t *testing.T) {
	refs, _ := lookup(paramMatcher(false), "/v1/resources/catalog/items", "GET",
		serves("gateway", "/v1/resources/:type/items", "GET"))
	if len(refs) != 0 {
		t.Fatalf("a parameter absorbed a literal with the option off: %+v", refs)
	}
}

func TestParamMatch_LiteralFillsParameter(t *testing.T) {
	refs, viaParam := lookup(paramMatcher(true), "/v1/resources/catalog/items", "GET",
		serves("gateway", "/v1/resources/:type/items", "GET"))
	if len(refs) != 1 || refs[0].Repo != "gateway" || !viaParam {
		t.Fatalf("want gateway via parameter, got %+v (viaParam=%v)", refs, viaParam)
	}
}

// A client carrying a gateway prefix still matches the whole server path.
func TestParamMatch_ClientPrefixMatchesWholeServerPath(t *testing.T) {
	refs, viaParam := lookup(paramMatcher(true), "/api/v1/resources/catalog/items", "GET",
		serves("gateway", "/v1/resources/:type/items", "GET"))
	if len(refs) != 1 || !viaParam {
		t.Fatalf("a prefixed client call must match by suffix, got %+v", refs)
	}
}

// The fallback never overrides an exact match: it runs only when the exact join found nothing.
func TestParamMatch_ExactMatchWins(t *testing.T) {
	refs, viaParam := lookup(paramMatcher(true), "/v1/resources/catalog/items", "GET",
		serves("literal", "/v1/resources/catalog/items", "GET"),
		serves("pattern", "/v1/resources/:type/items", "GET"))
	if got := repos(refs); len(got) != 1 || !got["literal"] || viaParam {
		t.Fatalf("want the exact route only, got %+v (viaParam=%v)", refs, viaParam)
	}
}

func TestParamMatch_NeedsTwoLiteralSegments(t *testing.T) {
	refs, _ := lookup(paramMatcher(true), "/api/catalog/7", "GET",
		serves("gateway", "/api/:type/:id", "GET"))
	if len(refs) != 0 {
		t.Fatalf("one agreeing literal segment matched: %+v", refs)
	}
}

// A leading parameter is a tenant or a version slot; letting it absorb a literal fits
// every service that shares the rest of the path.
//
// The case must need the WHOLE server path. `/:tenant/orders/open` is no test of this:
// the exact suffix join already reaches it through `/orders/open`, without any
// parameter. Here no suffix of either path equals one of the other, so only absorbing
// the leading parameter could match.
func TestParamMatch_ParameterMayNotLead(t *testing.T) {
	refs, viaParam := lookup(paramMatcher(true), "/acme/orders/7/lines", "GET",
		serves("gateway", "/:tenant/orders/:id/lines", "GET"))
	if len(refs) != 0 || viaParam {
		t.Fatalf("a leading parameter absorbed a literal: %+v (viaParam=%v)", refs, viaParam)
	}
}

func TestParamMatch_ClientPlaceholderNeverMatchesServerLiteral(t *testing.T) {
	refs, _ := lookup(paramMatcher(true), "/v1/resources/{}/items/list", "GET",
		serves("gateway", "/v1/resources/catalog/items/:id", "GET"))
	if len(refs) != 0 {
		t.Fatalf("a client placeholder matched a server literal: %+v", refs)
	}
}

// Two different patterns fit the same call. Choosing one would be a guess.
func TestParamMatch_TwoPatternsMatchNothing(t *testing.T) {
	refs, _ := lookup(paramMatcher(true), "/v1/resources/items", "GET",
		serves("a", "/v1/:kind/items", "GET"),
		serves("b", "/v1/resources/:id", "GET"))
	if len(refs) != 0 {
		t.Fatalf("two fitting patterns matched: %+v", refs)
	}
}

// The same pattern served by two repositories is not pattern ambiguity: both are
// returned and provider choice decides, as for any other match.
func TestParamMatch_SamePatternInTwoReposReturnsBoth(t *testing.T) {
	refs, _ := lookup(paramMatcher(true), "/v1/resources/catalog/items", "GET",
		serves("gateway", "/v1/resources/:type/items", "GET"),
		serves("replica", "/v1/resources/:type/items", "GET"))
	if got := repos(refs); len(got) != 2 {
		t.Fatalf("want both repos for provider choice, got %+v", refs)
	}
}

func TestParamMatch_VerbStillMatters(t *testing.T) {
	refs, _ := lookup(paramMatcher(true), "/v1/resources/catalog/items", "DELETE",
		serves("gateway", "/v1/resources/:type/items", "GET"))
	if len(refs) != 0 {
		t.Fatalf("a parameter match ignored the verb: %+v", refs)
	}
}
