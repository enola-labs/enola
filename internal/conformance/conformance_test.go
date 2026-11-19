package conformance

import (
	"testing"

	"github.com/enola-labs/enola/internal/diff"
	"github.com/enola-labs/enola/internal/facts"
)

// A small graph: api and web both import core. Changing core has a predicted radius of
// {core, api, web}; changing it and also editing an unrelated package does not.
func baseStore() *facts.Store {
	s := facts.NewStore()
	s.Add(
		facts.Fact{Kind: facts.KindModule, Name: "core", File: "core"},
		facts.Fact{Kind: facts.KindModule, Name: "api", File: "api"},
		facts.Fact{Kind: facts.KindModule, Name: "web", File: "web"},
		facts.Fact{Kind: facts.KindModule, Name: "unrelated", File: "unrelated"},
		facts.Fact{
			Kind: facts.KindDependency, Name: "api -> core", File: "api/a.go",
			Relations: []facts.Relation{{Kind: facts.RelImports, Target: "core"}},
		},
		facts.Fact{
			Kind: facts.KindDependency, Name: "web -> core", File: "web/w.go",
			Relations: []facts.Relation{{Kind: facts.RelImports, Target: "core"}},
		},
	)
	s.BuildGraph()
	return s
}

func moduleFact(name string) facts.Fact {
	return facts.Fact{Kind: facts.KindModule, Name: name, File: name}
}

// The finding this package exists to produce: a change that reached a package the caller
// never declared. Everything else can be green and this still matters.
func TestSpilloverIsReportedInDeclaredMode(t *testing.T) {
	base := baseStore()
	d := &diff.SnapshotDiff{FactsChanged: []diff.FactChange{
		{Before: moduleFact("core"), After: moduleFact("core")},
		{Before: moduleFact("unrelated"), After: moduleFact("unrelated")},
	}}

	got := Compute(base, base, d, Options{Target: "core", ExpectedPackages: []string{"core"}})

	if !got.Declared {
		t.Error("Declared = false, want true — a target was supplied")
	}
	if len(got.Spillover) != 1 || got.Spillover[0] != "unrelated" {
		t.Errorf("Spillover = %v, want [unrelated]", got.Spillover)
	}
	if got.MatchRatio >= 1.0 {
		t.Errorf("MatchRatio = %v, want < 1 when a package spilled over", got.MatchRatio)
	}
}

// A change confined to what was declared reports no spillover and a perfect ratio.
func TestNoSpilloverWhenTheChangeStaysInScope(t *testing.T) {
	base := baseStore()
	d := &diff.SnapshotDiff{FactsChanged: []diff.FactChange{
		{Before: moduleFact("core"), After: moduleFact("core")},
	}}

	got := Compute(base, base, d, Options{Target: "core"})

	if len(got.Spillover) != 0 {
		t.Errorf("Spillover = %v, want none", got.Spillover)
	}
	if got.MatchRatio != 1.0 {
		t.Errorf("MatchRatio = %v, want 1.0", got.MatchRatio)
	}
}

// In AUTO mode the packages the author edited are the best available statement of intent,
// so editing two packages without declaring anything is not spillover. Without this, every
// undeclared multi-package change would report a false conformance failure.
func TestAutoModeTreatsEditSitesAsIntent(t *testing.T) {
	base := baseStore()
	d := &diff.SnapshotDiff{FactsChanged: []diff.FactChange{
		{Before: moduleFact("core"), After: moduleFact("core")},
		{Before: moduleFact("unrelated"), After: moduleFact("unrelated")},
	}}

	got := Compute(base, base, d, Options{})

	if got.Declared {
		t.Error("Declared = true with no target or expected packages")
	}
	if len(got.Spillover) != 0 {
		t.Errorf("Spillover = %v, want none in auto mode", got.Spillover)
	}
}

// A package can be reached without any of its own facts moving — it gained an outbound
// dependency. Leaving that out would under-report the coupling case, which is the one
// most worth catching.
func TestNewCouplingCountsAsReached(t *testing.T) {
	base := baseStore()
	cur := facts.NewStore()
	cur.Add(
		moduleFact("core"),
		facts.Fact{
			Kind: facts.KindDependency, Name: "unrelated -> core", File: "unrelated/u.go",
			Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: "unrelated"}},
		},
	)
	d := &diff.SnapshotDiff{EdgesAdded: []diff.Edge{{Source: "unrelated -> core", Target: "core"}}}

	got := Compute(base, cur, d, Options{Target: "core", ExpectedPackages: []string{"core"}})

	found := false
	for _, p := range got.ActualPackages {
		if p == "unrelated" {
			found = true
		}
	}
	if !found {
		t.Errorf("ActualPackages = %v, want it to include the package that gained the edge", got.ActualPackages)
	}
}

// An exactly-named target must not be expanded by substring: a caller who named a real
// symbol meant that symbol, and expanding would drag in every namesake.
func TestExactTargetIsNotExpanded(t *testing.T) {
	base := baseStore()
	got := Compute(base, base, &diff.SnapshotDiff{}, Options{Target: "core"})

	if len(got.Targets) != 1 || got.Targets[0] != "core" {
		t.Errorf("Targets = %v, want exactly [core]", got.Targets)
	}
}

// A substring that matches nothing yields no targets rather than the whole graph.
func TestUnmatchedTargetYieldsNothing(t *testing.T) {
	base := baseStore()
	got := Compute(base, base, &diff.SnapshotDiff{}, Options{Target: "nosuchthing"})

	if len(got.Targets) != 0 {
		t.Errorf("Targets = %v, want none", got.Targets)
	}
}

// Added facts have no pre-change dependents, so they cannot contribute to a radius
// computed on the baseline graph. Deriving targets from them would predict a blast radius
// for something that did not exist to be depended upon.
func TestDerivedTargetsIgnoreAdditions(t *testing.T) {
	d := &diff.SnapshotDiff{
		FactsAdded:   []facts.Fact{{Kind: facts.KindSymbol, Name: "core.New"}},
		FactsRemoved: []facts.Fact{{Kind: facts.KindSymbol, Name: "core.Old"}},
	}

	got := derivedTargets(d)

	if len(got) != 1 || got[0] != "core.Old" {
		t.Errorf("derivedTargets = %v, want only the removed symbol", got)
	}
}

// goStore is the shape a Go repository has: packages named by import path and filed
// under their directory, and facts that declare no package (a dependency, a test
// reference, a constant) filed under that directory too.
func goStore() *facts.Store {
	gomod := func(name, dir string) facts.Fact {
		f := facts.Fact{Kind: facts.KindModule, Name: name, File: dir}
		f.SetProp("language", "go")
		return f
	}
	s := facts.NewStore()
	s.Add(
		gomod("example.com/app/core", "core"),
		gomod("example.com/app/api", "api"),
		gomod("example.com/app/unrelated", "unrelated"),
		facts.Fact{
			Kind: facts.KindDependency, Name: "example.com/app/api -> example.com/app/core", File: "api/a.go",
			Relations: []facts.Relation{{Kind: facts.RelImports, Target: "example.com/app/core"}},
		},
	)
	s.BuildGraph()
	return s
}

// A Go package is one package, however a fact names it. Facts that declare no package
// fell back to their directory, so a change to core counted "core" and
// "example.com/app/core" as two packages touched, and the directory one was always
// spillover: the target's own package reported as outside the target.
func TestGoPackagesAreNamedOnceByImportPath(t *testing.T) {
	base := goStore()
	d := &diff.SnapshotDiff{
		FactsAdded: []facts.Fact{
			{Kind: facts.KindDependency, Name: "example.com/app/core -> fmt", File: "core/c.go"},
			{Kind: facts.KindTestRef, Name: "core/c_test.go", File: "core/c_test.go"},
		},
		FactsChanged: []diff.FactChange{{
			Before: facts.Fact{Kind: facts.KindSymbol, Name: "example.com/app/core.Run", File: "core/c.go",
				Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: "example.com/app/core"}}},
			After: facts.Fact{Kind: facts.KindSymbol, Name: "example.com/app/core.Run", File: "core/c.go",
				Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: "example.com/app/core"}}},
		}},
	}

	for _, target := range []string{"example.com/app/core", "core"} {
		got := Compute(base, base, d, Options{Target: target})
		if len(got.ActualPackages) != 1 || got.ActualPackages[0] != "example.com/app/core" {
			t.Errorf("target %q: ActualPackages = %v, want [example.com/app/core]", target, got.ActualPackages)
		}
		if len(got.Spillover) != 0 {
			t.Errorf("target %q: Spillover = %v, want none: the change stayed in its target", target, got.Spillover)
		}
	}

	got := Compute(base, base, d, Options{ExpectedPackages: []string{"core"}})
	if len(got.Spillover) != 0 {
		t.Errorf("--expected core: Spillover = %v, want none", got.Spillover)
	}
}
