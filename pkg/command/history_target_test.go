package command

import (
	"os"
	"path/filepath"
	"testing"
)

// mkRepos creates dir/<name>/.git for each name, enough for workspace to see a repository.
func mkRepos(t *testing.T, dir string, names ...string) {
	t.Helper()
	for _, n := range names {
		if err := os.MkdirAll(filepath.Join(dir, n, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func repoNames(s historyScope) []string {
	out := make([]string, len(s.repos))
	for i, r := range s.repos {
		out[i] = filepath.Base(r)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Run from a folder of repositories, a history command used to resolve the folder itself
// and report an empty history: cluster snapshots record into each member, never the folder.
func TestHistoryTarget_FolderOfReposIsACluster(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	mkRepos(t, dir, "api", "web")
	t.Chdir(dir)

	r := &Runner{}
	for _, arg := range []string{"", dir} {
		s := r.historyTargetOf("gc", arg)
		if s.cluster != dir {
			t.Errorf("arg %q: cluster = %q, want the folder %s", arg, s.cluster, dir)
		}
		if got := repoNames(s); !equalStrings(got, []string{"api", "web"}) {
			t.Errorf("arg %q: repos = %v, want [api web]", arg, got)
		}
	}
}

// A cluster.yaml beside the repositories is the user's decision and wins over discovery:
// it may list fewer repositories, or ones elsewhere.
func TestHistoryTarget_ClusterConfigWins(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	mkRepos(t, dir, "api", "web", "scratch")
	cfg := filepath.Join(dir, "cluster.yaml")
	if err := os.WriteFile(cfg, []byte("repos:\n  - web\n  - api\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	r := &Runner{}
	for _, arg := range []string{"", ".", "cluster.yaml"} {
		s := r.historyTargetOf("gc", arg)
		if s.cluster != cfg {
			t.Errorf("arg %q: cluster = %q, want %s", arg, s.cluster, cfg)
		}
		if got := repoNames(s); !equalStrings(got, []string{"web", "api"}) {
			t.Errorf("arg %q: repos = %v, want [web api]", arg, got)
		}
	}
}

// A git repository is one repository even when checkouts are nested inside it, as for
// --generate.
func TestHistoryTarget_RepoStaysSingle(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	mkRepos(t, dir, "a", "b")
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	s := (&Runner{}).historyTargetOf("gc", "")
	if s.cluster != "" || len(s.repos) != 1 || s.repos[0] != dir {
		t.Errorf("got cluster %q repos %v, want the repository itself", s.cluster, s.repos)
	}
}

// An absolute history.dir is shared by every member; it is one history and is collected
// once.
func TestHistoryRoots_SharedDirCollectedOnce(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	mkRepos(t, dir, "api", "web")
	shared := filepath.Join(t.TempDir(), "hist")
	body := "repos:\n  - api\n  - web\nhistory:\n  dir: " + shared + "\n"
	if err := os.WriteFile(filepath.Join(dir, "cluster.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	roots, err := (&Runner{}).historyTargetOf("gc", "").roots()
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 1 || roots[0].root != shared || !equalStrings(roots[0].labels, []string{"api", "web"}) {
		t.Errorf("roots = %+v, want one shared root labelled [api web]", roots)
	}
}
