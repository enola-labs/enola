package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/config"
	"github.com/enola-labs/enola/internal/extractors/goextractor"
)

// A cluster extracted several repositories at a time is the cluster extracted one
// after another, fact for fact and in the same order, including the members whose
// label is only settled by the ones before them: a second owner's repository of the
// same name, another checkout of a member, and a member listed twice.
func TestGenerateCluster_JobsDoNotChangeTheUnion(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	base := t.TempDir()
	web := filepath.Join(base, "web")
	goCheckout(t, web, "git@github.com:acme/web.git")
	fork := filepath.Join(base, "fork", "web")
	goCheckout(t, fork, "git@github.com:other-org/web.git")
	clone := filepath.Join(base, "web-clone")
	goCheckout(t, clone, "https://github.com/acme/web")
	api := filepath.Join(base, "api")
	goCheckout(t, api, "git@github.com:acme/api.git")
	writeTree(t, api, map[string]string{"pkg/c/c.go": "package c\n\n// C is documented.\nfunc C() {}\n"})
	plain := t.TempDir()
	writeTree(t, plain, map[string]string{
		"go.mod":     "module example.com/plain\n\ngo 1.22\n",
		"pkg/p/p.go": "package p\n\nfunc P() {}\n",
	})
	repos := []string{web, api, fork, plain, clone, api}

	union := func(jobs int) ([]string, map[string]string, string) {
		cfg := config.Default()
		cfg.ClusterJobs = jobs
		cfg.Output.Root = t.TempDir()
		eng, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		eng.RegisterExtractor(goextractor.New())
		var logs bytes.Buffer
		log.SetOutput(&logs)
		_, err = eng.GenerateCluster(context.Background(), repos, false)
		log.SetOutput(os.Stderr)
		if err != nil {
			t.Fatalf("jobs %d: %v", jobs, err)
		}
		var lines []string
		for _, f := range eng.Store().FactsRef() {
			b, err := json.Marshal(f)
			if err != nil {
				t.Fatal(err)
			}
			lines = append(lines, string(b))
		}
		return lines, eng.RepoPaths(), logs.String()
	}

	one, onePaths, _ := union(1)
	four, fourPaths, logs := union(4)
	if !strings.Contains(logs, "extracting it again") {
		t.Error("no member took a label other than its own, so the fallback was not exercised")
	}
	if len(one) != len(four) {
		t.Fatalf("one job: %d facts, four jobs: %d", len(one), len(four))
	}
	for i := range one {
		if one[i] != four[i] {
			t.Fatalf("fact %d differs:\n one job:   %s\n four jobs: %s", i, one[i], four[i])
		}
	}
	if len(onePaths) != len(fourPaths) {
		t.Fatalf("repo paths: one job %v, four jobs %v", onePaths, fourPaths)
	}
	for label, path := range onePaths {
		if fourPaths[label] != path {
			t.Errorf("repo %q: one job %s, four jobs %s", label, path, fourPaths[label])
		}
	}
}

// The weight is the git index of the checkout holding the member: its own, a
// subdirectory's checkout's, or a worktree's own index. None outside a checkout.
func TestExtractionWeight(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	base := t.TempDir()
	main := filepath.Join(base, "demo")
	goCheckout(t, main, "git@github.com:acme/demo.git")
	wt := filepath.Join(base, "demo-wt")
	gitIn(t, main, "worktree", "add", "-q", wt, "-b", "wt")
	st, err := os.Stat(filepath.Join(main, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		dir  string
		want bool
	}{
		{main, true},
		{filepath.Join(main, "pkg", "a"), true},
		{wt, true},
		{t.TempDir(), false},
	} {
		got := extractionWeight(c.dir)
		if c.want && got != st.Size() {
			t.Errorf("extractionWeight(%s) = %d, want the index size %d", c.dir, got, st.Size())
		}
		if !c.want && got != 0 {
			t.Errorf("extractionWeight(%s) = %d outside a checkout, want 0", c.dir, got)
		}
	}
}
