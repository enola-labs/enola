package server_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
		if err := os.WriteFile(filepath.Join(dir, "a.ts"), []byte("export function f() {\n  return "+v+";\n}\n\nexport function g() {\n  return "+v+";\n}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git("add", "-A")
		git("commit", "-q", "-m", "fix: "+v)
	}
	s := startInMemory(t)
	call := func(args map[string]any) (ranked []map[string]any, symbols int) {
		t.Helper()
		res, err := s.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "touched", Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		text := res.Content[0].(*mcp.TextContent).Text
		var out struct {
			Note    string           `json:"note"`
			Symbols int              `json:"symbols"`
			Ranking []map[string]any `json:"ranking"`
		}
		if err := json.Unmarshal([]byte(text), &out); err != nil {
			t.Fatalf("%v: %s", err, strings.TrimSpace(text))
		}
		if !strings.Contains(out.Note, "nested") {
			t.Errorf("note %q does not say how nested functions count", out.Note)
		}
		return out.Ranking, out.Symbols
	}
	plain, symbols := call(map[string]any{"repo_path": dir, "top": 1})
	if symbols != 2 {
		t.Errorf("top 1 reported %d symbols, want all 2", symbols)
	}
	if len(plain) != 1 || plain[0]["count"] != float64(2) || plain[0]["commits"] != nil {
		t.Errorf("default: %+v", plain)
	}
	asked, _ := call(map[string]any{"repo_path": dir, "commits": true})
	if len(asked) != 2 || asked[0]["commits"] == nil {
		t.Errorf("commits asked for: %+v", asked)
	}
}

func TestTouchedToolTakesARepositoryLabel(t *testing.T) {
	root := t.TempDir()
	mk := func(name, fn string) string {
		t.Helper()
		dir := filepath.Join(root, name)
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
			if err := os.WriteFile(filepath.Join(dir, "a.ts"), []byte("export function "+fn+"() {\n  return "+v+";\n}\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			git("add", "-A")
			git("commit", "-q", "-m", "fix: "+v)
		}
		return dir
	}
	shop, depot := mk("shop", "checkout"), mk("depot", "restock")
	s := startInMemory(t)
	if res := s.call(t, "generate_snapshot", map[string]any{"repo_paths": []string{shop, depot}}); res.IsError {
		t.Fatalf("generate_snapshot: %s", res.Content[0].(*mcp.TextContent).Text)
	}
	res := s.call(t, "touched", map[string]any{"repo": "depot"})
	text := res.Content[0].(*mcp.TextContent).Text
	if res.IsError {
		t.Fatalf("touched by label: %s", text)
	}
	if !strings.Contains(text, "restock") || strings.Contains(text, "checkout") {
		t.Errorf("touched by label walked the wrong repository: %s", text)
	}
	res = s.call(t, "touched", map[string]any{"repo": "depot", "commits": true})
	var listed struct {
		CommitList []struct {
			Subject string   `json:"subject"`
			Symbols []string `json:"symbols"`
		} `json:"commit_list"`
	}
	if err := json.Unmarshal([]byte(res.Content[0].(*mcp.TextContent).Text), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.CommitList) != 2 || listed.CommitList[0].Subject != "fix: 2" || !slices.ContainsFunc(listed.CommitList[0].Symbols, func(n string) bool { return strings.HasSuffix(n, "restock") }) {
		t.Errorf("commit_list, newest first with the functions each changed: %+v", listed.CommitList)
	}
	if res := s.call(t, "touched", map[string]any{"repo": "warehouse"}); !res.IsError {
		t.Errorf("an unknown label was walked: %s", res.Content[0].(*mcp.TextContent).Text)
	}
}

// A commit that changed only files the ignore settings leave out has no symbols,
// and the result says why: it counts those files in total and on the commit.
func TestTouchedToolCountsIgnoredFiles(t *testing.T) {
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
	for i, name := range []string{"a.ts", "a.test.ts"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("export function f() {\n  return 1;\n}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git("add", "-A")
		git("commit", "-q", "-m", "fix: "+name+strings.Repeat("!", i))
	}
	s := startInMemory(t)
	res, err := s.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "touched", Arguments: map[string]any{"repo_path": dir, "commits": true}})
	if err != nil {
		t.Fatal(err)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	var out struct {
		FilesIgnored *int `json:"files_ignored"`
		CommitList   []struct {
			Subject      string   `json:"subject"`
			Symbols      []string `json:"symbols"`
			FilesIgnored int      `json:"files_ignored"`
		} `json:"commit_list"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("%v: %s", err, text)
	}
	if out.FilesIgnored == nil || *out.FilesIgnored != 1 {
		t.Errorf("files_ignored missing or wrong: %s", text)
	}
	if len(out.CommitList) != 2 || out.CommitList[0].FilesIgnored != 1 || len(out.CommitList[0].Symbols) != 0 ||
		out.CommitList[1].FilesIgnored != 0 || len(out.CommitList[1].Symbols) != 1 {
		t.Errorf("commit_list: %+v", out.CommitList)
	}
}
