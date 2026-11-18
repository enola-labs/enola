package server_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enola-labs/enola/pkg/bootstrap"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// A served instance confines every path it reads, and touched replays history into a
// temp directory of its own. The walk must still answer, and a link committed to the
// repository must not carry the extraction outside the roots.
func TestServe_TouchedWalksHistoryInsideRoots(t *testing.T) {
	s := startServed(t, bootstrap.ServeOptions{})
	dir := filepath.Join(s.root, "app")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.ts")
	if err := os.WriteFile(outside, []byte("export function leaked() {\n  return 1;\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init", "-q")
	if err := os.Symlink(outside, filepath.Join(dir, "link.ts")); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"1", "2"} {
		if err := os.WriteFile(filepath.Join(dir, "a.ts"), []byte("export function f() {\n  return "+v+";\n}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(outside, []byte("export function leaked() {\n  return "+v+";\n}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git("add", "-A")
		git("commit", "-q", "-m", "fix: "+v)
	}

	sess, err := s.connect(t, testToken)
	if err != nil {
		t.Fatal(err)
	}
	res, err := sess.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "touched", Arguments: map[string]any{"repo_path": dir}})
	if err != nil {
		t.Fatal(err)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	if res.IsError {
		t.Fatalf("touched refused in a served instance: %s", text)
	}
	var out struct {
		Ranking []struct {
			Name  string `json:"name"`
			Count int    `json:"count"`
		} `json:"ranking"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("%v: %s", err, strings.TrimSpace(text))
	}
	var sawF bool
	for _, r := range out.Ranking {
		if strings.Contains(r.Name, "leaked") {
			t.Errorf("a symbol read through a committed link outside the roots was ranked: %+v", r)
		}
		if strings.HasSuffix(r.Name, "f") && r.Count == 2 {
			sawF = true
		}
	}
	if !sawF {
		t.Errorf("f changed by both commits is not ranked twice: %s", text)
	}

	// A repository outside the roots is still refused.
	res, err = sess.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "touched", Arguments: map[string]any{"repo_path": filepath.Dir(outside)}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Errorf("touched walked a repository outside the roots: %s", res.Content[0].(*mcp.TextContent).Text)
	}
}
