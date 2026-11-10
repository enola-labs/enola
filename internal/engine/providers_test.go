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
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/providers"
)

// writeFakeProvider writes an executable script honoring the provider
// contract: --version prints a fixed semver, any other invocation prints the
// given JSONL.
func writeFakeProvider(t *testing.T, jsonl string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-provider")
	// The newline before the delimiter is load-bearing: without it the heredoc
	// body ends in `{...}EOF` on one line, which the validator now rejects as
	// trailing data — the exact sloppiness that used to pass silently.
	script := "#!/bin/sh\n" +
		"for a in \"$@\"; do if [ \"$a\" = \"--version\" ]; then echo 1.0.0; exit 0; fi; done\n" +
		"cat <<'EOF'\n" + strings.TrimSuffix(jsonl, "\n") + "\nEOF\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func providerSnapshot(t *testing.T, repo string, provs []providers.Provider) *facts.Snapshot {
	t.Helper()
	cfg := config.Default()
	cfg.Providers = provs
	eng, err := engine.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	eng.RegisterExtractor(goextractor.New())
	snap, err := eng.GenerateSnapshot(context.Background(), repo, false)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func TestProviders_FactsMergeLabeledAndCensusRecorded(t *testing.T) {
	repo := writeGoRepo(t)
	script := writeFakeProvider(t,
		`{"kind":"dependency","name":"prov-call: m.M -> m.N","file":"m.go","props":{"resolution_level":"name-only"},"relations":[{"kind":"calls","target":"m.N"}]}`)
	snap := providerSnapshot(t, repo, []providers.Provider{{Name: "fake", Command: []string{script}}})

	var got *facts.Fact
	for i, f := range snap.Facts {
		if f.Name == "prov-call: m.M -> m.N" {
			got = &snap.Facts[i]
		}
	}
	if got == nil {
		t.Fatal("provider fact did not reach the snapshot")
	}
	// The extraction-window placement is what earns the label: provider facts
	// must be repo-tagged exactly as measured ones.
	if got.Repo != filepath.Base(repo) {
		t.Errorf("provider fact repo = %q, want the repo label", got.Repo)
	}
	if got.Props[providers.PropProvider] != "fake" || got.Props[providers.PropProviderVersion] != "1.0.0" {
		t.Errorf("provider fact provenance = %+v", got.Props)
	}
	if len(snap.Meta.Providers) != 1 || snap.Meta.Providers[0].Name != "fake" ||
		snap.Meta.Providers[0].FactCount != 1 || snap.Meta.Providers[0].Skipped {
		t.Errorf("census = %+v", snap.Meta.Providers)
	}
	if rec := snap.Meta.Receipt(); len(rec.Providers) != 1 || rec.Providers[0].Name != "fake" {
		t.Errorf("receipt census = %+v", rec.Providers)
	}
}

func TestProviders_MissingCommandIsACensusSkipNeverAnError(t *testing.T) {
	repo := writeGoRepo(t)
	snap := providerSnapshot(t, repo, []providers.Provider{
		{Name: "ghost", Command: []string{"/no/such/enola-provider"}}})
	if len(snap.Meta.Providers) != 1 || !snap.Meta.Providers[0].Skipped ||
		!strings.Contains(snap.Meta.Providers[0].Reason, "not found") {
		t.Fatalf("census = %+v, want a named skip", snap.Meta.Providers)
	}
}

// The seam's determinism promise, measured end to end: two independent runs
// over the same tree with the same provider produce the same snapshot ID —
// the content fingerprint over the byte-stable fact serialization.
func TestProviders_SnapshotIsDeterministic(t *testing.T) {
	repo := writeGoRepo(t)
	// Two facts emitted in non-sorted order, so the assertion covers the
	// pre-merge sort as well as the run order.
	script := writeFakeProvider(t,
		`{"kind":"symbol","name":"zz-last","file":"m.go","props":{"resolution_level":"name-only"}}`+"\n"+
			`{"kind":"dependency","name":"prov-call: m.M -> m.N","file":"m.go","props":{"resolution_level":"name-only"},"relations":[{"kind":"calls","target":"m.N"}]}`)
	provs := []providers.Provider{{Name: "fake", Command: []string{script}}}
	first := providerSnapshot(t, repo, provs)
	second := providerSnapshot(t, repo, provs)
	if first.Meta.SnapshotID != second.Meta.SnapshotID {
		t.Fatalf("snapshot IDs differ across identical runs: %s vs %s",
			first.Meta.SnapshotID, second.Meta.SnapshotID)
	}
}

func TestProviders_RuntimeObservationAnnotatesTheExtractedRoute(t *testing.T) {
	repo := writeGoRepo(t)
	writeFile(t, filepath.Join(repo, "routes.go"), `package m

import (
	"net/http"

	"github.com/gorilla/mux"
)

func SetupRoutes() {
	router := mux.NewRouter()
	router.HandleFunc("/api/users", GetUsers).Methods("GET")
	router.HandleFunc("/api/orders", GetOrders).Methods("GET")
	_ = router
}

func GetUsers(w http.ResponseWriter, r *http.Request)  {}
func GetOrders(w http.ResponseWriter, r *http.Request) {}
`)
	script := writeFakeProvider(t,
		`{"kind":"route","name":"runtime-route: GET /api/users","file":".enola-runtime/boot.json","props":{"resolution_level":"runtime-observed","observed_via":"rails-boot","method":"GET","path":"/api/users"}}`)
	snap := providerSnapshot(t, repo, []providers.Provider{{Name: "runtime", Command: []string{script}}})

	byName := map[string]facts.Fact{}
	for _, f := range snap.Facts {
		if f.Kind == facts.KindRoute {
			byName[f.Name] = f
		}
	}
	observed := byName["/api/users"]
	if observed.Props[providers.PropRuntimeObserved] != true ||
		observed.Props[providers.PropObservedVia] != "rails-boot" {
		t.Errorf("observed route props = %+v, want runtime_observed true via rails-boot", observed.Props)
	}
	unobserved := byName["/api/orders"]
	if _, claimed := unobserved.Props[providers.PropRuntimeObserved]; claimed {
		t.Errorf("a route the booted table does not serve must stay unannotated: %+v", unobserved.Props)
	}
	observation := byName["runtime-route: GET /api/users"]
	if observation.Repo != filepath.Base(repo) {
		t.Errorf("observation repo = %q, want the repo label", observation.Repo)
	}
	if observation.Props[providers.PropProvider] != "runtime" {
		t.Errorf("observation provenance = %+v", observation.Props)
	}
}

func TestProviders_DeclaredContractStampsTheExtractedSymbol(t *testing.T) {
	repo := writeGoRepo(t)
	script := writeFakeProvider(t,
		`{"kind":"symbol","name":"rbs-signature: m.M","file":"sig/m.rbs","props":{"resolution_level":"declared","declared_in":"sig/m.rbs","receiver":"m","method":"M","singleton":true,"signature":"() -> void"},"relations":[{"kind":"has_method","target":"m.M"}]}`)
	snap := providerSnapshot(t, repo, []providers.Provider{{Name: "rbs", Command: []string{script}}})

	byName := map[string]facts.Fact{}
	for _, f := range snap.Facts {
		if f.Kind == facts.KindSymbol {
			byName[f.Name] = f
		}
	}
	stamped := byName["m.M"]
	if stamped.Props[providers.PropTyped] != true ||
		stamped.Props[providers.PropDeclaredSignature] != "() -> void" ||
		stamped.Props[providers.PropDeclaredIn] != "sig/m.rbs" {
		t.Errorf("extracted symbol props = %+v, want the declared contract stamped on", stamped.Props)
	}
	if stamped.File != "m.go" {
		t.Errorf("extractor identity must survive: file = %q", stamped.File)
	}
	declaration := byName["rbs-signature: m.M"]
	if declaration.Repo != filepath.Base(repo) {
		t.Errorf("declaration repo = %q, want the repo label", declaration.Repo)
	}
	if _, claimed := declaration.Props[providers.PropTyped]; claimed {
		t.Errorf("the declaration itself must never be stamped: %+v", declaration.Props)
	}
}

func TestProviders_ExtractorIdentityWinsCollisions(t *testing.T) {
	repo := writeGoRepo(t)
	// The go extractor emits a symbol named m.M for this fixture; a provider
	// claiming the same name+kind must be skipped, never overwrite it.
	script := writeFakeProvider(t,
		`{"kind":"symbol","name":"m.M","file":"m.go","props":{"resolution_level":"name-only","forged":true}}`)
	snap := providerSnapshot(t, repo, []providers.Provider{{Name: "fake", Command: []string{script}}})
	for _, f := range snap.Facts {
		if f.Name == "m.M" && f.Props["forged"] == true {
			t.Fatal("a provider fact overwrote an extractor identity")
		}
	}
	if snap.Meta.Providers[0].FactCount != 0 {
		t.Errorf("census must show the collision was not merged: %+v", snap.Meta.Providers[0])
	}
}
