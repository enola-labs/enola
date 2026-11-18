package engine_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/enola-labs/enola/internal/config"
	"github.com/enola-labs/enola/internal/version"
	"github.com/enola-labs/enola/pkg/bootstrap"
)

func gitCommitAll(t *testing.T, repo, msg string) {
	t.Helper()
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", msg}} {
		if args[0] == "init" {
			if _, err := os.Stat(filepath.Join(repo, ".git")); err == nil {
				continue
			}
		}
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("git %v: %v (%s)", args, err, out)
		}
	}
}

// A generate is skipped only when the graph on disk is what it would write: the same
// files, the same commit, the same config. Anything else is named, so a run that
// regenerates says why.
func TestCheckUnchanged(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	base := t.TempDir()
	src := filepath.Join("testdata", "repos")
	api := copyTree(t, filepath.Join(src, "go_httpclient_multirepo", "api"), base)
	sample := copyTree(t, filepath.Join(src, "go_sample"), base)
	for _, r := range []string{api, sample} {
		gitCommitAll(t, r, "init")
	}
	members := []string{api, sample}
	// Old enough that the hash records their times (a file modified in the last two
	// seconds before it is read is recorded without one).
	past := time.Now().Add(-time.Hour)
	_ = filepath.Walk(base, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			_ = os.Chtimes(p, past, past)
		}
		return nil
	})

	cfg := config.Default()
	cfg.Repos = members
	generate := func() *bootstrap.Engine {
		t.Helper()
		eng, err := bootstrap.NewEngineFromConfig(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := eng.GenerateCluster(context.Background(), members, false); err != nil {
			t.Fatal(err)
		}
		if err := eng.WriteGraph(); err != nil {
			t.Fatal(err)
		}
		return eng
	}
	eng := generate()
	expect := func(same bool, reason string) {
		t.Helper()
		u := eng.CheckUnchanged(members)
		if u.Same != same || !strings.Contains(u.Reason, reason) {
			t.Fatalf("CheckUnchanged = same %v, reason %q; want same %v, reason containing %q", u.Same, u.Reason, same, reason)
		}
	}
	expect(true, "")
	// Another release wrote it: rebuilt once, whatever the tree.
	built := version.Version
	version.Version = "9.9.9-other"
	expect(false, "written by enola")
	version.Version = built
	if u := eng.CheckUnchanged(members); u.Read != 0 {
		t.Errorf("an unchanged cluster read %d files; size and time should settle them", u.Read)
	}

	// The same content with a new time is read, and still the same.
	server := filepath.Join(api, "server.go")
	now := time.Now()
	if err := os.Chtimes(server, now, now); err != nil {
		t.Fatal(err)
	}
	if u := eng.CheckUnchanged(members); !u.Same || u.Read != 1 {
		t.Errorf("a touched file: same %v, %d read; want same, 1 read", u.Same, u.Read)
	}

	data, _ := os.ReadFile(server)
	if err := os.WriteFile(server, append(data, []byte("\n// changed\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	expect(false, "server.go changed")
	if err := os.WriteFile(server, data, 0o644); err != nil {
		t.Fatal(err)
	}
	expect(true, "") // read back to what was recorded

	extra := filepath.Join(sample, "extra.go")
	if err := os.WriteFile(extra, []byte("package gosample\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	expect(false, "extra.go added")
	if err := os.Remove(extra); err != nil {
		t.Fatal(err)
	}

	// A link to a directory is walked and never hashed: not an added file.
	if err := os.Symlink(filepath.Join(sample, "pkg"), filepath.Join(sample, "linked-dir.go")); err == nil {
		expect(true, "")
		if err := os.Remove(filepath.Join(sample, "linked-dir.go")); err != nil {
			t.Fatal(err)
		}
	}

	// A recorded file that is gone.
	victim := filepath.Join(api, "go.mod")
	keep, _ := os.ReadFile(victim)
	info, _ := os.Stat(victim)
	if err := os.Remove(victim); err != nil {
		t.Fatal(err)
	}
	expect(false, "removed")
	if err := os.WriteFile(victim, keep, 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(victim, info.ModTime(), info.ModTime())

	gitCommitAll(t, api, "another")
	expect(false, "another commit")
	eng = generate()
	expect(true, "")

	other := *cfg
	other.Ignore = append(append([]string{}, cfg.Ignore...), "**/never/**")
	eng2, err := bootstrap.NewEngineFromConfig(&other)
	if err != nil {
		t.Fatal(err)
	}
	if u := eng2.CheckUnchanged(members); u.Same {
		t.Error("another ignore set read as unchanged")
	}
	if u := eng.CheckUnchanged(members[:1]); u.Same {
		t.Error("another member set read as unchanged")
	}
}
