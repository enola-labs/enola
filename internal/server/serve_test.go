package server

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRoots_Confine(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	inside := filepath.Join(root, "a", "b")
	outside := filepath.Join(base, "rootless")
	for _, d := range []string{inside, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	roots, err := NewRoots([]string{root})
	if err != nil {
		t.Fatal(err)
	}

	for _, ok := range []string{root, inside, filepath.Join(root, "a", "..", "a")} {
		if err := roots.Confine(ok); err != nil {
			t.Errorf("Confine(%s) = %v, want inside", ok, err)
		}
	}
	for _, bad := range []string{
		outside, // a sibling sharing the root's name as a prefix
		filepath.Join(root, ".."),
		filepath.Join(root, "link"), // a symlink inside pointing out
		filepath.Join(root, "missing"),
		"/",
	} {
		if err := roots.Confine(bad); err == nil {
			t.Errorf("Confine(%s) = nil, want refused", bad)
		}
	}
	if err := roots.Confine(outside); !errors.Is(err, ErrOutsideRoots) {
		t.Errorf("Confine(outside) = %v, want ErrOutsideRoots", err)
	}
}

func TestNewRoots_RefusesWhatIsNotADirectory(t *testing.T) {
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, dirs := range [][]string{nil, {f}, {filepath.Join(t.TempDir(), "missing")}} {
		if _, err := NewRoots(dirs); err == nil {
			t.Errorf("NewRoots(%v) accepted it", dirs)
		}
	}
}

func TestConfineArgs(t *testing.T) {
	root := t.TempDir()
	roots, err := NewRoots([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	for _, c := range []struct {
		args map[string]any
		ok   bool
	}{
		{map[string]any{"repo_path": root}, true},
		{map[string]any{"repo_path": outside}, false},
		{map[string]any{"repo_paths": []string{root, outside}}, false},
		{map[string]any{"some_new_path": outside}, false}, // covered by name, not by listing
		{map[string]any{"baseline": "pinned"}, true},
		{map[string]any{"baseline": "Previous"}, true},
		{map[string]any{"baseline": outside}, false},
		{map[string]any{"focus": outside, "target": outside}, true}, // graph names, not filesystem paths
		{map[string]any{"repo_path": ""}, true},
	} {
		raw, _ := json.Marshal(c.args)
		if err := confineArgs(roots, raw); (err == nil) != c.ok {
			t.Errorf("confineArgs(%v) = %v, want ok=%v", c.args, err, c.ok)
		}
	}
}

func TestTokenVerifier(t *testing.T) {
	v := tokenVerifier([]string{"first-token-first-token-first-tok", "second-token-second-token-second-"})
	info, err := v(t.Context(), "second-token-second-token-second-", nil)
	if err != nil || info.UserID != tokenID("second-token-second-token-second-") {
		t.Fatalf("known token: %v %v", info, err)
	}
	if _, err := v(t.Context(), "first-token-first-token-first-to", nil); err == nil {
		t.Error("a prefix of a token was accepted")
	}
	if len(info.UserID) != 12 {
		t.Errorf("token id %q is not 12 hex digits", info.UserID)
	}
}
