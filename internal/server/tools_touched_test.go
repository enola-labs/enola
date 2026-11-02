package server_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestTouchedToolLeavesCommitsOutUnlessAsked(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "app")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init", "-q")
	for _, v := range []string{"1", "2"} {
		if err := os.WriteFile(filepath.Join(dir, "a.ts"), []byte("export function f() {\n  return "+v+";\n}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git("add", "-A")
		git("commit", "-q", "-m", "fix: "+v)
	}
	s := startInMemory(t)
	call := func(args map[string]any) (ranked []map[string]any) {
		t.Helper()
		res, err := s.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "touched", Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		text := res.Content[0].(*mcp.TextContent).Text
		var out struct {
			Ranking []map[string]any `json:"ranking"`
		}
		if err := json.Unmarshal([]byte(text), &out); err != nil {
			t.Fatalf("%v: %s", err, strings.TrimSpace(text))
		}
		return out.Ranking
	}
	plain := call(map[string]any{"repo_path": dir})
	if len(plain) != 1 || plain[0]["count"] != float64(2) || plain[0]["commits"] != nil {
		t.Errorf("default: %+v", plain)
	}
	asked := call(map[string]any{"repo_path": dir, "commits": true})
	if len(asked) != 1 || asked[0]["commits"] == nil {
		t.Errorf("commits asked for: %+v", asked)
	}
}
