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

// writeComposedHandlerRepo writes a service whose route handler delegates through an
// interface to a struct that implements it only by embedding one handler per
// endpoint, and whose handler reaches the target through a second interface.
func writeComposedHandlerRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for rel, body := range map[string]string{
		"go.mod":                     "module example.com/library\n\ngo 1.22\n",
		"internal/shelving/rules.go": "package shelving\n\nfunc Order() []string { return []string{\"title\"} }\n",
		"internal/api/api.go":        "package api\n\ntype ListParams struct{}\ntype ListResponse struct{}\n",
		"internal/app/catalog/catalog.go": `package catalog

import (
	"context"

	"example.com/library/internal/api"
	"example.com/library/internal/shelving"
)

type Service struct{}

func (s *Service) List(ctx context.Context, p api.ListParams) (*api.ListResponse, error) {
	_ = shelving.Order()
	return nil, nil
}
`,
		"internal/http/handlers.go": `package http

import (
	"context"
	"net/http"

	"example.com/library/internal/api"
)

type Lister interface {
	List(ctx context.Context, p api.ListParams) (*api.ListResponse, error)
}

type Books interface {
	ListBooks(ctx context.Context) error
	CountShelves(ctx context.Context) int
}

type listHandler struct{ lister Lister }

func (h *listHandler) ListBooks(ctx context.Context) error {
	_, err := h.lister.List(ctx, api.ListParams{})
	return err
}

type shelfHandler struct{}

func (h *shelfHandler) CountShelves(ctx context.Context) int { return 0 }

type composed struct {
	*listHandler
	*shelfHandler
}

func NewBooks(l Lister) Books { return &composed{listHandler: &listHandler{lister: l}, shelfHandler: &shelfHandler{}} }

type Gateway struct{ books Books }

func (g Gateway) Books(w http.ResponseWriter, r *http.Request) { _ = g.books.ListBooks(r.Context()) }

func Register(mux *http.ServeMux, g Gateway) { mux.HandleFunc("GET /books", g.Books) }
`,
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

// A struct that implements an interface only through the handlers it embeds still
// carries a call through that interface on to the embedded handler's method, so the
// route behind it is reached, not left at "by package only".
func TestImpact_EndpointThroughAPromotedMethod(t *testing.T) {
	s := startInMemory(t)
	if res := s.call(t, "generate_snapshot", map[string]any{"repo_path": writeComposedHandlerRepo(t)}); res.IsError {
		t.Fatal(text(res))
	}
	var full struct {
		Endpoints []struct {
			Path, Handler, Precision string
		} `json:"endpoints"`
	}
	raw := text(s.call(t, "impact_analysis", map[string]any{"target": "internal/shelving", "output_mode": "full"}))
	if err := json.Unmarshal([]byte(raw), &full); err != nil {
		t.Fatalf("full output: %v\n%.300s", err, raw)
	}
	found := false
	for _, e := range full.Endpoints {
		if e.Path != "/books" {
			continue
		}
		found = true
		if e.Precision != "interface" {
			t.Errorf("GET /books precision = %q, want interface: its handler reaches the target through the embedded handler: %+v", e.Precision, e)
		}
	}
	if !found {
		t.Errorf("GET /books is not listed: %+v", full.Endpoints)
	}
}
