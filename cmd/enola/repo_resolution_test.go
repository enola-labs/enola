package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var (
	buildOnce sync.Once
	builtDir  string
	builtBin  string
	buildErr  error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if builtDir != "" {
		_ = os.RemoveAll(builtDir)
	}
	os.Exit(code)
}

func enolaBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "enola-repoarg-bin-")
		if err != nil {
			buildErr = err
			return
		}
		builtDir = dir
		builtBin = filepath.Join(dir, "enola")
		cmd := exec.Command(goTool(), "build", "-o", builtBin, ".")
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("building enola: %v\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return builtBin
}

func goTool() string {
	if p, err := exec.LookPath("go"); err == nil {
		return p
	}
	return "go"
}

func sandboxEnv(home string) []string {
	return append(os.Environ(),
		"HOME="+home,
		"USERPROFILE="+home,
		"ENOLA_NO_UPDATE_CHECK=1",
	)
}

func writeGovernedRepo(t *testing.T, dir, ruleID string) string {
	t.Helper()
	files := map[string]string{
		"go.mod":     "module example.com/" + filepath.Base(dir) + "\n\ngo 1.21\n",
		"main.go":    "package main\n\nfunc main() {}\n",
		"pkg/a/a.go": "package a\n\nfunc Alpha() string { return \"a\" }\n",
		"pkg/b/b.go": "package b\n\nfunc Beta() string { return \"b\" }\n",
		"enola-intent.yaml": "components:\n" +
			"  - {name: pkg-a, match: [pkg/a/**]}\n" +
			"  - {name: pkg-b, match: [pkg/b/**]}\n" +
			"rules:\n" +
			"  - {id: " + ruleID + ", forbid: pkg-a, to: pkg-b, via: imports,\n" +
			"     because: " + ruleID + " keeps pkg-a deliverable alone}\n",
	}
	for path, body := range files {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func generateSnapshotCLI(t *testing.T, bin, home, workDir, repo string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--generate", repo)
	cmd.Dir = workDir
	cmd.Env = sandboxEnv(home)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("enola --generate %s: %v\n%s", repo, err, out)
	}
}

func serveMCP(t *testing.T, bin, home, workDir string, repoArgs ...string) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)

	cmd := exec.Command(bin, append([]string{"--no-dashboard"}, repoArgs...)...)
	cmd.Dir = workDir
	cmd.Env = sandboxEnv(home)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("server stderr:\n%s", stderr.String())
		}
	})

	client := mcp.NewClient(&mcp.Implementation{Name: "enola-repoarg-test", Version: "0"}, nil)
	cs, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("client Connect: %v\nserver stderr:\n%s", err, stderr.String())
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func callTool(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(%s) transport error: %v", name, err)
	}
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

func TestServe_RepoArgumentAuthoritative_CwdElsewhere(t *testing.T) {
	bin := enolaBinary(t)
	home := t.TempDir()
	repoA := writeGovernedRepo(t, filepath.Join(t.TempDir(), "repo-a"), "repo-a-guards-alpha")
	generateSnapshotCLI(t, bin, home, t.TempDir(), repoA)

	cs := serveMCP(t, bin, home, t.TempDir(), repoA)

	out := callTool(t, cs, "constraints_for", map[string]any{"target": "pkg/a/a.go"})
	if !strings.Contains(out, "repo-a-guards-alpha") {
		t.Errorf("constraints_for must answer for the repo the server was launched with, regardless of cwd; got:\n%s", out)
	}

	out = callTool(t, cs, "plan_check", map[string]any{"paths": []string{"pkg/a/a.go"}})
	if !strings.Contains(out, "repo-a-guards-alpha") {
		t.Errorf("plan_check must answer for the repo the server was launched with, regardless of cwd; got:\n%s", out)
	}
}

func TestServe_RepoArgumentWins_CwdInsideAnotherRepo(t *testing.T) {
	bin := enolaBinary(t)
	home := t.TempDir()
	repoA := writeGovernedRepo(t, filepath.Join(t.TempDir(), "repo-a"), "rule-of-repo-a")
	repoB := writeGovernedRepo(t, filepath.Join(t.TempDir(), "repo-b"), "rule-of-repo-b")
	neutral := t.TempDir()
	generateSnapshotCLI(t, bin, home, neutral, repoA)
	generateSnapshotCLI(t, bin, home, neutral, repoB)

	cs := serveMCP(t, bin, home, repoB, repoA)

	for _, tool := range []struct {
		name string
		args map[string]any
	}{
		{"constraints_for", map[string]any{"target": "pkg/a/a.go"}},
		{"plan_check", map[string]any{"paths": []string{"pkg/a/a.go"}}},
	} {
		out := callTool(t, cs, tool.name, tool.args)
		if !strings.Contains(out, "rule-of-repo-a") {
			t.Errorf("%s must answer for the launched repo A; got:\n%s", tool.name, out)
		}
		if strings.Contains(out, "rule-of-repo-b") || strings.Contains(out, repoB) {
			t.Errorf("%s answered from the cwd repo B instead of the launched repo A; got:\n%s", tool.name, out)
		}
	}
}

func TestServe_NoArgument_FallsBackToCwd(t *testing.T) {
	bin := enolaBinary(t)
	home := t.TempDir()
	repoA := writeGovernedRepo(t, filepath.Join(t.TempDir(), "repo-a"), "repo-a-guards-alpha")
	generateSnapshotCLI(t, bin, home, t.TempDir(), repoA)

	cs := serveMCP(t, bin, home, repoA)

	out := callTool(t, cs, "constraints_for", map[string]any{"target": "pkg/a/a.go"})
	if !strings.Contains(out, "repo-a-guards-alpha") {
		t.Errorf("with no repo argument the working directory must still resolve the repo; got:\n%s", out)
	}
}
