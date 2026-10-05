package resolve

import (
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
