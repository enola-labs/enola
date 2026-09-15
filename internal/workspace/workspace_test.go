package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/config"
)

func mkdirs(t *testing.T, root string, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if err := os.MkdirAll(filepath.Join(root, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Only the first level counts, a .git file counts as much as a directory, and hidden
// folders, node_modules and plain folders are not repositories.
func TestChildRepos_ImmediateRepositoriesOnly(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "api/.git", ".hidden/.git", "node_modules/.git", "plain/src", "deep/nested/.git")
	writeFile(t, filepath.Join(root, "sub", ".git"), "gitdir: ../.git/modules/sub\n")
	writeFile(t, filepath.Join(root, "README.md"), "x")

	if got, want := ChildRepos(root), []string{"api", "sub"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ChildRepos = %v, want %v", got, want)
	}
}

func TestFolded(t *testing.T) {
	one := t.TempDir()
	mkdirs(t, one, "api/.git", "docs")
	if got := Folded(one, ""); got != nil {
		t.Errorf("one child repository is an ordinary layout, got %v", got)
	}

	two := t.TempDir()
	mkdirs(t, two, "api/.git", "web/.git")
	if got := Folded(two, ""); !reflect.DeepEqual(got, []string{"api", "web"}) {
		t.Errorf("Folded = %v, want [api web]", got)
	}
	if got := Folded(two, filepath.Join(t.TempDir(), "mcp-arch.yaml")); len(got) != 2 {
		t.Errorf("a config elsewhere decides nothing about this folder, got %v", got)
	}
	if got := Folded(two, filepath.Join(two, "mcp-arch.yaml")); got != nil {
		t.Errorf("a config inside the folder means the layout was chosen, got %v", got)
	}
}

// The written file must load back as exactly the repositories it was written from,
// resolved against its own directory.
func TestWriteClusterConfig_LoadsAsTheSameRepositories(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "api/.git", "web/.git")
	repos := ChildRepos(root)

	path, err := WriteClusterConfig(root, repos)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(path)
	if !strings.Contains(string(body), "repos:\n  - api\n  - web\n") {
		t.Errorf("unexpected layout:\n%s", body)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("written config must load: %v", err)
	}
	got, err := cfg.RepoPaths()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(root, "api"), filepath.Join(root, "web")}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("RepoPaths = %v, want %v", got, want)
	}
}

func TestWriteClusterConfig_NeverOverwrites(t *testing.T) {
	root := t.TempDir()
	existing := filepath.Join(root, ClusterFileName)
	writeFile(t, existing, "repos:\n  - mine\n")

	if _, err := WriteClusterConfig(root, []string{"api", "web"}); !errors.Is(err, ErrClusterExists) {
		t.Fatalf("err = %v, want ErrClusterExists", err)
	}
	if body, _ := os.ReadFile(existing); string(body) != "repos:\n  - mine\n" {
		t.Errorf("existing config was changed:\n%s", body)
	}
}

func TestFoldWarning_NamesTheRemedyForTheFolderAsItIs(t *testing.T) {
	root := t.TempDir()
	repos := []string{"api", "web"}

	w := FoldWarning("enola", root, repos)
	for _, want := range []string{"holds 2 git repositories: api, web", "enola cluster init " + root, "--generate " + filepath.Join(root, ClusterFileName), DocsURL} {
		if !strings.Contains(w, want) {
			t.Errorf("warning lacks %q:\n%s", want, w)
		}
	}

	writeFile(t, filepath.Join(root, ClusterFileName), "repos: [api, web]\n")
	w = FoldWarning("enola", root, repos)
	if strings.Contains(w, "cluster init") || !strings.Contains(w, "already there") {
		t.Errorf("with a cluster config present the remedy is to use it:\n%s", w)
	}
}

func TestNames_SummarisesLongLists(t *testing.T) {
	repos := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}
	if got, want := Names(repos), "a, b, c, d, e, f, g, h and 2 more"; got != want {
		t.Errorf("Names = %q, want %q", got, want)
	}
}
