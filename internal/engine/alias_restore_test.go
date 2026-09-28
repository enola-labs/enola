package engine_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/pkg/bootstrap"
)

// The graph follows a package-qualified call into the repository that declares it,
// through the store's target aliases, and still does after the snapshot is restored
// from disk: the aliases are not in facts.jsonl, so restore must rebuild them before
// it builds the graph.
func TestGraph_CrossRepoAliasSurvivesRestore(t *testing.T) {
	const from, to = "src/catalog.CatalogService.items", "src/connectors.ResourceConnector.getCatalogItems"
	if _, err := os.Stat(customClientExample); err != nil {
		t.Skipf("example not present: %v", err)
	}
	root := copyTree(t, customClientExample, t.TempDir())
	config := filepath.Join(root, "cluster-with-params.yaml")

	eng, cfg, err := bootstrap.NewEngine(bootstrap.Options{ConfigPath: config})
	if err != nil {
		t.Fatal(err)
	}
	paths, err := cfg.RepoPaths()
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range paths {
		if _, err := eng.GenerateSnapshot(context.Background(), p, i > 0); err != nil {
			t.Fatalf("GenerateSnapshot(%s): %v", p, err)
		}
	}
	if p := eng.Store().Graph().FindPath(from, to, nil, 3); !p.Found {
		t.Fatalf("generated graph does not follow the aliased call: %+v", p)
	}
	backend := filepath.Join(root, "backend")
	if err := eng.WriteArtifacts(backend); err != nil {
		t.Fatalf("WriteArtifacts: %v", err)
	}

	restored, _, err := bootstrap.NewEngine(bootstrap.Options{ConfigPath: config})
	if err != nil {
		t.Fatal(err)
	}
	if err := restored.RestoreFromDir(eng.OutputDir(backend), nil, ""); err != nil {
		t.Fatalf("RestoreFromDir: %v", err)
	}
	if p := restored.Store().Graph().FindPath(from, to, nil, 3); !p.Found {
		t.Fatalf("restored graph lost the aliased call: %+v", p)
	}
}
