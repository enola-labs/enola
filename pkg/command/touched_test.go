package command

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/enola-labs/enola/internal/touched"
)

func touchedRepo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "app")
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	commit := func(subject string, files map[string]string) {
		t.Helper()
		for name, content := range files {
			path := filepath.Join(dir, name)
			if content == "" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				continue
			}
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		git("add", "-A")
		git("commit", "-q", "-m", subject)
	}
	git("init", "-q")
	const b1 = "export function beta() {\n  return 1;\n}\n"
	commit("start", map[string]string{
		"src/a.ts": "export function alpha() {\n  return 1;\n}\n\nexport function omega() {\n  return 1;\n}\n",
		"src/b.ts": b1,
	})
	commit("fix: alpha", map[string]string{"src/a.ts": "export function alpha() {\n  return 2;\n}\n\nexport function omega() {\n  return 1;\n}\n"})
	commit("chore: beta", map[string]string{"src/b.ts": "export function beta() {\n  return 2;\n}\n"})
	commit("fix: alpha and omega", map[string]string{"src/a.ts": "export function alpha() {\n  return 3;\n}\n\nexport function omega() {\n  return 3;\n}\n"})
	commit("fix: remove beta", map[string]string{"src/b.ts": ""})
	return dir
}

func runTouched(t *testing.T, args ...string) []byte {
	t.Helper()
	var out, errOut bytes.Buffer
	if code := testRunner().runTouched(context.Background(), args, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	return out.Bytes()
}

func TestTouchedRanksTheSymbolsCommitsChanged(t *testing.T) {
	dir := touchedRepo(t)
	raw := runTouched(t, "--subject", "^fix", "--json", dir)
	var res touched.Result
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if res.Symbols != 2 {
		t.Errorf("%d symbols, want 2", res.Symbols)
	}
	if len(res.Commits) != 3 || res.RankedCommits != 2 {
		t.Fatalf("%d commits, %d ranked, want 3 and 2", len(res.Commits), res.RankedCommits)
	}
	if res.Commits[0].Subject != "fix: alpha" {
		t.Errorf("first commit %q, want the oldest", res.Commits[0].Subject)
	}
	if len(res.Ranking) != 2 || res.Ranking[0].Name != "src.alpha" || res.Ranking[0].Count != 2 ||
		res.Ranking[1].Name != "src.omega" || res.Ranking[1].Count != 1 {
		t.Errorf("ranking %+v", res.Ranking)
	}

	if again := runTouched(t, "--subject", "^fix", "--json", dir); !bytes.Equal(raw, again) {
		t.Error("a second walk answered differently")
	}

	status, err := exec.Command("git", "-C", dir, "status", "--porcelain", "--ignored").Output()
	if err != nil {
		t.Fatal(err)
	}
	if len(status) != 0 {
		t.Errorf("the walk wrote into the repository: %s", status)
	}
}

func TestTouchedMaxSymbolsLeavesSweepsOutOfTheRanking(t *testing.T) {
	var res touched.Result
	if err := json.Unmarshal(runTouched(t, "--subject", "^fix", "--max-symbols", "1", "--json", touchedRepo(t)), &res); err != nil {
		t.Fatal(err)
	}
	if res.RankedCommits != 1 || len(res.Ranking) != 1 || res.Ranking[0].Name != "src.alpha" || res.Ranking[0].Count != 1 {
		t.Errorf("%d ranked, ranking %+v", res.RankedCommits, res.Ranking)
	}
}

func TestTouchedRefusesWhatItCannotWalk(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := testRunner().runTouched(context.Background(), []string{t.TempDir()}, &out, &errOut); code != 2 {
		t.Errorf("a folder with no git history: exit %d, want 2", code)
	}
	if code := testRunner().runTouched(context.Background(), []string{"--subject", "(", touchedRepo(t)}, &out, &errOut); code != 2 {
		t.Errorf("a bad --subject: exit %d, want 2", code)
	}
}

func TestTouchedGrainCreditsAFactoryOrItsParts(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "app")
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	write := func(subject, a, b string) {
		t.Helper()
		src := "export function plugin() {\n  return {\n    one() {\n      return " + a + ";\n    },\n    two() {\n      return " + b + ";\n    },\n  };\n}\n"
		if err := os.WriteFile(filepath.Join(dir, "src/p.ts"), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		git("add", "-A")
		git("commit", "-q", "-m", subject)
	}
	git("init", "-q")
	write("add", "1", "1")
	write("change", "2", "2")

	ranking := func(args ...string) map[string]int {
		var res touched.Result
		if err := json.Unmarshal(runTouched(t, append(args, "--subject", "^change", "--json", dir)...), &res); err != nil {
			t.Fatal(err)
		}
		got := map[string]int{}
		for _, r := range res.Ranking {
			got[r.Name] = r.Count
		}
		return got
	}
	if got := ranking(); got["src.plugin.one"] != 1 || got["src.plugin.two"] != 1 || got["src.plugin"] != 0 {
		t.Errorf("member grain: %v", got)
	}
	if got := ranking("--grain", "declaration"); got["src.plugin"] != 1 || len(got) != 1 {
		t.Errorf("declaration grain: %v", got)
	}
}

func TestTouchedFollowsAFileThroughARename(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "app")
	if err := os.MkdirAll(filepath.Join(dir, "old"), 0o755); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	write := func(path, subject, v string) {
		t.Helper()
		src := "export function f() {\n  const a = 1;\n  const b = 2;\n  const c = 3;\n  const d = 4;\n  return " + v + ";\n}\n"
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, path), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		git("add", "-A")
		git("commit", "-q", "-m", subject)
	}
	git("init", "-q")
	write("old/a.ts", "add", "0")
	write("old/a.ts", "fix: one", "1")
	git("mv", "old/a.ts", "mid.ts")
	git("commit", "-q", "-m", "refactor: move")
	write("mid.ts", "fix: two", "2")
	if err := os.MkdirAll(filepath.Join(dir, "new"), 0o755); err != nil {
		t.Fatal(err)
	}
	git("mv", "mid.ts", "new/a.ts")
	git("commit", "-q", "-m", "refactor: move again")
	// A new file at the first path is another file.
	write("old/a.ts", "fix: three", "3")

	var res touched.Result
	if err := json.Unmarshal(runTouched(t, "--subject", "^fix", "--json", dir), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Ranking) != 2 || res.Symbols != 2 {
		t.Fatalf("%d symbols, ranking %+v", res.Symbols, res.Ranking)
	}
	moved, other := res.Ranking[0], res.Ranking[1]
	if moved.File != "new/a.ts" || moved.Count != 2 || !slices.Equal(moved.MovedFrom, []string{"old/a.ts", "mid.ts"}) {
		t.Errorf("moved row %+v", moved)
	}
	if other.File != "old/a.ts" || other.Count != 1 || other.MovedFrom != nil {
		t.Errorf("new file at the old path %+v", other)
	}
}

func TestTouchedExcludeDropsCommitsSubjectKept(t *testing.T) {
	var res touched.Result
	if err := json.Unmarshal(runTouched(t, "--subject", "^fix", "--exclude", "omega|remove", "--json", touchedRepo(t)), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Commits) != 1 || res.Commits[0].Subject != "fix: alpha" {
		t.Errorf("commits %+v, want only fix: alpha", res.Commits)
	}
}
