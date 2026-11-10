package server_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeEndpointRepo writes a Go service shaped like a typical HTTP backend: a
// handler reaching filterconfig through calls; one reaching it through an interface
// with one implementation outside tests (a test double beside it); one through an
// interface with two, only one of which reaches it; one whose package imports it but
// whose handler only reads a variable (no call path); one registered with a handler
// held in a variable, which no symbol stands for; and one that has nothing to do
// with it.
func writeEndpointRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for rel, body := range map[string]string{
		"go.mod":                          "module example.com/shop\n\ngo 1.22\n",
		"internal/filterconfig/config.go": "package filterconfig\n\nvar DefaultSize = 10\n\nfunc Facets() []string { return []string{\"size\"} }\n",
		"internal/app/search/search.go": `package search

import "example.com/shop/internal/filterconfig"

type Service interface{ Facets() []string }

type service struct{}

func New() Service { return service{} }

func (service) Facets() []string { return filterconfig.Facets() }

type Ranker interface{ Rank() int }

type sizeRanker struct{}

func (sizeRanker) Rank() int { return len(filterconfig.Facets()) }

type fixedRanker struct{}

func (fixedRanker) Rank() int { return 1 }

func NewRanker(bySize bool) Ranker {
	if bySize {
		return sizeRanker{}
	}
	return fixedRanker{}
}
`,
		"internal/app/search/search_test.go": "package search\n\ntype fakeService struct{}\n\nfunc (fakeService) Facets() []string { return nil }\n",
		"internal/infra/http/server/server.go": `package server

import (
	"net/http"

	"example.com/shop/internal/app/search"
	"example.com/shop/internal/filterconfig"
)

type FiltersStrictServer struct {
	svc    search.Service
	ranker search.Ranker
}

func (s FiltersStrictServer) FiltersFacets(w http.ResponseWriter, r *http.Request) { _ = s.svc.Facets() }

func (s FiltersStrictServer) Rank(w http.ResponseWriter, r *http.Request) { _ = s.ranker.Rank() }

func Direct(w http.ResponseWriter, r *http.Request) { _ = filterconfig.Facets() }

func Meta(w http.ResponseWriter, r *http.Request) { _ = filterconfig.DefaultSize }

func Register(mux *http.ServeMux, s FiltersStrictServer) {
	mux.HandleFunc("GET /filters/facets", s.FiltersFacets)
	mux.HandleFunc("GET /direct", Direct)
	mux.HandleFunc("GET /meta", Meta)
	mux.HandleFunc("GET /rank", s.Rank)
	limited := http.MaxBytesHandler(http.HandlerFunc(Meta), 1<<20)
	mux.Handle("/wrapped", limited)
}
`,
		"internal/ops/ops.go": "package ops\n\nimport \"net/http\"\n\nfunc Health(w http.ResponseWriter, r *http.Request) {}\n\nfunc Register(mux *http.ServeMux) { mux.HandleFunc(\"GET /health\", Health) }\n",
	} {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// impact_analysis answers "which endpoints does this change affect", each at the
// precision the graph supports: through calls (an interface method with one
// implementation outside tests included), through an interface one of several
// implementations satisfies, or only because the handler's package depends on the
// target's. A route whose handler is no symbol is placed by the package registering
// it. An endpoint nothing connects is not listed.
func TestImpact_ListsTheEndpointsAChangeReaches(t *testing.T) {
	s := startInMemory(t)
	if res := s.call(t, "generate_snapshot", map[string]any{"repo_path": writeEndpointRepo(t)}); res.IsError {
		t.Fatal(text(res))
	}

	out := text(s.call(t, "impact_analysis", map[string]any{"target": "internal/filterconfig", "output_mode": "compact"}))
	for _, want := range []string{
		"## Endpoints affected (5: 2 through calls, 1 through an interface, 2 by package)",
		"- GET /direct → internal/infra/http/server.Direct (handler 1 call away)",
		"- GET /filters/facets → internal/infra/http/server.FiltersStrictServer.FiltersFacets (handler 2 calls away)",
		"- GET /rank → internal/infra/http/server.FiltersStrictServer.Rank (handler 2 calls away)",
		"- GET /meta → internal/infra/http/server.Meta (internal/infra/http/server, 1 import away)",
		"- /wrapped → limited (handler not resolved to a symbol; registered in internal/infra/http/server, 1 import away)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "/health") {
		t.Errorf("an endpoint nothing connects to the target was listed:\n%s", out)
	}

	// A symbol target has no package tier: only paths through calls count.
	sym := text(s.call(t, "impact_analysis", map[string]any{"target": "internal/filterconfig.Facets", "output_mode": "summary"}))
	if !strings.Contains(sym, "Endpoints affected (3: 2 through calls, 1 through an interface)") || strings.Contains(sym, "/meta") || strings.Contains(sym, "/wrapped") {
		t.Errorf("a symbol's endpoints:\n%s", sym)
	}

	// Full output carries them as data, with exact names.
	var full struct {
		Endpoints []struct {
			Path, Handler, Precision string
			Depth                    int
		} `json:"endpoints"`
	}
	raw := text(s.call(t, "impact_analysis", map[string]any{"target": "internal/filterconfig", "output_mode": "full"}))
	if err := json.Unmarshal([]byte(raw), &full); err != nil {
		t.Fatalf("full output: %v\n%.300s", err, raw)
	}
	if len(full.Endpoints) != 5 || full.Endpoints[0].Precision != "call" ||
		full.Endpoints[0].Handler != "example.com/shop/internal/infra/http/server.Direct" {
		t.Errorf("full endpoints = %+v", full.Endpoints)
	}

	// Something no handler reaches says so, rather than leaving the reader to guess.
	none := text(s.call(t, "impact_analysis", map[string]any{"target": "internal/app/search.New", "output_mode": "compact"}))
	if !strings.Contains(none, "Endpoints affected: none.") {
		t.Errorf("no endpoint reaches search.New, and the answer does not say so:\n%s", none)
	}
}
