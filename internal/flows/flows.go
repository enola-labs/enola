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
)

// Mechanisms an entry is reached through, beside the framework-roots ones
// (job, mailer, hook, task, callback), which pass through under their own name.
const (
	MechanismRoute      = "route"
	MechanismMain       = "main"
	MechanismRegistered = "registered"
	// MechanismCommand is a command of a command-line program, `enola check`: the
	// function an extractor found it runs carries the command as a prop.
	MechanismCommand = "command"

	// mechanismCallback is the framework-roots mechanism that is not an entry.
	mechanismCallback = "callback"
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
	// moduleList is how many of a flow's modules are listed, largest first.
	moduleList = 10
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
	ID        string `json:"id"`
	Name      string `json:"name"`
	Rank      int    `json:"rank"`
	Repo      string `json:"repo,omitempty"`
	Mechanism string `json:"mechanism"`
	// Side marks a flow whose entry is not the program: an example, a build or
	// docs script, a migration. Side flows rank below every other flow.
	Side    bool          `json:"side,omitempty"`
	Routes  []Ref         `json:"routes,omitempty"`
	Entry   Ref           `json:"entry"`
	Summary string        `json:"summary,omitempty"`
	Metrics Metrics       `json:"metrics"`
	Modules []ModuleShare `json:"modules,omitempty"`
	Stages  []Stage       `json:"stages,omitempty"`
	// SameReachAs names the flows folded into this one because they reach the
	// same symbols from a different entry.
	SameReachAs []string `json:"same_reach_as,omitempty"`
	// NestedIn names larger flows that contain nearly all of this one.
	NestedIn []string `json:"nested_in,omitempty"`

	entry int32
	// reach is every symbol the walk visited, entry included, sorted.
	reach []int32
}

// Source is what the analysis reads a graph through: every fact once, to find
// the entries, and then symbols by name as the walk reaches them. A fact store
// is one.
type Source interface {
	FactsRef() []facts.Fact
	LookupByExactName(name string) []facts.Fact
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

	src   Source
	nodes []node
	index map[[2]string]int32
	opts  Options
	// ifaces is built the first time the walk reaches an interface method.
	ifaces *dispatchIndex
}

// node is one symbol of the call graph. Same-named symbols of one repository
// are one node, as they are in the fact graph, because a call names its target
// and nothing else.
//
// A node exists once something names it and is expanded, its calls resolved,
// once the walk arrives at it. A repository of a million symbols has entries
// that reach a few thousand of them, and the rest are never looked at.
type node struct {
	fact       facts.Fact
	module     string
	callees    []int32
	unresolved int
	io         bool
	// pending holds the node's facts until it is expanded.
	pending  []facts.Fact
	expanded bool
}

type root struct {
	mechanism string
	routes    []facts.Fact
}

// Analyze finds the entries of a graph, walks each, and ranks what it found.
func Analyze(src Source, opts Options) *Analysis {
	if opts.MaxDepth <= 0 {
		opts.MaxDepth = defaultMaxDepth
	}
	if opts.MaxReach <= 0 {
		opts.MaxReach = defaultMaxReach
	}
	a := &Analysis{Roots: map[string]int{}, src: src, index: map[[2]string]int32{}, opts: opts}
	roots := a.findRoots()

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

	a.nameApart()
	a.markNested()
	// In a program that serves routes, its commands are how it is run and kept, and
	// its routes are what it does: `gitea web` starts the server every route runs in,
	// and `superset load-examples` touches every model. They rank after its routes.
	serves := map[string]bool{}
	for _, f := range a.Flows {
		if f.Mechanism == MechanismRoute {
			serves[f.Repo] = true
		}
	}
	behindRoutes := func(f Flow) bool { return f.Mechanism == MechanismCommand && serves[f.Repo] }
	sort.SliceStable(a.Flows, func(i, j int) bool {
		x, y := a.Flows[i], a.Flows[j]
		// The program first. An example that reaches more than the product does
		// is still an example.
		if x.Side != y.Side {
			return !x.Side
		}
		if bx, by := behindRoutes(x), behindRoutes(y); bx != by {
			return !bx
		}
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

// Through returns the indexes of the flows whose walk reaches a symbol, ranked,
// and whether any symbol carries the name at all: no flow reaching a symbol and
// no such symbol are different answers. A name is matched exactly first and as
// a suffix otherwise, so the short name a listing prints can be passed back.
func (a *Analysis) Through(symbol string) ([]int, bool) {
	names := map[string]bool{}
	for _, f := range a.src.LookupByExactName(symbol) {
		if f.Kind == facts.KindSymbol {
			names[symbol] = true
		}
	}
	if len(names) == 0 {
		for _, f := range a.src.FactsRef() {
			if f.Kind == facts.KindSymbol && strings.HasSuffix(f.Name, symbol) {
				names[f.Name] = true
			}
		}
	}
	// Only a symbol some walk arrived at has a node; the rest are in no flow.
	want := map[int32]bool{}
	for key, id := range a.index {
		if names[key[1]] {
			want[id] = true
		}
	}
	var out []int
	for i := range a.Flows {
		// An entry declared in several files has a node per file, under no name.
		if names[a.Flows[i].Entry.Name] {
			out = append(out, i)
			continue
		}
		for id := range want {
			if contains(a.Flows[i].reach, id) {
				out = append(out, i)
				break
			}
		}
	}
	return out, len(names) > 0
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

// nodeFor returns the node of the symbol a name refers to from inside a
// repository: that repository's own, or another's when exactly one repository
// declares the name.
func (a *Analysis) nodeFor(repo, name string) (int32, bool) {
	if id, ok := a.index[[2]string{repo, name}]; ok {
		return id, id >= 0
	}
	var own, other []facts.Fact
	foreign := ""
	ambiguous := false
	for _, f := range a.src.LookupByExactName(name) {
		if f.Kind != facts.KindSymbol {
			continue
		}
		switch {
		case f.Repo == repo:
			own = append(own, f)
		case foreign == "" || foreign == f.Repo:
			foreign = f.Repo
			other = append(other, f)
		default:
			ambiguous = true
		}
	}
	if len(own) == 0 {
		if len(other) == 0 || ambiguous {
			a.index[[2]string{repo, name}] = -1
			return -1, false
		}
		// The caller's spelling resolves to the other repository's node.
		id, ok := a.nodeFor(foreign, name)
		a.index[[2]string{repo, name}] = id
		return id, ok
	}
	// The fact that describes the node is the first by file, so it does not
	// depend on the order the facts were read in.
	first := own[0]
	for _, f := range own[1:] {
		if f.File < first.File || (f.File == first.File && f.Line < first.Line) {
			first = f
		}
	}
	id := int32(len(a.nodes))
	a.index[[2]string{repo, name}] = id
	a.nodes = append(a.nodes, node{fact: first, module: moduleOf(first), pending: own})
	return id, true
}

// expand resolves a node's calls, once.
func (a *Analysis) expand(id int32) {
	if a.nodes[id].expanded {
		return
	}
	pending := a.nodes[id].pending
	a.nodes[id].pending, a.nodes[id].expanded = nil, true
	var callees []int32
	unresolved, io := 0, false
	for _, f := range pending {
		if f.PropBool("performs_io") {
			io = true
		}
		for _, rel := range f.Relations {
			if rel.Kind != facts.RelCalls {
				continue
			}
			// nodeFor may grow a.nodes, so nothing holds a pointer into it here.
			target, ok := a.nodeFor(f.Repo, rel.Target)
			if !ok {
				unresolved++
				continue
			}
			if target != id && !containsUnsorted(callees, target) {
				callees = append(callees, target)
			}
		}
	}
	// An interface method has no calls of its own; what runs is its implementation.
	for _, impl := range a.implementations(id) {
		if !containsUnsorted(callees, impl) {
			callees = append(callees, impl)
		}
	}
	sort.Slice(callees, func(i, j int) bool { return a.less(callees[i], callees[j]) })
	n := &a.nodes[id]
	n.callees, n.unresolved, n.io = callees, unresolved, io
}

// findRoots collects the entries: a served route's handler, a symbol a
// framework invokes, and a program's main. A handler is one entry however many
// routes it serves. Test code is never an entry, since a test is a way into the
// code under test and not into the program.
func (a *Analysis) findRoots() map[int32]*root {
	roots := map[int32]*root{}
	var named []facts.Fact
	for _, f := range a.src.FactsRef() {
		switch f.Kind {
		case facts.KindRoute:
			if f.PropString("role") == "client" {
				continue
			}
			for _, rel := range f.Relations {
				if rel.Kind != facts.RelHandledBy {
					continue
				}
				id, ok := a.nodeFor(f.Repo, rel.Target)
				if !ok || !a.eligible(id) {
					continue
				}
				if roots[id] == nil {
					roots[id] = &root{mechanism: MechanismRoute}
				}
				roots[id].routes = append(roots[id].routes, f)
			}
		case facts.KindSymbol:
			if entryMechanism(f) != "" && !facts.IsTestPath(f.File) {
				named = append(named, f)
			}
		}
	}
	// After the routes, so a handler that is also a main is the route's entry.
	//
	// A call names its target and nothing else, so same-named symbols are one
	// node. For an entry that is wrong: ten programs in one directory each
	// declare a main, and merged they are one flow reaching what all ten reach.
	// An entry name declared in several files is an entry per file.
	files := map[[2]string]map[string]bool{}
	for _, f := range named {
		key := [2]string{f.Repo, f.Name}
		if files[key] == nil {
			files[key] = map[string]bool{}
		}
		files[key][f.File] = true
	}
	perFile := map[[3]string]int32{}
	for _, f := range named {
		var id int32
		if len(files[[2]string{f.Repo, f.Name}]) > 1 {
			key := [3]string{f.Repo, f.Name, f.File}
			existing, ok := perFile[key]
			if !ok {
				existing = int32(len(a.nodes))
				perFile[key] = existing
				a.nodes = append(a.nodes, node{fact: f, module: moduleOf(f)})
			}
			id = existing
			n := &a.nodes[id]
			n.pending = append(n.pending, f)
			if f.Line < n.fact.Line {
				n.fact = f
			}
		} else {
			var ok bool
			if id, ok = a.nodeFor(f.Repo, f.Name); !ok {
				continue
			}
		}
		if roots[id] != nil || !a.eligible(id) {
			continue
		}
		roots[id] = &root{mechanism: entryMechanism(f)}
	}
	for _, r := range roots {
		sort.Slice(r.routes, func(i, j int) bool { return routeLabel(r.routes[i]) < routeLabel(r.routes[j]) })
	}
	return roots
}

// entryMechanism is how a symbol is entered from outside the code, or "" when
// nothing says it is.
func entryMechanism(f facts.Fact) string {
	switch f.PropString("symbol_kind") {
	case facts.SymbolFunc, facts.SymbolMethod:
	default:
		return ""
	}
	switch root := f.PropString(facts.PropFrameworkRoot); {
	case f.PropString("command") != "":
		return MechanismCommand
	case root == mechanismCallback:
		// A callback runs inside a request some route already entered. It is a
		// step of that flow, not a way into the program.
		return ""
	case root != "":
		return root
	case isMain(f):
		return MechanismMain
	case f.PropBool("framework_registered"):
		return MechanismRegistered
	}
	return ""
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
		a.expand(id)
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
		Side:      a.isSide(id),
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
	} else if c := a.nodes[id].fact.PropString("command"); c != "" && r.mechanism == MechanismCommand {
		f.Name = c
	}
	perModule := map[string]int{}
	for _, member := range reach {
		// A symbol at the walk's last hop was reached and never walked from.
		a.expand(member)
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
	// The count is in the metrics; the list is for reading.
	if len(f.Modules) > moduleList {
		f.Modules = f.Modules[:moduleList]
	}
	return f
}

// markNested names, for each of the largest flows, the larger flows that
// contain nearly all of it.
func (a *Analysis) markNested() {
	byReach := make([]int, len(a.Flows))
	for i := range byReach {
		byReach[i] = i
	}
	sort.SliceStable(byReach, func(i, j int) bool {
		x, y := a.Flows[byReach[i]], a.Flows[byReach[j]]
		if len(x.reach) != len(y.reach) {
			return len(x.reach) > len(y.reach)
		}
		if x.Name != y.Name {
			return x.Name < y.Name
		}
		return x.Repo < y.Repo
	})
	if len(byReach) > nestWindow {
		byReach = byReach[:nestWindow]
	}
	for i := 1; i < len(byReach); i++ {
		inner := &a.Flows[byReach[i]]
		for j := 0; j < i; j++ {
			outer := a.Flows[byReach[j]]
			if len(outer.reach) <= len(inner.reach) {
				continue
			}
			if float64(overlap(inner.reach, outer.reach)) >= nestShare*float64(len(inner.reach)) {
				inner.NestedIn = append(inner.NestedIn, outer.Name)
			}
		}
	}
}

// nameApart gives flows that share a name the file of their entry, so the mains
// of one directory read as the separate programs they are. Route flows keep
// their name: two handlers behind one path is a fact about the routes.
func (a *Analysis) nameApart() {
	count := map[[2]string]int{}
	for _, f := range a.Flows {
		if len(f.Routes) == 0 {
			count[[2]string{f.Repo, f.Name}]++
		}
	}
	for i := range a.Flows {
		f := &a.Flows[i]
		if len(f.Routes) == 0 && count[[2]string{f.Repo, f.Name}] > 1 && f.Entry.File != "" {
			f.Name += " (" + path.Base(f.Entry.File) + ")"
		}
	}
}

// sideAnywhere are directory names that mean "not the program" wherever they
// sit in a path. sideAtRoot mean it only as the first segment: `tools/` at the
// top of a repository is build tooling, and `app/modules/tools/` is a feature.
var (
	sideAnywhere = map[string]bool{
		"example": true, "examples": true, "sample": true, "samples": true,
		"demo": true, "demos": true, "benchmark": true, "benchmarks": true,
		"bench": true, "benches": true, "fixtures": true, "testdata": true,
		"script": true, "scripts": true, "tool": true,
		"migrate": true, "migrations": true,
	}
	sideAtRoot = map[string]bool{
		"tools": true, "doc": true, "docs": true, "dev": true, "contrib": true,
		"hack": true, "eng": true, "build": true, "ci": true, "packaging": true,
	}
)

// isSide reports whether an entry is something other than the program: an
// example, a script, a migration, a tool that builds or documents it. The
// module's own role decides when an extractor recorded one; the path decides
// otherwise.
func (a *Analysis) isSide(id int32) bool {
	n := a.nodes[id]
	for _, m := range a.src.LookupByExactName(n.module) {
		if m.Kind != facts.KindModule || m.Repo != n.fact.Repo {
			continue
		}
		switch m.PropString(facts.PropModuleRole) {
		case facts.ModuleRoleTest, facts.ModuleRoleTooling:
			return true
		}
	}
	return sidePath(n.fact.File)
}

func sidePath(file string) bool {
	segments := strings.Split(file, "/")
	if len(segments) < 2 {
		return false
	}
	// A dot directory at the top holds configuration and automation.
	if strings.HasPrefix(segments[0], ".") || sideAtRoot[segments[0]] {
		return true
	}
	for _, seg := range segments[:len(segments)-1] {
		if sideAnywhere[seg] {
			return true
		}
		// A compound name such as `dubbo-demo` or `my_examples` says the same.
		for _, tok := range strings.FieldsFunc(seg, func(r rune) bool { return r == '-' || r == '_' }) {
			switch tok {
			case "example", "examples", "sample", "samples", "demo", "demos":
				if tok != seg {
					return true
				}
			}
		}
	}
	return false
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

// isMain reports whether a symbol is a program's main: a member called main, as
// in Go's `cmd/tool.main` and Python's `pkg/cli.main`, or Main on .NET, where
// the runtime looks for that spelling. Main elsewhere is an ordinary exported
// name and is no entry.
func isMain(f facts.Fact) bool {
	if f.Name == "main" || strings.HasSuffix(f.Name, ".main") {
		return true
	}
	switch f.PropString("language") {
	case "csharp", "fsharp", "vbnet":
		return f.Name == "Main" || strings.HasSuffix(f.Name, ".Main")
	}
	return false
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
