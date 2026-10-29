package engine_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/config"
	"github.com/enola-labs/enola/internal/engine"
	"github.com/enola-labs/enola/internal/extractors/goextractor"
	"github.com/enola-labs/enola/internal/extractors/manifestextractor"
	"github.com/enola-labs/enola/internal/facts"
)

// An extractor that walks from the repository root (manifests are read that way,
// because the ignore globs hide them) must see the same tree through a symlinked
// root as through the checkout, and a snapshot taken through the link must not leave
// a cache entry that a later snapshot of the checkout is served.
func TestSymlinkedRoot_ExtractorsWalkTheCheckout(t *testing.T) {
	real := t.TempDir()
	writeFile(t, filepath.Join(real, "go.mod"), "module real\n\ngo 1.21\n\nrequire github.com/acme/money v1.2.0\n")
	writeFile(t, filepath.Join(real, "r.go"), "package real\n\nfunc R() {}\n")
	link := filepath.Join(t.TempDir(), "aliased")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	snapshot := func(path string) []string {
		t.Helper()
		eng, err := engine.New(config.Default())
		if err != nil {
			t.Fatal(err)
		}
		eng.RegisterExtractor(goextractor.New())
		eng.RegisterExtractor(manifestextractor.New())
		if _, err := eng.GenerateSnapshot(context.Background(), path, false); err != nil {
			t.Fatal(err)
		}
		var deps []string
		for _, f := range eng.Store().ByKind(facts.KindDependency) {
			if strings.Contains(f.Name, "acme/money") {
				deps = append(deps, f.Name)
			}
		}
		return deps
	}
	viaLink := snapshot(link)
	viaCheckout := snapshot(real)
	if len(viaLink) == 0 {
		t.Fatal("the manifest's require was not read through the symlinked root")
	}
	if len(viaCheckout) != len(viaLink) {
		t.Errorf("checkout after a linked run read %v, the linked run %v", viaCheckout, viaLink)
	}
}
