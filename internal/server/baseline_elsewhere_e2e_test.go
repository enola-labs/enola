package server_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Baselines are kept per repository. A session that pinned one on one directory and then
// snapshotted another, or removed the directory it pinned in, used to hear only "No
// pinned baseline found", and in a skill test spent turns rebuilding what it already
// had. The answer now says where this session's pin is, or that it was removed.
func TestE2E_MissingPinSaysWhereThisSessionPinned(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	src := filepath.Join("..", "engine", "testdata", "repos", "go_httpclient_multirepo")
	before := copyTree(t, filepath.Join(src, "api"), t.TempDir())
	after := copyTree(t, filepath.Join(src, "consumer"), t.TempDir())

	s := startInMemory(t)
	if res := s.call(t, "generate_snapshot", map[string]any{"repo_path": before, "fresh": true}); res.IsError {
		t.Fatalf("generate_snapshot: %s", text(res))
	}
	if res := s.call(t, "set_baseline", nil); res.IsError {
		t.Fatalf("set_baseline: %s", text(res))
	}
	pin := filepath.Join(before, ".enola", "baseline")

	if res := s.call(t, "generate_snapshot", map[string]any{"repo_path": after, "fresh": true}); res.IsError {
		t.Fatalf("generate_snapshot: %s", text(res))
	}
	res := s.call(t, "diff_snapshot", nil)
	if !res.IsError || !strings.Contains(text(res), "baseline=\""+pin+"\"") {
		t.Errorf("diff_snapshot on another repository did not name this session's pin:\n%s", text(res))
	}
	if res := s.call(t, "diff_snapshot", map[string]any{"baseline": pin}); res.IsError {
		t.Errorf("the baseline it named does not load:\n%s", text(res))
	}

	// The pinned directory replaced, as an agent swapping an export does.
	if res := s.call(t, "generate_snapshot", map[string]any{"repo_path": before, "fresh": true}); res.IsError {
		t.Fatalf("generate_snapshot: %s", text(res))
	}
	if err := os.RemoveAll(pin); err != nil {
		t.Fatal(err)
	}
	res = s.call(t, "diff_snapshot", nil)
	if !res.IsError || !strings.Contains(text(res), "it is gone") {
		t.Errorf("diff_snapshot after the pin was removed did not say so:\n%s", text(res))
	}
}
