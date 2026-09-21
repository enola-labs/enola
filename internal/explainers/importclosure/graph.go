// Package importclosure answers what a Python entry point actually loads when it
// is imported.
//
// It exists because the module graph the other explainers read cannot answer that
// question. That graph is deliberately coarse: common.BuildModuleGraph resolves BOTH
// endpoints of every edge up to their enclosing module directory, because coupling is
// a property of packages, not of files. Import cost is the opposite — it is a property
// of files, and of the exact order Python walks them, so an edge from
// "pkg/api" to "pkg/shared" cannot say which of the twenty modules under pkg/shared
// were paid for.
//
// The two graphs are therefore kept separate on purpose, and this one is read by no
// coupling explainer. That is not tidiness: a package node accumulates an edge from
// every file beneath it, so folding import-time reachability into the module graph
// would make each namespace package the most-depended-upon node in the repository
// and turn every one of them into a god-class finding.
package importclosure

import (
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/explainers/common"
	"github.com/enola-labs/enola/internal/facts"
)

// Graph is the import-time adjacency between FILES: Edges[f] lists the files that
// importing f loads, in the sense that Python executes them before f finishes
// importing. Imports that do not run at module-import time — a function-local import,
// a TYPE_CHECKING block — carry Props["deferred"] and contribute no edge, since the
// whole point is to distinguish what an import costs from what a call might later.
type Graph struct {
	Edges map[string][]string
	// Files is every Python file the snapshot knows, so a caller can tell "not
	// reachable" from "not in this repository".
	Files map[string]bool
}

// Build derives the import-time file graph from a snapshot.
//
// Nothing is stored for it: the edges are a projection of the dependency facts the
// Python extractor already emits, so this adds no facts, no cache version, and no
// weight to a snapshot that does not ask the question.
func Build(store *facts.Store) *Graph {
	g := &Graph{Edges: map[string][]string{}, Files: map[string]bool{}}

	for _, f := range store.FactsRef() {
		if strings.HasSuffix(f.File, ".py") {
			g.Files[f.File] = true
		}
	}

	seen := map[[2]string]bool{}
	for _, dep := range store.ByKind(facts.KindDependency) {
		if lang, _ := dep.PropAny("language").(string); lang != "python" {
			continue
		}
		// Only imports that actually run when the module is imported.
		if deferred, _ := dep.PropAny("deferred").(bool); deferred {
			continue
		}
		// An external or stdlib target names no file in this repository.
		if src, _ := dep.PropAny(facts.PropSource).(string); src != facts.DepSourceInternal {
			continue
		}
		for _, rel := range dep.Relations {
			if rel.Kind != facts.RelImports {
				continue
			}
			target := rel.Target
			if strings.HasPrefix(target, ".") {
				// resolveImports rewrites relative targets at extraction time, but a
				// `from . import x` resolves to the importer's own directory and is
				// left as written.
				target = common.ResolveRelativeImport(fileDir(dep.File), target)
			}
			to := g.resolveFile(target)
			// `from pkg import submodule` binds a MODULE, not an attribute of the
			// package, so Python loads pkg/submodule.py as well — but the import
			// target names only pkg, and the submodule appears nowhere in the edge.
			// The re-exported names recover it, and requiring a file of that name to
			// exist keeps an ordinary `from pkg import a_function` from inventing one.
			for _, sub := range g.reexportedSubmodules(dep, to) {
				g.addEdge(seen, dep.File, sub)
			}
			if to == "" {
				continue
			}
			// Importing a.b.c executes a/__init__.py and a/b/__init__.py before
			// a/b/c.py — no import statement names them, but they run, and one that
			// re-exports pulls in a whole subtree the leaf never mentions. They are
			// attributed to the importer rather than chained through each other,
			// because a single import statement is what causes all of them; a chain
			// would claim a/__init__.py imports a/b/__init__.py, which it need not.
			for _, anc := range g.ancestorPackages(to) {
				g.addEdge(seen, dep.File, anc)
			}
			g.addEdge(seen, dep.File, to)
		}
	}
	for f := range g.Edges {
		sort.Strings(g.Edges[f])
	}
	return g
}

// resolveFile maps an import target to the file Python would execute for it: a
// module target names that module's file, a package target names its __init__.py.
// A target naming neither (an unresolved dotted path) yields "" rather than a guess.
func (g *Graph) resolveFile(target string) string {
	if target == "" {
		return ""
	}
	if f := target + ".py"; g.Files[f] {
		return f
	}
	if f := target + "/__init__.py"; g.Files[f] {
		return f
	}
	return ""
}

// Closure returns every file reachable from entry by import-time edges, mapped to
// the fewest hops it takes to reach it. The entry itself is at depth 0.
func (g *Graph) Closure(entry string) map[string]int {
	depth := map[string]int{entry: 0}
	queue := []string{entry}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, next := range g.Edges[cur] {
			if _, ok := depth[next]; ok {
				continue
			}
			depth[next] = depth[cur] + 1
			queue = append(queue, next)
		}
	}
	return depth
}

// fileDir returns the directory part of a slash path, or "" for a bare filename.
func fileDir(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[:i]
	}
	return ""
}

// addEdge records from -> to once, skipping the self-edge a package's own
// __init__.py would otherwise get from importing one of its submodules.
func (g *Graph) addEdge(seen map[[2]string]bool, from, to string) {
	if from == to {
		return
	}
	key := [2]string{from, to}
	if seen[key] {
		return
	}
	seen[key] = true
	g.Edges[from] = append(g.Edges[from], to)
}

// ancestorPackages returns the __init__.py files Python executes on the way to a
// target, outermost first.
//
// Only ancestors that are real packages count: a directory with no __init__.py is a
// namespace package, which executes nothing. An __init__.py that is empty executes
// nothing either, and produces no facts, so it is not a known file and cannot be
// named here — a bounded imprecision, since a file that runs no code can only ever be
// a missing leaf and never gates what lies beneath it.
func (g *Graph) ancestorPackages(target string) []string {
	dir := fileDir(target)
	if dir == "" {
		return nil
	}
	parts := strings.Split(dir, "/")
	var out []string
	for i := 1; i <= len(parts); i++ {
		init := strings.Join(parts[:i], "/") + "/__init__.py"
		if init == target || !g.Files[init] {
			continue
		}
		out = append(out, init)
	}
	return out
}

// reexportedSubmodules returns the submodule files a from-import binds by name.
//
// `from pkg import thing` is ambiguous in the source: thing may be an attribute pkg
// already defines, or a module pkg/thing.py that Python imports as a side effect. The
// extractor records the bound names on a package's own imports as `reexports`; a name
// matching a real file under the package is the module case, and anything else is an
// attribute and yields nothing.
//
// The package is taken from the resolved target where there is one, and otherwise
// from the importing file itself, which covers a package __init__.py importing its own
// submodules by absolute path — the target there names the package the importer IS,
// so it resolves to no distinct file.
func (g *Graph) reexportedSubmodules(dep facts.Fact, resolved string) []string {
	raw, ok := dep.PropAny("reexports").([]any)
	if !ok || len(raw) == 0 {
		return nil
	}
	pkgDir := ""
	switch {
	case strings.HasSuffix(resolved, "/__init__.py"):
		pkgDir = fileDir(resolved)
	case resolved == "" && strings.HasSuffix(dep.File, "/__init__.py"):
		pkgDir = fileDir(dep.File)
	default:
		return nil
	}
	var out []string
	for _, r := range raw {
		name, _ := r.(string)
		if name == "" || strings.Contains(name, ".") {
			continue // a dotted entry is the module path, not a bound short name
		}
		if f := g.resolveFile(pkgDir + "/" + name); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// EntryPoints returns the __init__.py of every TOP-LEVEL package: a package whose
// parent directory is not itself a package. These are the files an outside importer
// names, so they are the only ones whose closure is a cost somebody actually pays.
// Test packages are excluded — their import cost is nobody's dependency.
func (g *Graph) EntryPoints() []string {
	var out []string
	for f := range g.Files {
		if !strings.HasSuffix(f, "/__init__.py") {
			continue
		}
		// Nested packages are not entry points — nobody imports them from outside.
		// Every ancestor is checked, not just the immediate parent: an empty
		// __init__.py produces no facts and so is not a known file, and testing only
		// the parent made every package under one look top-level.
		nested := false
		for dir := fileDir(fileDir(f)); dir != ""; dir = fileDir(dir) {
			if g.Files[dir+"/__init__.py"] {
				nested = true
				break
			}
		}
		if nested {
			continue
		}
		if facts.IsTestPath(f) {
			continue
		}
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// packageFileCount is how many Python files live under a package, the denominator for
// "how much of this package does importing it load".
func (g *Graph) packageFileCount(pkg string) int {
	n := 0
	for f := range g.Files {
		if strings.HasPrefix(f, pkg+"/") || f == pkg+"/__init__.py" {
			n++
		}
	}
	return n
}

// Path returns one shortest import chain from entry to target, inclusive, or nil when
// target is unreachable. It re-walks rather than caching parents in Closure, because
// the chain is wanted for a handful of findings and the closure for every file.
func (g *Graph) Path(entry, target string) []string {
	if entry == target {
		return []string{entry}
	}
	parent := map[string]string{entry: ""}
	queue := []string{entry}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, next := range g.Edges[cur] {
			if _, ok := parent[next]; ok {
				continue
			}
			parent[next] = cur
			if next == target {
				var rev []string
				for f := target; f != ""; f = parent[f] {
					rev = append(rev, f)
				}
				for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
					rev[i], rev[j] = rev[j], rev[i]
				}
				return rev
			}
			queue = append(queue, next)
		}
	}
	return nil
}

// ClosureWithout is Closure with one file removed from the graph — the counterfactual
// that measures what reaching through that file is responsible for.
func (g *Graph) ClosureWithout(entry, excluded string) map[string]int {
	depth := map[string]int{entry: 0}
	queue := []string{entry}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, next := range g.Edges[cur] {
			if next == excluded {
				continue
			}
			if _, ok := depth[next]; ok {
				continue
			}
			depth[next] = depth[cur] + 1
			queue = append(queue, next)
		}
	}
	return depth
}
