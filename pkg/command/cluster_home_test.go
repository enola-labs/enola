package command

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/config"
	"github.com/enola-labs/enola/pkg/history"
)

// A cluster's artifacts and its history sit in one directory: config and pkg/history
// compute the cluster key separately (pkg/history may import only internal/facts), and
// this is what keeps them from drifting apart.
func TestClusterHome_HoldsTheClusterHistory(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	var members []string
	for _, n := range []string{"web", "api", "billing service"} {
		p := filepath.Join(root, n)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		members = append(members, p)
	}
	hist, err := history.ClusterRoot(members, "")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := config.Default().GraphOutputDir(members), filepath.Dir(hist); got != want {
		t.Errorf("cluster output %s and cluster history %s are in different directories", got, hist)
	}
}
