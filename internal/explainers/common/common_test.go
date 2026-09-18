package common

import (
	"math"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func TestResolveRelativeImport(t *testing.T) {
	tests := []struct {
		source string
		target string
		want   string
	}{
		{"src/components", "./utils", "src/components/utils"},
		{"src/components", "../hooks", "src/hooks"},
		{"src/components", "../../lib", "lib"},
		{"src/deep/nested", "../../../top", "top"},
		{"src", "./foo", "src/foo"},
		// "." as source: the dot stays in the joined result
		{".", "./foo", "./foo"},
		// Going up past root produces empty path
		{"src", "../../above", "above"},
	}

	for _, tt := range tests {
		t.Run(tt.source+"→"+tt.target, func(t *testing.T) {
			got := ResolveRelativeImport(tt.source, tt.target)
			if got != tt.want {
				t.Errorf("ResolveRelativeImport(%q, %q) = %q, want %q",
					tt.source, tt.target, got, tt.want)
			}
		})
	}
}

func TestFileDir(t *testing.T) {
	tests := []struct {
		file string
		want string
	}{
		{"internal/auth/login.go", "internal/auth"},
		{"main.go", "."},
		{"a/b/c/d.ts", "a/b/c"},
	}
	for _, tt := range tests {
		if got := FileDir(tt.file); got != tt.want {
			t.Errorf("FileDir(%q) = %q, want %q", tt.file, got, tt.want)
		}
	}
}

func TestBuildModuleGraph(t *testing.T) {
	s := facts.NewStore()
	for _, m := range []string{"src/a", "src/b", "src/c"} {
		s.Add(facts.Fact{Kind: facts.KindModule, Name: m})
	}
	deps := map[string][]string{
		"src/a": {"src/b", "fmt", "github.com/foo/bar"},
		"src/b": {"src/c", "react"},
	}
	for src, targets := range deps {
		for _, tgt := range targets {
			s.Add(facts.Fact{
				Kind:      facts.KindDependency,
				File:      src + "/file.go",
				Relations: []facts.Relation{{Kind: facts.RelImports, Target: tgt}},
			})
		}
	}

	graph := BuildModuleGraph(s)

	// src/a should only have edge to src/b (fmt and github.com/foo/bar are external)
	if edges := graph["src/a"]; len(edges) != 1 || edges[0] != "src/b" {
		t.Errorf("src/a edges = %v, want [src/b]", edges)
	}
	// src/b should only have edge to src/c (react is external)
	if edges := graph["src/b"]; len(edges) != 1 || edges[0] != "src/c" {
		t.Errorf("src/b edges = %v, want [src/c]", edges)
	}
	// every declared module is a key, even with no outgoing edges
	if _, ok := graph["src/c"]; !ok {
		t.Error("src/c missing as a graph key")
	}
}

// TestBuildModuleGraph_NestedFileResolvesToModule pins the fix for nested module
// layouts (e.g. a Swift/Xcode target Sources/Foo with files under Sources/Foo/Bar/…):
// a dependency whose File sits BELOW the module root must attribute to the module,
// not the leaf directory — otherwise the source node never matches a module-name
// target and a real cycle (Foo <-> Bar) is silently missed.
func TestBuildModuleGraph_NestedFileResolvesToModule(t *testing.T) {
	s := facts.NewStore()
	for _, m := range []string{"Sources/Foo", "Sources/Bar"} {
		s.Add(facts.Fact{Kind: facts.KindModule, Name: m})
	}
	// Foo imports Bar (from a file nested under Sources/Foo/Sub) and Bar imports Foo
	// (from a file nested under Sources/Bar/Deeper/Nested) — a 2-module cycle.
	s.Add(facts.Fact{
		Kind:      facts.KindDependency,
		File:      "Sources/Foo/Sub/Thing.swift",
		Relations: []facts.Relation{{Kind: facts.RelImports, Target: "Sources/Bar"}},
	})
	s.Add(facts.Fact{
		Kind:      facts.KindDependency,
		File:      "Sources/Bar/Deeper/Nested/Other.swift",
		Relations: []facts.Relation{{Kind: facts.RelImports, Target: "Sources/Foo"}},
	})

	graph := BuildModuleGraph(s)

	if edges := graph["Sources/Foo"]; len(edges) != 1 || edges[0] != "Sources/Bar" {
		t.Errorf("Sources/Foo edges = %v, want [Sources/Bar]", edges)
	}
	if edges := graph["Sources/Bar"]; len(edges) != 1 || edges[0] != "Sources/Foo" {
		t.Errorf("Sources/Bar edges = %v, want [Sources/Foo]", edges)
	}
	// The leaf directories must NOT appear as their own graph nodes.
	if _, ok := graph["Sources/Foo/Sub"]; ok {
		t.Error("leaf dir Sources/Foo/Sub leaked as a graph node")
	}
	// And the two modules form a strongly-connected component (a real cycle).
	if got := sccKey(StronglyConnectedComponents(graph)); got != "Sources/Bar,Sources/Foo" {
		t.Errorf("SCC = %q, want the Foo<->Bar cycle", got)
	}
}

// TestBuildModuleGraph_ClassSuffixedTargetResolves pins the fix for import
// targets that carry a trailing symbol segment (Kotlin/Java `import a.b.C` is
// emitted as target "a/b/C", where the module is "a/b"). A mutual pair of such
// imports must still form a cycle — an exact module-name match would drop both.
func TestBuildModuleGraph_ClassSuffixedTargetResolves(t *testing.T) {
	s := facts.NewStore()
	for _, m := range []string{"app/model", "app/common"} {
		s.Add(facts.Fact{Kind: facts.KindModule, Name: m})
	}
	// model imports a class in common; common imports a class in model.
	s.Add(facts.Fact{
		Kind:      facts.KindDependency,
		File:      "app/model/User.kt",
		Relations: []facts.Relation{{Kind: facts.RelImports, Target: "app/common/Pagination"}},
	})
	s.Add(facts.Fact{
		Kind:      facts.KindDependency,
		File:      "app/common/FlowUtils.kt",
		Relations: []facts.Relation{{Kind: facts.RelImports, Target: "app/model/GidUtil"}},
	})

	graph := BuildModuleGraph(s)

	if edges := graph["app/model"]; len(edges) != 1 || edges[0] != "app/common" {
		t.Errorf("app/model edges = %v, want [app/common]", edges)
	}
	if edges := graph["app/common"]; len(edges) != 1 || edges[0] != "app/model" {
		t.Errorf("app/common edges = %v, want [app/model]", edges)
	}
	if got := sccKey(StronglyConnectedComponents(graph)); got != "app/common,app/model" {
		t.Errorf("SCC = %q, want the model<->common cycle", got)
	}
}

func TestSymbolModule(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{"internal/auth.Login.Verify", "internal/auth"},
		{"pkg/foo.Bar", "pkg/foo"},
		{"standalone", "standalone"},
		{"", ""},
		// The name-only guess is WRONG for .NET, whose directories contain dots.
		// It is kept only as SymbolModuleIn's fallback; see the test below.
		{"MediaBrowser.Controller/Library.ILibraryManager", "MediaBrowser"},
	}
	for _, tt := range tests {
		if got := SymbolModule(tt.name); got != tt.want {
			t.Errorf("SymbolModule(%q) = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestMeanStdDev(t *testing.T) {
	if m, s := MeanStdDev(nil); m != 0 || s != 0 {
		t.Errorf("MeanStdDev(nil) = (%v, %v), want (0, 0)", m, s)
	}
	// values 2,4,4,4,5,5,7,9 → mean 5, population stddev 2
	vals := []float64{2, 4, 4, 4, 5, 5, 7, 9}
	mean, std := MeanStdDev(vals)
	if math.Abs(mean-5) > 1e-9 {
		t.Errorf("mean = %v, want 5", mean)
	}
	if math.Abs(std-2) > 1e-9 {
		t.Errorf("stddev = %v, want 2", std)
	}
	if got := OutlierThreshold(vals, 2); math.Abs(got-9) > 1e-9 {
		t.Errorf("OutlierThreshold(vals, 2) = %v, want 9", got)
	}
}

// TestOutlierThreshold_ZeroStdDev: with identical values the std dev is 0, so the
// threshold equals the mean — every value is <= it, so nothing is a strict
// outlier (why an all-equal fan-in/complexity distribution flags nothing).
func TestOutlierThreshold_ZeroStdDev(t *testing.T) {
	vals := []float64{7, 7, 7, 7}
	mean, std := MeanStdDev(vals)
	if std != 0 {
		t.Errorf("std of identical values = %v, want 0", std)
	}
	if got := OutlierThreshold(vals, 2); got != mean {
		t.Errorf("OutlierThreshold(identical, 2) = %v, want mean %v", got, mean)
	}
}

// sccKey renders a component partition as a stable string for comparison.
func sccKey(sccs [][]string) string {
	parts := make([]string, len(sccs))
	for i, scc := range sccs {
		parts[i] = strings.Join(scc, ",")
	}
	return strings.Join(parts, " | ")
}

func TestStronglyConnectedComponents(t *testing.T) {
	tests := []struct {
		name  string
		graph map[string][]string
		want  string // sccKey of the expected (sorted) partition
	}{
		{"empty", map[string][]string{}, ""},
		{"single isolated", map[string][]string{"a": nil}, "a"},
		{"simple cycle", map[string][]string{"b": {"a"}, "a": {"b"}}, "a,b"},
		{"triangle + tail", map[string][]string{"a": {"b"}, "b": {"c"}, "c": {"a", "d"}, "d": nil}, "a,b,c | d"},
		{"two disjoint cycles", map[string][]string{"a": {"b"}, "b": {"a"}, "c": {"d"}, "d": {"c"}}, "a,b | c,d"},
		{"self loop is singleton", map[string][]string{"a": {"a"}}, "a"},
		{"neighbor-only node included", map[string][]string{"a": {"b"}}, "a | b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sccKey(StronglyConnectedComponents(tt.graph)); got != tt.want {
				t.Errorf("SCC(%v) = %q, want %q", tt.graph, got, tt.want)
			}
		})
	}
}

// TestStronglyConnectedComponents_Deterministic: repeated calls re-range the
// graph map (Go randomizes iteration), but the sorted output must be identical.
func TestStronglyConnectedComponents_Deterministic(t *testing.T) {
	graph := map[string][]string{
		"a": {"b"}, "b": {"c"}, "c": {"a", "d"},
		"d": {"e"}, "e": {"d"},
		"f": {"a"}, "g": nil,
	}
	want := sccKey(StronglyConnectedComponents(graph))
	for i := 0; i < 50; i++ {
		if got := sccKey(StronglyConnectedComponents(graph)); got != want {
			t.Fatalf("non-deterministic SCC output on iteration %d:\nwant %q\ngot  %q", i, want, got)
		}
	}
}

// TestBuildModuleGraph_ExcludesTestRole: modules tagged module_role=test are
// dropped as both nodes and edge endpoints.
func TestBuildModuleGraph_ExcludesTestRole(t *testing.T) {
	s := facts.NewStore()
	s.Add(facts.Fact{Kind: facts.KindModule, Name: "src/app"})
	s.Add(facts.Fact{Kind: facts.KindModule, Name: "src/apptest",
		Props: map[string]any{facts.PropModuleRole: facts.ModuleRoleTest}})
	// app imports the test module and vice versa; the test edges must not appear.
	s.Add(facts.Fact{Kind: facts.KindDependency, File: "src/app/f.go",
		Relations: []facts.Relation{{Kind: facts.RelImports, Target: "src/apptest"}}})
	s.Add(facts.Fact{Kind: facts.KindDependency, File: "src/apptest/f.go",
		Relations: []facts.Relation{{Kind: facts.RelImports, Target: "src/app"}}})

	graph := BuildModuleGraph(s)
	if _, ok := graph["src/apptest"]; ok {
		t.Error("test-role module should not be a graph node")
	}
	if len(graph["src/app"]) != 0 {
		t.Errorf("edge to a test-role module should be dropped, got %v", graph["src/app"])
	}
}

// TestBuildModuleGraph_SingleSegmentInternalModule: a top-level internal module
// with a single-segment name (e.g. "config") must be included as an import edge.
// Before the fix, IsExternalImport("config") returned true and the edge was
// dropped before the authoritative moduleNames gate.
func TestBuildModuleGraph_SingleSegmentInternalModule(t *testing.T) {
	s := facts.NewStore()
	s.Add(facts.Fact{Kind: facts.KindModule, Name: "config"})
	s.Add(facts.Fact{Kind: facts.KindModule, Name: "handlers"})
	s.Add(facts.Fact{
		Kind:      facts.KindDependency,
		File:      "handlers/h.go",
		Relations: []facts.Relation{{Kind: facts.RelImports, Target: "config"}},
	})

	graph := BuildModuleGraph(s)

	if edges := graph["handlers"]; len(edges) != 1 || edges[0] != "config" {
		t.Errorf("handlers edges = %v, want [config]", edges)
	}
}

// TestBuildModuleGraph_ExternalStillDropped: single-segment names that are NOT
// declared modules (Go stdlib "fmt", npm "react") must still be dropped — the
// moduleNames gate remains authoritative after removing the IsExternalImport
// pre-filter.
func TestBuildModuleGraph_ExternalStillDropped(t *testing.T) {
	s := facts.NewStore()
	s.Add(facts.Fact{Kind: facts.KindModule, Name: "handlers"})
	s.Add(facts.Fact{
		Kind: facts.KindDependency,
		File: "handlers/h.go",
		Relations: []facts.Relation{
			{Kind: facts.RelImports, Target: "fmt"},
			{Kind: facts.RelImports, Target: "react"},
		},
	})

	graph := BuildModuleGraph(s)

	if edges := graph["handlers"]; len(edges) != 0 {
		t.Errorf("handlers edges = %v, want none (fmt/react are not modules)", edges)
	}
}

// TestBuildModuleGraph_UntaggedTestModuleExcluded pins the real defect behind
// new/55. The test-module exclusion above keys on the module_role prop — which only
// java, kotlin, ruby and swift emit. Go, Python, TypeScript, PHP and C/C++ emit no
// module_role at all, so their test trees walked straight into the cycles and
// dependency-depth graphs: on python/superset, 10 of the 10 dependency-depth findings
// were test modules, and 2 of the 10 cycles ran through them.
//
// Where the prop is absent, fall back to the path.
func TestBuildModuleGraph_UntaggedTestModuleExcluded(t *testing.T) {
	s := facts.NewStore()
	// Python/TS/Go: no module_role prop at all.
	s.Add(facts.Fact{Kind: facts.KindModule, Name: "superset/dao"})
	s.Add(facts.Fact{Kind: facts.KindModule, Name: "tests/unit_tests/dao"})
	s.Add(facts.Fact{Kind: facts.KindDependency, File: "tests/unit_tests/dao/f.py",
		Relations: []facts.Relation{{Kind: facts.RelImports, Target: "superset/dao"}}})
	s.Add(facts.Fact{Kind: facts.KindDependency, File: "superset/dao/f.py",
		Relations: []facts.Relation{{Kind: facts.RelImports, Target: "tests/unit_tests/dao"}}})

	graph := BuildModuleGraph(s)
	if _, ok := graph["tests/unit_tests/dao"]; ok {
		t.Error("an untagged test module must not be a graph node")
	}
	if len(graph["superset/dao"]) != 0 {
		t.Errorf("edge to an untagged test module should be dropped, got %v", graph["superset/dao"])
	}
}

// TestBuildModuleGraph_AuthoritativeRoleWins is the anti-regression guard, and the
// reason new/55's own prescribed fix was refuted. A large Android app has a
// production package literally named `app/src/main/java/.../ui/base/testing`. Gradle
// says src/main, so the extractor tags it module_role=production — an AUTHORITATIVE
// signal. The path heuristic must never override it, or a real production package is
// silently dropped from the architecture graph for having "testing" in its name.
//
// This is why the fallback is a fallback and not a union: the report proposed
// widening ModuleRoleForPath itself, which would have suppressed this package.
func TestBuildModuleGraph_AuthoritativeRoleWins(t *testing.T) {
	s := facts.NewStore()
	s.Add(facts.Fact{Kind: facts.KindModule, Name: "app/src/main/java/de/example/app/ui/base/testing",
		Props: map[string]any{facts.PropModuleRole: facts.ModuleRoleProduction}})
	s.Add(facts.Fact{Kind: facts.KindModule, Name: "app/src/main/java/de/example/app/ui",
		Props: map[string]any{facts.PropModuleRole: facts.ModuleRoleProduction}})
	s.Add(facts.Fact{Kind: facts.KindDependency, File: "app/src/main/java/de/example/app/ui/f.kt",
		Relations: []facts.Relation{{Kind: facts.RelImports, Target: "app/src/main/java/de/example/app/ui/base/testing"}}})

	graph := BuildModuleGraph(s)
	if _, ok := graph["app/src/main/java/de/example/app/ui/base/testing"]; !ok {
		t.Error("a module the extractor authoritatively tagged production must stay in the graph, " +
			"even though its path contains a test segment")
	}
	if len(graph["app/src/main/java/de/example/app/ui"]) != 1 {
		t.Error("the edge into it must survive")
	}
}

// TestBuildModuleGraph_RepoPrefixedFilesResolve covers append mode, where a fact's
// File is repo-prefixed ("server/index.js") while module facts keep their bare name
// ("."). Walking up from the prefixed directory reaches no module, so nearestModule
// falls back to returning the raw directory — and every cross-repo edge used to hang
// off a phantom node no module or target ever referenced. Cycles and dependency-depth
// both read this graph, so the coupling that spans repositories went uncounted.
func TestBuildModuleGraph_RepoPrefixedFilesResolve(t *testing.T) {
	s := facts.NewStore()
	// Two repos, each with a bare module name, as the engine emits them in append mode.
	s.Add(facts.Fact{Kind: facts.KindModule, Name: "handlers", Repo: "server", File: "server/handlers"})
	s.Add(facts.Fact{Kind: facts.KindModule, Name: "store", Repo: "server", File: "server/store"})
	s.Add(facts.Fact{
		Kind: facts.KindDependency, Repo: "server", File: "server/handlers/h.go",
		Relations: []facts.Relation{{Kind: facts.RelImports, Target: "store"}},
	})
	s.Add(facts.Fact{
		Kind: facts.KindDependency, Repo: "server", File: "server/store/s.go",
		Relations: []facts.Relation{{Kind: facts.RelImports, Target: "handlers"}},
	})

	graph := BuildModuleGraph(s)

	if edges := graph["handlers"]; len(edges) != 1 || edges[0] != "store" {
		t.Errorf("handlers edges = %v, want [store]", edges)
	}
	if _, ok := graph["server/handlers"]; ok {
		t.Error("repo-prefixed dir leaked as a phantom graph node")
	}
	// And the cycle is actually detectable — the property enola check depends on.
	if got := sccKey(StronglyConnectedComponents(graph)); got != "handlers,store" {
		t.Errorf("SCC = %q, want the handlers<->store cycle", got)
	}
}

// TestModuleDir pins the two spaces apart: a repo-prefixed file resolves into
// module-name space, while a single-repo file is unchanged.
func TestModuleDir(t *testing.T) {
	tests := []struct {
		file, repo, want string
	}{
		{"server/index.js", "server", "."},
		{"consumer/src/client.ts", "consumer", "src"},
		{"src/client.ts", "", "src"},
		{"index.js", "", "."},
		// A repo label that is not actually a prefix must not be stripped blindly.
		{"src/client.ts", "other", "src"},
	}
	for _, tt := range tests {
		got := ModuleDir(facts.Fact{File: tt.file, Repo: tt.repo})
		if got != tt.want {
			t.Errorf("ModuleDir(%q, repo=%q) = %q, want %q", tt.file, tt.repo, got, tt.want)
		}
	}
}

// TestSymbolModuleIn pins the resolution the name alone cannot do. A fact is named
// "<module>.<declaration>" and .NET puts dots in both halves, so
// "Jellyfin.Api.BaseJellyfinApiController" reads identically to module "Jellyfin"
// plus type "Api.BaseJellyfinApiController". Guessing attributed 9,106 jellyfin
// symbols to a module called "MediaBrowser" that no module fact declares.
func TestSymbolModuleIn(t *testing.T) {
	declared := map[string]bool{
		"internal/auth":                   true,
		"MediaBrowser.Controller/Library": true,
		"Jellyfin.Api":                    true,
		"Jellyfin.Api/Controllers":        true,
		"Sources/Foo":                     true,
	}
	tests := []struct {
		name string
		want string
	}{
		// Unchanged for every language that already worked.
		{"internal/auth.Login.Verify", "internal/auth"},
		// A dotted directory before the last slash.
		{"MediaBrowser.Controller/Library.ILibraryManager", "MediaBrowser.Controller/Library"},
		// A dotted LEAF directory — the case no name-based split can settle.
		{"Jellyfin.Api.BaseJellyfinApiController", "Jellyfin.Api"},
		// Longest prefix wins: the nested module, not its parent.
		{"Jellyfin.Api/Controllers.AudioController.GetAudioStream", "Jellyfin.Api/Controllers"},
		// Swift names by TARGET rather than file directory; matching the declared
		// set is what keeps that correct too.
		{"Sources/Foo.Thing.run", "Sources/Foo"},
		// No module matches: fall back to the guess rather than returning nothing.
		{"unknown/place.Thing", "unknown/place"},
		{"standalone", "standalone"},
	}
	for _, tt := range tests {
		if got := SymbolModuleIn(tt.name, declared); got != tt.want {
			t.Errorf("SymbolModuleIn(%q) = %q, want %q", tt.name, got, tt.want)
		}
	}
}
