package http

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/linkers/crossrepo/routeindex"
	"github.com/enola-labs/enola/internal/linkers/vocab"
	"github.com/enola-labs/enola/pkg/plugin"
)

// evidence records what an edge was told, so a test can read its confidence and samples.
type evidence struct {
	confidences []string
	samples     map[string][]string
}

func (e *evidence) Via(string)          {}
func (e *evidence) Confidence(c string) { e.confidences = append(e.confidences, c) }
func (e *evidence) Sample(b plugin.Bucket, v string) {
	e.samples[b.Name] = append(e.samples[b.Name], v)
}
func (e *evidence) Unverified(plugin.Bucket, int) {}

type evidenceSink struct {
	plugin.EvidenceSink
	edges map[[2]string]*evidence
}

func (s *evidenceSink) Edge(consumer, provider string) plugin.EdgeEvidence {
	key := [2]string{consumer, provider}
	if s.edges[key] == nil {
		s.edges[key] = &evidence{samples: map[string][]string{}}
	}
	return s.edges[key]
}

func (s *evidenceSink) Coverage(string, string) *plugin.Coverage { return &plugin.Coverage{} }

func paramVocab(on bool) *vocab.Set {
	v := vocab.Default()
	v.MatchLiteralAgainstParams = on
	return v
}

// The reported case: a literal call reaching a handler under a :type parameter. With the
// option off it links nothing; on, it links probable and names the endpoint as resting
// on a parameter.
func TestParamMatch_EdgeIsProbableAndNamed(t *testing.T) {
	call := configuredCall("/v1/resources/catalog/items", "GET", "resource-api")
	server := servedBy("gateway", "/v1/resources/:type/items", "GET")

	off := &evidenceSink{edges: map[[2]string]*evidence{}}
	New(paramVocab(false)).Contribute(fakeInput{facts: []facts.Fact{call, server}}, off)
	if len(off.edges) != 0 {
		t.Fatalf("option off drew an edge: %+v", off.edges)
	}

	on := &evidenceSink{edges: map[[2]string]*evidence{}}
	New(paramVocab(true)).Contribute(fakeInput{facts: []facts.Fact{call, server}}, on)
	e := on.edges[[2]string{"sdk", "gateway"}]
	if e == nil {
		t.Fatalf("option on drew no sdk -> gateway edge: %+v", on.edges)
	}
	for _, c := range e.confidences {
		if c != "probable" {
			t.Errorf("a parameter match reported confidence %q", c)
		}
	}
	want := "GET /v1/resources/catalog/items"
	if got := e.samples[plugin.BucketEndpoints.Name]; len(got) != 1 || got[0] != want {
		t.Errorf("endpoints = %v, want [%s]", got, want)
	}
	if got := e.samples[plugin.BucketParamEndpoints.Name]; len(got) != 1 || got[0] != want {
		t.Errorf("param_segment_endpoints = %v, want [%s]", got, want)
	}
}

// The verdict passes index the server through the same builder, so a route an edge
// reached by parameter is neither an unused server route nor an unmatched client call.
func TestParamMatch_VerdictsAgreeWithTheEdge(t *testing.T) {
	call := configuredCall("/v1/resources/catalog/items", "GET", "resource-api")
	server := servedBy("gateway", "/v1/resources/:type/items", "GET")
	m := routeindex.New(paramVocab(true))
	all := []facts.Fact{call, server}

	evaluated, unused := ServerRouteVerdicts(m, all)
	if id := routeindex.RouteIdentity(server); !evaluated[id] || unused[id] {
		t.Errorf("the parameter route an edge reached is not counted as called (evaluated=%v unused=%v)", evaluated[id], unused[id])
	}
	if reason, ok := UnmatchedClientRouteKeys(m, all)[routeindex.RouteIdentity(call)]; ok {
		t.Errorf("the call an edge resolved is still reported unmatched: %s", reason)
	}
}
