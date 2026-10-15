package coverage

import (
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func TestAttachUnresolvedSites(t *testing.T) {
	store := facts.NewStore()
	route := func(repo, name, file string, line int, props map[string]any) {
		p := map[string]any{facts.PropRole: facts.RoleClient, "method": "GET"}
		for k, v := range props {
			p[k] = v
		}
		store.Add(facts.Fact{Kind: facts.KindRoute, Name: name, Repo: repo, File: file, Line: line, Props: p})
	}
	miss := map[string]any{"unmatched_by_server": true, "unmatched_reason": "path_unknown"}
	route("mobile", "/v1/me", "lib/me.dart", 9, miss)
	route("mobile", "/v1/me", "lib/profile.dart", 3, miss)
	route("mobile", "/v1/me/addresses", "lib/addr.dart", 5, nil)
	route("mobile", "/v1/fake", "lib/fake.dart", 1, map[string]any{"unmatched_by_server": true, "test_double": true})
	route("web", "/v1/search", "app/search.ts", 7, miss)

	r := Report{{Service: "mobile", UnresolvedTotal: 2}, {Service: "api"}}
	r.AttachUnresolvedSites(store)
	if len(r[0].UnresolvedSites) != 2 || len(r[1].UnresolvedSites) != 0 {
		t.Fatalf("sites = %+v", r)
	}
	out := r.RenderUnresolvedSites()
	for _, want := range []string{"mobile — 2 unresolved call sites", "GET /v1/me  (path_unknown)", "lib/me.dart:9", "lib/profile.dart:3"} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q:\n%s", want, out)
		}
	}
	if strings.Count(out, "GET /v1/me ") != 1 || strings.Contains(out, "fake") || strings.Contains(out, "search") {
		t.Errorf("render:\n%s", out)
	}
}
