package server_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enola-labs/enola/pkg/bootstrap"
)

// twoRepos lays out two copies of go_sample in one root, api and web, each with a
// symbol only it declares.
func twoRepos(t *testing.T) (root, api, web string) {
	t.Helper()
	root = t.TempDir()
	src := filepath.Join("..", "engine", "testdata", "repos", "go_sample")
	api = filepath.Join(root, "api")
	web = filepath.Join(root, "web")
	for _, dst := range []string{api, web} {
		if err := os.Rename(copyTree(t, src, t.TempDir()), dst); err != nil {
			t.Fatal(err)
		}
	}
	addSymbol(t, api, "OnlyInAPI")
	addSymbol(t, web, "OnlyInWeb")
	return root, api, web
}

// A session's graph is unloaded; another session then indexes the same repository
// linked with a second one, which rewrites its artifacts with the union. The first
// session must not be served that union as its own graph: its call is refused, and
// generate_snapshot gives it its own graph back.
func TestServePool_UnloadedGraphIsNotReloadedFromAnotherSnapshot(t *testing.T) {
	root, api, web := twoRepos(t)
	s := startServedAt(t, bootstrap.ServeOptions{MemoryBudget: 1}, root, api) // every graph is over it
	a, err := s.connect(t, testToken)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.connect(t, testToken)
	if err != nil {
		t.Fatal(err)
	}
	a.generate(t, api)
	if res := b.call(t, "generate_snapshot", map[string]any{"repo_paths": []string{api, web}}); res.IsError {
		t.Fatal(text(res))
	}

	res := a.call(t, "query_facts", map[string]any{"name": "OnlyInWeb", "kind": "symbol", "output_mode": "summary"})
	if !res.IsError {
		t.Fatalf("the session that indexed api alone was answered from the api+web graph:\n%s", text(res))
	}
	if !strings.Contains(text(res), "another snapshot") || !strings.Contains(text(res), "generate_snapshot") {
		t.Errorf("the refusal does not say why or what to do:\n%s", text(res))
	}

	a.generate(t, api)
	if a.has(t, "OnlyInWeb") {
		t.Error("after generate_snapshot the session still sees web")
	}
	if !a.has(t, "OnlyInAPI") {
		t.Error("after generate_snapshot the session does not see api")
	}
}

// A session keeps the snapshot it built after another session rebuilds the same
// repository changed: two graphs, one only that session holds. The budget counts that
// one too and unloads it, where it used to be invisible to the pool and held forever.
func TestServePool_BudgetCountsGraphsOnlySessionsHold(t *testing.T) {
	for _, tc := range []struct {
		budget   int64
		wantHeld int
	}{{0, 2}, {1, 0}} {
		s := startServed(t, bootstrap.ServeOptions{MemoryBudget: tc.budget})
		a, err := s.connect(t, testToken)
		if err != nil {
			t.Fatal(err)
		}
		b, err := s.connect(t, testToken)
		if err != nil {
			t.Fatal(err)
		}
		a.generate(t, s.repo)
		addSymbol(t, s.repo, "Changed")
		b.generate(t, s.repo)
		if st := s.pool.Stats(); st.Held != tc.wantHeld {
			t.Errorf("budget %d: %d graphs held, want %d: %+v", tc.budget, st.Held, tc.wantHeld, st)
		}
	}
}
