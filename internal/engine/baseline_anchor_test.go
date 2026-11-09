package engine

import (
	"os"
	"path/filepath"
	"testing"
)

// A cluster's pinned baseline lives in one member's output dir, and every surface
// that pins or reads it must pick the same member whatever order it was handed the
// members in: a cluster.yaml, a folder of repositories and an MCP repo_paths list
// each have their own order, or none.
func TestBaselineAnchor_SameMemberInEveryOrder(t *testing.T) {
	parent := t.TempDir()
	a, m, z := filepath.Join(parent, "a"), filepath.Join(parent, "m"), filepath.Join(parent, "z")
	for _, dir := range []string{a, m, z} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, order := range [][]string{{a, m, z}, {z, m, a}, {m, z, a}} {
		if got := BaselineAnchor(order); got != a {
			t.Errorf("BaselineAnchor(%v) = %s, want %s", order, got, a)
		}
	}
	if got := BaselineAnchor([]string{z}); got != z {
		t.Errorf("a single repository is its own anchor: got %s, want %s", got, z)
	}
	if got := BaselineAnchor(nil); got != "" {
		t.Errorf("no members has no anchor: got %q", got)
	}
}

// The order is decided on canonical paths, and the member is returned as it was
// given, so a symlinked checkout picks the same member as its real path and the
// caller's output dir is resolved from the path it already uses.
func TestBaselineAnchor_DecidesOnCanonicalPaths(t *testing.T) {
	parent := t.TempDir()
	real := filepath.Join(parent, "z-real")
	other := filepath.Join(parent, "m")
	for _, dir := range []string{real, other} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// "a-link" sorts first as written, but it is z-real, which sorts last.
	link := filepath.Join(parent, "a-link")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	if got := BaselineAnchor([]string{link, other}); got != other {
		t.Errorf("BaselineAnchor ordered by the written path: got %s, want %s", got, other)
	}
}
