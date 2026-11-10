package common

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// The two snapshot shapes a dependency fact can arrive in. Neither the raw file
// directory nor the repo-stripped one is correct for both, which is the whole reason
// ModuleDirCandidates exists.
//
// The single-repo case here is the one that regressed in the field: a Python package
// carrying its repository's own name (cognee/cognee, superset/superset). Stripping the
// repo label removed a REAL path segment, so the edge was attributed to a module name
// that existed nowhere and every cycle through that tree went undetected.
func TestResolveModuleDir_BothSnapshotShapes(t *testing.T) {
	tests := []struct {
		name string
		fact facts.Fact
		prod []string
		want string
	}{
		{
			name: "single repo, package shares the repo name",
			fact: facts.Fact{Repo: "cognee", File: "cognee/benchcyca/__init__.py"},
			prod: []string{"cognee", "cognee/benchcyca", "cognee/benchcycb"},
			want: "cognee/benchcyca",
		},
		{
			name: "append mode, file is repo-prefixed",
			fact: facts.Fact{Repo: "consumer", File: "consumer/src/client.ts"},
			prod: []string{"src", "routes"},
			want: "src",
		},
		{
			name: "single repo, package differs from the repo name",
			fact: facts.Fact{Repo: "proj", File: "pkg/a/__init__.py"},
			prod: []string{"pkg", "pkg/a", "pkg/b"},
			want: "pkg/a",
		},
		{
			name: "nested layout resolves up to the module root",
			fact: facts.Fact{Repo: "app", File: "Sources/Foo/Bar/X.swift"},
			prod: []string{"Sources/Foo"},
			want: "Sources/Foo",
		},
		{
			name: "no enclosing module falls back to the repo-stripped dir",
			fact: facts.Fact{Repo: "app", File: "app/unknown/x.go"},
			prod: []string{"other"},
			want: "unknown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prod := make(map[string]bool, len(tt.prod))
			for _, m := range tt.prod {
				prod[m] = true
			}
			if got := resolveModuleDir(tt.fact, prod, map[string]bool{}, nil); got != tt.want {
				t.Errorf("resolveModuleDir() = %q, want %q", got, tt.want)
			}
		})
	}
}

// An exact match on ANY candidate must beat a walk-up on any other. Without that
// ordering the repo label itself ("consumer") can absorb an edge whose real module is
// the stripped path, so the two snapshot shapes cross-match.
func TestResolveModuleDir_ExactBeatsWalkUp(t *testing.T) {
	f := facts.Fact{Repo: "consumer", File: "consumer/src/client.ts"}
	prod := map[string]bool{"consumer": true, "src": true}

	if got := resolveModuleDir(f, prod, map[string]bool{}, nil); got != "src" {
		t.Errorf("resolveModuleDir() = %q, want %q — a walk-up to the repo label beat an exact match", got, "src")
	}
}

// A module cycle must close when the package carries the repo's name. This is the
// end-to-end shape of the field regression, expressed on the graph the cycles
// explainer actually reads.
func TestBuildModuleGraph_CycleClosesWhenPackageSharesRepoName(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		facts.Fact{Kind: facts.KindModule, Name: "cognee", Repo: "cognee", File: "cognee"},
		facts.Fact{Kind: facts.KindModule, Name: "cognee/a", Repo: "cognee", File: "cognee/a"},
		facts.Fact{Kind: facts.KindModule, Name: "cognee/b", Repo: "cognee", File: "cognee/b"},
		facts.Fact{
			Kind: facts.KindDependency, Name: "cognee/a/__init__ -> cognee.b",
			Repo: "cognee", File: "cognee/a/__init__.py",
			Relations: []facts.Relation{{Kind: facts.RelImports, Target: "cognee/b"}},
		},
		facts.Fact{
			Kind: facts.KindDependency, Name: "cognee/b/__init__ -> cognee.a",
			Repo: "cognee", File: "cognee/b/__init__.py",
			Relations: []facts.Relation{{Kind: facts.RelImports, Target: "cognee/a"}},
		},
	)

	graph := BuildModuleGraph(store)

	for _, edge := range [][2]string{{"cognee/a", "cognee/b"}, {"cognee/b", "cognee/a"}} {
		found := false
		for _, to := range graph[edge[0]] {
			if to == edge[1] {
				found = true
			}
		}
		if !found {
			t.Errorf("missing edge %s -> %s; graph = %v", edge[0], edge[1], graph)
		}
	}
	// The phantom node the bug produced: the repo-stripped source dir, which names no
	// module and therefore anchors edges nothing else can reach.
	if len(graph["a"]) > 0 {
		t.Errorf("edge attributed to phantom node %q (repo prefix wrongly stripped): %v", "a", graph["a"])
	}
}
