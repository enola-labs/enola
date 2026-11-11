package goextractor

import (
	"slices"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// storedFuncRepo is a read-through cache that stores the fetch function its
// constructor is handed, forwarded through two constructors, and a holder built with
// a closure that calls a client.
func storedFuncRepo() map[string]string {
	return map[string]string{
		"store/cache.go": `package store

import "context"

type Source[K comparable, V any] func(ctx context.Context, keys []K) (map[K]V, error)

type ReadThrough[K comparable, V any] struct {
	source Source[K, V]
	keyFn  func(K) string
	ttlFn  func(K, V) int
}

func NewReadThrough[K comparable, V any](name string, source Source[K, V], keyFn func(K) string) *ReadThrough[K, V] {
	return newReadThrough[K, V](name, source, keyFn)
}

func newReadThrough[K comparable, V any](name string, source Source[K, V], keyFn func(K) string) *ReadThrough[K, V] {
	return &ReadThrough[K, V]{source: source, keyFn: keyFn}
}

func NewReadThroughTTL[K comparable, V any](name string, source Source[K, V], keyFn func(K) string, ttlFn func(K, V) int) *ReadThrough[K, V] {
	r := NewReadThrough[K, V](name, source, keyFn)
	r.ttlFn = ttlFn
	return r
}

func (r *ReadThrough[K, V]) GetMany(ctx context.Context, keys []K) (map[K]V, error) {
	for _, k := range keys {
		_ = r.keyFn(k)
	}
	got, err := r.source(ctx, keys)
	r.writeBack(got)
	return got, err
}

func (r *ReadThrough[K, V]) writeBack(got map[K]V) {
	for k, v := range got {
		_ = r.ttlFn(k, v)
	}
}
`,
		"shelf/shelf.go": `package shelf

import (
	"context"

	"testmod/store"
)

type Book struct{ Title string }

type catalog interface {
	Books(ctx context.Context, ids []int) (map[int]Book, error)
}

type booksCache struct {
	rt *store.ReadThrough[int, Book]
}

func bookKey(id int) string { return "book" }

func bookTTL(id int, b Book) int { return 60 }

func newBooksCache(api catalog) *booksCache {
	fetch := func(ctx context.Context, ids []int) (map[int]Book, error) {
		return api.Books(ctx, ids)
	}
	return &booksCache{rt: store.NewReadThroughTTL("books", fetch, bookKey, bookTTL)}
}

func (c *booksCache) get(ctx context.Context, ids []int) (map[int]Book, error) {
	return c.rt.GetMany(ctx, ids)
}
`,
	}
}

func flowsOf(t *testing.T, ff []facts.Fact, name string) []string {
	t.Helper()
	f, ok := findFact(ff, name)
	if !ok {
		t.Fatalf("expected a fact for %s", name)
	}
	return stringSliceProp(f.Props[propFuncFlows])
}

func stringSliceProp(v any) []string {
	switch x := v.(type) {
	case []string:
		return x
	case []any:
		var out []string
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// Each step of the value's way is recorded where it happens: the closure handed to
// the constructor, each forwarding of the parameter, the store into a field, the
// fields a method calls, and the method called on the holder field.
func TestFuncFlows_ReadThroughCache(t *testing.T) {
	ff := extractAll(t, storedFuncRepo())
	want := map[string][]string{
		"shelf.newBooksCache": {
			"holder testmod/shelf.booksCache.rt <- testmod/store.NewReadThroughTTL(1=testmod/shelf.newBooksCache.func1,2=testmod/shelf.bookKey,3=testmod/shelf.bookTTL)",
		},
		"store.NewReadThroughTTL": {
			"arg testmod/store.NewReadThrough#1 <- @1",
			"arg testmod/store.NewReadThrough#2 <- @2",
			"field testmod/store.ReadThrough.ttlFn <- @3",
		},
		"store.NewReadThrough": {
			"arg testmod/store.newReadThrough#1 <- @1",
			"arg testmod/store.newReadThrough#2 <- @2",
		},
		"store.newReadThrough": {
			"field testmod/store.ReadThrough.keyFn <- @2",
			"field testmod/store.ReadThrough.source <- @1",
		},
		"store.ReadThrough.GetMany": {
			"fieldcall testmod/store.ReadThrough.keyFn",
			"fieldcall testmod/store.ReadThrough.source",
		},
		"store.ReadThrough.writeBack": {"fieldcall testmod/store.ReadThrough.ttlFn"},
		"shelf.booksCache.get": {
			"holdercall testmod/shelf.booksCache.rt -> testmod/store.ReadThrough.GetMany",
		},
	}
	for name, entries := range want {
		got := flowsOf(t, ff, name)
		for _, e := range entries {
			if !slices.Contains(got, e) {
				t.Errorf("%s: missing flow %q; got %q", name, e, got)
			}
		}
	}
	closure, ok := findFact(ff, "shelf.newBooksCache.func1")
	if !ok {
		t.Fatal("expected a fact for the fetch closure, shelf.newBooksCache.func1")
	}
	if !hasRelation(closure, facts.RelCalls, "shelf.catalog.Books") {
		t.Errorf("the closure should call the client it wraps; relations: %v", closure.Relations)
	}
	enclosing, _ := findFact(ff, "shelf.newBooksCache")
	if !hasRelation(enclosing, facts.RelNames, "shelf.newBooksCache.func1") {
		t.Errorf("the constructor should name its closure; relations: %v", enclosing.Relations)
	}
}

// A method-set match the implementer rules reject is kept as a candidate: the
// second match of a one-method interface, and a match beside a stated implementer.
// A signature too many types share keeps none.
func TestImplementerCandidates(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"lib/lib.go": `package lib

import "context"

type Params struct{ Query string }
type Result struct{ IDs []int }

// One method, two types with it.
type searcher interface {
	Search(ctx context.Context, p Params) (*Result, error)
}

type Client struct{}

func (c *Client) Search(ctx context.Context, p Params) (*Result, error) { return nil, nil }

// A decorator stated through its constructor, which may also hand back what it wraps.
type Searcher interface {
	Search(ctx context.Context, p Params) (*Result, error)
}

type flagged struct{ inner Searcher }

func (f *flagged) Search(ctx context.Context, p Params) (*Result, error) { return f.inner.Search(ctx, p) }

func Wrap(inner Searcher, on bool) Searcher {
	if !on {
		return inner
	}
	return &flagged{inner: inner}
}

// One method every type below has.
type namer interface{ Name() string }

type a struct{}
type b struct{}
type c struct{}
type d struct{}
type e struct{}

func (a) Name() string { return "a" }
func (b) Name() string { return "b" }
func (c) Name() string { return "c" }
func (d) Name() string { return "d" }
func (e) Name() string { return "e" }
`,
	})
	props := func(name string) (stated, inferred, cands []string) {
		f, ok := findFact(ff, name)
		if !ok {
			t.Fatalf("expected a fact for %s", name)
		}
		return stringSliceProp(f.Props[propImplementers]), stringSliceProp(f.Props[propImplementersInferred]), stringSliceProp(f.Props[propImplementerCandidates])
	}
	if _, inferred, cands := props("lib.searcher"); len(inferred) != 0 || !slices.Equal(cands, []string{"lib.Client", "lib.flagged"}) {
		t.Errorf("searcher: inferred %v, candidates %v; want both matches as candidates only", inferred, cands)
	}
	if stated, _, cands := props("lib.Searcher"); !slices.Equal(stated, []string{"lib.flagged"}) || !slices.Equal(cands, []string{"lib.Client"}) {
		t.Errorf("Searcher: stated %v, candidates %v; want the wrapper stated and the client a candidate", stated, cands)
	}
	if _, inferred, cands := props("lib.namer"); len(inferred) != 0 || len(cands) != 0 {
		t.Errorf("namer: inferred %v, candidates %v; five matches is a shared signature, want none", inferred, cands)
	}
}
