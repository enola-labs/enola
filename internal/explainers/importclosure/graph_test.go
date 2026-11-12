package importclosure

import (
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// dep builds an import dependency fact as the Python extractor emits one.
func dep(file, target string, deferred bool) facts.Fact {
	p := map[string]any{"language": "python", facts.PropSource: facts.DepSourceInternal}
	if deferred {
		p["deferred"] = true
	}
	return facts.Fact{
		Kind:      facts.KindDependency,
		Name:      file + " -> " + target,
		File:      file,
		Props:     p,
		Relations: []facts.Relation{{Kind: facts.RelImports, Target: target}},
	}
}

func sym(file string) facts.Fact {
	return facts.Fact{Kind: facts.KindSymbol, Name: file + ".x", File: file, Props: map[string]any{"language": "python"}}
}

func build(ff ...facts.Fact) *Graph {
	s := facts.NewStore()
	s.Add(ff...)
	return Build(s)
}

func TestBuild_ResolvesModulesAndPackages(t *testing.T) {
	g := build(
		sym("pkg/__init__.py"), sym("pkg/api.py"), sym("pkg/db.py"), sym("pkg/sub/__init__.py"),
		dep("pkg/__init__.py", "pkg/api", false),
		dep("pkg/api.py", "pkg/db", false),
		dep("pkg/api.py", "pkg/sub", false), // a package target loads its __init__.py
	)
	c := g.Closure("pkg/__init__.py")
	for file, want := range map[string]int{
		"pkg/__init__.py": 0, "pkg/api.py": 1, "pkg/db.py": 2, "pkg/sub/__init__.py": 2,
	} {
		if got, ok := c[file]; !ok || got != want {
			t.Errorf("closure[%s] = %d (present=%v), want %d", file, got, ok, want)
		}
	}
}

// TestBuild_DeferredImportsAreNotOnThePath is the reason the prop exists: a lazy
// import is a dependency, but it is not something `import pkg` pays for.
func TestBuild_DeferredImportsAreNotOnThePath(t *testing.T) {
	g := build(
		sym("pkg/__init__.py"), sym("pkg/api.py"), sym("pkg/heavy.py"),
		dep("pkg/__init__.py", "pkg/api", false),
		dep("pkg/api.py", "pkg/heavy", true), // function-local
	)
	c := g.Closure("pkg/__init__.py")
	if _, reached := c["pkg/heavy.py"]; reached {
		t.Error("a deferred import put pkg/heavy.py on the import path")
	}
	if len(c) != 2 {
		t.Errorf("closure has %d files, want 2", len(c))
	}
}

func TestBuild_SkipsExternalAndUnresolved(t *testing.T) {
	ext := dep("pkg/api.py", "requests", false)
	ext.Props[facts.PropSource] = facts.DepSourceExternal
	g := build(
		sym("pkg/api.py"), ext,
		dep("pkg/api.py", "pkg.unresolved.dotted", false), // names no file
	)
	if n := len(g.Edges["pkg/api.py"]); n != 0 {
		t.Errorf("pkg/api.py has %d edges, want 0 (external and unresolved targets name no file)", n)
	}
}

// TestBuild_CyclesTerminate — Python import cycles are real and common; the walk
// must not spin on one.
func TestBuild_CyclesTerminate(t *testing.T) {
	g := build(
		sym("pkg/a.py"), sym("pkg/b.py"),
		dep("pkg/a.py", "pkg/b", false),
		dep("pkg/b.py", "pkg/a", false),
	)
	c := g.Closure("pkg/a.py")
	if len(c) != 2 || c["pkg/b.py"] != 1 {
		t.Errorf("closure = %v, want both files with b at depth 1", c)
	}
}

// TestBuild_ClosureIsShortestPath pins that depth is hop count, not discovery order.
func TestBuild_ClosureIsShortestPath(t *testing.T) {
	g := build(
		sym("pkg/a.py"), sym("pkg/b.py"), sym("pkg/c.py"), sym("pkg/d.py"),
		dep("pkg/a.py", "pkg/b", false),
		dep("pkg/a.py", "pkg/d", false),
		dep("pkg/b.py", "pkg/c", false),
		dep("pkg/c.py", "pkg/d", false),
	)
	if got := g.Closure("pkg/a.py")["pkg/d.py"]; got != 1 {
		t.Errorf("depth of pkg/d.py = %d, want 1 (direct edge, not the 3-hop route)", got)
	}
}

// TestBuild_ParentPackagesAreOnThePath covers Python's implicit package execution:
// importing a.b.c runs a/__init__.py and a/b/__init__.py first. No import statement
// names them, so an edge-for-edge reading of the source misses them entirely.
func TestBuild_ParentPackagesAreOnThePath(t *testing.T) {
	g := build(
		sym("app.py"), sym("a/__init__.py"), sym("a/b/__init__.py"), sym("a/b/c.py"),
		dep("app.py", "a/b/c", false),
	)
	c := g.Closure("app.py")
	for _, want := range []string{"a/__init__.py", "a/b/__init__.py", "a/b/c.py"} {
		if _, ok := c[want]; !ok {
			t.Errorf("%s not on the import path", want)
		}
	}
}

// TestBuild_ParentPackageGatesItsSubtree is why this matters more than its own file
// count: a package __init__.py that re-exports pulls in modules the importer never
// named, and without it that whole subtree is invisible.
func TestBuild_ParentPackageGatesItsSubtree(t *testing.T) {
	g := build(
		sym("app.py"), sym("a/__init__.py"), sym("a/leaf.py"), sym("a/router.py"), sym("a/deep.py"),
		dep("app.py", "a/leaf", false),
		dep("a/__init__.py", "a/router", false), // the barrel re-export
		dep("a/router.py", "a/deep", false),
	)
	c := g.Closure("app.py")
	for _, want := range []string{"a/router.py", "a/deep.py"} {
		if _, ok := c[want]; !ok {
			t.Errorf("%s unreachable — the parent package's re-export was not followed", want)
		}
	}
}

// TestBuild_NamespacePackageExecutesNothing — a directory with no __init__.py is a
// namespace package. Nothing runs for it, so it must not become a node.
func TestBuild_NamespacePackageExecutesNothing(t *testing.T) {
	g := build(
		sym("app.py"), sym("ns/pkg/__init__.py"), sym("ns/pkg/mod.py"),
		dep("app.py", "ns/pkg/mod", false),
	)
	c := g.Closure("app.py")
	if _, ok := c["ns/__init__.py"]; ok {
		t.Error("ns/ has no __init__.py but was put on the import path")
	}
	if _, ok := c["ns/pkg/__init__.py"]; !ok {
		t.Error("ns/pkg is a real package and must be on the path")
	}
}

// TestBuild_PackageDoesNotImportItself guards the self-edge a package's own
// __init__.py would take from importing one of its submodules.
func TestBuild_PackageDoesNotImportItself(t *testing.T) {
	g := build(
		sym("a/__init__.py"), sym("a/mod.py"),
		dep("a/__init__.py", "a/mod", false),
	)
	for _, to := range g.Edges["a/__init__.py"] {
		if to == "a/__init__.py" {
			t.Error("a/__init__.py imports itself")
		}
	}
}

// TestBuild_DeferredImportCarriesNoParents — a lazy import runs nothing, so it does
// not pay for its target's parent packages either.
func TestBuild_DeferredImportCarriesNoParents(t *testing.T) {
	g := build(
		sym("app.py"), sym("a/__init__.py"), sym("a/b/__init__.py"), sym("a/b/c.py"),
		dep("app.py", "a/b/c", true),
	)
	if c := g.Closure("app.py"); len(c) != 1 {
		t.Errorf("closure = %v, want only the entry point", c)
	}
}

// TestBuild_FromPackageImportSubmodule covers `from pkg import submodule`, where the
// bound name is a MODULE Python loads, not an attribute of the package. The import
// target names only the package, so without the re-exported names the submodule — and
// anything it pulls in — is invisible.
//
// Both forms of the prop: []string is what the extractor writes, so a cold build sees
// it; []any is what a warm build reads back from the extractor cache and a restore
// from facts.jsonl. Reading only []any made a cold build report a smaller closure than
// a warm one over identical facts.
func TestBuild_FromPackageImportSubmodule(t *testing.T) {
	for name, reexports := range map[string]any{
		"as extracted": []string{"submod", "a_function"},
		"as decoded":   []any{"submod", "a_function"},
	} {
		d := dep("app.py", "pkg", false)
		d.Props["from"] = true
		d.Props["reexports"] = reexports
		g := build(
			sym("app.py"), sym("pkg/__init__.py"), sym("pkg/submod.py"), d,
		)
		c := g.Closure("app.py")
		if _, ok := c["pkg/submod.py"]; !ok {
			t.Errorf("%s: pkg/submod.py not loaded — the re-exported submodule was not followed", name)
		}
		// "a_function" names no file, so nothing may be invented for it.
		for f := range c {
			if strings.HasSuffix(f, "a_function.py") {
				t.Errorf("%s: invented a file for the non-module name: %s", name, f)
			}
		}
	}
}
