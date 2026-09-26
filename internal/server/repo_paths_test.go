package server_test

import (
	"path/filepath"
	"strings"
	"testing"
)

// crossRepoEdges lists the cross-repo dependency edges in the store, by name.
func crossRepoEdges(t *testing.T, s *session) string {
	t.Helper()
	return text(s.call(t, "query_facts", map[string]any{
		"prop": "type", "prop_value": "cross_repo", "output_mode": "names",
	}))
}

// multirepoFixture copies the two-repo HTTP client fixture: consumer calls api.
func multirepoFixture(t *testing.T) (api, consumer string) {
	t.Helper()
	src := filepath.Join("..", "engine", "testdata", "repos", "go_httpclient_multirepo")
	return copyTree(t, filepath.Join(src, "api"), t.TempDir()), copyTree(t, filepath.Join(src, "consumer"), t.TempDir())
}

// TestRepoPaths_OneCallEqualsSequentialAppends: repo_paths exists to replace one call
// per repository, so it must leave exactly the graph those calls would have: the same
// service nodes and the same cross-repo edges.
func TestRepoPaths_OneCallEqualsSequentialAppends(t *testing.T) {
	api, consumer := multirepoFixture(t)

	seq := startInMemory(t)
	if res := seq.call(t, "generate_snapshot", map[string]any{"repo_path": api, "fresh": true}); res.IsError {
		t.Fatalf("snapshot api: %s", text(res))
	}
	if res := seq.call(t, "generate_snapshot", map[string]any{"repo_path": consumer, "append": true}); res.IsError {
		t.Fatalf("append consumer: %s", text(res))
	}
	want := crossRepoEdges(t, seq)
	if !strings.Contains(want, "consumer") {
		t.Fatalf("fixture produced no cross-repo edge from consumer; got:\n%s", want)
	}

	one := startInMemory(t)
	res := one.call(t, "generate_snapshot", map[string]any{"repo_paths": []string{api, consumer}})
	if res.IsError {
		t.Fatalf("repo_paths: %s", text(res))
	}
	if !strings.Contains(text(res), "Indexed 2 repositories in one call") {
		t.Errorf("repo_paths answer does not say what it indexed:\n%s", text(res))
	}
	if got := crossRepoEdges(t, one); got != want {
		t.Errorf("repo_paths linked differently from sequential appends.\none call:\n%s\nsequential:\n%s", got, want)
	}
	cov := text(one.call(t, "coverage_report", map[string]any{}))
	for _, label := range []string{filepath.Base(api), filepath.Base(consumer)} {
		if !strings.Contains(cov, label) {
			t.Errorf("service %q missing after repo_paths; coverage_report:\n%s", label, cov)
		}
	}
}

// TestRepoPaths_AppendKeepsWhatIsLoaded: with append=true the list is added to the
// store, not a replacement for it.
func TestRepoPaths_AppendKeepsWhatIsLoaded(t *testing.T) {
	api, consumer := multirepoFixture(t)
	s := startInMemory(t)
	s.snapshot(t) // go_sample

	if res := s.call(t, "generate_snapshot", map[string]any{"repo_paths": []string{api, consumer}, "append": true}); res.IsError {
		t.Fatalf("repo_paths append: %s", text(res))
	}
	cov := text(s.call(t, "coverage_report", map[string]any{}))
	for _, label := range []string{filepath.Base(s.repo), filepath.Base(api), filepath.Base(consumer)} {
		if !strings.Contains(cov, label) {
			t.Errorf("service %q missing after appending repo_paths; coverage_report:\n%s", label, cov)
		}
	}
}

// TestRepoPaths_RefusesConflictingArguments: each refusal names what to change, and
// none of them touches the store.
func TestRepoPaths_RefusesConflictingArguments(t *testing.T) {
	api, consumer := multirepoFixture(t)
	twin := filepath.Join(t.TempDir(), filepath.Base(api))
	copyTree(t, api, filepath.Dir(twin))

	s := startInMemory(t)
	s.snapshot(t)
	before := text(s.call(t, "coverage_report", map[string]any{}))

	for name, tc := range map[string]struct {
		args map[string]any
		want string
	}{
		"with repo_path": {map[string]any{"repo_paths": []string{api}, "repo_path": consumer}, "not both"},
		"with fresh":     {map[string]any{"repo_paths": []string{api}, "fresh": true}, "drop fresh"},
		"missing dir":    {map[string]any{"repo_paths": []string{filepath.Join(api, "nope")}}, "not a directory"},
		"same label":     {map[string]any{"repo_paths": []string{api, twin}}, "share the repo label"},
	} {
		res := s.call(t, "generate_snapshot", tc.args)
		if !res.IsError || !strings.Contains(text(res), tc.want) {
			t.Errorf("%s: want an error containing %q; got error=%v:\n%s", name, tc.want, res.IsError, text(res))
		}
	}
	if after := text(s.call(t, "coverage_report", map[string]any{})); after != before {
		t.Errorf("a refused repo_paths call changed the store:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}
