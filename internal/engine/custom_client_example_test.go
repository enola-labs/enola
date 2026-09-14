package engine_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/enola-labs/enola/internal/config"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/linkers/crossrepo/routeindex"
	httpsignal "github.com/enola-labs/enola/internal/linkers/crossrepo/signals/http"
	"github.com/enola-labs/enola/pkg/bootstrap"
	"github.com/enola-labs/enola/pkg/coverage"
)

// customClientExample is the published demonstration under examples/custom-client.
const customClientExample = "../../examples/custom-client"

// linkExample snapshots a fresh copy of the example under one of its configs, exactly as
// `enola --generate <config>` does, and returns the linked store and the config.
func linkExample(t *testing.T, configFile string) (*facts.Store, *config.Config) {
	t.Helper()
	if _, err := os.Stat(customClientExample); err != nil {
		t.Skipf("example not present: %v", err)
	}
	root := copyTree(t, customClientExample, t.TempDir())
	eng, cfg, err := bootstrap.NewEngine(bootstrap.Options{ConfigPath: filepath.Join(root, configFile)})
	if err != nil {
		t.Fatalf("bootstrap.NewEngine(%s): %v", configFile, err)
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
	return eng.Store(), cfg
}

// exampleCallers asks `enola endpoint`'s question the way the CLI does: callers from the
// linker's matching under the config's linking vocabulary.
func exampleCallers(t *testing.T, store *facts.Store, cfg *config.Config, query string) []string {
	t.Helper()
	linkVocab, err := cfg.LinkingVocab()
	if err != nil {
		t.Fatal(err)
	}
	result := store.AnalyzeEndpoint(query, 25, httpsignal.NewCallerFinder(routeindex.New(linkVocab), store.All()))
	files := make([]string, 0, len(result.Callers))
	for _, c := range result.Callers {
		files = append(files, c.File)
	}
	sort.Strings(files)
	return files
}

func crossRepoEdge(store *facts.Store, name string) (facts.Fact, bool) {
	for _, f := range store.ByKind(facts.KindDependency) {
		if f.Name == name && f.PropString("synthetic") == "crossrepo" {
			return f, true
		}
	}
	return facts.Fact{}, false
}

func stringList(f facts.Fact, key string) []string {
	var out []string
	switch v := f.Props[key].(type) {
	case []string:
		out = append(out, v...)
	case []any:
		for _, s := range v {
			if str, ok := s.(string); ok {
				out = append(out, str)
			}
		}
	}
	sort.Strings(out)
	return out
}

// TestPublishedCustomClientExample_StillDemonstratesWhatItClaims runs the example a
// reader is told to run and asserts what its README says each step shows.
//
// A worked example is a claim like any other, and an unexercised one rots quietly: a
// linker change could stop drawing the edge, the script would keep running, and the
// README would go on describing an outcome that no longer happens.
func TestPublishedCustomClientExample_StillDemonstratesWhatItClaims(t *testing.T) {
	// 1. Nothing declared. The SDK's calls are invisible, so there is no sdk -> gateway,
	//    while backend's import of the SDK already links.
	store, _ := linkExample(t, "cluster.yaml")
	if _, ok := crossRepoEdge(store, "sdk -> gateway"); ok {
		t.Error("step 1: sdk -> gateway exists with no client declared; the README's starting point no longer holds")
	}
	if _, ok := crossRepoEdge(store, "backend -> sdk"); !ok {
		t.Error("step 1: backend -> sdk is missing; the path the README walks starts with it")
	}

	// 2. The client declared. The literal call links to gateway; the call through the
	//    :type parameter stays unresolved; the method-result path is skipped by name.
	store, cfg := linkExample(t, "cluster-with-client.yaml")
	edge, ok := crossRepoEdge(store, "sdk -> gateway")
	if !ok {
		t.Fatal("step 2: declaring the client drew no sdk -> gateway edge")
	}
	if got, want := stringList(edge, "endpoints"), []string{"POST /v1/catalog/imports"}; !reflect.DeepEqual(got, want) {
		t.Errorf("step 2: endpoints = %v, want %v", got, want)
	}
	if got := edge.PropString("confidence"); got != "verified" {
		t.Errorf("step 2: confidence = %q, want verified (one provider, the full served path)", got)
	}
	if report := coverage.Build(store, "sdk"); len(report) != 1 || report[0].UnresolvedTotal != 1 {
		t.Errorf("step 2: sdk must show exactly one unresolved call (the :type route), got %+v", report)
	}
	clients := coverage.BuildClients(store)
	if len(clients) != 1 || clients[0].Spec != "sdk-http" || clients[0].CallSites != 3 ||
		clients[0].Routes != 2 || clients[0].Skipped["dynamic_path"] != 1 {
		t.Errorf("step 2: the client account must read 3 call sites, 2 routes, 1 dynamic_path; got %+v", clients)
	}
	connector := []string{"sdk/src/connectors/resource-connector.ts"}
	if got := exampleCallers(t, store, cfg, "POST /v1/catalog/imports"); !reflect.DeepEqual(got, connector) {
		t.Errorf("step 2: enola endpoint must name the connector as the caller of the literal route, got %v", got)
	}
	if got := exampleCallers(t, store, cfg, "GET /v1/resources"); len(got) != 0 {
		t.Errorf("step 2: without parameter matching the :type route has no caller, as it has no edge; got %v", got)
	}

	// 3. Parameter matching on. Both calls link, and the one that needed a parameter is
	//    named in param_segment_endpoints. The edge itself reads verified: an edge takes
	//    the strongest confidence of its endpoints, and the literal POST is a verified
	//    match, which is exactly why the looser endpoint is listed on its own. The whole
	//    path backend -> sdk -> gateway is in the graph.
	store, cfg = linkExample(t, "cluster-with-params.yaml")
	edge, ok = crossRepoEdge(store, "sdk -> gateway")
	if !ok {
		t.Fatal("step 3: sdk -> gateway disappeared with parameter matching on")
	}
	if got, want := stringList(edge, "endpoints"), []string{"GET /v1/resources/catalog/items", "POST /v1/catalog/imports"}; !reflect.DeepEqual(got, want) {
		t.Errorf("step 3: endpoints = %v, want %v", got, want)
	}
	if got, want := stringList(edge, "param_segment_endpoints"), []string{"GET /v1/resources/catalog/items"}; !reflect.DeepEqual(got, want) {
		t.Errorf("step 3: param_segment_endpoints = %v, want %v", got, want)
	}
	if got := edge.PropString("confidence"); got != "verified" {
		t.Errorf("step 3: confidence = %q, want verified (the strongest endpoint's)", got)
	}
	if _, ok := crossRepoEdge(store, "backend -> sdk"); !ok {
		t.Error("step 3: backend -> sdk is missing, so the README's backend -> sdk -> gateway path is not in the graph")
	}
	if report := coverage.Build(store, "sdk"); len(report) != 1 || report[0].UnresolvedTotal != 0 {
		t.Errorf("step 3: sdk must have no unresolved calls, got %+v", report)
	}

	// 4. enola endpoint names the connector as the caller of the :type route, because the
	//    linker now reaches it.
	if got := exampleCallers(t, store, cfg, "GET /v1/resources"); !reflect.DeepEqual(got, connector) {
		t.Errorf("step 4: enola endpoint must name the connector as the caller of the :type route, got %v", got)
	}
}
