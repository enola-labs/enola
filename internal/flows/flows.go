// Package flows ranks the paths a program takes from where it is entered.
//
// The graph already records who calls whom. What it does not say is which of
// those chains matter: a repository with eleven thousand symbols has a handful
// of ways in that reach most of it, and a reader new to the code wants those
// first. A flow is one entry (a served route, a program's main, a symbol a
// framework invokes) with everything the calls from it reach.
//
// Nothing here is a verdict. A call the extractor could not resolve ends the
// walk at that point, so every flow carries the count of calls it did not
// follow, and a reach is a floor on what the entry touches rather than a
// measurement of it.
package flows

import (
	"path"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/linkers/binders/frameworkroots"
)

// Mechanisms an entry is reached through, beside the framework-roots ones
// (job, mailer, hook, task, callback), which pass through under their own name.
const (
	MechanismRoute      = "route"
	MechanismMain       = "main"
	MechanismRegistered = "registered"
)

const (
	defaultMaxDepth = 8
	// defaultMaxReach bounds one flow's walk. A repository of a million symbols
	// has entries that reach a large share of them, and a ranking does not need
	// the exact size of a set that large; a flow cut here says so.
	defaultMaxReach = 5000
	// nestWindow is how many of the largest flows are compared pairwise for
	// nesting. The comparison is quadratic and only the head of the ranking is
	// ever read.
	nestWindow = 200
	// nestShare is the share of a flow that must lie inside a larger one for
	// it to be reported as a stage of it rather than a path of its own.
	nestShare = 0.9
)

// Options bounds the walk; a zero field means its default.
type Options struct {
	MaxDepth int
	MaxReach int
}

// Ref names a fact a flow points at, with the id facts.jsonl writes for it.
type Ref struct {
	Name   string `json:"name"`
	Method string `json:"method,omitempty"`
	ID     string `json:"id"`
	File   string `json:"file,omitempty"`
	Line   int    `json:"line,omitempty"`
	Doc    string `json:"doc,omitempty"`
}

// Metrics are the numbers a flow was ranked on.
type Metrics struct {
	Reach      int `json:"reach"`
	Modules    int `json:"modules"`
	MaxDepth   int `json:"max_depth"`
	PerformsIO int `json:"performs_io,omitempty"`
	// UnresolvedCalls counts calls made inside the flow whose target is no
	// symbol in the graph: a library, or a callee the extractor could not name.
	UnresolvedCalls int `json:"unresolved_calls"`
	// Truncated is set when the walk stopped at the reach bound.
	Truncated bool `json:"truncated,omitempty"`
}

// ModuleShare is one module a flow passes through and how much of it sits there.
type ModuleShare struct {
	Name    string `json:"name"`
	Symbols int    `json:"symbols"`
}

// Stage is one step of a flow's stage tree, in pre-order.
type Stage struct {
	Order  int    `json:"order"`
	Depth  int    `json:"depth"`
	Parent string `json:"parent,omitempty"`
	Ref
	// Reach is this stage's share of the flow: the symbols it reaches, out of
	// its parent's share, that no stage placed before it had already reached.
	Reach int `json:"reach"`
}

// Flow is one entry and what it reaches.
type Flow struct {
	ID        string        `json:"id"`
	Name      string        `json:"name"`
	Rank      int           `json:"rank"`
	Repo      string        `json:"repo,omitempty"`
	Mechanism string        `json:"mechanism"`
	Routes    []Ref         `json:"routes,omitempty"`
	Entry     Ref           `json:"entry"`
	Summary   string        `json:"summary,omitempty"`
	Metrics   Metrics       `json:"metrics"`
	Modules   []ModuleShare `json:"modules,omitempty"`
	Stages    []Stage       `json:"stages,omitempty"`
	// SameReachAs names the flows folded into this one because they reach the
	// same symbols from a different entry.
	SameReachAs []string `json:"same_reach_as,omitempty"`
	// NestedIn names larger flows that contain nearly all of this one.
	NestedIn []string `json:"nested_in,omitempty"`

	entry int32
	// reach is every symbol the walk visited, entry included, sorted.
	reach []int32
}

// Analysis is the ranked flows of one store, with the call graph kept so a
// flow's stages can be built when it is asked for.
type Analysis struct {
	// Roots counts the entries found per mechanism, before any was dropped.
	Roots map[string]int `json:"roots"`
	// Leaves counts entries that reach nothing the graph can follow. They are
	// roots and not flows: there is no path to describe.
	Leaves int    `json:"leaves"`
	Flows  []Flow `json:"flows"`

	nodes []node
	opts  Options
}

// node is one symbol of the call graph. Same-named symbols of one repository
// are one node, as they are in the fact graph, because a call names its target
// and nothing else.
type node struct {
	fact       facts.Fact
	module     string
	callees    []int32
	unresolved int
	io         bool
}

type root struct {
	mechanism string
	routes    []facts.Fact
}

// Analyze finds the entries of a store, walks each, and ranks what it found.
func Analyze(store *facts.Store, opts Options) *Analysis {
	if opts.MaxDepth <= 0 {
		opts.MaxDepth = defaultMaxDepth
	}
	if opts.MaxReach <= 0 {
		opts.MaxReach = defaultMaxReach
	}
	a := &Analysis{Roots: map[string]int{}, opts: opts}
	index := a.buildNodes(store)
	roots := a.findRoots(store, index)

	entries := make([]int32, 0, len(roots))
	for id := range roots {
		entries = append(entries, id)
	}
	sort.Slice(entries, func(i, j int) bool { return a.less(entries[i], entries[j]) })

	// Entries reaching the same symbols are one flow seen from several doors.
	// The first by name keeps it and lists the others.
	bySignature := map[string]int{}
	for _, id := range entries {
		r := roots[id]
		a.Roots[r.mechanism]++
		reach, depth, truncated := a.walk(id, nil)
		if len(reach) < 2 {
			a.Leaves++
			continue
		}
		f := a.newFlow(id, r, reach, depth, truncated)
		sig := signature(reach, id)
		if kept, ok := bySignature[sig]; ok {
			a.Flows[kept].SameReachAs = append(a.Flows[kept].SameReachAs, f.Name)
			continue
		}
		bySignature[sig] = len(a.Flows)
		a.Flows = append(a.Flows, f)
	}

	sort.SliceStable(a.Flows, func(i, j int) bool {
		x, y := a.Flows[i], a.Flows[j]
		if x.Metrics.Reach != y.Metrics.Reach {
			return x.Metrics.Reach > y.Metrics.Reach
		}
		if x.Metrics.Modules != y.Metrics.Modules {
			return x.Metrics.Modules > y.Metrics.Modules
		}
		if x.Name != y.Name {
			return x.Name < y.Name
		}
		return x.Repo < y.Repo
	})
	for i := range a.Flows {
		a.Flows[i].Rank = i + 1
	}
	a.markNested()
	return a
}

// Describe builds the stage tree and the summary of the first n flows. They are
// left out of Analyze because a stage tree costs a walk per candidate callee,
// and a ranking of several hundred flows is read a few at a time.
func (a *Analysis) Describe(n int) {
	if n > len(a.Flows) {
		n = len(a.Flows)
	}
	for i := 0; i < n; i++ {
		a.DescribeFlow(i)
	}
}

// DescribeFlow builds the stage tree and summary of one flow, by index.
func (a *Analysis) DescribeFlow(i int) {
	f := &a.Flows[i]
	if f.Summary != "" {
		return
	}
	f.Stages = a.stages(f)
	f.Summary = summarize(f)
}

// Through returns the indexes of the flows whose walk reaches a symbol, ranked.
// A name is matched exactly first and as a suffix otherwise, so the short name
// a listing prints can be passed back.
func (a *Analysis) Through(symbol string) []int {
	want := map[int32]bool{}
	for id := range a.nodes {
		if a.nodes[id].fact.Name == symbol {
			want[int32(id)] = true
		}
	}
	if len(want) == 0 {
		for id := range a.nodes {
			if name := a.nodes[id].fact.Name; strings.HasSuffix(name, symbol) {
				want[int32(id)] = true
			}
		}
	}
	var out []int
	for i := range a.Flows {
		for id := range want {
			if contains(a.Flows[i].reach, id) {
				out = append(out, i)
				break
			}
		}
	}
	return out
}

// Find returns the index of the flow with this name, or of the one flow whose
// name contains it. More than one match is no match: the caller lists them.
func (a *Analysis) Find(name string) (int, []string) {
	var partial []int
	for i := range a.Flows {
		if a.Flows[i].Name == name {
			return i, nil
		}
		if strings.Contains(a.Flows[i].Name, name) {
			partial = append(partial, i)
		}
	}
	if len(partial) == 1 {
		return partial[0], nil
	}
	names := make([]string, 0, len(partial))
	for _, i := range partial {
		names = append(names, a.Flows[i].Name)
	}
	return -1, names
}

func (a *Analysis) buildNodes(store *facts.Store) map[[2]string]int32 {
	index := map[[2]string]int32{}
	byName := map[string][]int32{}
	symbols := store.ByKind(facts.KindSymbol)
	owner := make([]int32, len(symbols))
	for i, f := range symbols {
		key := [2]string{f.Repo, f.Name}
		id, ok := index[key]
		if !ok {
			id = int32(len(a.nodes))
			index[key] = id
			byName[f.Name] = append(byName[f.Name], id)
			a.nodes = append(a.nodes, node{fact: f, module: moduleOf(f)})
		}
		owner[i] = id
		if f.PropBool("performs_io") {
			a.nodes[id].io = true
		}
	}
	for i, f := range symbols {
		n := &a.nodes[owner[i]]
		for _, rel := range f.Relations {
			if rel.Kind != facts.RelCalls {
				continue
			}
			target, ok := index[[2]string{f.Repo, rel.Target}]
			if !ok {
				// Another repository's symbol, when exactly one carries the name.
				if ids := byName[rel.Target]; len(ids) == 1 {
					target, ok = ids[0], true
				}
			}
			if !ok {
				n.unresolved++
				continue
			}
			if target != owner[i] && !containsUnsorted(n.callees, target) {
				n.callees = append(n.callees, target)
			}
		}
	}
	for id := range a.nodes {
		callees := a.nodes[id].callees
		sort.Slice(callees, func(i, j int) bool { return a.less(callees[i], callees[j]) })
	}
	return index
}

// findRoots collects the entries: a served route's handler, a symbol a
// framework invokes, and a program's main. A handler is one entry however many
// routes it serves. Test code is never an entry, since a test is a way into the
// code under test and not into the program.
func (a *Analysis) findRoots(store *facts.Store, index map[[2]string]int32) map[int32]*root {
	roots := map[int32]*root{}
	for _, route := range store.ByKind(facts.KindRoute) {
		if route.PropString("role") == "client" {
			continue
		}
		for _, rel := range route.Relations {
			if rel.Kind != facts.RelHandledBy {
				continue
			}
			id, ok := index[[2]string{route.Repo, rel.Target}]
			if !ok || !a.eligible(id) {
				continue
			}
			if roots[id] == nil {
				roots[id] = &root{mechanism: MechanismRoute}
			}
			roots[id].routes = append(roots[id].routes, route)
		}
	}
	for id := range a.nodes {
		id := int32(id)
		if roots[id] != nil || !a.eligible(id) {
			continue
		}
		f := a.nodes[id].fact
		switch f.PropString("symbol_kind") {
		case facts.SymbolFunc, facts.SymbolMethod:
		default:
			continue
		}
		switch {
		case f.PropString(frameworkroots.RootProp) != "":
			roots[id] = &root{mechanism: f.PropString(frameworkroots.RootProp)}
		case isMain(f.Name):
			roots[id] = &root{mechanism: MechanismMain}
		case f.PropBool("framework_registered"):
			roots[id] = &root{mechanism: MechanismRegistered}
		}
	}
	for _, r := range roots {
		sort.Slice(r.routes, func(i, j int) bool { return routeLabel(r.routes[i]) < routeLabel(r.routes[j]) })
	}
	return roots
}

func (a *Analysis) eligible(id int32) bool {
	return !facts.IsTestPath(a.nodes[id].fact.File)
}

// walk is a breadth-first walk of the calls from one symbol, returning what it
// reached (sorted, the start included), the deepest hop, and whether it stopped
// at the reach bound. Symbols in blocked are neither entered nor counted.
func (a *Analysis) walk(start int32, blocked map[int32]bool) ([]int32, int, bool) {
	seen := map[int32]int{start: 0}
	queue := []int32{start}
	deepest, truncated := 0, false
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		depth := seen[id]
		if depth >= a.opts.MaxDepth {
			continue
		}
		for _, callee := range a.nodes[id].callees {
			if _, ok := seen[callee]; ok || blocked[callee] {
				continue
			}
			if len(seen) >= a.opts.MaxReach {
				truncated = true
				break
			}
			seen[callee] = depth + 1
			if depth+1 > deepest {
				deepest = depth + 1
			}
			queue = append(queue, callee)
		}
		if truncated {
			break
		}
	}
	reach := make([]int32, 0, len(seen))
	for id := range seen {
		reach = append(reach, id)
	}
	sort.Slice(reach, func(i, j int) bool { return reach[i] < reach[j] })
	return reach, deepest, truncated
}

func (a *Analysis) newFlow(id int32, r *root, reach []int32, depth int, truncated bool) Flow {
	entry := a.nodes[id].fact
	f := Flow{
		ID:        facts.FactID(entry.Repo, "flow", entry.Name, entry.File),
		Name:      entry.Name,
		Repo:      entry.Repo,
		Mechanism: r.mechanism,
		Entry:     a.ref(id),
		Metrics:   Metrics{Reach: len(reach), MaxDepth: depth, Truncated: truncated},
		entry:     id,
		reach:     reach,
	}
	for _, route := range r.routes {
		f.Routes = append(f.Routes, Ref{
			Name:   route.Name,
			Method: route.PropString("method"),
			ID:     route.Identity(),
			File:   route.File,
			Line:   route.Line,
		})
	}
	if len(r.routes) > 0 {
		f.Name = routeLabel(r.routes[0])
	}
	perModule := map[string]int{}
	for _, member := range reach {
		n := a.nodes[member]
		perModule[n.module]++
		f.Metrics.UnresolvedCalls += n.unresolved
		if n.io {
			f.Metrics.PerformsIO++
		}
	}
	f.Metrics.Modules = len(perModule)
	for name, count := range perModule {
		f.Modules = append(f.Modules, ModuleShare{Name: name, Symbols: count})
	}
	sort.Slice(f.Modules, func(i, j int) bool {
		if f.Modules[i].Symbols != f.Modules[j].Symbols {
			return f.Modules[i].Symbols > f.Modules[j].Symbols
		}
		return f.Modules[i].Name < f.Modules[j].Name
	})
	return f
}

// markNested names, for each flow near the head of the ranking, the larger
// flows that contain nearly all of it. The flows are already ranked largest
// first, so a container always sits earlier than what it contains.
func (a *Analysis) markNested() {
	window := len(a.Flows)
	if window > nestWindow {
		window = nestWindow
	}
	for i := 1; i < window; i++ {
		inner := &a.Flows[i]
		for j := 0; j < i; j++ {
			outer := a.Flows[j]
			if len(outer.reach) <= len(inner.reach) {
				continue
			}
			if float64(overlap(inner.reach, outer.reach)) >= nestShare*float64(len(inner.reach)) {
				inner.NestedIn = append(inner.NestedIn, outer.Name)
			}
		}
	}
}

func (a *Analysis) ref(id int32) Ref {
	f := a.nodes[id].fact
	return Ref{Name: f.Name, ID: f.Identity(), File: f.File, Line: f.Line, Doc: f.PropString("doc")}
}

// less orders two symbols by name, then repository, so every list built from
// node ids comes out the same on every run.
func (a *Analysis) less(x, y int32) bool {
	fx, fy := a.nodes[x].fact, a.nodes[y].fact
	if fx.Name != fy.Name {
		return fx.Name < fy.Name
	}
	return fx.Repo < fy.Repo
}

// moduleOf is the module a symbol is declared in, or its directory when the
// extractor recorded no containing module.
func moduleOf(f facts.Fact) string {
	for _, rel := range f.Relations {
		if rel.Kind == facts.RelDeclares {
			return rel.Target
		}
	}
	if dir := path.Dir(f.File); dir != "." {
		return dir
	}
	return ""
}

// isMain reports whether a symbol name ends in a member called main: Go's
// `cmd/tool.main`, Python's `pkg/cli.main`.
func isMain(name string) bool {
	return name == "main" || strings.HasSuffix(name, ".main")
}

func routeLabel(route facts.Fact) string {
	if method := route.PropString("method"); method != "" && method != facts.MethodAny {
		return method + " " + route.Name
	}
	return route.Name
}

// signature identifies what a flow reaches apart from its own entry, so two
// entries with the same downstream compare equal.
func signature(reach []int32, entry int32) string {
	var b strings.Builder
	for _, id := range reach {
		if id == entry {
			continue
		}
		b.WriteByte(byte(id))
		b.WriteByte(byte(id >> 8))
		b.WriteByte(byte(id >> 16))
		b.WriteByte(byte(id >> 24))
	}
	return b.String()
}

// overlap counts the ids two sorted slices share.
func overlap(x, y []int32) int {
	n, i, j := 0, 0, 0
	for i < len(x) && j < len(y) {
		switch {
		case x[i] < y[j]:
			i++
		case x[i] > y[j]:
			j++
		default:
			n++
			i++
			j++
		}
	}
	return n
}

func contains(sorted []int32, id int32) bool {
	i := sort.Search(len(sorted), func(i int) bool { return sorted[i] >= id })
	return i < len(sorted) && sorted[i] == id
}

func containsUnsorted(ids []int32, id int32) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}
