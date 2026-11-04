package history

import (
	"path/filepath"
	"strings"
	"testing"
)

// One member set is one cluster history, however the members are listed, and it is
// named after their folder. A different set is a different history.
func TestClusterRoot(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	api, web := filepath.Join(dir, "services", "api"), filepath.Join(dir, "services", "web")

	r1, err := ClusterRoot([]string{api, web}, "")
	if err != nil {
		t.Fatal(err)
	}
	r2, _ := ClusterRoot([]string{web, api}, "")
	if r1 != r2 {
		t.Errorf("member order changed the root: %s vs %s", r1, r2)
	}
	if !strings.HasPrefix(filepath.Base(filepath.Dir(r1)), "services-cluster-") {
		t.Errorf("root %s is not named after the members' folder", r1)
	}
	if r3, _ := ClusterRoot([]string{api}, ""); r3 == r1 {
		t.Error("a different member set shares the history")
	}

	shared := filepath.Join(dir, "hist")
	r4, _ := ClusterRoot([]string{api, web}, shared)
	if filepath.Dir(filepath.Dir(r4)) != shared || filepath.Base(filepath.Dir(r4)) != ClusterDirName {
		t.Errorf("with history.dir the cluster lives in %s/%s/<key>, got %s", shared, ClusterDirName, r4)
	}
}
