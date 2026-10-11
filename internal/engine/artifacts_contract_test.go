package engine_test

// What a snapshot leaves on disk is read by other processes: a later run of the
// gate, a restarted server, the dashboard, a consumer loading the artifacts into
// its own store. They find files by name and fields by key, so both are a
// contract, and neither is written down anywhere a compiler checks.
//
// These tests pin the two things a refactor of the writers can move without
// noticing: WHICH files exist after each step of the snapshot, baseline and
// receipt cycle, and the key shape of every JSON document among them. Values are
// left out on purpose. The fact goldens pin what is extracted; this pins where it
// lands and how it is laid out.
//
// Regenerate with:
//
//	go test ./internal/engine -run TestArtifacts -update

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/enola-labs/enola/pkg/bootstrap"
)

const artifactsSessionPID = 4242

var (
	// Per-workspace names end in a hash of an absolute path.
	workspaceKeyRe = regexp.MustCompile(`-[0-9a-f]{8}\b`)
	// History stores content-addressed files.
	contentNameRe = regexp.MustCompile(`[0-9a-f]{12,}`)
)

// listTree returns every file under root, relative and slash-separated, with the
// machine-specific parts of each name normalized.
func listTree(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		rel = workspaceKeyRe.ReplaceAllString(rel, "-<KEY>")
		rel = contentNameRe.ReplaceAllString(rel, "<CONTENT>")
		out = append(out, rel)
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("walking %s: %v", root, err)
	}
	sort.Strings(out)
	return out
}

// keyShape flattens a JSON document into its sorted key paths, each with the
// type of what it holds. Array elements are merged under "[]", so the shape does
// not depend on how many there are.
func keyShape(t *testing.T, data []byte) []string {
	t.Helper()
	var doc any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	seen := map[string]bool{}
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch x := v.(type) {
		case map[string]any:
			if len(x) == 0 {
				seen[path+" object"] = true
			}
			for k, child := range x {
				walk(path+"."+k, child)
			}
		case []any:
			if len(x) == 0 {
				seen[path+"[] empty"] = true
			}
			for _, child := range x {
				walk(path+"[]", child)
			}
		case string:
			seen[path+" string"] = true
		case float64:
			seen[path+" number"] = true
		case bool:
			seen[path+" bool"] = true
		case nil:
			seen[path+" null"] = true
		}
	}
	walk("", doc)
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func assertTextGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", "golden", "artifacts", name)
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run `go test ./internal/engine -run TestArtifacts -update` to create it): %v", err)
	}
	if !bytes.Equal(want, got) {
		t.Errorf("%s differs from its golden; run `go test ./internal/engine -run TestArtifacts -update` and review the diff.\n--- want\n%s\n--- got\n%s",
			name, want, got)
	}
}

// artifactsRun snapshots go_sample the way a serving session does and returns
// the engine, the repository and the sandboxed home.
func artifactsRun(t *testing.T) (eng *bootstrap.Engine, repo, home string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	repo = copyTree(t, filepath.Join("testdata", "repos", "go_sample"), t.TempDir())

	eng, _, err := bootstrap.NewEngine(bootstrap.Options{
		ConfigPath: filepath.Join(t.TempDir(), "no-such-config.yaml"),
	})
	if err != nil {
		t.Fatalf("bootstrap.NewEngine: %v", err)
	}
	eng.SetSessionClient(artifactsSessionPID)
	return eng, repo, home
}

func snapshotAndWrite(t *testing.T, eng *bootstrap.Engine, repo string) {
	t.Helper()
	if _, err := eng.GenerateSnapshot(context.Background(), repo, false); err != nil {
		t.Fatalf("GenerateSnapshot: %v", err)
	}
	if err := eng.WriteArtifacts(repo); err != nil {
		t.Fatalf("WriteArtifacts: %v", err)
	}
	if err := eng.WriteGlobalReceipt(); err != nil {
		t.Fatalf("WriteGlobalReceipt: %v", err)
	}
}

// TestArtifacts_Layout pins which files exist after each step: a first snapshot,
// a second one (which rotates the first into previous/), and a pinned baseline.
func TestArtifacts_Layout(t *testing.T) {
	eng, repo, home := artifactsRun(t)
	outDir := eng.OutputDir(repo)

	var doc strings.Builder
	record := func(step string) {
		fmt.Fprintf(&doc, "## %s\n\noutput dir:\n", step)
		for _, f := range listTree(t, outDir) {
			fmt.Fprintf(&doc, "  %s\n", f)
		}
		doc.WriteString("home:\n")
		for _, f := range listTree(t, filepath.Join(home, ".enola")) {
			fmt.Fprintf(&doc, "  %s\n", f)
		}
		doc.WriteString("\n")
	}

	snapshotAndWrite(t, eng, repo)
	record("first snapshot")

	snapshotAndWrite(t, eng, repo)
	record("second snapshot")

	if err := eng.SetBaseline(repo); err != nil {
		t.Fatalf("SetBaseline: %v", err)
	}
	record("baseline pinned")

	snapshotAndWrite(t, eng, repo)
	record("snapshot after the pin")

	assertTextGolden(t, "layout.txt", []byte(doc.String()))
}

// TestArtifacts_Shape pins the key shape of every JSON document a snapshot
// writes, in the repository and under the home directory.
func TestArtifacts_Shape(t *testing.T) {
	eng, repo, home := artifactsRun(t)
	snapshotAndWrite(t, eng, repo)
	outDir := eng.OutputDir(repo)

	docs := map[string]string{}
	for _, f := range listTree(t, outDir) {
		// The extractor cache is keyed by content hash and holds raw facts: it is
		// the engine's own scratch file, read by nothing else.
		if strings.HasSuffix(f, ".json") && f != "extractor_cache.json" {
			docs["output/"+f] = filepath.Join(outDir, filepath.FromSlash(f))
		}
	}
	// Under home the names carry the workspace key, so resolve them by walking.
	homeRoot := filepath.Join(home, ".enola")
	err := filepath.WalkDir(homeRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".json") {
			return err
		}
		rel, _ := filepath.Rel(homeRoot, path)
		rel = workspaceKeyRe.ReplaceAllString(filepath.ToSlash(rel), "-<KEY>")
		docs["home/"+rel] = path
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) == 0 {
		t.Fatal("the snapshot wrote no JSON documents")
	}

	names := make([]string, 0, len(docs))
	for name := range docs {
		names = append(names, name)
	}
	sort.Strings(names)

	var out strings.Builder
	for _, name := range names {
		data, err := os.ReadFile(docs[name])
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&out, "## %s\n\n", name)
		for _, line := range keyShape(t, data) {
			fmt.Fprintf(&out, "%s\n", line)
		}
		out.WriteString("\n")
	}
	assertTextGolden(t, "shape.txt", []byte(out.String()))
}
