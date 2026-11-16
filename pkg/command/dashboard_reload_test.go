package command

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/pkg/bootstrap"
)

// A dashboard whose first load failed (the graph's files were being replaced as it
// started) tries again on its first poll, rather than taking the receipt it could not
// load for the one it shows and waiting for the next write.
func TestSnapshotReloader_RetriesAFailedFirstLoad(t *testing.T) {
	repo := writeGoRepo(t)
	cfgPath := filepath.Join(t.TempDir(), "mcp-arch.yaml")
	if err := os.WriteFile(cfgPath, []byte("repo: "+repo+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gen, _, err := bootstrap.NewEngine(bootstrap.Options{ConfigPath: cfgPath})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gen.GenerateSnapshot(context.Background(), repo, false); err != nil {
		t.Fatal(err)
	}
	if err := gen.WriteGraph(); err != nil {
		t.Fatal(err)
	}

	dash, _, err := bootstrap.NewEngine(bootstrap.Options{ConfigPath: cfgPath})
	if err != nil {
		t.Fatal(err)
	}
	reload := snapshotReloader(dash, dash.GraphDir([]string{repo}), false)
	changed, err := reload()
	if err != nil || !changed {
		t.Fatalf("the first poll did not load the graph: changed=%v err=%v", changed, err)
	}
	if dash.Store().Count() == 0 {
		t.Error("the dashboard holds no facts")
	}
	if changed, _ := reload(); changed {
		t.Error("the same graph was loaded twice")
	}
}
