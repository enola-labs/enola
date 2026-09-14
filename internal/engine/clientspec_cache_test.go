package engine_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/pkg/bootstrap"
)

// TestClientSpecs_EditingOnlyTheConfigReExtracts: the declared clients decide which call
// sites become routes, and nothing but the extractor's cache key tells a warm cache they
// changed. Two snapshots of one unchanged tree, the first with no client declared and
// the second with one, must differ. Had the second reused the first's cached TypeScript
// facts, the client would silently read as having found nothing.
func TestClientSpecs_EditingOnlyTheConfigReExtracts(t *testing.T) {
	root := copyTree(t, filepath.Join("testdata", "repos", "ts_custom_client_cluster"), t.TempDir())
	sdk := filepath.Join(root, "sdk")

	configuredRoutes := func(configPath string) int {
		t.Helper()
		eng, _, err := bootstrap.NewEngine(bootstrap.Options{ConfigPath: configPath})
		if err != nil {
			t.Fatalf("bootstrap.NewEngine: %v", err)
		}
		snap, err := eng.GenerateSnapshot(context.Background(), sdk, false)
		if err != nil {
			t.Fatalf("GenerateSnapshot: %v", err)
		}
		n := 0
		for _, f := range snap.Facts {
			if f.Kind == facts.KindRoute && f.PropString(facts.PropSource) == facts.RouteSourceConfiguredHTTPClient {
				n++
			}
		}
		return n
	}

	if n := configuredRoutes(filepath.Join(t.TempDir(), "no-such-config.yaml")); n != 0 {
		t.Fatalf("with no client declared, %d configured routes were read", n)
	}
	if n := configuredRoutes(filepath.Join("testdata", "configs", "ts_custom_client_no_alias.yaml")); n == 0 {
		t.Fatal("declaring a client on an unchanged tree read no routes: the cached result from before the edit was served")
	}
}
