package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/clientspec"
	"github.com/enola-labs/enola/internal/config"
)

func folderOfRepos(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		if err := os.MkdirAll(filepath.Join(dir, n, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// No cluster config yet: one is written, the run indexes both repositories, and the
// settings of the config in force survive.
func TestClusterConfig_WritesTheConfigAndKeepsSettingsInForce(t *testing.T) {
	dir := folderOfRepos(t, "api", "web")
	inForce := config.Default()
	inForce.SourcePath = filepath.Join(t.TempDir(), "mcp-arch.yaml")
	inForce.Clients = []clientspec.Spec{{Name: "sdk-http", Language: "typescript"}}

	var out bytes.Buffer
	run, path, err := clusterConfig(&out, "enola", inForce, dir, []string{"api", "web"})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "cluster.yaml"); path != want {
		t.Fatalf("path = %q, want %q\n%s", path, want, out.String())
	}
	written, err := config.Load(path)
	if err != nil {
		t.Fatalf("written config must load: %v", err)
	}
	want := []string{filepath.Join(dir, "api"), filepath.Join(dir, "web")}
	if paths, _ := written.RepoPaths(); !reflect.DeepEqual(paths, want) {
		t.Errorf("written RepoPaths = %v, want %v", paths, want)
	}
	if paths, _ := run.RepoPaths(); !reflect.DeepEqual(paths, want) {
		t.Errorf("run RepoPaths = %v, want %v", paths, want)
	}
	if len(run.Clients) != 1 || len(inForce.Repos) != 0 {
		t.Errorf("run must keep clients without changing the config in force: run=%+v inForce.Repos=%v", run.Clients, inForce.Repos)
	}
	for _, want := range []string{"Detected 2 git repositories in " + dir + ": api, web", "Wrote " + path, "Settings from", "--generate --no-cluster " + dir} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output omits %q:\n%s", want, out.String())
		}
	}
}

func TestClusterConfig_ExistingConfigIsUsedNotRewritten(t *testing.T) {
	dir := folderOfRepos(t, "api", "web")
	existing := filepath.Join(dir, "cluster.yaml")
	if err := os.WriteFile(existing, []byte("repos:\n  - api\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	run, path, err := clusterConfig(&out, "enola", config.Default(), dir, []string{"api", "web"})
	if err != nil || path != existing {
		t.Fatalf("path = %q, err = %v; want the existing config", path, err)
	}
	if body, _ := os.ReadFile(existing); string(body) != "repos:\n  - api\n" {
		t.Errorf("existing config was rewritten:\n%s", body)
	}
	if paths, _ := run.RepoPaths(); len(paths) != 1 {
		t.Errorf("the existing config's repositories must be the ones indexed, got %v", paths)
	}
	if !strings.Contains(out.String(), "already there") {
		t.Errorf("output must say the existing config is used:\n%s", out.String())
	}
}

// A folder that cannot be written to is still indexed as a cluster, without a file.
func TestClusterConfig_UnwritableFolderStillClusters(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root writes through directory permissions")
	}
	dir := folderOfRepos(t, "api", "web")
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	var out bytes.Buffer
	run, path, err := clusterConfig(&out, "enola", config.Default(), dir, []string{"api", "web"})
	if err != nil || path != "" {
		t.Fatalf("path = %q, err = %v; want a cluster run without a file", path, err)
	}
	if paths, _ := run.RepoPaths(); len(paths) != 2 {
		t.Errorf("RepoPaths = %v, want the two repositories", paths)
	}
	if !strings.Contains(out.String(), "for this run only") {
		t.Errorf("output must say nothing was written:\n%s", out.String())
	}
}

func TestFoldedRepos_ClusterConfigSaysNothing(t *testing.T) {
	dir := folderOfRepos(t, "api", "web")
	if _, repos := foldedRepos(&config.Config{Repo: dir}); len(repos) != 2 {
		t.Errorf("a folder of two repositories as repo: must be reported, got %v", repos)
	}
	if _, repos := foldedRepos(&config.Config{Repo: dir, Repos: []string{"api", "web"}}); repos != nil {
		t.Errorf("a config with repos: already names a cluster, got %v", repos)
	}
}
