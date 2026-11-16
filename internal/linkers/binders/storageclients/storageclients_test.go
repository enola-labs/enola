package storageclients

import (
	"context"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func calls(name, file, repo string, targets ...string) facts.Fact {
	f := facts.Fact{Kind: facts.KindSymbol, Name: name, File: file, Repo: repo}
	for _, t := range targets {
		f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelCalls, Target: t})
	}
	return f
}

func imports(file, repo, path string) facts.Fact {
	return facts.Fact{Kind: facts.KindDependency, Name: "x -> " + path, File: file, Repo: repo,
		Relations: []facts.Relation{{Kind: facts.RelImports, Target: path}}}
}

func dependsOn(store *facts.Store, name string) []string {
	var out []string
	for _, f := range store.FactsRef() {
		if f.Kind != facts.KindSymbol || f.Name != name {
			continue
		}
		for _, r := range f.Relations {
			if r.Kind == facts.RelDependsOn {
				out = append(out, r.Target)
			}
		}
	}
	return out
}

// A method call on a Redis client type, under the package name in a file importing
// the library or under its import path, is a use of the repository's Redis. A call
// to the package's constructor, or a type of the same package name the file does not
// import from the library, is not.
func TestBind_RedisClientCalls(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		imports("shop/cache/redis.go", "shop", "github.com/redis/go-redis/v9"),
		calls("shop/cache.Redis.Get", "shop/cache/redis.go", "shop", "redis.UniversalClient.Get"),
		calls("shop/setup.newRedis", "shop/cache/redis.go", "shop", "redis.NewUniversalClient"),
		calls("shop/carts.Store.Load", "shop/carts/store.go", "shop", "github.com/redis/go-redis/v9.Client.HGetAll"),
		// Its own package named redis, not the client library.
		calls("shop/queue.Worker.Run", "shop/queue/worker.go", "shop", "redis.Queue.Pop"),
		imports("ledger/cache.go", "ledger", "github.com/redis/go-redis/v9"),
		calls("ledger/cache.Get", "ledger/cache.go", "ledger", "redis.Client.Get"),
	)
	if err := New().Bind(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"shop/cache.Redis.Get":  "redis:shop",
		"shop/carts.Store.Load": "redis:shop",
		"ledger/cache.Get":      "redis:ledger",
	} {
		if got := dependsOn(store, name); len(got) != 1 || got[0] != want {
			t.Errorf("%s depends on %v, want [%s]", name, got, want)
		}
	}
	for _, name := range []string{"shop/setup.newRedis", "shop/queue.Worker.Run"} {
		if got := dependsOn(store, name); len(got) != 0 {
			t.Errorf("%s depends on %v, want nothing", name, got)
		}
	}
	nodes := map[string]bool{}
	for _, f := range store.ByKind(facts.KindStorage) {
		nodes[f.Name] = f.PropString("engine") == "redis" && f.PropString("storage_kind") == "cache"
	}
	if len(nodes) != 2 || !nodes["redis:shop"] || !nodes["redis:ledger"] {
		t.Errorf("storage nodes = %v, want one Redis cache per repository", nodes)
	}
}

// Binding a graph that was bound before adds nothing: a refresh restores a bound graph
// and binds it again, and every Redis node and edge was added a second time. A store
// no member uses any longer goes, with its edges.
func TestBind_AgainAddsNothing(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		imports("shop/cache/redis.go", "shop", "github.com/redis/go-redis/v9"),
		calls("shop/cache.Redis.Get", "shop/cache/redis.go", "shop", "redis.UniversalClient.Get"),
	)
	for range 2 {
		if err := New().Bind(context.Background(), store); err != nil {
			t.Fatal(err)
		}
	}
	nodes := 0
	for _, f := range store.ByKind(facts.KindStorage) {
		if f.Name == "redis:shop" {
			nodes++
		}
	}
	if nodes != 1 {
		t.Errorf("%d redis:shop nodes after binding twice, want 1", nodes)
	}
	if got := dependsOn(store, "shop/cache.Redis.Get"); len(got) != 1 {
		t.Errorf("depends_on after binding twice = %v, want one", got)
	}

	// The call is gone (the member was re-read without it): so are the node and edge.
	store.UpdateWhere(func(f *facts.Fact) {
		if f.Name == "shop/cache.Redis.Get" {
			f.Relations = []facts.Relation{f.Relations[len(f.Relations)-1]} // the depends_on only
		}
	})
	if err := New().Bind(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	if n := len(store.ByKind(facts.KindStorage)); n != 0 {
		t.Errorf("%d storage nodes left for a store nothing uses", n)
	}
	if got := dependsOn(store, "shop/cache.Redis.Get"); len(got) != 0 {
		t.Errorf("depends_on left to a store nothing uses: %v", got)
	}
}
