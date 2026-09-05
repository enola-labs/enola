package dashboard

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/enola-labs/enola/pkg/facts"
)

func TestBuildModuleGraphRanksBoundsAndLinks(t *testing.T) {
	st := facts.NewStore()
	st.Add(
		facts.Fact{Kind: facts.KindModule, Name: "core", File: "src/core", Relations: []facts.Relation{{Kind: facts.RelImports, Target: "api"}, {Kind: facts.RelImports, Target: "storage"}}},
		facts.Fact{Kind: facts.KindModule, Name: "api", File: "src/api", Relations: []facts.Relation{{Kind: facts.RelImports, Target: "storage"}}},
		facts.Fact{Kind: facts.KindModule, Name: "storage", File: "src/storage"},
		facts.Fact{Kind: facts.KindModule, Name: "core_test", File: "tests/core", Props: map[string]any{facts.PropModuleRole: facts.ModuleRoleTest}, Relations: []facts.Relation{{Kind: facts.RelImports, Target: "core"}}},
	)

	view := buildModuleGraph(st)
	if view == nil || len(view.Nodes) != 3 || len(view.Edges) != 3 {
		t.Fatalf("view = %+v, want 3 production nodes and 3 directed edges", view)
	}
	if view.Nodes[0].Name != "core" || view.Nodes[0].FanIn != 0 || view.Nodes[0].FanOut != 2 {
		t.Errorf("top node = %+v, want core with fan-in 0 / fan-out 2", view.Nodes[0])
	}
}

func TestModuleGraphPageExplainsScopeAndProvidesModuleTable(t *testing.T) {
	st := facts.NewStore()
	st.Add(
		facts.Fact{Kind: facts.KindModule, Name: "core", File: "src/core", Relations: []facts.Relation{{Kind: facts.RelImports, Target: "api"}}},
		facts.Fact{Kind: facts.KindModule, Name: "api", File: "src/api"},
	)
	s := newTestServer(8080, fakeArtifacts{store: st})
	rec := httptest.NewRecorder()
	s.handleIndex(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rec.Body.String()

	for _, want := range []string{
		"Showing 2 of 2 connected modules", "1 visible module dependencies",
		"Most-connected modules in this view", "Consumer modules", "Dependency modules",
		`onclick="focusModule('`, "Search every module", "Layered by dependency direction",
		"selectDependency(this)", "module-level evidence", "window.location.assign('/?module='",
		"Architecture inspector", "Nothing selected", "Fit view", "toggleGraphFit(this)",
		"Map shows module imports", "symbol-level finding", "enola-selected-finding",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("module graph page missing %q", want)
		}
	}
}

func TestBuildModuleGraphFocusesOnImmediateNeighborhood(t *testing.T) {
	st := facts.NewStore()
	st.Add(
		facts.Fact{Kind: facts.KindModule, Name: "core", Relations: []facts.Relation{{Kind: facts.RelImports, Target: "api"}}},
		facts.Fact{Kind: facts.KindModule, Name: "api", Relations: []facts.Relation{{Kind: facts.RelImports, Target: "storage"}}},
		facts.Fact{Kind: facts.KindModule, Name: "storage"},
		facts.Fact{Kind: facts.KindModule, Name: "unrelated", Relations: []facts.Relation{{Kind: facts.RelImports, Target: "other"}}},
		facts.Fact{Kind: facts.KindModule, Name: "other"},
	)
	view := buildModuleGraphFocused(st, "api")
	if view == nil || !view.Focused || view.FocusName != "api" || len(view.Nodes) != 3 {
		t.Fatalf("focused view = %+v, want api plus core/storage", view)
	}
	if len(view.Edges) != 2 {
		t.Fatalf("focused edges = %+v, want only edges incident to api", view.Edges)
	}
	roles := map[string]string{}
	positions := map[string]int{}
	for _, node := range view.Nodes {
		if node.Name == "unrelated" || node.Name == "other" {
			t.Fatalf("focused view contains unrelated node %+v", node)
		}
		roles[node.Name], positions[node.Name] = node.Role, node.X
	}
	if roles["core"] != "consumer" || roles["api"] != "selected" || roles["storage"] != "dependency" {
		t.Fatalf("focused roles = %+v", roles)
	}
	if positions["core"] >= positions["api"] || positions["api"] >= positions["storage"] {
		t.Fatalf("focused x positions = %+v, want consumer < selected < dependency", positions)
	}
	if view.Edges[0].Kind != facts.RelImports || view.Edges[0].SourceName == "" || view.Edges[0].TargetName == "" {
		t.Fatalf("edge lacks inspectable evidence: %+v", view.Edges[0])
	}
	if len(view.AllModules) != 5 {
		t.Fatalf("search index = %d modules, want all 5", len(view.AllModules))
	}
}

func TestBuildModuleGraphFocusedIncludesNeighborhoodEdges(t *testing.T) {
	st := facts.NewStore()
	st.Add(
		facts.Fact{Kind: facts.KindModule, Name: "core", Relations: []facts.Relation{{Kind: facts.RelImports, Target: "api"}, {Kind: facts.RelImports, Target: "storage"}}},
		facts.Fact{Kind: facts.KindModule, Name: "api", Relations: []facts.Relation{{Kind: facts.RelImports, Target: "storage"}}},
		facts.Fact{Kind: facts.KindModule, Name: "storage"},
	)
	view := buildModuleGraphFocused(st, "api")
	if view == nil || len(view.Edges) != 3 {
		t.Fatalf("focused edges = %+v, want core->api, api->storage, and core->storage (both endpoints in the neighborhood)", view.Edges)
	}
	found := false
	for _, e := range view.Edges {
		if e.SourceName == "core" && e.TargetName == "storage" {
			found = true
		}
	}
	if !found {
		t.Fatalf("focused edges = %+v, want core->storage even though neither endpoint is the focus module", view.Edges)
	}
}

func TestBuildModuleGraphLayersConsumersBeforeDependencies(t *testing.T) {
	st := facts.NewStore()
	st.Add(
		facts.Fact{Kind: facts.KindModule, Name: "web", Relations: []facts.Relation{{Kind: facts.RelImports, Target: "service"}}},
		facts.Fact{Kind: facts.KindModule, Name: "service", Relations: []facts.Relation{{Kind: facts.RelImports, Target: "storage"}}},
		facts.Fact{Kind: facts.KindModule, Name: "storage"},
	)
	view := buildModuleGraph(st)
	positions := map[string]int{}
	for _, node := range view.Nodes {
		positions[node.Name] = node.X
	}
	if positions["web"] >= positions["service"] || positions["service"] >= positions["storage"] {
		t.Fatalf("layer positions = %+v, want web < service < storage", positions)
	}
}

func TestBuildModuleGraphCondensesCyclesIntoClusterNode(t *testing.T) {
	st := facts.NewStore()
	st.Add(
		facts.Fact{Kind: facts.KindModule, Name: "a", Relations: []facts.Relation{{Kind: facts.RelImports, Target: "b"}, {Kind: facts.RelImports, Target: "db"}}},
		facts.Fact{Kind: facts.KindModule, Name: "b", Relations: []facts.Relation{{Kind: facts.RelImports, Target: "c"}, {Kind: facts.RelImports, Target: "db"}}},
		facts.Fact{Kind: facts.KindModule, Name: "c", Relations: []facts.Relation{{Kind: facts.RelImports, Target: "a"}}},
		facts.Fact{Kind: facts.KindModule, Name: "app", Relations: []facts.Relation{{Kind: facts.RelImports, Target: "a"}}},
		facts.Fact{Kind: facts.KindModule, Name: "db"},
	)

	view := buildModuleGraph(st)
	if view == nil {
		t.Fatal("view is nil")
	}

	var cluster *moduleNode
	for i := range view.Nodes {
		if view.Nodes[i].Role == "cluster" {
			if cluster != nil {
				t.Fatalf("found more than one cluster node: %+v and %+v", *cluster, view.Nodes[i])
			}
			cluster = &view.Nodes[i]
		}
	}
	if cluster == nil {
		t.Fatalf("view = %+v, want one cluster node for the a/b/c cycle", view)
	}
	members := append([]string(nil), cluster.Members...)
	sort.Strings(members)
	if fmt.Sprint(members) != fmt.Sprint([]string{"a", "b", "c"}) {
		t.Fatalf("cluster members = %v, want [a b c]", members)
	}
	if !strings.Contains(cluster.Display, "+2") {
		t.Fatalf("cluster display = %q, want it to mention +2 (b and c folded in)", cluster.Display)
	}

	// The condensation of any cyclic graph is acyclic, so every drawn edge
	// must run strictly left to right — this is the concrete check that the
	// original tangle (same-column back-edges) can no longer occur.
	for _, e := range view.Edges {
		if view.Nodes[e.Source].X >= view.Nodes[e.Target].X {
			t.Errorf("edge %+v is not forward: source X=%d, target X=%d", e, view.Nodes[e.Source].X, view.Nodes[e.Target].X)
		}
	}

	seen := map[[2]int]int{}
	for _, e := range view.Edges {
		seen[[2]int{e.Source, e.Target}]++
	}
	for pair, count := range seen {
		if count > 1 {
			t.Errorf("duplicate edge %v drawn %d times", pair, count)
		}
	}
	if len(view.Edges) != 2 {
		t.Fatalf("edges = %+v, want exactly app->cluster and cluster->db, deduped from the two members (a and b) that both point at db", view.Edges)
	}
}

func TestBuildModuleGraphCapsLargeRepositories(t *testing.T) {
	st := facts.NewStore()
	for i := 0; i < moduleGraphLimit+10; i++ {
		name := fmt.Sprintf("m%02d", i)
		target := fmt.Sprintf("m%02d", (i+1)%(moduleGraphLimit+10))
		st.Add(facts.Fact{Kind: facts.KindModule, Name: name, Relations: []facts.Relation{{Kind: facts.RelImports, Target: target}}})
	}
	view := buildModuleGraph(st)
	if view == nil || len(view.Nodes) != moduleGraphOverviewLimit || !view.Limited || view.Total != moduleGraphLimit+10 {
		t.Fatalf("view = %+v, want %d of %d modules", view, moduleGraphOverviewLimit, moduleGraphLimit+10)
	}
}

func TestBuildModuleGraphCapsOverviewEdges(t *testing.T) {
	st := facts.NewStore()
	for i := 0; i < moduleGraphOverviewLimit; i++ {
		relations := make([]facts.Relation, 0, moduleGraphOverviewLimit-1)
		for j := 0; j < moduleGraphOverviewLimit; j++ {
			if i < j {
				relations = append(relations, facts.Relation{Kind: facts.RelImports, Target: fmt.Sprintf("m%02d", j)})
			}
		}
		st.Add(facts.Fact{Kind: facts.KindModule, Name: fmt.Sprintf("m%02d", i), Relations: relations})
	}
	view := buildModuleGraph(st)
	if len(view.Edges) != moduleGraphOverviewEdges || view.OmittedEdges == 0 {
		t.Fatalf("overview edges = %d, omitted = %d", len(view.Edges), view.OmittedEdges)
	}
}
