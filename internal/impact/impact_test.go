package impact

import (
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/resolve"
)

func TestCanonicalTarget_FileRefUsesExtensionlessDependencyNode(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		facts.Fact{Kind: facts.KindFileRef, Name: "ui/src/api/api.ts", File: "ui/src/api/api.ts"},
		facts.Fact{Kind: facts.KindDependency, Name: "ui/src/page -> ui/src/api/api", Relations: []facts.Relation{
			{Kind: facts.RelImports, Target: "ui/src/api/api"},
		}},
	)
	got, res := canonicalTarget(store, "ui/src/api/api.ts")
	if got != "ui/src/api/api" {
		t.Fatalf("canonical target = %q", got)
	}
	if res == nil || res.Query != "ui/src/api/api.ts" || res.Matched != got {
		t.Fatalf("normalization was not disclosed: %+v", res)
	}
}

func TestCanonicalTarget_GenuineFileRefStaysPut(t *testing.T) {
	store := facts.NewStore()
	store.Add(facts.Fact{Kind: facts.KindFileRef, Name: "ui/src/setup.ts", File: "ui/src/setup.ts"})
	got, res := canonicalTarget(store, "ui/src/setup.ts")
	if got != "ui/src/setup.ts" || res != nil {
		t.Fatalf("got %q, %+v", got, res)
	}
}

func fileStore() *facts.Store {
	s := facts.NewStore()
	s.Add(
		facts.Fact{Kind: facts.KindSymbol, Name: "pkg/a.Alpha", Repo: "shop", File: "pkg/a/a.go", Line: 3},
		// Beta calls Alpha from the same file: part of the file, not a dependent of it.
		facts.Fact{Kind: facts.KindSymbol, Name: "pkg/a.Beta", Repo: "shop", File: "pkg/a/a.go", Line: 9,
			Relations: []facts.Relation{{Kind: facts.RelCalls, Target: "pkg/a.Alpha"}}},
		facts.Fact{Kind: facts.KindSymbol, Name: "pkg/b.UsesAlpha", Repo: "shop", File: "pkg/b/b.go", Line: 4,
			Relations: []facts.Relation{{Kind: facts.RelCalls, Target: "pkg/a.Alpha"}}},
		facts.Fact{Kind: facts.KindSymbol, Name: "pkg/c.UsesBeta", Repo: "shop", File: "pkg/c/c.go", Line: 4,
			Relations: []facts.Relation{{Kind: facts.RelCalls, Target: "pkg/a.Beta"}}},
		facts.Fact{Kind: facts.KindDependency, Name: "pkg/d -> fmt", Repo: "shop", File: "pkg/d/d.go",
			Relations: []facts.Relation{{Kind: facts.RelImports, Target: "fmt"}}},
	)
	s.BuildGraph()
	return s
}

// A source file is not a node. Asked about one, the answer is the impact of the
// symbols it declares, taken together, and it says so.
func TestAnalyze_FilePathIsTheSymbolsItDeclares(t *testing.T) {
	const root = "/work/shop"
	r := resolve.Resolver{Store: fileStore(), RepoPath: root}
	for _, target := range []string{"pkg/a/a.go", root + "/pkg/a/a.go"} {
		report, err := Analyze(r, Request{Target: target})
		if err != nil {
			t.Fatalf("Analyze(%q): %v", target, err)
		}
		if !report.Resolved() || report.Target != "pkg/a/a.go" || report.Resolution.Matched != "pkg/a/a.go" {
			t.Errorf("Analyze(%q): target %q, resolution %+v; want the file", target, report.Target, report.Resolution)
		}
		if got := strings.Join(report.Seeds, ","); got != "pkg/a.Alpha,pkg/a.Beta" {
			t.Errorf("Analyze(%q): seeds = %s", target, got)
		}
		var deps []string
		for _, n := range report.ByDepth[1] {
			deps = append(deps, n.Name)
		}
		if got := strings.Join(deps, ","); got != "pkg/b.UsesAlpha,pkg/c.UsesBeta" || report.TotalDependents != 2 {
			t.Errorf("Analyze(%q): dependents %s, total %d; want the two callers outside the file", target, got, report.TotalDependents)
		}
	}
}

// A file with no symbol has nothing to walk back from, and a path to no file is
// still an unknown name: neither becomes an empty answer.
func TestAnalyze_FilePathWithoutSymbolsStaysAnError(t *testing.T) {
	r := resolve.Resolver{Store: fileStore()}
	for _, target := range []string{"pkg/d/d.go", "pkg/a/missing.go"} {
		if report, err := Analyze(r, Request{Target: target}); err == nil {
			t.Errorf("Analyze(%q) = %+v; want an error", target, report)
		}
	}
}
