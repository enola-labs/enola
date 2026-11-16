package command

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// A cluster config lists its members in whatever order its author wrote them, and the
// MCP tools see the same cluster as a repo_paths list or a folder with an order of its
// own. The pin therefore lives in the cluster's own directory, keyed by the member
// set, and in no member: `enola check` and diff_snapshot read the same pin, and every
// member's own baseline stays its own.
func TestBaseline_ClusterPinsInItsOwnDirectory(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	bin := filepath.Join(t.TempDir(), "enola")
	if out, err := exec.Command("go", "build", "-o", bin, "../../cmd/enola").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	parent := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(parent, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("api/go.mod", "module example.com/api\n\ngo 1.21\n")
	write("api/main.go", "package main\n\nfunc main() { serve() }\n\nfunc serve() {}\n")
	write("web/go.mod", "module example.com/web\n\ngo 1.21\n")
	write("web/main.go", "package main\n\nfunc main() { render() }\n\nfunc render() {}\n")
	// Listed in reverse: the first entry is not the member the set chooses.
	write("cluster.yaml", "repos:\n  - web\n  - api\n")

	home := t.TempDir()
	run := func(args ...string) (int, string) {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Dir = parent
		cmd.Env = append(os.Environ(), "HOME="+home, "ENOLA_NO_UPDATE_CHECK=1", "ENOLA_NO_PROMPTS=1")
		out, err := cmd.CombinedOutput()
		if exit, ok := err.(*exec.ExitError); ok {
			return exit.ExitCode(), string(out)
		} else if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return 0, string(out)
	}

	if code, out := run("baseline", "pin", "cluster.yaml"); code != 0 {
		t.Fatalf("baseline pin exited %d:\n%s", code, out)
	}
	// A member keeps its extractor cache; it gets no snapshot and no pin.
	for _, member := range []string{"api", "web"} {
		for _, name := range []string{"facts.jsonl", filepath.Join("baseline", "facts.jsonl")} {
			if _, err := os.Stat(filepath.Join(parent, member, ".enola", name)); err == nil {
				t.Errorf("the cluster wrote %s into %s's output dir", name, member)
			}
		}
	}
	pins, _ := filepath.Glob(filepath.Join(home, ".enola", "graphs", "*-cluster-*", "baseline", "facts.jsonl"))
	if len(pins) != 1 {
		t.Errorf("want one pin, in the cluster's own directory; found %v", pins)
	}
	if code, out := run("baseline", "show", "cluster.yaml"); code != 0 {
		t.Errorf("baseline show exited %d against the cluster's own pin:\n%s", code, out)
	}
	if code, out := run("check", "cluster.yaml"); code != 0 {
		t.Errorf("check exited %d against the cluster's own pin:\n%s", code, out)
	}
}
