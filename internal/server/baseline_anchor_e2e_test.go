package server_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/config"
)

// A multi-repo graph pins its baseline in the cluster's own directory, keyed by the
// member set, which is where `enola check` and `enola baseline` read it whatever order
// the members arrive in. Neither member gets a pin, and a pin left in a member's own
// output dir is that member's, not the cluster's.
func TestE2E_ClusterBaselineLivesWhereTheCLIReadsIt(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	src := filepath.Join("..", "engine", "testdata", "repos", "go_httpclient_multirepo")
	parent := t.TempDir()
	api := copyTree(t, filepath.Join(src, "api"), parent)
	consumer := copyTree(t, filepath.Join(src, "consumer"), parent)
	// The CLI reads the cluster in another order than the MCP list below.
	clusterPin := filepath.Join(config.Default().GraphOutputDir([]string{consumer, api}), "baseline")
	memberPin := func(member string) string { return filepath.Join(member, ".enola", "baseline") }
	pinned := func(dir string) bool {
		_, err := os.Stat(filepath.Join(dir, "facts.jsonl"))
		return err == nil
	}

	s := startInMemory(t)
	if res := s.call(t, "generate_snapshot", map[string]any{"repo_paths": []string{api, consumer}}); res.IsError {
		t.Fatalf("generate_snapshot: %s", text(res))
	}
	if res := s.call(t, "set_baseline", nil); res.IsError {
		t.Fatalf("set_baseline: %s", text(res))
	}
	if !pinned(clusterPin) {
		t.Errorf("set_baseline did not pin in the cluster's directory, where the CLI reads")
	}
	for _, m := range []string{api, consumer} {
		if pinned(memberPin(m)) {
			t.Errorf("set_baseline pinned in member %s", m)
		}
	}
	if res := s.call(t, "diff_snapshot", nil); res.IsError {
		t.Errorf("diff_snapshot found no pin in the cluster's directory:\n%s", text(res))
	}
	if res := s.call(t, "compare_receipts", nil); res.IsError {
		t.Errorf("compare_receipts found no pin in the cluster's directory:\n%s", text(res))
	}

	// A pin in a member's own output dir is not the cluster's.
	if err := os.MkdirAll(filepath.Join(api, ".enola"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(clusterPin, memberPin(api)); err != nil {
		t.Fatal(err)
	}
	res := s.call(t, "diff_snapshot", nil)
	if !res.IsError || !strings.Contains(text(res), "No pinned baseline") {
		t.Errorf("diff_snapshot graded against a pin in a member:\n%s", text(res))
	}
}
