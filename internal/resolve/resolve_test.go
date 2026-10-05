package resolve

import (
	"fmt"
	"slices"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func storeWith(ff ...facts.Fact) *facts.Store {
	s := facts.NewStore()
	s.Add(ff...)
	return s
}

// An absolute path must land on the file the facts name, and whether that file
// carries the repo label depends on how the graph was built, not on whether a
// label map exists: a single repository restored from disk has the map and bare files.
func TestNormalizeToRelative_FollowsHowTheFactsNameFiles(t *testing.T) {
	const root = "/work/shop"
	paths := map[string]string{"shop": root}

	bare := storeWith(facts.Fact{Kind: facts.KindSymbol, Name: "pkg/a.Alpha", Repo: "shop", File: "pkg/a/a.go"})
	// An appended repository also holds derived facts that name a bare directory.
	prefixed := storeWith(
		facts.Fact{Kind: facts.KindSymbol, Name: "pkg/a.Alpha", Repo: "shop", File: "shop/pkg/a/a.go"},
		facts.Fact{Kind: facts.KindDependency, Name: "module-edge: pkg/a -> pkg/b", Repo: "shop", File: "pkg/a"},
	)
	// A top-level directory named after the repository does not make its files prefixed.
	sameName := storeWith(
		facts.Fact{Kind: facts.KindSymbol, Name: "shop.Cart", Repo: "shop", File: "shop/cart.py"},
		facts.Fact{Kind: facts.KindSymbol, Name: "setup.main", Repo: "shop", File: "setup.py"},
	)

	cases := []struct {
		name string
		r    Resolver
		in   string
		want string
	}{
		{"restored single repo: map and bare files", Resolver{Store: bare, RepoPaths: paths, RepoPath: root}, root + "/pkg/a/a.go", "pkg/a/a.go"},
		{"restored single repo: its root", Resolver{Store: bare, RepoPaths: paths, RepoPath: root}, root, "."},
		{"generated single repo: no map", Resolver{Store: bare, RepoPath: root}, root + "/pkg/a/a.go", "pkg/a/a.go"},
		{"appended repo: prefixed files", Resolver{Store: prefixed, RepoPaths: paths}, root + "/pkg/a/a.go", "shop/pkg/a/a.go"},
		{"appended repo: its root", Resolver{Store: prefixed, RepoPaths: paths}, root, "shop"},
		{"directory named after the repo", Resolver{Store: sameName, RepoPaths: paths, RepoPath: root}, root + "/shop/cart.py", "shop/cart.py"},
		{"no facts for the label keeps the prefix", Resolver{Store: facts.NewStore(), RepoPaths: paths}, root + "/pkg/a/a.go", "shop/pkg/a/a.go"},
		{"no store keeps the prefix", Resolver{RepoPaths: paths}, root + "/pkg/a/a.go", "shop/pkg/a/a.go"},
		{"outside every root", Resolver{Store: bare, RepoPaths: paths, RepoPath: root}, "/elsewhere/a.go", "/elsewhere/a.go"},
		{"already relative", Resolver{Store: bare, RepoPaths: paths, RepoPath: root}, "pkg/a/a.go", "pkg/a/a.go"},
	}
	for _, c := range cases {
		if got := c.r.NormalizeToRelative(c.in); got != c.want {
			t.Errorf("%s: NormalizeToRelative(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

// The path is only the first step: the name it becomes has to resolve.
func TestNodeName_AbsolutePathInARestoredSingleRepo(t *testing.T) {
	const root = "/work/shop"
	store := storeWith(
		facts.Fact{Kind: facts.KindModule, Name: "pkg/a", Repo: "shop", File: "pkg/a"},
		facts.Fact{Kind: facts.KindSymbol, Name: "pkg/a.Alpha", Repo: "shop", File: "pkg/a/a.go"},
	)
	for name, r := range map[string]Resolver{
		"restored":  {Store: store, RepoPaths: map[string]string{"shop": root}, RepoPath: root},
		"generated": {Store: store, RepoPath: root},
	} {
		got, _, err := r.NodeName(root + "/pkg/a")
		if err != nil || got != "pkg/a" {
			t.Errorf("%s: NodeName(%q) = %q, %v; want pkg/a", name, root+"/pkg/a", got, err)
		}
	}
}

// reversed returns a store holding the same facts in the opposite order, which is
// what separates a graph just extracted from the same graph read back from disk.
func reversed(ff []facts.Fact) *facts.Store {
	back := make([]facts.Fact, len(ff))
	for i, f := range ff {
		back[len(ff)-1-i] = f
	}
	return storeWith(back...)
}

// Two names the term fits equally well are a tie, and a tie must not be settled by
// which fact the store happens to hold first.
func TestNodeName_TieDoesNotDependOnStoreOrder(t *testing.T) {
	ff := []facts.Fact{
		{Kind: facts.KindSymbol, Name: "Vote::ALL_COMMENT_REASONS", Repo: "shop", File: "app/models/vote.rb", Line: 12},
		{Kind: facts.KindSymbol, Name: "Vote::COMMENT_REASONS", Repo: "shop", File: "app/models/vote.rb", Line: 4},
	}
	for _, input := range []string{"COMMENT_REASONS", "repo:shop COMMENT_REASONS", "file:app/models COMMENT_REASONS"} {
		var picks []string
		for _, store := range []*facts.Store{storeWith(ff...), reversed(ff)} {
			got, res, err := Resolver{Store: store}.NodeName(input)
			if err != nil || res == nil || !res.Ambiguous {
				t.Fatalf("NodeName(%q) = %q, %+v, %v; want an ambiguous pick", input, got, res, err)
			}
			picks = append(picks, got)
		}
		// The shorter name is the one the term covers more of.
		if picks[0] != "Vote::COMMENT_REASONS" || picks[1] != picks[0] {
			t.Errorf("NodeName(%q) picked %q and %q in the two store orders; want Vote::COMMENT_REASONS both times", input, picks[0], picks[1])
		}
	}
}

// A scoped term that matches more facts than candidateLimit must still find the
// name it was typed for, wherever that fact sits in the store.
func TestNodeName_ScopedExactNameSurvivesTheCandidateLimit(t *testing.T) {
	ff := make([]facts.Fact, 0, candidateLimit+101)
	for i := range candidateLimit + 100 {
		ff = append(ff, facts.Fact{
			Kind: facts.KindDependency, Repo: "shop", File: "internal/engine/engine.go",
			Name: fmt.Sprintf("internal/engine%03d -> example.com/shop/internal/extractors", i),
		})
	}
	ff = append(ff, facts.Fact{Kind: facts.KindModule, Name: "internal/extractors", Repo: "shop", File: "internal/extractors"})

	for name, store := range map[string]*facts.Store{"module last": storeWith(ff...), "module first": reversed(ff)} {
		got, _, err := Resolver{Store: store}.NodeName("repo:shop internal/extractors")
		if err != nil || got != "internal/extractors" {
			t.Errorf("%s: NodeName = %q, %v; want internal/extractors", name, got, err)
		}
	}
}

func TestGatherCandidates_KeepsTheBestOfEachName(t *testing.T) {
	ff := []facts.Fact{
		{Kind: facts.KindSymbol, Name: "pkg.Run", Repo: "shop", File: "pkg/b.go", Props: map[string]any{"symbol_kind": facts.SymbolFunc}},
		{Kind: facts.KindSymbol, Name: "pkg.Run", Repo: "shop", File: "pkg/a.go", Props: map[string]any{"symbol_kind": facts.SymbolStruct}},
		{Kind: facts.KindSymbol, Name: "pkg.Runner", Repo: "shop", File: "pkg/a.go", Props: map[string]any{"symbol_kind": facts.SymbolStruct}},
	}
	sq := parseScopedQuery("repo:shop Run")
	for name, store := range map[string]*facts.Store{"as written": storeWith(ff...), "reversed": reversed(ff)} {
		got := Resolver{Store: store}.gatherCandidates(store, sq, sq.Term)
		if len(got) != 2 || got[0].Name != "pkg.Run" || got[0].File != "pkg/a.go" || got[1].Name != "pkg.Runner" {
			t.Errorf("%s: got %d candidates %+v; want pkg.Run (the struct in pkg/a.go) then pkg.Runner", name, len(got), got)
		}
	}
}

func TestSuggestNames_NearestFiveWhateverTheStoreOrder(t *testing.T) {
	var ff []facts.Fact
	for _, n := range []string{"pkg.HandlerRegistryBuilder", "pkg.Handler", "pkg.HandlerFunc", "web.Handler", "pkg.HandlerSet", "pkg.HandlerMap", "pkg.Handlers"} {
		ff = append(ff, facts.Fact{Kind: facts.KindSymbol, Name: n, Repo: "shop", File: "pkg/h.go"})
	}
	want := []string{"pkg.Handler", "web.Handler", "pkg.Handlers", "pkg.HandlerMap", "pkg.HandlerSet"}
	sq := parseScopedQuery("Handlr")
	for name, store := range map[string]*facts.Store{"as written": storeWith(ff...), "reversed": reversed(ff)} {
		// "Handlr" matches nothing; its longest run is the probe, so ask with the run that does.
		got := Resolver{Store: store}.suggestNames(store, sq, "Handler")
		if !slices.Equal(got, want) {
			t.Errorf("%s: suggestNames = %v, want %v", name, got, want)
		}
	}
}

// Tiers fold case, so two names that differ only in case tie for either spelling.
// The one spelled as typed is the one that was asked for, whichever it is.
func TestNodeName_CaseAsTypedDecidesBetweenFoldedTies(t *testing.T) {
	ff := []facts.Fact{
		{Kind: facts.KindSymbol, Name: "internal/intent.EdgeRoles", Repo: "shop", File: "internal/intent/roles.go", Props: map[string]any{"symbol_kind": facts.SymbolFunc}},
		{Kind: facts.KindSymbol, Name: "internal/explainers/constraints.rule.edgeRoles", Repo: "shop", File: "internal/explainers/constraints/rule.go", Props: map[string]any{"symbol_kind": facts.SymbolMethod}},
		{Kind: facts.KindSymbol, Name: "internal/intent.edgeRolesFor", Repo: "shop", File: "internal/intent/roles.go", Props: map[string]any{"symbol_kind": facts.SymbolFunc}},
	}
	for input, want := range map[string]string{
		"edgeRoles":           "internal/explainers/constraints.rule.edgeRoles",
		"EdgeRoles":           "internal/intent.EdgeRoles",
		"repo:shop edgeRoles": "internal/explainers/constraints.rule.edgeRoles",
		"intent.EdgeRoles":    "internal/intent.EdgeRoles",
	} {
		for name, store := range map[string]*facts.Store{"as written": storeWith(ff...), "reversed": reversed(ff)} {
			got, res, err := Resolver{Store: store}.NodeName(input)
			if err != nil || got != want {
				t.Errorf("%s: NodeName(%q) = %q, %v; want %q", name, input, got, err, want)
				continue
			}
			// Three candidates is past the threshold: only a decisive pick gets through.
			if res == nil || !res.AutoPicked {
				t.Errorf("%s: NodeName(%q) should be a reported auto-pick, got %+v", name, input, res)
			}
		}
	}
}

// Case tells folded ties apart; it does not choose between two names that both keep it.
func TestNodeName_TwoNamesSpelledAsTypedStayAmbiguous(t *testing.T) {
	store := storeWith(
		facts.Fact{Kind: facts.KindSymbol, Name: "internal/resolve.Resolver", File: "internal/resolve/resolve.go", Props: map[string]any{"symbol_kind": facts.SymbolStruct}},
		facts.Fact{Kind: facts.KindSymbol, Name: "pkg/bootstrap.Engine.Resolver", File: "pkg/bootstrap/bootstrap.go", Props: map[string]any{"symbol_kind": facts.SymbolMethod}},
		facts.Fact{Kind: facts.KindSymbol, Name: "internal/explainers/constraints.resolver", File: "internal/explainers/constraints/ownership.go", Props: map[string]any{"symbol_kind": facts.SymbolStruct}},
	)
	got, res, err := Resolver{Store: store}.NodeName("Resolver")
	if err != nil || got != "" || res == nil || !res.Ambiguous {
		t.Fatalf("NodeName(Resolver) = %q, %+v, %v; want no pick and candidates", got, res, err)
	}
	// The two that keep the case lead; the folded match comes last.
	if n := candidateNames(res.Candidates, ""); len(n) != 3 || n[0] != "internal/resolve.Resolver" || n[1] != "pkg/bootstrap.Engine.Resolver" {
		t.Errorf("candidates = %v; want the type, then the method, then the folded match", n)
	}
}
