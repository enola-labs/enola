package server_test

// Contract goldens for the MCP tool surface.
//
// The e2e tests beside this file assert that a tool's answer CONTAINS the right
// thing. That survives a refactor that reorders a table, drops a section or
// changes a default, which is exactly what moving handlers and renderers between
// files and packages tends to do. These tests pin the whole answer instead: every
// registered tool, driven through the wire over fixture repos, byte for byte
// against a committed golden.
//
// A mismatch means an agent would see a different document for the same call. A
// reviewer diffs the regenerated golden to decide whether that was intended.
// Regenerate with:
//
//	go test ./internal/server -run TestContract -update
//
// Machine and run specifics (temp paths, timestamps, durations, content hashes)
// are normalized away so the goldens hold across machines and runs.

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "regenerate golden files in testdata/contract")

// contractCase is one tool call whose full answer is pinned.
type contractCase struct {
	name string
	tool string
	args map[string]any
}

// contractCalled records which tools the scenarios drove, so a tool registered
// without a contract case fails TestContract_EveryToolIsPinned.
var contractCalled = map[string]bool{}

// contractScenarios records which scenarios ran. The completeness check only
// means something when all of them did.
var contractScenarios = map[string]bool{}

const contractScenarioCount = 4

var contractNormalizers = []struct {
	re   *regexp.Regexp
	with string
}{
	{regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})`), "<TIME>"},
	{regexp.MustCompile(`sha256:[0-9a-f]{64}`), "sha256:<HASH>"},
	// Bumped on every extractor change; pinning it would re-record goldens for
	// changes that leave every answer alone.
	{regexp.MustCompile(`"extractor_version": "v\d+"`), `"extractor_version": "<VERSION>"`},
	{regexp.MustCompile(`\b\d+(\.\d+)?(ns|µs|ms|s)\b`), "<DURATION>"},
	{regexp.MustCompile(`\b\d+(\.\d+)?m\d+(\.\d+)?s\b`), "<DURATION>"},
}

// normalize strips what differs between machines and runs. roots are the temp
// directories the scenario wrote to; each is replaced in both its literal and its
// symlink-resolved form, because macOS hands out /var paths that resolve to
// /private/var and the server reports whichever it canonicalized.
func normalize(t *testing.T, out string, roots ...string) string {
	t.Helper()
	if home := os.Getenv("HOME"); home != "" {
		roots = append(roots, home)
	}
	for _, root := range roots {
		if resolved, err := filepath.EvalSymlinks(root); err == nil && resolved != root {
			out = strings.ReplaceAll(out, resolved, "<TMP>")
		}
		out = strings.ReplaceAll(out, root, "<TMP>")
	}
	// A call made before any snapshot falls back to the working directory.
	if cwd, err := os.Getwd(); err == nil {
		out = strings.ReplaceAll(out, cwd, "<CWD>")
	}
	for _, n := range contractNormalizers {
		out = n.re.ReplaceAllString(out, n.with)
	}
	return out
}

// assertGolden compares got with testdata/contract/<rel>, or rewrites it under -update.
func assertGolden(t *testing.T, rel, got string) {
	t.Helper()
	path := filepath.Join("testdata", "contract", rel)
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (run with -update to create it)", path, err)
	}
	if got != string(want) {
		t.Errorf("%s differs from its golden (run with -update if intended).\n--- want\n%s\n--- got\n%s", rel, want, got)
	}
}

// pin runs each case and compares the normalized answer with its golden. A tool
// error is part of the contract, so it is recorded rather than failed on.
func (s *session) pin(t *testing.T, scenario string, roots []string, cases []contractCase) {
	t.Helper()
	contractScenarios[scenario] = true
	for _, c := range cases {
		contractCalled[c.tool] = true
		res := s.call(t, c.tool, c.args)
		got := normalize(t, text(res), roots...)
		if res.IsError {
			got = "[tool error]\n" + got
		}
		assertGolden(t, filepath.Join(scenario, c.name+".golden"), got)
	}
}

func fixtureRepo(t *testing.T, name string) (root, repo string) {
	t.Helper()
	root = t.TempDir()
	return root, copyTree(t, filepath.Join("..", "engine", "testdata", "repos", name), root)
}

// TestContract_ToolCatalogue pins what an agent is told about each tool: its name,
// its description and its argument schema. Descriptions are prompts, so a reworded
// one changes agent behavior as surely as a changed handler does.
func TestContract_ToolCatalogue(t *testing.T) {
	s := startInMemory(t)
	res, err := s.cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	sort.Slice(res.Tools, func(i, j int) bool { return res.Tools[i].Name < res.Tools[j].Name })

	type entry struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		InputSchema any    `json:"input_schema"`
	}
	entries := make([]entry, 0, len(res.Tools))
	for _, tool := range res.Tools {
		entries = append(entries, entry{tool.Name, tool.Description, tool.InputSchema})
	}
	doc, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	assertGolden(t, "tools.golden.json", string(doc)+"\n")
}

// TestContract_SingleRepo pins the read tools over go_sample: modules ".",
// "pkg/a", "pkg/b" with a deliberate pkg/a <-> pkg/b import cycle.
func TestContract_SingleRepo(t *testing.T) {
	eng, cfg := newTestEngine(t)
	s := connect(t, eng, cfg)
	root, repo := fixtureRepo(t, "go_sample")
	roots := []string{root}

	s.pin(t, "single", roots, []contractCase{
		{"query_facts_before_snapshot", "query_facts", map[string]any{"kind": "module"}},
		{"generate_snapshot", "generate_snapshot", map[string]any{"repo_path": repo}},

		{"query_facts_modules", "query_facts", map[string]any{"kind": "module"}},
		{"query_facts_symbols_compact", "query_facts", map[string]any{"kind": "symbol", "output_mode": "compact"}},
		{"query_facts_names", "query_facts", map[string]any{"kinds": []string{"symbol", "module"}, "output_mode": "names"}},
		{"query_facts_summary", "query_facts", map[string]any{"output_mode": "summary"}},
		{"query_facts_file_prefix", "query_facts", map[string]any{"file_prefix": "pkg/a", "output_mode": "compact"}},
		{"query_facts_relation_related", "query_facts", map[string]any{"relation": "calls", "include_related": true}},
		{"query_facts_no_match", "query_facts", map[string]any{"name": "no-such-thing"}},

		{"show_symbol", "show_symbol", map[string]any{"name": "pkg/a.Alpha"}},
		{"show_symbol_context", "show_symbol", map[string]any{"name": "Alpha", "context_lines": 4}},
		{"show_symbol_no_match", "show_symbol", map[string]any{"name": "no-such-thing"}},

		{"explore_module", "explore", map[string]any{"focus": "pkg/a"}},
		{"explore_file", "explore", map[string]any{"focus": "pkg/a/a.go"}},
		{"explore_symbol", "explore", map[string]any{"focus": "pkg/b.Beta"}},
		{"explore_depth2_summary", "explore", map[string]any{"focus": "pkg/a", "depth": 2}},
		{"explore_depth2_compact", "explore", map[string]any{"focus": "pkg/a", "depth": 2, "output_mode": "compact"}},
		{"explore_no_match", "explore", map[string]any{"focus": "no-such-thing"}},

		{"traverse_summary", "traverse", map[string]any{"start": "pkg/a"}},
		{"traverse_reverse_compact", "traverse", map[string]any{"start": "pkg/a", "direction": "reverse", "output_mode": "compact"}},
		{"traverse_calls_full", "traverse", map[string]any{"start": "..main", "relation_kinds": []string{"calls"}, "output_mode": "full"}},
		{"traverse_ambiguous", "traverse", map[string]any{"start": "a"}},

		{"find_path", "find_path", map[string]any{"from": "..main", "to": "pkg/a.Alpha"}},
		{"find_path_none", "find_path", map[string]any{"from": "pkg/b.Beta", "to": "..main"}},

		{"impact_summary", "impact_analysis", map[string]any{"target": "pkg/a.Alpha"}},
		{"impact_compact_forward", "impact_analysis", map[string]any{"target": "pkg/a.Alpha", "output_mode": "compact", "include_forward": true}},
		{"impact_full", "impact_analysis", map[string]any{"target": "pkg/a.Alpha", "output_mode": "full"}},
		{"impact_file", "impact_analysis", map[string]any{"target": "pkg/a/a.go"}},
		{"impact_ambiguous", "impact_analysis", map[string]any{"target": "a"}},

		{"query_insights_summary", "query_insights", map[string]any{}},
		{"query_insights_cycles_compact", "query_insights", map[string]any{"explainer": "cycles", "output_mode": "compact"}},
		{"query_insights_full", "query_insights", map[string]any{"output_mode": "full"}},
		{"query_insights_min_confidence", "query_insights", map[string]any{"min_confidence": 0.9}},
		{"query_insights_unknown_explainer", "query_insights", map[string]any{"explainer": "no-such-explainer"}},

		{"package_metrics_summary", "package_metrics", map[string]any{}},
		{"package_metrics_compact", "package_metrics", map[string]any{"output_mode": "compact", "sort_by": "distance"}},
		{"package_metrics_full", "package_metrics", map[string]any{"output_mode": "full"}},

		{"find_orphans_summary", "find_orphans", map[string]any{}},
		{"find_orphans_compact", "find_orphans", map[string]any{"output_mode": "compact"}},
		{"find_orphans_full_tests", "find_orphans", map[string]any{"output_mode": "full", "include_tests": true, "include_entrypoints": true}},

		{"analyze_performance", "analyze_performance", map[string]any{}},

		{"coverage_report", "coverage_report", map[string]any{}},
		{"endpoint_impact_no_routes", "endpoint_impact", map[string]any{"endpoint": "/v1/things"}},

		{"governing_intent_not_asked", "governing_intent", map[string]any{"target": "pkg/a/a.go"}},
		{"constraints_for_not_asked", "constraints_for", map[string]any{"target": "pkg/a/a.go"}},
		{"plan_check_undeclared", "plan_check", map[string]any{"paths": []string{"pkg/a/a.go"}}},

		{"snapshot_receipt_summary", "snapshot_receipt", map[string]any{}},
		{"architecture_history", "architecture_history", map[string]any{}},
		{"architecture_history_all", "architecture_history", map[string]any{"all": true}},
		{"architecture_blame_first", "architecture_blame", map[string]any{"pattern": "pkg/a -> pkg/b", "first": true}},
		{"architecture_blame_findings", "architecture_blame", map[string]any{"pattern": "cycle", "findings": true}},
		{"architecture_blame", "architecture_blame", map[string]any{"pattern": "pkg/a"}},

		{"max_tokens_truncation", "query_facts", map[string]any{"max_tokens": 50}},
	})
}

// TestContract_Governed pins the intent tools over a repo that declares
// components, a rule its own code breaks, and a decision page anchored to pkg/a.
func TestContract_Governed(t *testing.T) {
	eng, cfg := newTestEngine(t)
	s := connect(t, eng, cfg)
	root, repo := fixtureRepo(t, "go_sample")

	for path, body := range map[string]string{
		"enola-intent.yaml": "components:\n" +
			"  - {name: pkg-a, match: [pkg/a/**]}\n" +
			"  - {name: pkg-b, match: [pkg/b/**]}\n" +
			"rules:\n" +
			"  - {id: a-avoids-b, forbid: pkg-a, to: pkg-b, via: imports,\n" +
			"     because: a must stay deliverable without b}\n",
		"decision.md": "---\nenola_intent:\n  page:\n    type: decision\n    status: accepted\n    relations:\n      - {rel: part-of, to: epic.md}\n    anchors:\n      - {repo: go_sample, path: pkg/a}\n---\nbody\n",
		"epic.md":     "---\nenola_intent:\n  page:\n    type: epic\n    status: living\n---\nbody\n",
	} {
		if err := os.WriteFile(filepath.Join(repo, path), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	s.pin(t, "governed", []string{root}, []contractCase{
		{"generate_snapshot", "generate_snapshot", map[string]any{"repo_path": repo}},

		{"governing_intent_file", "governing_intent", map[string]any{"target": "pkg/a/a.go"}},
		{"governing_intent_fact", "governing_intent", map[string]any{"target": "pkg/a.Alpha"}},
		{"governing_intent_ungoverned", "governing_intent", map[string]any{"target": "main.go"}},
		{"governing_intent_page", "governing_intent", map[string]any{"target": "decision.md"}},
		{"governing_intent_no_match", "governing_intent", map[string]any{"target": "no/such/thing.xyz"}},

		{"constraints_for_bound", "constraints_for", map[string]any{"target": "pkg/a/a.go"}},
		{"constraints_for_unwritten", "constraints_for", map[string]any{"target": "pkg/a/new_file.go"}},
		{"constraints_for_unbound", "constraints_for", map[string]any{"target": "elsewhere/free.go"}},

		{"plan_check_paths", "plan_check", map[string]any{"paths": []string{"pkg/a/new_file.go"}}},
		{"plan_check_symbols", "plan_check", map[string]any{"symbols": []string{"pkg/b.Beta"}}},
		{"plan_check_unbound", "plan_check", map[string]any{"paths": []string{"elsewhere/free.go"}}},
		{"plan_check_empty", "plan_check", map[string]any{}},

		{"show_symbol_governed", "show_symbol", map[string]any{"name": "pkg/a.Alpha"}},
		{"impact_governed", "impact_analysis", map[string]any{"target": "pkg/a.Alpha"}},
		{"query_insights_constraints", "query_insights", map[string]any{"explainer": "constraints", "output_mode": "compact"}},
		{"query_insights_intent", "query_insights", map[string]any{"explainer": "intent", "output_mode": "compact"}},
	})
}

// TestContract_MultiRepo pins the cross-repo tools over an api service and the
// consumer that calls its one route.
func TestContract_MultiRepo(t *testing.T) {
	eng, cfg := newTestEngine(t)
	s := connect(t, eng, cfg)
	root, cluster := fixtureRepo(t, "go_httpclient_multirepo")
	api, consumer := filepath.Join(cluster, "api"), filepath.Join(cluster, "consumer")

	s.pin(t, "multirepo", []string{root}, []contractCase{
		{"generate_snapshot", "generate_snapshot", map[string]any{"repo_paths": []string{api, consumer}}},

		{"query_facts_services", "query_facts", map[string]any{"kind": "service"}},
		{"query_facts_routes_compact", "query_facts", map[string]any{"kind": "route", "output_mode": "compact"}},
		{"query_facts_repo_summary", "query_facts", map[string]any{"repo": "consumer", "output_mode": "summary"}},
		{"query_facts_unmatched_routes", "query_facts", map[string]any{"kind": "route", "prop": "unmatched_by_clients", "prop_value": "true"}},

		{"explore_service", "explore", map[string]any{"focus": "api"}},
		{"explore_repo_file", "explore", map[string]any{"focus": "consumer/client.go"}},

		{"endpoint_impact", "endpoint_impact", map[string]any{"endpoint": "GET /v1/things"}},
		{"endpoint_impact_no_match", "endpoint_impact", map[string]any{"endpoint": "/no/such/route"}},

		{"coverage_report_summary", "coverage_report", map[string]any{}},
		{"coverage_report_full", "coverage_report", map[string]any{"output_mode": "full"}},
		{"coverage_report_repo", "coverage_report", map[string]any{"repo": "consumer"}},

		{"query_insights_summary", "query_insights", map[string]any{}},
		{"query_insights_crossrepo", "query_insights", map[string]any{"explainer": "crossrepo", "output_mode": "compact"}},
		{"query_insights_unused_routes", "query_insights", map[string]any{"explainer": "unused-routes", "output_mode": "compact"}},
		{"query_insights_repo", "query_insights", map[string]any{"repo": "api"}},

		{"traverse_cross_repo", "traverse", map[string]any{"start": "kind:service consumer", "output_mode": "compact"}},
		{"impact_cross_repo", "impact_analysis", map[string]any{"target": "repo:api getThing", "output_mode": "compact"}},
		{"package_metrics_repo", "package_metrics", map[string]any{"repo": "api", "output_mode": "compact"}},
		{"find_orphans_repo", "find_orphans", map[string]any{"repo": "consumer", "output_mode": "compact"}},

		{"snapshot_receipt_summary", "snapshot_receipt", map[string]any{}},

		{"generate_snapshot_fresh_append_refused", "generate_snapshot", map[string]any{"repo_path": api, "fresh": true, "append": true}},
	})
}

// TestContract_Change pins the verification loop: snapshot, pin a baseline, edit,
// snapshot again, then read what the change did.
func TestContract_Change(t *testing.T) {
	eng, cfg := newTestEngine(t)
	s := connect(t, eng, cfg)
	root, repo := fixtureRepo(t, "go_sample")
	roots := []string{root}

	s.pin(t, "change", roots, []contractCase{
		{"set_baseline_before_snapshot", "set_baseline", map[string]any{}},
		{"diff_snapshot_before_snapshot", "diff_snapshot", map[string]any{}},
		{"generate_snapshot", "generate_snapshot", map[string]any{"repo_path": repo}},
		{"diff_snapshot_no_baseline", "diff_snapshot", map[string]any{}},
		{"compare_receipts_no_baseline", "compare_receipts", map[string]any{}},
		{"set_baseline", "set_baseline", map[string]any{}},
		{"diff_snapshot_unchanged", "diff_snapshot", map[string]any{}},
	})

	// A new package that pkg/a starts importing: one module, one symbol and one
	// coupling edge the baseline does not have.
	if err := os.MkdirAll(filepath.Join(repo, "pkg", "c"), 0o755); err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]string{
		"pkg/c/c.go":     "package c\n\n// Gamma is new since the baseline.\nfunc Gamma() string { return \"gamma\" }\n",
		"pkg/a/gamma.go": "package a\n\nimport \"example.com/gosample/pkg/c\"\n\n// UsesGamma couples pkg/a to the new package.\nfunc UsesGamma() string { return c.Gamma() }\n",
	} {
		if err := os.WriteFile(filepath.Join(repo, path), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	s.pin(t, "change", roots, []contractCase{
		{"generate_snapshot_after_edit", "generate_snapshot", map[string]any{"repo_path": repo}},
		{"diff_snapshot_summary", "diff_snapshot", map[string]any{}},
		{"diff_snapshot_compact", "diff_snapshot", map[string]any{"output_mode": "compact"}},
		{"diff_snapshot_full", "diff_snapshot", map[string]any{"output_mode": "full"}},
		{"diff_snapshot_focus", "diff_snapshot", map[string]any{"focus": "pkg/c", "output_mode": "compact"}},
		{"diff_snapshot_previous", "diff_snapshot", map[string]any{"baseline": "previous"}},
		{"diff_snapshot_target", "diff_snapshot", map[string]any{"target": "pkg/a.Alpha", "output_mode": "compact"}},
		{"diff_snapshot_target_expected", "diff_snapshot", map[string]any{"target": "pkg/a.Alpha", "expected_packages": []string{"pkg/c"}, "output_mode": "compact"}},
		{"compare_receipts_summary", "compare_receipts", map[string]any{}},
		{"snapshot_receipt_full", "snapshot_receipt", map[string]any{"output_mode": "full"}},
	})
}

// TestContract_EveryToolIsPinned fails when a tool is registered that no scenario
// above drives. It reads what those tests recorded, so it must run after them
// (Go runs a file's tests in source order) and is skipped when a -run filter left
// any of them out.
func TestContract_EveryToolIsPinned(t *testing.T) {
	if len(contractScenarios) < contractScenarioCount {
		t.Skip("not every contract scenario ran")
	}
	s := startInMemory(t)
	res, err := s.cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for _, tool := range res.Tools {
		if !contractCalled[tool.Name] {
			t.Errorf("tool %q is registered but has no contract golden; add a case to a TestContract scenario", tool.Name)
		}
	}
}

// toolCatalogueBudget caps the bytes of every tool's name, description and input
// schema together. An agent whose client loads the tools up front (alwaysLoad) pays
// for all of them in every session, before its first question; a description that
// grows past this has to earn the room from another.
const toolCatalogueBudget = 29_500

func TestToolCatalogue_SizeBudget(t *testing.T) {
	s := startInMemory(t)
	res, err := s.cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	total := 0
	for _, tool := range res.Tools {
		schema, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		total += len(tool.Name) + len(tool.Description) + len(schema)
	}
	if total > toolCatalogueBudget {
		t.Errorf("tool definitions are %d bytes, over the budget of %d", total, toolCatalogueBudget)
	}
	t.Logf("tool definitions: %d bytes of %d", total, toolCatalogueBudget)
}
