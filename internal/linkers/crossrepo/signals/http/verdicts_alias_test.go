package http

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/linkers/crossrepo/routeindex"
	"github.com/enola-labs/enola/internal/linkers/vocab"
)

// A path served by gateway and replica, called by a configured client whose service
// name the config aliases. The alias states which repository the call reaches, so only
// that repository's route counts as called. Without an alias, or with one naming neither
// repository, nothing says which is called and both still count: flagging a route in
// use is the unsafe direction for a verdict that may drive its removal.
func TestServerRouteVerdicts_AliasNamesWhoWasCalled(t *testing.T) {
	call := configuredCall("/v1/catalog/imports", "POST", "resource-api")
	gatewayImports := servedBy("gateway", "/v1/catalog/imports", "POST")
	replicaImports := servedBy("replica", "/v1/catalog/imports", "POST")
	// Another client calls replica on its own, so replica is a provider whose routes
	// receive a verdict at all.
	web := facts.Fact{Kind: facts.KindRoute, Name: "/v1/replica/health-report", Repo: "web",
		Props: map[string]any{"role": "client", "method": "GET", "source": facts.RouteSourceTSHTTPClient}}
	replicaReport := servedBy("replica", "/v1/replica/health-report", "GET")
	all := []facts.Fact{call, gatewayImports, replicaImports, web, replicaReport}

	gateway := routeindex.RouteIdentity(gatewayImports)
	replica := routeindex.RouteIdentity(replicaImports)

	evaluated, unused := ServerRouteVerdicts(routeindex.New(withAliases(map[string]string{"resource-api": "gateway"})), all)
	if !evaluated[gateway] || unused[gateway] {
		t.Errorf("the route the alias sends the call to is not counted as called (evaluated=%v unused=%v)",
			evaluated[gateway], unused[gateway])
	}
	if !evaluated[replica] || !unused[replica] {
		t.Errorf("replica's route counts as called although the alias sends the call to gateway (evaluated=%v unused=%v)",
			evaluated[replica], unused[replica])
	}

	_, unused = ServerRouteVerdicts(routeindex.New(vocab.Default()), all)
	if unused[gateway] || unused[replica] {
		t.Errorf("with no alias, a candidate was flagged unused (gateway=%v replica=%v)", unused[gateway], unused[replica])
	}

	_, unused = ServerRouteVerdicts(routeindex.New(withAliases(map[string]string{"resource-api": "billing"})), all)
	if unused[gateway] || unused[replica] {
		t.Errorf("an alias naming neither repository narrowed the verdict (gateway=%v replica=%v)", unused[gateway], unused[replica])
	}
}

// A hint that picks a provider with no declaration behind it narrows nothing. A name in
// the source that happens to equal a repository label is not a statement of which
// repository the call reaches, so the verdict stays generous.
func TestServerRouteVerdicts_HintAloneDoesNotNarrow(t *testing.T) {
	call := facts.Fact{Kind: facts.KindRoute, Name: "/v1/catalog/imports", Repo: "sdk",
		Props: map[string]any{"role": "client", "method": "POST",
			"source": facts.RouteSourceTSHTTPClient, "target_hint": "gateway"}}
	replicaImports := servedBy("replica", "/v1/catalog/imports", "POST")
	web := facts.Fact{Kind: facts.KindRoute, Name: "/v1/replica/health-report", Repo: "web",
		Props: map[string]any{"role": "client", "method": "GET", "source": facts.RouteSourceTSHTTPClient}}
	all := []facts.Fact{call, servedBy("gateway", "/v1/catalog/imports", "POST"), replicaImports,
		web, servedBy("replica", "/v1/replica/health-report", "GET")}

	evaluated, unused := ServerRouteVerdicts(routeindex.New(vocab.Default()), all)
	if id := routeindex.RouteIdentity(replicaImports); !evaluated[id] || unused[id] {
		t.Errorf("a hint with no alias narrowed the verdict: replica flagged unused (evaluated=%v unused=%v)", evaluated[id], unused[id])
	}
}
