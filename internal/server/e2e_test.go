package server_test

// End-to-end tests for the MCP tool surface — the contract AI agents actually
// hit. The existing server_test.go (package server) exercises internal helper
// methods against hand-built stores; this file instead spins up the real
// engine+server over an in-memory MCP transport and drives each registered tool
// through the wire (JSON args in, CallToolResult out), so the tool handlers,
// arg unmarshaling, name resolution, and result rendering are all covered.
//
// Fixtures are shared with the engine golden tests (../engine/testdata/repos).
// We use go_sample because the Go extractor is stdlib-based and its fact graph
// is small and stable: modules ".", "pkg/a", "pkg/b"; symbols "..main",
// "pkg/a.Alpha", "pkg/b.Beta"; with a deliberate pkg/a<->pkg/b import cycle.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/config"
	"github.com/enola-labs/enola/pkg/bootstrap"
	"github.com/enola-labs/enola/pkg/cli"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// expectedTools is the OSS MCP tool surface. If a tool is added or renamed,
// this list (and the per-tool coverage below) should move with it.
var expectedTools = []string{
	"generate_snapshot",
	"query_facts",
	"explore",
	"show_symbol",
	"traverse",
	"find_path",
	"impact_analysis",
	"governing_intent",
	"constraints_for",
	"plan_check",
	"coverage_report",
}

// session bundles a connected client and the temp repo the server was pointed at.
type session struct {
	cs   *mcp.ClientSession
	repo string
}

// startInMemory wires bootstrap.NewEngine + NewServer to an in-memory transport
// and returns a connected client session plus a fresh temp copy of go_sample.
func startInMemory(t *testing.T) *session {
	t.Helper()
	eng, cfg := newTestEngine(t)
	s := connect(t, eng, cfg)
	s.repo = copyTree(t, filepath.Join("..", "engine", "testdata", "repos", "go_sample"), t.TempDir())
	return s
}

// newTestEngine builds a bootstrap engine with all OSS plugins and a config that
// falls back to defaults (no config file on disk).
func newTestEngine(t *testing.T) (*bootstrap.Engine, *config.Config) {
	t.Helper()
	eng, cfg, err := bootstrap.NewEngine(bootstrap.Options{
		ConfigPath: filepath.Join(t.TempDir(), "no-such-config.yaml"),
	})
	if err != nil {
		t.Fatalf("bootstrap.NewEngine: %v", err)
	}
	return eng, cfg
}

// connect wires the given engine into an MCP server over an in-memory transport
// and returns a connected client session.
func connect(t *testing.T, eng *bootstrap.Engine, cfg *config.Config) *session {
	t.Helper()
	ctx := context.Background()

	srv, err := bootstrap.NewServer(eng, cfg)
	if err != nil {
		t.Fatalf("bootstrap.NewServer: %v", err)
	}

	serverT, clientT := mcp.NewInMemoryTransports()
	if _, err := srv.MCP().Connect(ctx, serverT, nil); err != nil {
		t.Fatalf("server Connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "enola-test", Version: "0"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client Connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	return &session{cs: cs}
}

// call invokes a tool and fails the test on transport error (a transport error
// is distinct from a tool-level IsError, which several assertions check for).
func (s *session) call(t *testing.T, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := s.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(%s) transport error: %v", name, err)
	}
	return res
}

// text concatenates the text content of a tool result.
func text(res *mcp.CallToolResult) string {
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

// snapshot runs generate_snapshot against the session's temp repo so the
// server's engine is populated before the other tools are exercised.
func (s *session) snapshot(t *testing.T) {
	t.Helper()
	res := s.call(t, "generate_snapshot", map[string]any{"repo_path": s.repo})
	if res.IsError {
		t.Fatalf("generate_snapshot returned error: %s", text(res))
	}
	if !strings.Contains(text(res), "Facts:") {
		t.Fatalf("generate_snapshot summary missing 'Facts:'; got:\n%s", text(res))
	}
}

func TestE2E_ListTools(t *testing.T) {
	s := startInMemory(t)
	res, err := s.cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	got := map[string]bool{}
	for _, tool := range res.Tools {
		got[tool.Name] = true
	}
	for _, name := range expectedTools {
		if !got[name] {
			t.Errorf("expected tool %q to be registered; registered: %v", name, keys(got))
		}
	}
}

// The `--list` catalogue in pkg/cli is hand-written (the registered MCP
// descriptions are multi-paragraph agent prompts, unusable in a terminal list),
// so nothing but this test keeps it honest: registering a tool without
// cataloguing it — or cataloguing one that was renamed away — fails here.
func TestE2E_ToolCatalogueMatchesRegisteredTools(t *testing.T) {
	s := startInMemory(t)
	res, err := s.cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	registered := map[string]bool{}
	for _, tool := range res.Tools {
		registered[tool.Name] = true
	}
	catalogued := map[string]bool{}
	for _, entry := range cli.OSSTools() {
		if entry.Description == "" {
			t.Errorf("catalogue entry %q has no description", entry.Name)
		}
		catalogued[entry.Name] = true
	}

	for name := range registered {
		if !catalogued[name] {
			t.Errorf("tool %q is registered but missing from cli.OSSTools()", name)
		}
	}
	for name := range catalogued {
		if !registered[name] {
			t.Errorf("tool %q is in cli.OSSTools() but not registered by the server", name)
		}
	}
}

func TestE2E_GenerateAndQuery(t *testing.T) {
	s := startInMemory(t)
	s.snapshot(t)

	// query_facts(kind=module) must surface the fixture's modules.
	out := text(s.call(t, "query_facts", map[string]any{"kind": "module"}))
	for _, mod := range []string{"pkg/a", "pkg/b"} {
		if !strings.Contains(out, mod) {
			t.Errorf("query_facts(kind=module) missing %q; got:\n%s", mod, out)
		}
	}

	// kinds= batch filter is OR within the dimension.
	out = text(s.call(t, "query_facts", map[string]any{"kinds": []string{"symbol"}, "output_mode": "names"}))
	if !strings.Contains(out, "Alpha") || !strings.Contains(out, "Beta") {
		t.Errorf("query_facts(kinds=[symbol]) missing known symbols; got:\n%s", out)
	}
}

func TestE2E_ShowSymbol(t *testing.T) {
	s := startInMemory(t)
	s.snapshot(t)

	out := text(s.call(t, "show_symbol", map[string]any{"name": "Alpha", "context_lines": 10}))
	if !strings.Contains(out, "Alpha") {
		t.Errorf("show_symbol(Alpha) missing symbol name; got:\n%s", out)
	}
	if !strings.Contains(out, "a.go") {
		t.Errorf("show_symbol(Alpha) missing source file reference; got:\n%s", out)
	}
}

func TestE2E_Explore(t *testing.T) {
	s := startInMemory(t)
	s.snapshot(t)

	res := s.call(t, "explore", map[string]any{"focus": "pkg/a"})
	if res.IsError {
		t.Fatalf("explore(pkg/a) errored: %s", text(res))
	}
	if !strings.Contains(text(res), "Alpha") {
		t.Errorf("explore(pkg/a) should mention symbol Alpha; got:\n%s", text(res))
	}
}

func TestE2E_Traverse(t *testing.T) {
	s := startInMemory(t)
	s.snapshot(t)

	// pkg/b imports pkg/a, so traversing reverse from pkg/a must reach pkg/b.
	out := text(s.call(t, "traverse", map[string]any{
		"start": "pkg/a", "direction": "reverse", "output_mode": "compact",
	}))
	if !strings.Contains(out, "pkg/b") {
		t.Errorf("traverse(reverse, pkg/a) should reach pkg/b; got:\n%s", out)
	}
}

func TestE2E_FindPath(t *testing.T) {
	s := startInMemory(t)
	s.snapshot(t)

	// Positive: main calls Alpha, so a forward path exists.
	res := s.call(t, "find_path", map[string]any{"from": "..main", "to": "pkg/a.Alpha"})
	if res.IsError {
		t.Fatalf("find_path(main -> Alpha) errored: %s", text(res))
	}
	if !strings.Contains(text(res), "Alpha") {
		t.Errorf("find_path(main -> Alpha) should report a path through Alpha; got:\n%s", text(res))
	}

	// Negative: Beta only reaches Alpha/Beta (the cycle), never main, so there
	// is no forward path. This must be a graceful "no path" answer, not an error.
	res = s.call(t, "find_path", map[string]any{"from": "pkg/b.Beta", "to": "..main"})
	if res.IsError {
		t.Fatalf("find_path with no route should not be a tool error; got:\n%s", text(res))
	}
	if !strings.Contains(strings.ToLower(text(res)), "no path") {
		t.Errorf("find_path(Beta -> main) should report no path; got:\n%s", text(res))
	}
}

func TestE2E_ImpactAnalysis(t *testing.T) {
	s := startInMemory(t)
	s.snapshot(t)

	// main and Beta both call Alpha, so changing Alpha impacts 2 dependents.
	// Summary mode reports the count + hotspot modules; compact mode names them.
	out := text(s.call(t, "impact_analysis", map[string]any{"target": "pkg/a.Alpha"}))
	if !strings.Contains(out, "2 total") {
		t.Errorf("impact_analysis(Alpha) should report 2 dependents; got:\n%s", out)
	}
	out = text(s.call(t, "impact_analysis", map[string]any{"target": "pkg/a.Alpha", "output_mode": "compact"}))
	if !strings.Contains(out, "Beta") {
		t.Errorf("impact_analysis(Alpha, compact) should list Beta as a dependent; got:\n%s", out)
	}
}

// TestE2E_GoverningIntent drives the reverse query through the wire in both
// directions, plus its two distinct empty states: a snapshot with no compiled
// pages answers "not asked", and a governed snapshot answers per target.
func TestE2E_GoverningIntent(t *testing.T) {
	s := startInMemory(t)
	s.snapshot(t)

	out := text(s.call(t, "governing_intent", map[string]any{"target": "pkg/a/a.go"}))
	if !strings.Contains(out, "not asked") {
		t.Errorf("a snapshot without compiled pages must answer 'not asked'; got:\n%s", out)
	}

	repo := filepath.Join(t.TempDir(), "backend")
	if err := os.MkdirAll(filepath.Join(repo, "pkg/a"), 0o755); err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]string{
		"go.mod":      "module example.com/backend\n\ngo 1.21\n",
		"main.go":     "package main\n\nfunc main() {}\n",
		"pkg/a/a.go":  "package a\n\nfunc Alpha() {}\n",
		"decision.md": "---\nenola_intent:\n  page:\n    type: decision\n    status: accepted\n    relations:\n      - {rel: part-of, to: epic.md}\n    anchors:\n      - {repo: backend, path: pkg/a}\n---\nbody\n",
		"epic.md":     "---\nenola_intent:\n  page:\n    type: epic\n    status: living\n---\nbody\n",
	} {
		full := filepath.Join(repo, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if res := s.call(t, "generate_snapshot", map[string]any{"repo_path": repo, "fresh": true}); res.IsError {
		t.Fatalf("generate_snapshot(backend) returned error: %s", text(res))
	}

	out = text(s.call(t, "governing_intent", map[string]any{"target": "pkg/a/a.go"}))
	if !strings.Contains(out, "decision.md (decision, accepted)") {
		t.Errorf("governed file must list its page with type/status; got:\n%s", out)
	}
	if !strings.Contains(out, "part-of epic.md (epic, living)") {
		t.Errorf("governing page must carry its relation trail with joined target meta; got:\n%s", out)
	}

	out = text(s.call(t, "governing_intent", map[string]any{"target": "main.go"}))
	if !strings.Contains(out, "asked, none governs") {
		t.Errorf("an unanchored file in a governed snapshot must answer 'asked, none governs'; got:\n%s", out)
	}

	out = text(s.call(t, "governing_intent", map[string]any{"target": "decision.md"}))
	if !strings.Contains(out, "backend pkg/a") || !strings.Contains(out, "measured fact(s)") {
		t.Errorf("a page target must report anchor coverage; got:\n%s", out)
	}

	out = text(s.call(t, "governing_intent", map[string]any{"target": "no/such/thing.xyz"}))
	if !strings.Contains(out, "Nothing measured matches") {
		t.Errorf("an unmatched target must say so; got:\n%s", out)
	}

	out = text(s.call(t, "show_symbol", map[string]any{"name": "Alpha"}))
	if !strings.Contains(out, "Governed by: decision.md (decision, accepted)") {
		t.Errorf("show_symbol must surface the governing pages beside the source; got:\n%s", out)
	}
}

// TestE2E_ConstraintsFor drives the pre-edit contract through the wire: a
// snapshot with no declared components answers "not asked"; a governed
// snapshot answers per target with the binding rules and the violations whose
// evidence names it — including a path no fact measures, which is the pre-edit
// point.
func TestE2E_ConstraintsFor(t *testing.T) {
	s := startInMemory(t)
	s.snapshot(t)

	out := text(s.call(t, "constraints_for", map[string]any{"target": "pkg/a/a.go"}))
	if !strings.Contains(out, "not asked") {
		t.Errorf("a snapshot without declared components must answer 'not asked'; got:\n%s", out)
	}

	// Constraints are the repo's declared desired architecture, so they live in
	// the always-validated declaration file — not on a page.
	decl := "components:\n" +
		"  - {name: pkg-a, match: [pkg/a/**]}\n" +
		"  - {name: pkg-b, match: [pkg/b/**]}\n" +
		"rules:\n" +
		"  - {id: a-avoids-b, forbid: pkg-a, to: pkg-b, via: imports,\n" +
		"     because: a must stay deliverable without b}\n"
	if err := os.WriteFile(filepath.Join(s.repo, "enola-intent.yaml"), []byte(decl), 0o644); err != nil {
		t.Fatal(err)
	}
	if res := s.call(t, "generate_snapshot", map[string]any{"repo_path": s.repo, "fresh": true}); res.IsError {
		t.Fatalf("generate_snapshot returned error: %s", text(res))
	}

	out = text(s.call(t, "constraints_for", map[string]any{"target": "pkg/a/a.go"}))
	for _, want := range []string{
		"pkg-a",
		"a-avoids-b",
		"pkg-a must not reach pkg-b via imports",
		"a must stay deliverable without b",
		"ratchet",
		"Constraint a-avoids-b violated",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("contract for a bound file must carry %q; got:\n%s", want, out)
		}
	}

	// A file nobody has written yet still answers by pattern — the pre-edit case.
	out = text(s.call(t, "constraints_for", map[string]any{"target": "pkg/a/new_file.go"}))
	if !strings.Contains(out, "pkg-a must not reach pkg-b via imports") {
		t.Errorf("an unwritten file inside a component must still get its contract; got:\n%s", out)
	}

	out = text(s.call(t, "constraints_for", map[string]any{"target": "elsewhere/free.go"}))
	if strings.Contains(out, "a-avoids-b") {
		t.Errorf("an unbound target must bind to nothing; got:\n%s", out)
	}
}

func TestE2E_PlanCheck(t *testing.T) {
	s := startInMemory(t)
	s.snapshot(t)

	out := text(s.call(t, "plan_check", map[string]any{"paths": []string{"pkg/a/a.go"}}))
	if !strings.Contains(out, `"constraints_declared": false`) {
		t.Errorf("plan_check on an undeclared repo must say constraints_declared false; got:\n%s", out)
	}

	decl := "components:\n" +
		"  - {name: pkg-a, match: [pkg/a/**]}\n" +
		"  - {name: pkg-b, match: [pkg/b/**]}\n" +
		"rules:\n" +
		"  - {id: a-avoids-b, forbid: pkg-a, to: pkg-b, via: imports,\n" +
		"     because: a must stay deliverable without b}\n"
	if err := os.WriteFile(filepath.Join(s.repo, "enola-intent.yaml"), []byte(decl), 0o644); err != nil {
		t.Fatal(err)
	}
	if res := s.call(t, "generate_snapshot", map[string]any{"repo_path": s.repo, "fresh": true}); res.IsError {
		t.Fatalf("generate_snapshot returned error: %s", text(res))
	}

	out = text(s.call(t, "plan_check", map[string]any{"paths": []string{"pkg/a/new_file.go"}}))
	for _, want := range []string{"pkg-a", "a-avoids-b", "a must stay deliverable without b", `"no_rule_governs": false`} {
		if !strings.Contains(out, want) {
			t.Errorf("plan_check for a governed pre-edit path must carry %q; got:\n%s", want, out)
		}
	}

	patch := "--- /dev/null\n+++ b/pkg/a/extra.go\n@@ -0,0 +1,3 @@\n+package a\n+\n+func Extra() int { return 3 }\n"
	out = text(s.call(t, "plan_check", map[string]any{"patch": patch}))
	if !strings.Contains(out, `"unchanged"`) || !strings.Contains(out, "a-avoids-b") {
		t.Errorf("the counterfactual must carry the pre-existing a-avoids-b breach as unchanged; got:\n%s", out)
	}
	if !strings.Contains(out, `"new": []`) {
		t.Errorf("an additive patch must introduce no new breach; got:\n%s", out)
	}
	if !strings.Contains(out, `"source": "enola-intent.yaml"`) {
		t.Errorf("governance must cite the working tree's declaring file, not the engine's resolved copy; got:\n%s", out)
	}

	res := s.call(t, "plan_check", map[string]any{"patch": patch})
	if res.IsError {
		t.Fatalf("plan_check errored: %s", text(res))
	}

	res = s.call(t, "plan_check", map[string]any{"patch": "garbage, not a diff\n"})
	if !res.IsError || !strings.Contains(text(res), "no file changes") {
		t.Errorf("a non-diff patch must be a named error; got IsError=%v:\n%s", res.IsError, text(res))
	}

	drifted := filepath.Join(s.repo, "pkg", "b", "b.go")
	data, err := os.ReadFile(drifted)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(drifted, append(data, []byte("\nfunc Drifted() {}\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	out = text(s.call(t, "plan_check", map[string]any{"paths": []string{"pkg/a/a.go"}}))
	if !strings.Contains(out, `"staleness"`) || !strings.Contains(out, "modified") {
		t.Errorf("plan_check over a drifted tree must state the staleness; got:\n%s", out)
	}
}

func TestE2E_CoverageReport(t *testing.T) {
	s := startInMemory(t)
	s.snapshot(t)

	// coverage_report is registered in OSS; on a single Go repo it should return
	// a non-error report (its content varies, so we only assert it succeeds).
	res := s.call(t, "coverage_report", map[string]any{})
	if res.IsError {
		t.Fatalf("coverage_report errored: %s", text(res))
	}
	if strings.TrimSpace(text(res)) == "" {
		t.Errorf("coverage_report returned empty output")
	}
}

// TestE2E_RequiredArgValidation checks that tools with required arguments fail
// (as a tool error or a transport error) when the required arg is omitted,
// rather than silently succeeding.
func TestE2E_RequiredArgValidation(t *testing.T) {
	s := startInMemory(t)
	s.snapshot(t)

	cases := []struct {
		tool string
		args map[string]any
	}{
		{"explore", map[string]any{}},
		{"traverse", map[string]any{}},
		{"find_path", map[string]any{"from": "pkg/a"}}, // missing "to"
		{"impact_analysis", map[string]any{}},
		{"show_symbol", map[string]any{}},
		{"constraints_for", map[string]any{}},
	}
	for _, c := range cases {
		t.Run(c.tool, func(t *testing.T) {
			res, err := s.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: c.tool, Arguments: c.args})
			if err != nil {
				return // transport-level rejection is an acceptable failure mode
			}
			if !res.IsError {
				t.Errorf("%s with missing required arg should be an error; got:\n%s", c.tool, text(res))
			}
		})
	}
}

// writeSnapshotToDisk indexes repo with a throwaway engine and writes its
// artifacts (including .enola/facts.jsonl) to disk, simulating a workspace that
// already has a prior snapshot on disk for AutoLoadSnapshot to pick up.
func writeSnapshotToDisk(t *testing.T, repo string) {
	t.Helper()
	eng, _ := newTestEngine(t)
	if _, err := eng.GenerateSnapshot(context.Background(), repo, false); err != nil {
		t.Fatalf("prep GenerateSnapshot(%s): %v", repo, err)
	}
	if err := eng.WriteArtifacts(repo); err != nil {
		t.Fatalf("prep WriteArtifacts(%s): %v", repo, err)
	}
}

// TestE2E_AutoLoadedSnapshotResetOnFreshGenerate is a regression test for the
// bug where a snapshot auto-loaded at startup caused the first
// generate_snapshot(append=false) to silently switch to append mode, carrying
// the auto-loaded repo forward as a stale service node. A non-append call must
// discard the auto-loaded state and index only the requested repo.
func TestE2E_AutoLoadedSnapshotResetOnFreshGenerate(t *testing.T) {
	// repoA: a fixture whose snapshot we pre-write to disk so AutoLoadSnapshot
	// picks it up at startup. ts_sample gives a distinct repo label from repoB.
	repoA := copyTree(t, filepath.Join("..", "engine", "testdata", "repos", "ts_sample"), t.TempDir())
	writeSnapshotToDisk(t, repoA)

	// Build an engine pointed at repoA and auto-load its snapshot, exactly as the
	// server does on startup in a pre-populated workspace.
	eng, cfg := newTestEngine(t)
	cfg.Repo = repoA
	bootstrap.AutoLoadSnapshot(eng, cfg)
	if eng.Store().Count() == 0 {
		t.Fatalf("expected AutoLoadSnapshot to populate the store from %s", repoA)
	}
	s := connect(t, eng, cfg)

	// First generate_snapshot, for a DIFFERENT repo, with no append. It must reset.
	repoB := copyTree(t, filepath.Join("..", "engine", "testdata", "repos", "go_sample"), t.TempDir())
	res := s.call(t, "generate_snapshot", map[string]any{"repo_path": repoB})
	if res.IsError {
		t.Fatalf("generate_snapshot(repoB) errored: %s", text(res))
	}
	if out := text(res); strings.Contains(out, "Multi-repo mode active") || strings.Contains(out, "auto-enabled") {
		t.Errorf("non-append generate_snapshot over auto-loaded state must not enter append mode; got:\n%s", out)
	}

	// coverage_report must report no service nodes (single-repo) — the stale
	// repoA service must be gone.
	if cov := text(s.call(t, "coverage_report", map[string]any{})); !strings.Contains(cov, "No service nodes") {
		t.Errorf("expected no service nodes after fresh single-repo snapshot; got:\n%s", cov)
	}

	// repoA's facts must have been discarded entirely.
	repoALabel := filepath.Base(repoA)
	if q := text(s.call(t, "query_facts", map[string]any{"kind": "service"})); strings.Contains(q, repoALabel) {
		t.Errorf("expected repoA (%s) to be discarded, but it still appears as a service node; got:\n%s", repoALabel, q)
	}
}

// TestE2E_MultiRepoAppendStillAccumulates guards against the session-flag gate
// over-resetting: a genuine multi-repo flow (first snapshot resets, then
// append=true) must still accumulate both repos as service nodes.
func TestE2E_MultiRepoAppendStillAccumulates(t *testing.T) {
	s := startInMemory(t)
	s.snapshot(t) // go_sample, first snapshot (no append): resets, marks session

	repoB := copyTree(t, filepath.Join("..", "engine", "testdata", "repos", "ts_sample"), t.TempDir())
	res := s.call(t, "generate_snapshot", map[string]any{"repo_path": repoB, "append": true})
	if res.IsError {
		t.Fatalf("append generate_snapshot errored: %s", text(res))
	}
	if !strings.Contains(text(res), "Multi-repo mode active") {
		t.Errorf("append=true should report multi-repo mode; got:\n%s", text(res))
	}

	cov := text(s.call(t, "coverage_report", map[string]any{}))
	for _, label := range []string{"go_sample", "ts_sample"} {
		if !strings.Contains(cov, label) {
			t.Errorf("coverage_report should list service %q after append; got:\n%s", label, cov)
		}
	}
}

// TestE2E_AppendWithoutRepoPathIsRefused: append names one repository to add. Without
// repo_path it used to fall back to the configured repo, the directory the agent was
// started in, which can be a folder of every repository the user has. It must fail at
// once, with the store untouched, instead of indexing that.
func TestE2E_AppendWithoutRepoPathIsRefused(t *testing.T) {
	s := startInMemory(t)
	s.snapshot(t)
	before := text(s.call(t, "coverage_report", map[string]any{}))

	res := s.call(t, "generate_snapshot", map[string]any{"append": true, "no_cluster": true})
	if !res.IsError || !strings.Contains(text(res), "needs repo_path") {
		t.Fatalf("append without repo_path must be refused naming repo_path; got error=%v:\n%s", res.IsError, text(res))
	}
	if after := text(s.call(t, "coverage_report", map[string]any{})); after != before {
		t.Errorf("a refused append changed the store:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// TestE2E_FreshForcesSingleRepoOverAutoAppend covers the auto-append footgun:
// once a multi-repo store is loaded, a plain generate_snapshot on a different repo
// would auto-append (merging it in), but fresh=true must force a clean single-repo
// reset instead. It also asserts fresh+append is rejected as contradictory.
func TestE2E_FreshForcesSingleRepoOverAutoAppend(t *testing.T) {
	s := startInMemory(t)
	s.snapshot(t) // go_sample (s.repo): first snapshot resets, marks session

	// Append a second repo → a genuine 2-repo store with service nodes.
	repoB := copyTree(t, filepath.Join("..", "engine", "testdata", "repos", "ts_sample"), t.TempDir())
	if res := s.call(t, "generate_snapshot", map[string]any{"repo_path": repoB, "append": true}); res.IsError {
		t.Fatalf("append generate_snapshot errored: %s", text(res))
	}

	// A plain (no-flag) snapshot of a THIRD repo trips the auto-append heuristic —
	// and must now say so loudly, pointing at the fresh=true remedy rather than
	// silently merging.
	repoC := copyTree(t, filepath.Join("..", "engine", "testdata", "repos", "python_sample"), t.TempDir())
	warn := text(s.call(t, "generate_snapshot", map[string]any{"repo_path": repoC}))
	if !strings.Contains(warn, "Auto-appended") || !strings.Contains(warn, "fresh=true") {
		t.Errorf("a plain repo-switch snapshot must warn about the auto-append and name the fresh=true remedy; got:\n%s", warn)
	}

	// Re-snapshot it with fresh=true. This must suppress the auto-append and reset
	// to a clean single-repo snapshot, discarding the accumulated repos.
	res := s.call(t, "generate_snapshot", map[string]any{"repo_path": repoC, "fresh": true})
	if res.IsError {
		t.Fatalf("fresh generate_snapshot errored: %s", text(res))
	}
	out := text(res)
	for _, banned := range []string{"Multi-repo mode active", "auto-enabled", "Auto-appended"} {
		if strings.Contains(out, banned) {
			t.Errorf("fresh=true must force a single-repo snapshot, but summary contained %q; got:\n%s", banned, out)
		}
	}

	// The prior repos must be gone: a single-repo snapshot has no service nodes.
	if cov := text(s.call(t, "coverage_report", map[string]any{})); !strings.Contains(cov, "No service nodes") {
		t.Errorf("expected no service nodes after fresh single-repo snapshot; got:\n%s", cov)
	}
	svc := text(s.call(t, "query_facts", map[string]any{"kind": "service"}))
	for _, gone := range []string{"go_sample", "ts_sample"} {
		if strings.Contains(svc, gone) {
			t.Errorf("expected %q to be discarded by fresh=true, but it still appears; got:\n%s", gone, svc)
		}
	}

	// fresh + append is contradictory and must be rejected, not silently resolved.
	bad := s.call(t, "generate_snapshot", map[string]any{"repo_path": repoC, "fresh": true, "append": true})
	if !bad.IsError || !strings.Contains(text(bad), "mutually exclusive") {
		t.Errorf("fresh+append must error as mutually exclusive; got IsError=%v:\n%s", bad.IsError, text(bad))
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// copyTree recursively copies src into a fresh subdir of dstParent and returns
// the new root. Kept local to this package (the engine golden harness has its
// own copy) so the e2e tests stay self-contained.
func copyTree(t *testing.T, src, dstParent string) string {
	t.Helper()
	dst := filepath.Join(dstParent, filepath.Base(src))
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dst, err)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatalf("read fixture dir %s: %v", src, err)
	}
	for _, e := range entries {
		sp := filepath.Join(src, e.Name())
		if e.IsDir() {
			copyTree(t, sp, dst)
			continue
		}
		data, err := os.ReadFile(sp)
		if err != nil {
			t.Fatalf("read %s: %v", sp, err)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), data, 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	return dst
}
