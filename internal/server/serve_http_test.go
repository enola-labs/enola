package server_test

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/enola-labs/enola/internal/config"
	"github.com/enola-labs/enola/internal/engine"
	"github.com/enola-labs/enola/internal/server"
	"github.com/enola-labs/enola/pkg/bootstrap"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const testToken = "0123456789abcdef0123456789abcdef"

// bearer adds the token to every request the client sends.
type bearer struct {
	token string
	next  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.next.RoundTrip(r)
}

// served is one served instance behind a real HTTP listener, with the root it is
// confined to and a copy of go_sample inside that root.
type served struct {
	url  string
	root string
	repo string
	pool *bootstrap.ServedPool
}

func startServed(t *testing.T, sc bootstrap.ServeOptions, tweak ...func(*config.Config)) *served {
	t.Helper()
	root := t.TempDir()
	repo := copyTree(t, filepath.Join("..", "engine", "testdata", "repos", "go_sample"), root)
	return startServedAt(t, sc, root, repo, tweak...)
}

func startServedAt(t *testing.T, sc bootstrap.ServeOptions, root, repo string, tweak ...func(*config.Config)) *served {
	t.Helper()
	_, cfg := newTestEngine(t)
	for _, f := range tweak {
		f(cfg)
	}
	sc.Roots = []string{root}
	pool, roots, err := bootstrap.NewServedPool(cfg, sc)
	if err != nil {
		t.Fatal(err)
	}
	h, err := pool.HTTPHandler(bootstrap.HTTPOptions{Tokens: []string{testToken}})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return &served{url: ts.URL + "/mcp", root: roots[0], repo: repo, pool: pool}
}

func (s *served) connect(t *testing.T, token string) (*session, error) {
	t.Helper()
	transport := &mcp.StreamableClientTransport{
		Endpoint:   s.url,
		HTTPClient: &http.Client{Transport: bearer{token, http.DefaultTransport}},
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "enola-http-test", Version: "0"}, nil)
	cs, err := client.Connect(context.Background(), transport, nil)
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = cs.Close() })
	return &session{cs: cs, repo: s.repo}, nil
}

func TestServe_RefusesMissingOrWrongToken(t *testing.T) {
	s := startServed(t, bootstrap.ServeOptions{})
	for _, header := range []string{"", "Bearer wrong-token-wrong-token-wrong-token", "Basic " + testToken} {
		req, _ := http.NewRequest(http.MethodPost, s.url, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("Authorization %q: status %d, want 401", header, resp.StatusCode)
		}
	}
	if _, err := s.connect(t, "not-the-token-not-the-token-not-the"); err == nil {
		t.Error("a client with the wrong token connected")
	}
}

func TestServe_RefusesBrowserCrossOrigin(t *testing.T) {
	s := startServed(t, bootstrap.ServeOptions{})
	req, _ := http.NewRequest(http.MethodPost, s.url, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Origin", "https://attacker.example")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-site browser request: status %d, want 403", resp.StatusCode)
	}
}

func TestServe_OnlyMCPPathIsServed(t *testing.T) {
	s := startServed(t, bootstrap.ServeOptions{})
	resp, err := http.Get(strings.TrimSuffix(s.url, "/mcp") + "/")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /: status %d, want 404", resp.StatusCode)
	}
}

func TestServe_GenerateAndQueryOverHTTP(t *testing.T) {
	s := startServed(t, bootstrap.ServeOptions{})
	sess, err := s.connect(t, testToken)
	if err != nil {
		t.Fatal(err)
	}
	if ins := sess.cs.InitializeResult().Instructions; strings.Contains(ins, "ToolSearch") || !strings.Contains(ins, "directories it serves") {
		t.Errorf("served instructions are the agent's, or do not mention confinement:\n%s", ins)
	}
	// The machine-wide receipt belongs to the agent on this machine, not to a server.
	// HOME is shared by every test in this binary, so the check is that this test left
	// it as it found it.
	home, _ := os.UserHomeDir()
	receipt := filepath.Join(home, ".enola", "receipt.json")
	before, _ := os.ReadFile(receipt)
	sess.snapshot(t)
	if after, _ := os.ReadFile(receipt); string(after) != string(before) {
		t.Error("a served instance wrote ~/.enola/receipt.json")
	}
	res := sess.call(t, "query_facts", map[string]any{"kind": "symbol", "output_mode": "summary"})
	if res.IsError || !strings.Contains(text(res), "symbol") {
		t.Fatalf("query_facts over HTTP: %s", text(res))
	}
}

func TestServe_ListsExactlyTheServedTools(t *testing.T) {
	s := startServed(t, bootstrap.ServeOptions{})
	sess, err := s.connect(t, testToken)
	if err != nil {
		t.Fatal(err)
	}
	lr, err := sess.cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tool := range lr.Tools {
		got = append(got, tool.Name)
	}
	sort.Strings(got)
	want := slices.Clone(server.ServedTools)
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Errorf("served tools\n got %v\nwant %v", got, want)
	}
}

// Every tool the stdio server registers must be decided for HTTP: served, or kept
// off on purpose. A new tool that is neither fails here instead of being exposed to
// consumers, or hidden from them, by nobody's decision.
func TestServedToolsAreDecided(t *testing.T) {
	sess := startInMemory(t)
	lr, err := sess.cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	decided := map[string]bool{}
	for _, n := range append(slices.Clone(server.ServedTools), server.UnservedTools...) {
		decided[n] = true
	}
	for _, tool := range lr.Tools {
		if !decided[tool.Name] {
			t.Errorf("tool %q is neither in server.ServedTools nor in server.UnservedTools", tool.Name)
		}
	}
}

func TestServe_UnlistedToolIsRefused(t *testing.T) {
	saved := server.ServedTools
	server.ServedTools = slices.DeleteFunc(slices.Clone(saved), func(n string) bool { return n == "query_facts" })
	t.Cleanup(func() { server.ServedTools = saved })
	s := startServed(t, bootstrap.ServeOptions{})
	sess, err := s.connect(t, testToken)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sess.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "query_facts", Arguments: map[string]any{}}); err == nil {
		t.Error("a tool outside the allowlist ran")
	}
}

func TestServe_RefusesPathsOutsideRoots(t *testing.T) {
	s := startServed(t, bootstrap.ServeOptions{})
	sess, err := s.connect(t, testToken)
	if err != nil {
		t.Fatal(err)
	}
	outside := copyTree(t, filepath.Join("..", "engine", "testdata", "repos", "go_sample"), t.TempDir())
	escape := filepath.Join(s.root, "escape")
	if err := os.Symlink(outside, escape); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		tool string
		args map[string]any
	}{
		{"generate_snapshot", map[string]any{"repo_path": outside}},
		{"generate_snapshot", map[string]any{"repo_path": escape}},
		{"generate_snapshot", map[string]any{"repo_path": filepath.Join(s.root, "..", filepath.Base(filepath.Dir(outside)))}},
		{"generate_snapshot", map[string]any{"repo_paths": []string{s.repo, outside}}},
		{"touched", map[string]any{"repo_path": outside}},
		{"architecture_history", map[string]any{"repo_path": outside}},
		{"diff_snapshot", map[string]any{"baseline": outside}},
		{"compare_receipts", map[string]any{"baseline": outside}},
	}
	for _, c := range cases {
		res := sess.call(t, c.tool, c.args)
		if !res.IsError {
			t.Errorf("%s(%v) ran outside the roots:\n%s", c.tool, c.args, text(res))
		}
	}
	// No default: a call that names no repository is not handed the working directory.
	if res := sess.call(t, "generate_snapshot", map[string]any{}); !res.IsError {
		t.Errorf("generate_snapshot with no repo_path indexed the working directory:\n%s", text(res))
	}
}

// A cluster config inside a root can list a repository outside it. The argument is
// inside, so only the engine's guard on every repository a run reads stops it.
func TestServe_ClusterMemberOutsideRootsIsRefused(t *testing.T) {
	s := startServed(t, bootstrap.ServeOptions{})
	sess, err := s.connect(t, testToken)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	outside := copyTree(t, filepath.Join("..", "engine", "testdata", "repos", "go_sample"), t.TempDir())
	// A folder is a cluster when it holds git repositories; its cluster.yaml, when
	// present, is read as the member list, and that list can name anything.
	folder := filepath.Join(s.root, "cluster")
	var members []string
	for _, name := range []string{"one", "two"} {
		m := filepath.Join(folder, name)
		copyTree(t, filepath.Join("..", "engine", "testdata", "repos", "go_sample"), m)
		if out, err := exec.Command("git", "-C", m, "init", "-q").CombinedOutput(); err != nil {
			t.Fatalf("git init: %v\n%s", err, out)
		}
		members = append(members, m)
	}
	cfgPath := filepath.Join(folder, "cluster.yaml")
	if err := os.WriteFile(cfgPath, []byte("repos:\n  - "+members[0]+"\n  - "+outside+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := sess.call(t, "generate_snapshot", map[string]any{"repo_path": folder})
	if !res.IsError || !strings.Contains(text(res), "outside") {
		t.Errorf("a cluster member outside the roots was indexed (IsError=%v):\n%s", res.IsError, text(res))
	}
}

func TestServe_CallTimeoutCancels(t *testing.T) {
	s := startServed(t, bootstrap.ServeOptions{CallTimeout: time.Nanosecond})
	sess, err := s.connect(t, testToken)
	if err != nil {
		t.Fatal(err)
	}
	res, err := sess.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "generate_snapshot", Arguments: map[string]any{"repo_path": s.repo}})
	if err == nil && !res.IsError {
		t.Error("generate_snapshot finished inside a 1ns call timeout")
	}
}

// tree lists every path under dir with its size and mode, to show a directory was
// left exactly as it was.
func tree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		out[rel] = fmt.Sprintf("%d %s %d", info.Size(), info.Mode(), info.ModTime().UnixNano())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// With an output root, a served instance leaves the repositories it reads exactly as
// it found them: the snapshot, the extractor cache, the pinned baseline and the
// previous run all live under the root, and a restart restores from there.
func TestServe_OutputRootLeavesRepositoriesUntouched(t *testing.T) {
	out := t.TempDir()
	withRoot := func(c *config.Config) { c.Output.Root = out }
	s := startServed(t, bootstrap.ServeOptions{}, withRoot)
	before := tree(t, s.root)

	sess, err := s.connect(t, testToken)
	if err != nil {
		t.Fatal(err)
	}
	sess.snapshot(t)
	if res := sess.call(t, "set_baseline", map[string]any{}); res.IsError {
		t.Fatalf("set_baseline: %s", text(res))
	}
	untouched := func(before map[string]string) {
		t.Helper()
		after := tree(t, s.root)
		if maps.Equal(before, after) {
			return
		}
		for p := range after {
			if _, ok := before[p]; !ok {
				t.Errorf("written into the served repositories: %s", p)
			}
		}
		t.Fatalf("the served repositories changed")
	}
	untouched(before)

	// The consumer's change, then the second half of the loop.
	if err := os.WriteFile(filepath.Join(s.repo, "pkg", "a", "extra.go"), []byte("package a\n\nfunc Extra() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before = tree(t, s.root)
	sess.snapshot(t)
	if res := sess.call(t, "diff_snapshot", map[string]any{}); res.IsError || !strings.Contains(text(res), "Extra") {
		t.Fatalf("diff_snapshot against the pinned baseline does not show the change: %s", text(res))
	}
	if res := sess.call(t, "diff_snapshot", map[string]any{"baseline": "previous"}); res.IsError {
		t.Fatalf("diff_snapshot against the previous run: %s", text(res))
	}

	untouched(before)
	cfg := config.Default()
	cfg.Output.Root = out
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	dir := cfg.OutputDirFor(s.repo)
	for _, f := range []string{"facts.jsonl", "snapshot.meta.json", "baseline/facts.jsonl", "previous/facts.jsonl", "extractor_cache.json"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s is not under the output root: %v", f, err)
		}
	}

	// A restart restores the graph from the output root, not from the repository, and
	// the first session asking for it adopts it instead of indexing.
	again := startServedAt(t, bootstrap.ServeOptions{}, s.root, s.repo, withRoot)
	if err := again.pool.Warm([]string{s.repo}); err != nil {
		t.Fatalf("restoring from the output root: %v", err)
	}
	sess2, err := again.connect(t, testToken)
	if err != nil {
		t.Fatal(err)
	}
	res := sess2.call(t, "generate_snapshot", map[string]any{"repo_path": s.repo})
	if res.IsError || !strings.Contains(text(res), "Reused the graph") {
		t.Fatalf("generate_snapshot after a restore from the output root did not reuse it: %s", text(res))
	}
}

// A folder of repositories is indexed as a cluster, and the stdio server writes a
// cluster.yaml listing them into the folder. A served instance writes nothing there.
func TestServe_ClusterWritesNoConfigIntoTheFolder(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	out := t.TempDir()
	s := startServed(t, bootstrap.ServeOptions{}, func(c *config.Config) { c.Output.Root = out })
	folder := filepath.Join(s.root, "cluster")
	for _, name := range []string{"one", "two"} {
		m := filepath.Join(folder, name)
		copyTree(t, filepath.Join("..", "engine", "testdata", "repos", "go_sample"), m)
		if b, err := exec.Command("git", "-C", m, "init", "-q").CombinedOutput(); err != nil {
			t.Fatalf("git init: %v\n%s", err, b)
		}
	}
	before := tree(t, folder)
	sess, err := s.connect(t, testToken)
	if err != nil {
		t.Fatal(err)
	}
	res := sess.call(t, "generate_snapshot", map[string]any{"repo_path": folder})
	if res.IsError || !strings.Contains(text(res), "indexed as a cluster") {
		t.Fatalf("generate_snapshot on a folder of repositories: %s", text(res))
	}
	if after := tree(t, folder); !maps.Equal(before, after) {
		t.Errorf("a served cluster snapshot wrote into the folder")
	}
}

// addSymbol writes a Go file declaring name into repo, so two copies of go_sample can
// be told apart by what their graphs hold.
func addSymbol(t *testing.T, repo, name string) {
	t.Helper()
	src := "package a\n\nfunc " + name + "() {}\n"
	if err := os.WriteFile(filepath.Join(repo, "pkg", "a", strings.ToLower(name)+".go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (s *session) has(t *testing.T, name string) bool {
	t.Helper()
	res := s.call(t, "query_facts", map[string]any{"name": name, "kind": "symbol", "output_mode": "summary"})
	if res.IsError {
		t.Fatalf("query_facts(%s): %s", name, text(res))
	}
	return !strings.Contains(text(res), "Found **0**")
}

func (s *session) generate(t *testing.T, repo string) string {
	t.Helper()
	res := s.call(t, "generate_snapshot", map[string]any{"repo_path": repo})
	if res.IsError {
		t.Fatalf("generate_snapshot(%s): %s", repo, text(res))
	}
	return text(res)
}

// Each session serves the graph it asked for: one consumer indexing another
// repository does not change what a second consumer is querying.
func TestServe_SessionsHaveTheirOwnGraph(t *testing.T) {
	s := startServed(t, bootstrap.ServeOptions{})
	other := copyAs(t, filepath.Join(s.root, "second"))
	addSymbol(t, s.repo, "OnlyInFirst")
	addSymbol(t, other, "OnlyInSecond")

	a, err := s.connect(t, testToken)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.connect(t, testToken)
	if err != nil {
		t.Fatal(err)
	}
	a.generate(t, s.repo)
	b.generate(t, other)
	if !a.has(t, "OnlyInFirst") || a.has(t, "OnlyInSecond") {
		t.Error("session a does not serve the graph it generated")
	}
	if !b.has(t, "OnlyInSecond") || b.has(t, "OnlyInFirst") {
		t.Error("session b does not serve the graph it generated")
	}
	if st := s.pool.Stats(); st.Graphs != 2 || st.Sessions != 2 {
		t.Errorf("pool holds %d graphs for %d sessions, want 2 and 2", st.Graphs, st.Sessions)
	}
}

// A second session asking for the same unchanged repositories adopts the graph the
// first built; once the tree changes, it builds a new one.
func TestServe_SessionsShareAnUnchangedGraph(t *testing.T) {
	s := startServed(t, bootstrap.ServeOptions{})
	a, err := s.connect(t, testToken)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.connect(t, testToken)
	if err != nil {
		t.Fatal(err)
	}
	if out := a.generate(t, s.repo); strings.Contains(out, "Reused") {
		t.Fatalf("the first generate reused a graph nobody built:\n%s", out)
	}
	if out := b.generate(t, s.repo); !strings.Contains(out, "Reused the graph") {
		t.Fatalf("the second session did not reuse the unchanged graph:\n%s", out)
	}
	if st := s.pool.Stats(); st.Graphs != 1 {
		t.Errorf("pool holds %d graphs for one set of repositories", st.Graphs)
	}

	addSymbol(t, s.repo, "AddedLater")
	if out := b.generate(t, s.repo); strings.Contains(out, "Reused") {
		t.Fatalf("a changed tree was answered from the old graph:\n%s", out)
	}
	if !b.has(t, "AddedLater") {
		t.Error("the rebuilt graph does not hold the change")
	}
	// The order repositories are listed in does not split one graph into two.
	other := copyAs(t, filepath.Join(s.root, "second"))
	if res := a.call(t, "generate_snapshot", map[string]any{"repo_paths": []string{s.repo, other}}); res.IsError {
		t.Fatal(text(res))
	}
	res := b.call(t, "generate_snapshot", map[string]any{"repo_paths": []string{other, s.repo}})
	if res.IsError || !strings.Contains(text(res), "Reused the graph") {
		t.Errorf("the same set listed in another order was not reused:\n%s", text(res))
	}
}

// Past the memory budget the least recently used graph is unloaded, and the session
// that held it gets it back from disk at its next call, with the same answers.
func TestServe_BudgetUnloadsAndReloads(t *testing.T) {
	s := startServed(t, bootstrap.ServeOptions{MemoryBudget: 1}) // every graph is over it
	addSymbol(t, s.repo, "Survives")
	a, err := s.connect(t, testToken)
	if err != nil {
		t.Fatal(err)
	}
	a.generate(t, s.repo)
	if st := s.pool.Stats(); st.Evictions == 0 || st.Graphs != 0 {
		t.Fatalf("a graph over the budget stayed loaded: %+v", st)
	}
	res := a.call(t, "query_facts", map[string]any{"name": "Survives", "kind": "symbol", "output_mode": "summary"})
	if res.IsError || strings.Contains(text(res), "Found **0**") {
		t.Fatalf("the reloaded graph lost its facts:\n%s", text(res))
	}
	if !strings.Contains(text(res), "reloaded from disk") {
		t.Errorf("the session was not told its graph was reloaded:\n%s", text(res))
	}
}

// Many sessions indexing and querying one repository at once: every call succeeds,
// and the artifacts they all write into one output directory are one run's, whole.
func TestServe_ConcurrentSessionsOnOneRepository(t *testing.T) {
	s := startServed(t, bootstrap.ServeOptions{MaxConcurrent: 4})
	const n = 8
	sessions := make([]*session, n)
	for i := range sessions {
		sess, err := s.connect(t, testToken)
		if err != nil {
			t.Fatal(err)
		}
		sessions[i] = sess
	}
	errs := make(chan string, n*3)
	var wg sync.WaitGroup
	for _, sess := range sessions {
		wg.Add(1)
		go func(sess *session) {
			defer wg.Done()
			for _, call := range []struct {
				name string
				args map[string]any
			}{
				{"generate_snapshot", map[string]any{"repo_path": s.repo}},
				{"query_facts", map[string]any{"kind": "symbol", "output_mode": "summary"}},
				{"generate_snapshot", map[string]any{"repo_path": s.repo, "fresh": true}},
			} {
				res, err := sess.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: call.name, Arguments: call.args})
				switch {
				case err != nil:
					errs <- call.name + ": " + err.Error()
				case res.IsError:
					errs <- call.name + ": " + text(res)
				}
			}
		}(sess)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	if _, err := bootstrap.LoadSnapshotDir(filepath.Join(s.repo, ".enola")); err != nil {
		t.Errorf("the output directory written by every session at once does not load: %v", err)
	}
}

// A session that ends is forgotten; the graph it built stays for the next one.
func TestServe_EndedSessionIsDetached(t *testing.T) {
	s := startServed(t, bootstrap.ServeOptions{})
	a, err := s.connect(t, testToken)
	if err != nil {
		t.Fatal(err)
	}
	a.generate(t, s.repo)
	if err := a.cs.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for s.pool.Stats().Sessions != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("a closed session is still attached: %+v", s.pool.Stats())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if st := s.pool.Stats(); st.Graphs != 1 {
		t.Errorf("the closed session's graph left the pool: %+v", st)
	}
	b, err := s.connect(t, testToken)
	if err != nil {
		t.Fatal(err)
	}
	if out := b.generate(t, s.repo); !strings.Contains(out, "Reused the graph") {
		t.Errorf("the next session did not reuse the graph:\n%s", out)
	}
}

// copyAs copies go_sample to dst, a directory of another name, so its repository
// label differs from the original's.
func copyAs(t *testing.T, dst string) string {
	t.Helper()
	tmp := copyTree(t, filepath.Join("..", "engine", "testdata", "repos", "go_sample"), t.TempDir())
	if err := os.Rename(tmp, dst); err != nil {
		t.Fatal(err)
	}
	return dst
}

// Two consumers on one repository share its pinned baseline, since it lives in the
// repository's output directory. Each is told when the baseline it diffs against was
// pinned by the other, by token, while the other is connected; once it is gone, the
// pin is simply the baseline.
func TestServe_PinsAreAttributedToTheirSession(t *testing.T) {
	s := startServed(t, bootstrap.ServeOptions{})
	a, err := s.connect(t, testToken)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.connect(t, testToken)
	if err != nil {
		t.Fatal(err)
	}
	const other = "another session of this server (token " // + the token id
	a.generate(t, s.repo)
	if res := a.call(t, "set_baseline", map[string]any{}); res.IsError {
		t.Fatal(text(res))
	}
	if out := text(a.call(t, "diff_snapshot", map[string]any{})); strings.Contains(out, other) {
		t.Errorf("a session's own pin was attributed to another session:\n%s", out)
	}

	mark := engine.ReadSessionMark(filepath.Join(s.repo, ".enola", "baseline"), engine.PinMarkFile)
	if mark == nil || !strings.HasPrefix(mark.Session, server.TokenID(testToken)+"/") {
		t.Fatalf("the pin mark does not name the served session: %+v", mark)
	}

	b.generate(t, s.repo)
	if out := text(b.call(t, "diff_snapshot", map[string]any{})); !strings.Contains(out, other+server.TokenID(testToken)+")") {
		t.Errorf("a diff against another connected session's pin did not say so:\n%s", out)
	}
	if out := text(b.call(t, "set_baseline", map[string]any{})); !strings.Contains(out, "This replaced a baseline another running session of this server") {
		t.Errorf("re-pinning over another connected session's baseline did not say so:\n%s", out)
	}
	if out := text(a.call(t, "diff_snapshot", map[string]any{})); !strings.Contains(out, other) {
		t.Errorf("the first session was not told its baseline was replaced:\n%s", out)
	}

	if err := b.cs.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for s.pool.Stats().Sessions != 1 {
		if time.Now().After(deadline) {
			t.Fatal("the closed session is still attached")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if out := text(a.call(t, "diff_snapshot", map[string]any{})); strings.Contains(out, other) {
		t.Errorf("a pin by a session that has ended is still attributed to it:\n%s", out)
	}
}
