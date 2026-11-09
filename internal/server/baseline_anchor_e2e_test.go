package server_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/engine"
)

// A multi-repo graph pins its baseline in the member engine.BaselineAnchor chooses
// from the member set, the member `enola check` and `enola baseline` read. The MCP
// tools anchored on the member indexed last instead, so a pin written by one surface
// was invisible to the other, and once both had pinned each graded against its own.
func TestE2E_ClusterBaselineLivesWhereTheCLIReadsIt(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	src := filepath.Join("..", "engine", "testdata", "repos", "go_httpclient_multirepo")
	parent := t.TempDir()
	api := copyTree(t, filepath.Join(src, "api"), parent)
	consumer := copyTree(t, filepath.Join(src, "consumer"), parent)
	anchor := engine.BaselineAnchor([]string{consumer, api})
	if anchor != api {
		t.Fatalf("fixture: the member set should choose api, got %s", anchor)
	}
	pinIn := func(member string) string { return filepath.Join(member, ".enola", "baseline") }
	pinned := func(member string) bool {
		_, err := os.Stat(filepath.Join(pinIn(member), "facts.jsonl"))
		return err == nil
	}

	s := startInMemory(t)
	// api first: the member indexed last is consumer, which is not the anchor.
	if res := s.call(t, "generate_snapshot", map[string]any{"repo_paths": []string{api, consumer}}); res.IsError {
		t.Fatalf("generate_snapshot: %s", text(res))
	}
	if res := s.call(t, "set_baseline", nil); res.IsError {
		t.Fatalf("set_baseline: %s", text(res))
	}
	if !pinned(api) {
		t.Errorf("set_baseline did not pin in api, the member the CLI reads")
	}
	if pinned(consumer) {
		t.Errorf("set_baseline pinned in consumer, the member indexed last")
	}

	// A pin only the anchor holds, as `enola baseline pin` leaves it, is the one
	// diff_snapshot and compare_receipts grade against.
	if err := os.RemoveAll(pinIn(consumer)); err != nil {
		t.Fatal(err)
	}
	if res := s.call(t, "diff_snapshot", nil); res.IsError {
		t.Errorf("diff_snapshot found no pin in the anchor:\n%s", text(res))
	}
	if res := s.call(t, "compare_receipts", nil); res.IsError {
		t.Errorf("compare_receipts found no pin in the anchor:\n%s", text(res))
	}

	// A pin left in another member is not read: the anchor's is the cluster's pin.
	if err := os.RemoveAll(pinIn(api)); err != nil {
		t.Fatal(err)
	}
	if res := s.call(t, "set_baseline", nil); res.IsError {
		t.Fatalf("set_baseline: %s", text(res))
	}
	if err := os.Rename(pinIn(api), pinIn(consumer)); err != nil {
		t.Fatal(err)
	}
	res := s.call(t, "diff_snapshot", nil)
	if !res.IsError || !strings.Contains(text(res), "No pinned baseline") {
		t.Errorf("diff_snapshot graded against a pin outside the anchor:\n%s", text(res))
	}
}
