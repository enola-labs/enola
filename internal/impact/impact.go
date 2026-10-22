// Package impact computes the blast radius of one node, for impact_analysis and enola impact.
package impact

import (
	"encoding/json"
	"errors"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/resolve"
)

// Request is one impact query; a zero bound means its default.
type Request struct {
	Target         string
	MaxDepth       int
	MaxNodes       int
	IncludeForward bool
}

// Report is the traversal, plus how the target was resolved when not by exact name.
type Report struct {
	Resolution *resolve.NameResolution `json:"resolution,omitempty"`
	// Seeds names what was walked back from when the target stands for several
	// nodes: the symbols a source file declares, the constants nested under a
	// namespace, or the symbols declared beneath a directory. SeedKind says which.
	Seeds    []string `json:"seeds,omitempty"`
	SeedKind string   `json:"seed_kind,omitempty"`
	// Packages counts the listed dependents per owning package, most first, when
	// the repository declares packages with a manifest per directory.
	Packages []PackageCount `json:"packages,omitempty"`
	// Modules lists the modules that import the target module, directly or through
	// others, nearest first, when the target is a module. It is its own walk, over
	// imports alone, so it is complete where the symbol walk is cut at max_nodes:
	// "which packages depend on this one" is the question, and three thousand
	// symbols are not its answer.
	Modules []ModuleDependent `json:"modules,omitempty"`
	facts.ImpactResult
}

// ModuleDependent is a module that depends on the target and how many hops away.
type ModuleDependent struct {
	Module string `json:"module"`
	Repo   string `json:"repo,omitempty"`
	Depth  int    `json:"depth"`
}

// PackageCount is how many listed dependents one declared package holds.
type PackageCount struct {
	Package    string `json:"package"`
	Dependents int    `json:"dependents"`
}

// What a report's Seeds stand for.
const (
	SeedsFile      = "file"
	SeedsNamespace = "namespace"
	SeedsDirectory = "directory"
	SeedsPackage   = "package"
	SeedsFamily    = "family"
)

// ErrNoFacts and ErrNoGraph are values so each caller can word its own remedy.
var (
	ErrNoFacts = errors.New("no facts available")
	ErrNoGraph = errors.New("no graph available")
)

// Analyze resolves the target and walks its dependents. An ambiguous target returns candidates, not an error.
func Analyze(r resolve.Resolver, req Request) (Report, error) {
	store := r.Store
	if store == nil || store.Count() == 0 {
		return Report{}, ErrNoFacts
	}
	graph := store.Graph()
	if graph == nil {
		return Report{}, ErrNoGraph
	}

	if req.Target == "" {
		return Report{}, errors.New("target is required")
	}

	targetName, res, err := r.NodeName(req.Target)
	if err != nil {
		// Nothing is named that. A path to a source file or a directory still says what was meant.
		if file, seeds := declaredIn(r, req.Target); len(seeds) > 0 {
			return withPackages(store, Report{
				Resolution:   &resolve.NameResolution{Query: req.Target, Matched: file},
				Seeds:        seeds,
				SeedKind:     SeedsFile,
				ImpactResult: graph.ImpactSetOf(file, seeds, req.MaxDepth, req.MaxNodes, req.IncludeForward),
			}), nil
		}
		if dir, seeds, beside := declaredUnder(r, req.Target, "", false); len(seeds) > 0 {
			return withPackages(store, Report{
				Resolution:   &resolve.NameResolution{Query: req.Target, Matched: dir},
				Seeds:        seeds,
				SeedKind:     SeedsDirectory,
				ImpactResult: graph.ImpactSetOf(dir, append(seeds, beside...), req.MaxDepth, req.MaxNodes, req.IncludeForward),
			}), nil
		}
		return Report{}, err
	}
	if targetName != "" {
		canonical, normalization := canonicalTarget(store, targetName)
		if canonical != targetName {
			targetName = canonical
			// An exact file_ref normally has no resolution note. Surface this
			// normalization because it materially changes what was traversed.
			if res == nil {
				res = normalization
			}
		}
	}

	// Over threshold: refuse to guess; return resolution with empty results.
	if res != nil && res.Matched == "" {
		return Report{
			Resolution: res,
			ImpactResult: facts.ImpactResult{
				Target:  req.Target,
				ByDepth: map[int][]facts.TraversalNode{},
				Edges:   []facts.TraversalEdge{},
			},
		}, nil
	}

	// One method under several owners, an interface's and its implementations':
	// the impact of any of them is the impact of the call that may reach each.
	if res != nil && len(res.Family) > 1 {
		return withPackages(store, Report{
			Resolution:   res,
			Seeds:        res.Family,
			SeedKind:     SeedsFamily,
			ImpactResult: graph.ImpactSetOf(targetName, res.Family, req.MaxDepth, req.MaxNodes, req.IncludeForward),
		}), nil
	}

	// A namespace is its nested constants, and a directory holding no code of its
	// own is the code beneath it: walked as one node, either answers "nothing".
	if seeds := nestedUnder(store, targetName); len(seeds) > 0 {
		return withPackages(store, Report{
			Resolution:   res,
			Seeds:        seeds,
			SeedKind:     SeedsNamespace,
			ImpactResult: graph.ImpactSetOf(targetName, append([]string{targetName}, seeds...), req.MaxDepth, req.MaxNodes, req.IncludeForward),
		}), nil
	}
	if repo, ok := directoryModule(store, targetName); ok {
		if _, seeds, beside := declaredUnder(r, targetName, repo, true); len(seeds) > 0 {
			names := append(append([]string{targetName}, seeds...), beside...)
			return withPackages(store, Report{
				Resolution:   res,
				Seeds:        seeds,
				SeedKind:     SeedsDirectory,
				ImpactResult: graph.ImpactSetOf(targetName, names, req.MaxDepth, req.MaxNodes, req.IncludeForward),
			}), nil
		}
	}

	// A module with code of its own. Its symbols are the module, not its
	// dependents: walked from the module alone they came back at depth 1, so a Go
	// package answered "who depends on me" with its own functions, and its importers
	// were lost among their symbols. They are seeds instead, and the importing
	// modules are listed by a walk of their own.
	if repo, ok := directoryModule(store, targetName); ok {
		own, beside := ownSymbols(store, targetName, repo)
		names := append(append([]string{targetName}, own...), beside...)
		report := Report{Resolution: res, ImpactResult: graph.ImpactSetOf(targetName, names, req.MaxDepth, req.MaxNodes, req.IncludeForward)}
		if len(own) > 0 {
			report.Seeds, report.SeedKind = own, SeedsPackage
		}
		report.Modules = moduleDependents(graph, targetName)
		return withPackages(store, report), nil
	}

	result := graph.ImpactSet(targetName, req.MaxDepth, req.MaxNodes, req.IncludeForward)
	return withPackages(store, Report{Resolution: res, ImpactResult: result}), nil
}

// maxModuleDependents bounds the module walk; it is far above any package's real
// importer count, so the list is complete in practice.
const maxModuleDependents = 2000

// maxModuleDepth is the module walk's depth, the traversal's own ceiling. It does
// not follow the request's max_depth: that bounds the symbol walk, whose size
// grows with it, while "which packages depend on this one" is transitive and
// cheap. Bounded by the symbol walk's default of 3, a package four imports away
// was silently missing from a list that read as complete.
const maxModuleDepth = 20

// moduleDependents lists the modules that import module, directly or transitively.
func moduleDependents(graph *facts.Graph, module string) []ModuleDependent {
	walk := graph.TraverseFrom([]string{module}, "reverse",
		[]string{facts.RelImports, facts.RelDependsOn}, []string{facts.KindModule}, maxModuleDepth, maxModuleDependents)
	var out []ModuleDependent
	for _, n := range walk.Nodes {
		if n.Depth == 0 || n.Name == module {
			continue
		}
		out = append(out, ModuleDependent{Module: n.Name, Repo: n.Repo, Depth: n.Depth})
	}
	slices.SortFunc(out, func(a, b ModuleDependent) int {
		if a.Depth != b.Depth {
			return a.Depth - b.Depth
		}
		return strings.Compare(a.Module, b.Module)
	})
	return out
}

// ownSymbols returns the non-test symbols a module declares in its own directory,
// not in directories beneath it (in Go those are other packages), and the test
// facts there, which are walked from but not reported.
func ownSymbols(store *facts.Store, module, repo string) ([]string, []string) {
	var own, beside []string
	for _, m := range store.LookupByExactName(module) {
		if m.Kind != facts.KindModule || m.Repo != repo {
			continue
		}
		dir := strings.TrimSuffix(m.File, "/")
		store.QueryEach(facts.QueryOpts{FilePrefix: dir + "/"}, func(f *facts.Fact) {
			if path.Dir(f.File) != dir || f.Repo != repo {
				return
			}
			switch {
			case f.Kind == facts.KindTestRef || facts.IsTestPath(f.File):
				beside = append(beside, f.Name)
			case f.Kind == facts.KindSymbol && !f.PropBool(propGenerated) && !facts.IsDocument(*f):
				own = append(own, f.Name)
			}
		})
	}
	slices.Sort(own)
	slices.Sort(beside)
	return slices.Compact(own), slices.Compact(beside)
}

// Resolved reports whether a traversal ran; when false, no dependents means "not asked", not "none".
func (r Report) Resolved() bool {
	return r.Resolution == nil || r.Resolution.Matched != ""
}

// JSON encodes the report as impact_analysis does for output_mode=full.
func (r Report) JSON() ([]byte, error) {
	return json.MarshalIndent(r, "", "  ")
}

// declaredIn reads target as a path to a source file, absolute or relative, and
// returns the file as the facts name it with the symbols it declares, sorted. A
// file holding no symbol returns none: there is nothing to walk back from.
func declaredIn(r resolve.Resolver, target string) (string, []string) {
	rel := r.NormalizeToRelative(target)
	// In a multi-repo graph the facts name the file under its repo label.
	for _, file := range append([]string{rel}, r.ExpandFilePrefix(rel)...) {
		var names []string
		for _, f := range r.Store.ByFile(file) {
			if f.Kind == facts.KindSymbol {
				names = append(names, f.Name)
			}
		}
		if len(names) > 0 {
			slices.Sort(names)
			return file, slices.Compact(names)
		}
	}
	return "", nil
}

// canonicalTarget maps reference-only TypeScript/JavaScript file nodes to the
// extensionless module target imports actually point at. A file_ref records top-level
// calls for dead-code analysis; it is not the dependency node, so reverse traversal from
// it can truthfully see no edges while the corresponding module has many callers.
func canonicalTarget(store *facts.Store, target string) (string, *resolve.NameResolution) {
	exact := store.LookupByExactName(target)
	referenceOnly := false
	for _, f := range exact {
		switch f.Kind {
		case facts.KindFileRef, facts.KindTestRef:
			referenceOnly = true
		default:
			return target, nil
		}
	}
	if !referenceOnly {
		return target, nil
	}

	ext := strings.ToLower(filepath.Ext(target))
	switch ext {
	case ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs":
	default:
		return target, nil
	}
	candidate := strings.TrimSuffix(target, filepath.Ext(target))
	// A module target may be an implicit graph node (relations name it even when no
	// standalone fact does), so confirm it through either a fact or a relation.
	confirmed := len(store.LookupByExactName(candidate)) > 0
	if !confirmed {
		for _, f := range store.All() {
			for _, rel := range f.Relations {
				if rel.Target == candidate {
					confirmed = true
					break
				}
			}
			if confirmed {
				break
			}
		}
	}
	if !confirmed {
		return target, nil
	}
	return candidate, &resolve.NameResolution{Query: target, Matched: candidate, AutoPicked: true}
}

// namespaceSeparator joins a namespace to the constants nested in it.
const namespaceSeparator = "::"

// nestedUnder returns the symbols nested under name when name is a namespace:
// every symbol it resolves to is a module or a generated stub, and other symbols
// are named "<name>::…". A compactly nested declaration (`module A::B::C`) never
// declares A on its own, so the only node named A may be a stub, and A's
// dependents are its constants' dependents. Generated stubs and test code are not
// seeds: they declare nothing anyone calls.
func nestedUnder(store *facts.Store, name string) []string {
	exact := store.LookupByExactName(name)
	if len(exact) == 0 {
		return nil
	}
	repo := ""
	for _, f := range exact {
		if f.Kind != facts.KindSymbol {
			return nil
		}
		if f.PropString("symbol_kind") != facts.SymbolInterface && !f.PropBool(propGenerated) {
			return nil
		}
		repo = f.Repo
	}
	prefix := name + namespaceSeparator
	var names []string
	store.QueryEach(facts.QueryOpts{Kind: facts.KindSymbol, Name: prefix, Repo: repo}, func(f *facts.Fact) {
		if strings.HasPrefix(f.Name, prefix) && !f.PropBool(propGenerated) && !facts.IsTestPath(f.File) {
			names = append(names, f.Name)
		}
	})
	slices.Sort(names)
	return slices.Compact(names)
}

// propGenerated marks a fact read from generated code.
const propGenerated = "generated"

// directoryModule reports whether name is a module fact, with the repo that holds
// it. Whether it stands for the code beneath it is declaredUnder's call: a package
// with code of its own already has edges, while a directory that holds packages
// (or a package root whose code sits in subdirectories) has none but its
// manifest's.
func directoryModule(store *facts.Store, name string) (string, bool) {
	for _, f := range store.LookupByExactName(name) {
		if f.Kind == facts.KindModule {
			return f.Repo, true
		}
	}
	return "", false
}

// declaredUnder reads target as a directory and returns the directory as the
// facts name it, the non-test symbols declared beneath it, and the test facts
// beneath it. The test facts are walked from but not reported: a directory's own
// tests are not its dependents. With onlyNested, a directory with code directly in
// it returns nothing: that module is already a node with edges of its own.
func declaredUnder(r resolve.Resolver, target, repo string, onlyNested bool) (string, []string, []string) {
	rel := strings.TrimSuffix(r.NormalizeToRelative(target), "/")
	if rel == "" || rel == "." {
		return "", nil, nil
	}
	dirs := r.ExpandFilePrefix(rel)
	if repo != "" && !strings.HasPrefix(rel, repo+"/") {
		dirs = []string{repo + "/" + rel}
	}
	for _, dir := range dirs {
		var names, beside []string
		direct := false
		r.Store.QueryEach(facts.QueryOpts{FilePrefix: dir + "/"}, func(f *facts.Fact) {
			switch {
			case f.Kind == facts.KindTestRef || facts.IsTestPath(f.File):
				beside = append(beside, f.Name)
			case f.Kind != facts.KindSymbol || f.PropBool(propGenerated) || facts.IsDocument(*f):
				// A README beside a package's subdirectories is prose, not code of its
				// own: counting it would stop the directory standing for its code.
			default:
				names = append(names, f.Name)
				if path.Dir(f.File) == dir {
					direct = true
				}
			}
		})
		if onlyNested && direct {
			return "", nil, nil
		}
		if len(names) > 0 {
			slices.Sort(names)
			slices.Sort(beside)
			return dir, slices.Compact(names), slices.Compact(beside)
		}
	}
	return "", nil, nil
}

// withPackages fills in Packages: the listed dependents per owning package, when
// the store declares packages (module facts marked as package manifests).
func withPackages(store *facts.Store, report Report) Report {
	var roots []string
	for _, f := range store.ByKind(facts.KindModule) {
		if f.PropBool(propPackageManifest) && f.File != "" {
			roots = append(roots, path.Dir(f.File))
		}
	}
	if len(roots) == 0 {
		return report
	}
	counts := map[string]int{}
	for depth, nodes := range report.ByDepth {
		if depth == 0 {
			continue
		}
		for _, n := range nodes {
			if pkg := owningRoot(roots, n.File); pkg != "" {
				counts[pkg]++
			}
		}
	}
	for pkg, n := range counts {
		report.Packages = append(report.Packages, PackageCount{Package: pkg, Dependents: n})
	}
	slices.SortFunc(report.Packages, func(a, b PackageCount) int {
		if a.Dependents != b.Dependents {
			return b.Dependents - a.Dependents
		}
		return strings.Compare(a.Package, b.Package)
	})
	return report
}

// propPackageManifest marks a module fact read from a per-directory package manifest.
const propPackageManifest = "packwerk"

// owningRoot returns the longest root that file sits under, or "" for none.
func owningRoot(roots []string, file string) string {
	best := ""
	for _, root := range roots {
		if (root == "." || strings.HasPrefix(file, root+"/")) && len(root) > len(best) {
			best = root
		}
	}
	return best
}
