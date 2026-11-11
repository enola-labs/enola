package server_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// writeStoredFuncRepo writes two read-through caches of one generic type, each built
// with its own fetch closure calling its own client, each behind its own route.
func writeStoredFuncRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for rel, body := range map[string]string{
		"go.mod": "module example.com/library\n\ngo 1.22\n",
		"internal/store/cache.go": `package store

import "context"

type Source[K comparable, V any] func(ctx context.Context, keys []K) (map[K]V, error)

type ReadThrough[K comparable, V any] struct {
	source Source[K, V]
}

func NewReadThrough[K comparable, V any](source Source[K, V]) *ReadThrough[K, V] {
	return newReadThrough[K, V](source)
}

func newReadThrough[K comparable, V any](source Source[K, V]) *ReadThrough[K, V] {
	return &ReadThrough[K, V]{source: source}
}

func (r *ReadThrough[K, V]) GetMany(ctx context.Context, keys []K) (map[K]V, error) {
	return r.load(ctx, keys)
}

func (r *ReadThrough[K, V]) load(ctx context.Context, keys []K) (map[K]V, error) {
	return r.source(ctx, keys)
}
`,
		"internal/books/books.go": `package books

import (
	"context"
	"net/http"

	"example.com/library/internal/store"
)

type Book struct{ Title string }

type catalog interface {
	Books(ctx context.Context, ids []int) (map[int]Book, error)
}

type booksCache struct{ rt *store.ReadThrough[int, Book] }

func newBooksCache(api catalog) *booksCache {
	fetch := func(ctx context.Context, ids []int) (map[int]Book, error) { return api.Books(ctx, ids) }
	return &booksCache{rt: store.NewReadThrough(fetch)}
}

func (c *booksCache) get(ctx context.Context, ids []int) (map[int]Book, error) {
	return c.rt.GetMany(ctx, ids)
}

type Handler struct{ cache *booksCache }

func (h *Handler) Titles(w http.ResponseWriter, r *http.Request) { _, _ = h.cache.get(r.Context(), []int{1}) }

func Register(mux *http.ServeMux, h *Handler) { mux.HandleFunc("GET /titles", h.Titles) }
`,
		"internal/authors/authors.go": `package authors

import (
	"context"
	"net/http"

	"example.com/library/internal/store"
)

type Author struct{ Name string }

type registry interface {
	Authors(ctx context.Context, ids []int) (map[int]Author, error)
}

type authorsCache struct{ rt *store.ReadThrough[int, Author] }

func newAuthorsCache(api registry) *authorsCache {
	fetch := func(ctx context.Context, ids []int) (map[int]Author, error) { return api.Authors(ctx, ids) }
	return &authorsCache{rt: store.NewReadThrough(fetch)}
}

func (c *authorsCache) get(ctx context.Context, ids []int) (map[int]Author, error) {
	return c.rt.GetMany(ctx, ids)
}

type Handler struct{ cache *authorsCache }

func (h *Handler) Names(w http.ResponseWriter, r *http.Request) { _, _ = h.cache.get(r.Context(), []int{1}) }

func Register(mux *http.ServeMux, h *Handler) { mux.HandleFunc("GET /authors", h.Names) }
`,
		"internal/clients/catalog.go": `package clients

import (
	"context"

	"example.com/library/internal/books"
)

type Catalog struct{}

func (c *Catalog) Books(ctx context.Context, ids []int) (map[int]books.Book, error) { return nil, nil }
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

// A client called only from a fetch closure a cache was built with reaches the route
// whose handler uses that cache, and not the route of another cache of the same type
// built with another closure.
func TestImpact_ThroughAFunctionStoredInACache(t *testing.T) {
	s := startInMemory(t)
	if res := s.call(t, "generate_snapshot", map[string]any{"repo_path": writeStoredFuncRepo(t)}); res.IsError {
		t.Fatal(text(res))
	}
	var full struct {
		Endpoints []struct {
			Method, Path, Precision string
		} `json:"endpoints"`
	}
	raw := text(s.call(t, "impact_analysis", map[string]any{
		"target": "example.com/library/internal/clients.Catalog.Books", "output_mode": "full",
	}))
	if err := json.Unmarshal([]byte(raw), &full); err != nil {
		t.Fatalf("full output: %v\n%.300s", err, raw)
	}
	paths := map[string]string{}
	for _, e := range full.Endpoints {
		paths[e.Method+" "+e.Path] = e.Precision
	}
	if p, ok := paths["GET /titles"]; !ok || p == "package" {
		t.Errorf("GET /titles should be reached through the cache's stored fetch, not by package; endpoints: %+v", full.Endpoints)
	}
	if _, ok := paths["GET /authors"]; ok {
		t.Errorf("GET /authors uses another cache of the same type built with another fetch; endpoints: %+v", full.Endpoints)
	}
}
