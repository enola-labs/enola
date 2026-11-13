package deployment

import (
	"reflect"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/pkg/plugin"
)

type fakeInput struct {
	plugin.SignalInput
	facts []facts.Fact
}

func (f fakeInput) Facts() []facts.Fact { return f.facts }

type fakeEdge struct {
	via     []string
	samples []string
}

func (e *fakeEdge) Via(v string)                     { e.via = append(e.via, v) }
func (e *fakeEdge) Confidence(string)                {}
func (e *fakeEdge) Sample(_ plugin.Bucket, v string) { e.samples = append(e.samples, v) }
func (e *fakeEdge) Unverified(plugin.Bucket, int)    {}

type fakeSink struct {
	plugin.EvidenceSink
	edges map[[2]string]*fakeEdge
}

func (s *fakeSink) Edge(c, p string) plugin.EdgeEvidence {
	k := [2]string{c, p}
	if s.edges[k] == nil {
		s.edges[k] = &fakeEdge{}
	}
	return s.edges[k]
}

func TestAReleaseLinksItsRepositoryToTheCodeItRuns(t *testing.T) {
	app := func(name, deploys string) facts.Fact {
		p := map[string]any{"k8s_kind": "Application"}
		if deploys != "" {
			p["deploys_repo"] = deploys
		}
		return facts.Fact{Kind: facts.KindSymbol, Repo: "shop-deploy", Name: name, Props: p}
	}
	sink := &fakeSink{edges: map[[2]string]*fakeEdge{}}
	New().Contribute(fakeInput{facts: []facts.Fact{
		app("web-eu1", "shop-web"),
		app("web-us1", "shop-web"),
		app("mesh", ""),
	}}, sink)
	if len(sink.edges) != 1 {
		t.Fatalf("edges = %v, want one", sink.edges)
	}
	e := sink.edges[[2]string{"shop-deploy", "shop-web"}]
	if e == nil {
		t.Fatal("no edge shop-deploy -> shop-web")
	}
	if !reflect.DeepEqual(e.via, []string{facts.ViaDeployment, facts.ViaDeployment}) || !reflect.DeepEqual(e.samples, []string{"web-eu1", "web-us1"}) {
		t.Errorf("via = %v, samples = %v", e.via, e.samples)
	}
}
