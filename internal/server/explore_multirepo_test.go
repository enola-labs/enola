package server_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeGoRepo writes a Go module named after dir whose internal/svc package
// declares symbol and imports strings.
func writeGoRepo(t *testing.T, dir, symbol string) {
	t.Helper()
	files := map[string]string{
		"go.mod": "module example.com/" + filepath.Base(dir) + "\n\ngo 1.22\n",
		"internal/svc/svc.go": "package svc\n\nimport \"strings\"\n\n// " + symbol + " upper-cases s.\nfunc " + symbol +
			"(s string) string { return strings.ToUpper(s) }\n",
	}
	for rel, body := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestExplore_RepoPrefixedDirectoryInAMultiRepoStore: every file path in a multi-repo
// store carries its repo label, so that is the form an agent copies. explore on it
// matched the directory as a file, whose only fact is its own module, and answered
// "Total facts: 1". It must answer as the module of THAT repository, dependencies
// included, even when another repository has a module of the same name.
func TestExplore_RepoPrefixedDirectoryInAMultiRepoStore(t *testing.T) {
	root := t.TempDir()
	alpha, beta := filepath.Join(root, "alpha"), filepath.Join(root, "beta")
	writeGoRepo(t, alpha, "AlphaUpper")
	writeGoRepo(t, beta, "BetaUpper")

	s := startInMemory(t)
	if res := s.call(t, "generate_snapshot", map[string]any{"repo_paths": []string{alpha, beta}}); res.IsError {
		t.Fatalf("repo_paths: %s", text(res))
	}

	out := text(s.call(t, "explore", map[string]any{"focus": "beta/internal/svc"}))
	if !strings.HasPrefix(out, "# Module: internal/svc") {
		t.Fatalf("a repo-prefixed module directory was not explored as a module:\n%s", out)
	}
	if !strings.Contains(out, "BetaUpper") || strings.Contains(out, "AlphaUpper") {
		t.Errorf("explore picked the wrong repository's module:\n%s", out)
	}
	if !strings.Contains(out, "strings") {
		t.Errorf("the module's dependencies are missing in a multi-repo store:\n%s", out)
	}
}
