package http

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/linkers/vocab"
	"github.com/enola-labs/enola/pkg/plugin"
)

// A WordPress hook is no HTTP route: a hook point in one repository is not matched
// against a registration in another, and it is no unresolved outbound call either.
func TestWordPressHooksAreNotHTTP(t *testing.T) {
	hook := func(repo, name, method, role string) facts.Fact {
		props := map[string]any{"method": method, "framework": "wordpress", facts.PropRouteType: facts.RouteTypeHook}
		if role != "" {
			props[facts.PropRole] = role
		}
		return facts.Fact{Kind: facts.KindRoute, Name: name, Repo: repo, Props: props}
	}
	fire := hook("core", "ACTION init", "ACTION", facts.RoleClient)
	register := hook("plugin", "ACTION init", "ACTION", "")
	sink := &coverageSink{coverage: map[string]*plugin.Coverage{}}
	New(vocab.Default()).Contribute(fakeInput{facts: []facts.Fact{fire, register}}, sink)
	if len(sink.edges) != 0 {
		t.Fatalf("hooks linked as HTTP: %+v", sink.edges)
	}
	if cov := sink.coverage["core"]; cov != nil && cov.Detected != 0 {
		t.Fatalf("a hook point counted as an outbound HTTP call: %+v", cov)
	}
}

// coverageSink keeps the coverage it hands out, per repository.
type coverageSink struct {
	fakeSink
	coverage map[string]*plugin.Coverage
}

func (s *coverageSink) Coverage(repo, _ string) *plugin.Coverage {
	if s.coverage[repo] == nil {
		s.coverage[repo] = &plugin.Coverage{}
	}
	return s.coverage[repo]
}
