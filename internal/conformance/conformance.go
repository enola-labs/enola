// Package conformance answers a question a delta cannot: did the change stay inside the
// scope you declared for it?
//
// A diff is a function of two snapshots, so it can report what changed and nothing about
// what you MEANT to change. Conformance takes a third input — a declared target, or a
// list of packages you expected to touch — runs reverse-dependency impact analysis on the
// PRE-CHANGE graph, and compares the blast radius that was predicted against the one that
// actually happened.
//
// The interesting output is the mismatch. A change that reaches a package outside its
// predicted radius is a change that did something its author did not describe, and that
// is worth knowing even when every finding is clean and the build is green.
//
// It lives beside the delta rather than inside it: diff.Compute must stay a pure function
// of two snapshots, and folding a third input into it would make every diff depend on
// something most callers never supply.
package conformance

import (
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/enola-labs/enola/internal/diff"
	"github.com/enola-labs/enola/internal/facts"
)

// maxResolvedTargets caps how many nodes a substring target expands to, so a short or
// common string cannot turn one declaration into a whole-graph traversal.
const maxResolvedTargets = 25

// Options declares the intended scope. With both fields empty the mode is AUTO: the
// targets are derived from the delta and the packages the change edited are taken as its
// own intent, which reports spillover beyond the edit sites without requiring anyone to
// have declared anything.
type Options struct {
	// Target is the symbol, type or package the caller intended to change. Matched
	// exactly if such a node exists, else as a case-insensitive substring.
	Target string
	// ExpectedPackages are packages the caller expected to touch, in addition to
	// whatever the impact analysis predicts.
	ExpectedPackages []string
	// MaxDepth / MaxNodes bound the reverse-dependency traversal. Zero means the
	// defaults below.
	MaxDepth int
	MaxNodes int
}

const (
	defaultMaxDepth = 3
	defaultMaxNodes = 200
)

// Report is the predicted-vs-actual comparison. Package lists are sorted, so two runs
// over the same inputs render identically.
type Report struct {
	// Declared is true when the caller supplied a target or expected packages. It
	// changes what spillover MEANS: in declared mode it is a conformance failure, in
	// auto mode it is an observation about a radius nobody claimed.
	Declared bool `json:"declared"`
	// Targets are the graph nodes the impact analysis actually ran on.
	Targets []string `json:"analyzed_targets,omitempty"`

	PredictedPackages []string `json:"predicted_packages"`
	ActualPackages    []string `json:"actual_packages"`
	Matched           []string `json:"matched"`
	// Spillover are packages the change touched that were neither predicted nor
	// declared — the finding this package exists to produce.
	Spillover []string `json:"spillover"`
	// Unrealized are predicted packages the change did not touch. Usually benign: a
	// change narrower than its blast radius is a change that avoided trouble.
	Unrealized []string `json:"unrealized,omitempty"`

	MatchRatio          float64 `json:"match_ratio"`
	PredictedDependents int     `json:"predicted_total_dependents"`
}

// Compute compares the predicted blast radius against the actual one.
//
// base is the pre-change store (the baseline's facts, with a graph built over them);
// cur is the post-change store, needed only to attribute a new edge's source to a package.
func Compute(base, cur *facts.Store, d *diff.SnapshotDiff, opts Options) Report {
	if d == nil || base == nil {
		return Report{}
	}
	declared := strings.TrimSpace(opts.Target) != "" || len(opts.ExpectedPackages) > 0
	pk := newPackager(base, cur)

	// Packages whose code directly changed.
	editSites := map[string]bool{}
	for _, f := range d.FactsAdded {
		addPkg(editSites, pk.of(f))
	}
	for _, c := range d.FactsChanged {
		addPkg(editSites, pk.of(c.After))
	}
	for _, f := range d.FactsRemoved {
		addPkg(editSites, pk.of(f))
	}
	// Plus packages that gained an outbound dependency. A package can be reached by a
	// change without any of its own facts moving — that is the coupling footprint, and
	// leaving it out would under-report exactly the case worth catching.
	actual := map[string]bool{}
	for p := range editSites {
		actual[p] = true
	}
	for _, e := range d.EdgesAdded {
		addPkg(actual, pk.ofName(cur, e.Source))
	}

	targets := opts.resolveTargets(base, d, pk)

	// Predicted radius: reverse dependents on the PRE-change graph. Built explicitly
	// because a store assembled from a loaded baseline has no graph index until asked.
	predicted := map[string]bool{}
	dependents := 0
	g := facts.NewGraph(base.All())
	for _, t := range targets {
		addPkg(predicted, pk.ofName(base, t)) // the target's own package is always expected
		res := g.ImpactSet(t, opts.maxDepth(), opts.maxNodes(), false)
		dependents += res.TotalDependents
		for _, nodes := range res.ByDepth {
			for _, n := range nodes {
				addPkg(predicted, pk.ofNode(base, n))
			}
		}
	}
	for _, p := range opts.ExpectedPackages {
		addPkg(predicted, pk.dir("", strings.TrimSpace(p)))
	}

	// In AUTO mode the edit sites ARE the intent — nobody declared anything, so the
	// places the author edited are the best available statement of what they meant. In
	// DECLARED mode the caller's scope stands alone, which is what lets an edit in an
	// undeclared package count as spillover.
	expected := clone(predicted)
	if !declared {
		for p := range editSites {
			expected[p] = true
		}
	}

	matched := intersect(actual, expected)
	ratio := 1.0
	if len(actual) > 0 {
		ratio = float64(len(matched)) / float64(len(actual))
	}

	return Report{
		Declared:            declared,
		Targets:             targets,
		PredictedPackages:   sortedKeys(predicted),
		ActualPackages:      sortedKeys(actual),
		Matched:             sortedKeys(matched),
		Spillover:           sortedKeys(minus(actual, expected)),
		Unrealized:          sortedKeys(minus(predicted, actual)),
		MatchRatio:          round2(ratio),
		PredictedDependents: dependents,
	}
}

func (o Options) maxDepth() int {
	if o.MaxDepth <= 0 {
		return defaultMaxDepth
	}
	return o.MaxDepth
}

func (o Options) maxNodes() int {
	if o.MaxNodes <= 0 {
		return defaultMaxNodes
	}
	return o.MaxNodes
}

// resolveTargets turns the declaration into concrete nodes present in the baseline graph,
// or derives them from the delta when nothing was declared.
func (o Options) resolveTargets(base *facts.Store, d *diff.SnapshotDiff, pk packager) []string {
	target := strings.TrimSpace(o.Target)
	if target == "" {
		return derivedTargets(d)
	}
	// A Go package's directory names that package, which is named by its import path.
	target = pk.dir("", target)
	// An exact node wins outright: a caller who named a real symbol meant that symbol,
	// and expanding it by substring would drag in every namesake.
	if len(base.LookupByExactName(target)) > 0 {
		return []string{target}
	}
	needle := strings.ToLower(target)
	seen := map[string]bool{}
	var out []string
	for _, f := range base.All() {
		if f.Name == "" || seen[f.Name] {
			continue
		}
		if strings.Contains(strings.ToLower(f.Name), needle) {
			seen[f.Name] = true
			out = append(out, f.Name)
			if len(out) >= maxResolvedTargets {
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// derivedTargets extracts the PRE-EXISTING symbols the change touched — the ones whose
// reverse dependents form an auto-derived blast radius.
//
// Added facts and edges contribute nothing: they are absent from the baseline graph, so
// they have no pre-change dependents to find.
func derivedTargets(d *diff.SnapshotDiff) []string {
	seen := map[string]bool{}
	var out []string
	add := func(name string) {
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	for _, c := range d.FactsChanged {
		add(c.After.Name)
	}
	for _, f := range d.FactsRemoved {
		add(f.Name)
	}
	for _, e := range d.EdgesRemoved {
		add(e.Source)
	}
	sort.Strings(out)
	return out
}

// packager names the package a fact belongs to, one name per package. A Go package is
// named by its import path and filed under its directory, so a fact that declares no
// package (a dependency, a test reference, a constant) would otherwise name it by
// directory, and one package would count as two: the directory one never predicted,
// never declared, and so always reported as spillover.
type packager struct{ dirs facts.ModuleDirs }

func newPackager(stores ...*facts.Store) packager {
	var mods []facts.Fact
	for _, s := range stores {
		if s == nil {
			continue
		}
		for _, f := range s.All() {
			if f.Kind == facts.KindModule {
				mods = append(mods, f)
			}
		}
	}
	return packager{dirs: facts.NewModuleDirs(mods)}
}

// dir returns the package living in directory dir, or dir itself when it is its name.
func (p packager) dir(repo, dir string) string {
	if n := p.dirs.Name(repo, dir); n != "" {
		return n
	}
	return dir
}

// of returns the package a fact belongs to: its own name for a module fact, otherwise
// the target of its `declares` relation, falling back to the package in the file's
// directory.
func (p packager) of(f facts.Fact) string {
	if f.Kind == facts.KindModule {
		return f.Name
	}
	for _, r := range f.Relations {
		if r.Kind == facts.RelDeclares {
			return r.Target
		}
	}
	if f.File != "" {
		return p.dir(f.Repo, path.Dir(f.File))
	}
	return ""
}

func (p packager) ofName(store *facts.Store, name string) string {
	if store == nil {
		return ""
	}
	for _, f := range store.ByName(name) {
		if pkg := p.of(f); pkg != "" {
			return pkg
		}
	}
	return ""
}

func (p packager) ofNode(store *facts.Store, n facts.TraversalNode) string {
	if pkg := p.ofName(store, n.Name); pkg != "" {
		return pkg
	}
	if n.File != "" {
		return p.dir("", path.Dir(n.File))
	}
	return ""
}

func addPkg(set map[string]bool, p string) {
	if p = strings.TrimSpace(p); p != "" && p != "." {
		set[p] = true
	}
}

func clone(a map[string]bool) map[string]bool {
	out := make(map[string]bool, len(a))
	for k := range a {
		out[k] = true
	}
	return out
}

func intersect(a, b map[string]bool) map[string]bool {
	out := map[string]bool{}
	for k := range a {
		if b[k] {
			out[k] = true
		}
	}
	return out
}

func minus(a, b map[string]bool) map[string]bool {
	out := map[string]bool{}
	for k := range a {
		if !b[k] {
			out[k] = true
		}
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// round2 keeps the ratio stable in its rendered form; it is reported, not compared.
func round2(f float64) float64 { return float64(int(f*100+0.5)) / 100 }

// Render describes the comparison in prose, for the text output of a tool that was asked
// to check conformance.
//
// Returns "" when nothing was declared AND nothing spilled over. In auto mode a change
// that stayed inside its own edit sites is the ordinary case, and printing a paragraph
// to say so on every diff would train readers to skip the section that matters when it is
// not empty.
func (r Report) Render() string {
	if !r.Declared && len(r.Spillover) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n## Scope\n\n")

	switch {
	case len(r.Spillover) > 0 && r.Declared:
		sb.WriteString("**Reached beyond the declared scope.** ")
	case len(r.Spillover) > 0:
		sb.WriteString("**Reached beyond the packages this change edited.** ")
	default:
		sb.WriteString("Stayed within the declared scope. ")
	}
	sb.WriteString(pluralf("%d of %d package(s) touched were predicted or declared",
		len(r.Matched), len(r.ActualPackages)))
	if len(r.ActualPackages) > 0 {
		sb.WriteString(", match ratio " + trimFloat(r.MatchRatio))
	}
	sb.WriteString(".\n")

	if len(r.Spillover) > 0 {
		sb.WriteString("\nSpillover — touched but neither predicted nor declared:\n")
		for _, p := range r.Spillover {
			sb.WriteString("  - " + p + "\n")
		}
		sb.WriteString("\nA package here was changed by something the declaration did not describe.\n" +
			"That is worth reading even when every finding is clean.\n")
	}
	if len(r.Unrealized) > 0 {
		sb.WriteString("\nPredicted but not touched (usually fine — the change was narrower than its blast radius):\n")
		for _, p := range r.Unrealized {
			sb.WriteString("  - " + p + "\n")
		}
	}
	return sb.String()
}

func pluralf(format string, matched, total int) string {
	return fmt.Sprintf(format, matched, total)
}

// trimFloat renders a ratio without trailing zeros, so "1" rather than "1.00".
func trimFloat(f float64) string {
	s := strconv.FormatFloat(f, 'f', 2, 64)
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}
