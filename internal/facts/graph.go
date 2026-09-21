package facts

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Graph provides adjacency-list indexes and traversal operations over a Store.
// It is a derived index rebuilt from the Store's facts after each snapshot generation.
//
// Node names are interned to dense uint32 IDs and the adjacency is held in CSR
// (compressed sparse row) form: one offset array indexed by node ID, plus flat target
// and relation-kind arrays shared by every node. Nothing here is per-node-allocated.
//
// The shape it replaced — two map[string][]Edge plus a map[string][]int fact index —
// cost 854 MiB on the Linux kernel's 1.89M facts: ~1.8M map keys times three,
// ~3.6M separately allocated edge slices, and 5.4M 32-byte Edge structs holding string
// headers. Those slices are also what made the graph ~21 live objects per fact, which
// for a long-running MCP server is a GC scan cost paid on every query, not just once.
type Graph struct {
	mu sync.RWMutex

	facts []Fact // reference to the store's facts (for metadata lookups)

	// Node ID space. Every name a fact declares AND every name an edge targets gets
	// an ID. IDs below declaredNodes were assigned while walking the facts, so they
	// are exactly the names some fact declares; anything at or above it is a dangling
	// edge target with no backing fact.
	ids           map[string]uint32
	names         []string
	declaredNodes uint32

	// Relation-kind table. The vocabulary is closed and tiny (the RelXxx constants,
	// plus anything an extractor invents), so an ID indexes it instead of repeating
	// the string on all 5.4M edges.
	relKinds []string
	relIDs   map[string]uint16

	// CSR adjacency over node IDs. fwdOff has len(names)+1 entries; the edges of node
	// n are the half-open range [fwdOff[n], fwdOff[n+1]) in fwdTgt/fwdRel. Within a
	// node's range, edges keep the order they were added — traversal output, and so
	// every golden file, depends on it.
	fwdOff []uint32
	fwdTgt []uint32
	fwdRel []uint16

	// Reverse adjacency, same layout. revTgt holds the SOURCE of each incoming edge.
	revOff []uint32
	revTgt []uint32
	revRel []uint16

	// Node ID → the indices in facts of every fact declaring that name, also CSR.
	// Only covers IDs below declaredNodes.
	factOff  []uint32
	factIdxs []int32
}

// noNode marks "this fact declares no name" in the per-fact ID scratch array.
const noNode = ^uint32(0)

// graphBuilder accumulates edges as three parallel arrays of IDs during construction
// and lays them out as CSR at the end.
//
// Edges are appended in INSERTION ORDER and that order is preserved through to the
// final layout, because the old map-of-slices appended in the same order and
// traversal walks it. Dedup keeps the FIRST occurrence for the same reason.
type graphBuilder struct {
	g   *Graph
	src []uint32
	tgt []uint32
	rel []uint16
}

// idFor returns the node ID for name, assigning the next one if it is new.
func (b *graphBuilder) idFor(name string) uint32 {
	if id, ok := b.g.ids[name]; ok {
		return id
	}
	id := uint32(len(b.g.names))
	b.g.ids[name] = id
	b.g.names = append(b.g.names, name)
	return id
}

// relIDFor returns the ID for a relation kind, assigning the next one if it is new.
func (b *graphBuilder) relIDFor(kind string) uint16 {
	if id, ok := b.g.relIDs[kind]; ok {
		return id
	}
	id := uint16(len(b.g.relKinds))
	b.g.relIDs[kind] = id
	b.g.relKinds = append(b.g.relKinds, kind)
	return id
}

// Edge represents a directed relationship between two facts.
type Edge struct {
	RelKind string // "imports", "calls", "declares", "implements", "depends_on", "has_method", "handled_by"
	Target  string // target fact name (forward) or source fact name (reverse)
}

// TraversalResult holds the output of a graph traversal.
type TraversalResult struct {
	Nodes []TraversalNode `json:"nodes"`
	Edges []TraversalEdge `json:"edges"`
	Stats TraversalStats  `json:"stats"`
}

// TraversalNode is a node visited during traversal.
type TraversalNode struct {
	Name  string `json:"name"`
	Kind  string `json:"kind"`
	File  string `json:"file,omitempty"`
	Line  int    `json:"line,omitempty"`
	Repo  string `json:"repo,omitempty"` // owning repo (multi-repo mode); makes cross-repo dependents legible
	Depth int    `json:"depth"`
	// Unresolved marks a node whose name is the target of an edge but has no
	// backing fact in the store. This happens for inferred call targets that
	// could not be matched to a declared symbol (e.g. interface-method dispatch,
	// or calls into packages that weren't analyzed). The edge is real; the
	// destination symbol just isn't in the graph.
	Unresolved bool `json:"unresolved,omitempty"`
	// Conflated lists the distinct fact kinds sharing this node's name, when more
	// than one does. Graph nodes are keyed by name, so same-named facts merge into
	// one node and their edges are unioned. Kind/File/Repo above describe whichever
	// fact best fits the relation that reached the node; this field admits that the
	// node also stands for others, so a walk through it is not read as more precise
	// than it is.
	Conflated []string `json:"conflated,omitempty"`
}

// TraversalEdge is an edge traversed during traversal.
type TraversalEdge struct {
	Source string `json:"source"`
	Target string `json:"target"`
	Kind   string `json:"kind"`
}

// TraversalStats summarizes a traversal.
type TraversalStats struct {
	NodesVisited    int  `json:"nodes_visited"`
	EdgesTraversed  int  `json:"edges_traversed"`
	MaxDepthReached int  `json:"max_depth_reached"`
	Truncated       bool `json:"truncated"`
}

// ImpactResult holds depth-bucketed impact analysis results.
type ImpactResult struct {
	Target          string                  `json:"target"`
	ByDepth         map[int][]TraversalNode `json:"by_depth"`
	Edges           []TraversalEdge         `json:"edges"`
	TotalDependents int                     `json:"total_dependents"` // true count of transitive dependents within max_depth, independent of the max_nodes display cap
	Summary         string                  `json:"summary"`
	Stats           TraversalStats          `json:"stats"`
	Forward         *TraversalResult        `json:"forward_dependencies,omitempty"`
	CrossRepoImpact []string                `json:"cross_repo_impact,omitempty"` // other repos with a dependent on the target
	GoverningIntent []GoverningPage         `json:"governing_intent,omitempty"`  // knowledge pages anchored to the target's file
}

// GoverningPage is a knowledge page whose declared anchors cover the file a
// fact lives in — the decision/spec trail governing the code under analysis.
// Relations carry the page's own outgoing edges, resolved against the
// compiled page set, so the trail continues past the first hop: the page
// that governs a file names what it is part of, depends on, or supersedes.
type GoverningPage struct {
	Page      string              `json:"page"`
	Type      string              `json:"type,omitempty"`
	Status    string              `json:"status,omitempty"`
	Relations []GoverningRelation `json:"relations,omitempty"`
}

// GoverningRelation is one outgoing typed edge of a governing page. Target
// type and status join from the target's own page declaration when the
// target compiles; a target outside the compiled set keeps only its path.
type GoverningRelation struct {
	Rel      string `json:"rel"`
	To       string `json:"to"`
	ToType   string `json:"to_type,omitempty"`
	ToStatus string `json:"to_status,omitempty"`
}

// PathResult holds a shortest-path result.
type PathResult struct {
	From  string          `json:"from"`
	To    string          `json:"to"`
	Found bool            `json:"found"`
	Path  []TraversalNode `json:"path,omitempty"`
	Edges []TraversalEdge `json:"edges,omitempty"`
}

// NewGraph builds a Graph from a slice of facts. The graph constructs both forward
// and reverse adjacency lists in a single O(F+R) pass.
//
// For dependency facts (kind="dependency") with "imports" relations, the graph also
// creates synthetic edges from the containing module (derived from the file's directory)
// to the import target. This bridges the structural gap where modules and their
// dependencies are separate facts: module "internal/server" ←→ dependency fact
// "internal/server -> internal/config" → target "internal/config".
//
// For cross-repo call targets (e.g. "github.com/dejo1307/go-auth/adapters.Handler.Login"
// emitted by an external consumer), the graph normalises the target by stripping known
// Go module path prefixes (stored in KindModule facts as props["modulePath"]). This
// allows edges to land on the correct fact in the loaded external repo.
func NewGraph(ff []Fact) *Graph {
	g := &Graph{
		facts:  ff,
		ids:    make(map[string]uint32, len(ff)),
		relIDs: make(map[string]uint16, 16),
	}
	b := &graphBuilder{g: g}

	// First pass: assign a node ID to every name a fact declares, and collect module
	// names and Go module paths. Doing declared names first is what makes
	// declaredNodes a usable boundary: after this loop, an ID below it is a name some
	// fact declares and an ID at or above it is a dangling edge target.
	moduleNames := make(map[string]bool)
	modulePaths := make(map[string]struct{}) // Go module paths for cross-repo normalisation
	nameID := make([]uint32, len(ff))        // per-fact node ID, so the next pass needn't re-hash
	for i, f := range ff {
		nameID[i] = noNode
		if f.Name != "" {
			nameID[i] = b.idFor(f.Name)
		}
		if f.Kind == KindModule {
			moduleNames[f.Name] = true
			if mp, ok := f.PropAny("modulePath").(string); ok && mp != "" {
				modulePaths[mp] = struct{}{}
			}
		}
	}
	g.declaredNodes = uint32(len(g.names))

	// Index which facts declare each node, as CSR. Every fact declaring the name is
	// recorded, not just the first: which one a node means depends on the edge that
	// reaches it, and that is not known until traversal time. Counting first and
	// filling in fact order keeps each node's list in ascending fact index, as
	// appending to a per-name slice did.
	g.factOff = make([]uint32, g.declaredNodes+1)
	for _, id := range nameID {
		if id != noNode {
			g.factOff[id+1]++
		}
	}
	for i := uint32(0); i < g.declaredNodes; i++ {
		g.factOff[i+1] += g.factOff[i]
	}
	g.factIdxs = make([]int32, g.factOff[g.declaredNodes])
	fillPos := make([]uint32, g.declaredNodes)
	copy(fillPos, g.factOff[:g.declaredNodes])
	for i, id := range nameID {
		if id != noNode {
			g.factIdxs[fillPos[id]] = int32(i)
			fillPos[id]++
		}
	}

	// Second pass: build adjacency lists
	for fi, f := range ff {
		for _, rel := range f.Relations {
			target := rel.Target
			// For unresolved call targets, attempt cross-repo normalisation by
			// stripping known Go module path prefixes.
			if rel.Kind == RelCalls {
				if !g.declares(target) {
					if normalized := normalizeExternalTarget(target, modulePaths); normalized != "" {
						if g.declares(normalized) {
							target = normalized
						}
					}
				}
			}
			b.addEdge(b.srcID(nameID[fi], f.Name), rel.Kind, target)
		}

		// For dependency facts with imports, also create module→target edges
		// so that traversing from a module follows through to its imports.
		// The target is resolved to the nearest ancestor that is a known module,
		// handling cases where import paths point to files within a module directory
		// (e.g., "src/types/tournament" resolves to module "src/types").
		if f.Kind == KindDependency && f.File != "" {
			modName := fileDirectory(f.File)
			if moduleNames[modName] {
				for _, rel := range f.Relations {
					if rel.Kind == RelImports {
						target := resolveToModule(rel.Target, moduleNames)
						if target != "" && target != modName {
							b.addEdge(b.idFor(modName), RelImports, target)
						}
					}
				}
			}
		}
	}

	// Third pass: synthesize "has_method" edges linking an owner type symbol
	// (struct/interface/class/type) to its method symbols. Extractors emit a
	// method as a sibling fact named "<owner>.<method>" or, for a Ruby instance
	// method, "<owner>#<method>", with no edge back to the owner, so forward
	// traversal from a type would otherwise surface none of its methods (and
	// transitively none of their calls). This is language-agnostic: any fact named
	// "<knownType>#<member>" or "<knownType>.<member>" gets wired to its owner, and
	// methodOwner tries both because a "#" is not always the separator.
	for _, f := range ff {
		if f.Kind != KindSymbol {
			continue
		}
		sk, _ := f.PropAny("symbol_kind").(string)
		if sk != SymbolMethod && sk != SymbolFunc && sk != SymbolGetter {
			continue
		}
		if owner := g.methodOwner(f.Name); owner != "" {
			b.addEdge(b.idFor(owner), RelHasMethod, f.Name)
		}
	}

	b.finish()
	return g
}

// srcID resolves the source node of a fact's edges. A fact with no name still gets a
// node — the map-based graph created a forward[""] bucket for it, and silently dropping
// those edges would change the graph rather than tidy it.
func (b *graphBuilder) srcID(id uint32, name string) uint32 {
	if id != noNode {
		return id
	}
	return b.idFor(name)
}

// addEdge records one directed edge. Duplicates are tolerated here and removed in
// finish, which is cheaper than the map of concatenated "source\x00kind\x00target"
// keys this replaced: on the Linux kernel that map held 5.4M freshly built strings,
// all of them garbage the moment construction ended.
func (b *graphBuilder) addEdge(src uint32, relKind, target string) {
	b.src = append(b.src, src)
	b.tgt = append(b.tgt, b.idFor(target))
	b.rel = append(b.rel, b.relIDFor(relKind))
}

// declares reports whether some fact declares this name — the CSR equivalent of the
// old `_, exists := g.factIdx[name]` test. Only IDs below declaredNodes qualify:
// anything above was minted for an edge target that nothing declares.
func (g *Graph) declares(name string) bool {
	id, ok := g.ids[name]
	return ok && id < g.declaredNodes
}

// finish deduplicates the accumulated edges and lays them out as CSR.
func (b *graphBuilder) finish() {
	g := b.g
	n := uint32(len(g.names))

	keep := b.dedup(n)
	if keep < len(b.src) {
		// Compact in place, preserving insertion order.
		w := 0
		for i := range b.src {
			if b.src[i] == noNode {
				continue // marked as a duplicate by dedup
			}
			b.src[w], b.tgt[w], b.rel[w] = b.src[i], b.tgt[i], b.rel[i]
			w++
		}
		b.src, b.tgt, b.rel = b.src[:w], b.tgt[:w], b.rel[:w]
	}

	g.fwdOff, g.fwdTgt, g.fwdRel = buildCSR(b.src, b.tgt, b.rel, n)
	g.revOff, g.revTgt, g.revRel = buildCSR(b.tgt, b.src, b.rel, n)
}

// dedup marks duplicate (source, relation, target) triples by setting their source to
// noNode, and returns how many edges survive. The FIRST occurrence of a triple is the
// one kept, matching the old edgeSeen map.
//
// Duplicates can only occur within one source's edges, so grouping by source bounds
// every comparison to a single node's fan-out — which is 2.9 on average even on the
// Linux kernel, where a global structure would need 5.4M entries.
func (b *graphBuilder) dedup(n uint32) int {
	e := len(b.src)
	if e == 0 {
		return 0
	}

	off, order := groupBy(b.src, n)
	dropped := 0
	var seen map[uint64]struct{}
	for s := uint32(0); s < n; s++ {
		run := order[off[s]:off[s+1]]
		if len(run) < 2 {
			continue
		}
		if len(run) <= dedupScanLimit {
			// Pairwise for small fan-outs: no allocation, and the run is short
			// enough that the quadratic term is smaller than a map's constant.
			for i := 1; i < len(run); i++ {
				for j := 0; j < i; j++ {
					if b.src[run[j]] == noNode {
						continue // already dropped; not a valid comparison partner
					}
					if b.tgt[run[i]] == b.tgt[run[j]] && b.rel[run[i]] == b.rel[run[j]] {
						b.src[run[i]] = noNode
						dropped++
						break
					}
				}
			}
			continue
		}
		if seen == nil {
			seen = make(map[uint64]struct{}, len(run))
		} else {
			clear(seen)
		}
		for _, ei := range run {
			k := uint64(b.tgt[ei])<<16 | uint64(b.rel[ei])
			if _, dup := seen[k]; dup {
				b.src[ei] = noNode
				dropped++
				continue
			}
			seen[k] = struct{}{}
		}
	}
	return e - dropped
}

// dedupScanLimit is the fan-out below which pairwise comparison beats a hash set.
const dedupScanLimit = 16

// groupBy counting-sorts edge indices by key, returning CSR offsets and the indices
// grouped under them. It is STABLE — within a group, indices stay in ascending order,
// which is what preserves insertion order in the final adjacency.
func groupBy(key []uint32, n uint32) (off, order []uint32) {
	off = make([]uint32, n+1)
	for _, k := range key {
		if k != noNode {
			off[k+1]++
		}
	}
	for i := uint32(0); i < n; i++ {
		off[i+1] += off[i]
	}
	order = make([]uint32, off[n])
	pos := make([]uint32, n)
	copy(pos, off[:n])
	for i, k := range key {
		if k == noNode {
			continue
		}
		order[pos[k]] = uint32(i)
		pos[k]++
	}
	return off, order
}

// buildCSR lays out edges keyed by `from`, storing `to` and `rel` in the flat arrays.
// Forward and reverse adjacency are the same call with the two ends swapped.
func buildCSR(from, to []uint32, rel []uint16, n uint32) (off, tgt []uint32, kinds []uint16) {
	off, order := groupBy(from, n)
	tgt = make([]uint32, len(order))
	kinds = make([]uint16, len(order))
	for i, ei := range order {
		tgt[i] = to[ei]
		kinds[i] = rel[ei]
	}
	return off, tgt, kinds
}

// lookup resolves a node name to its ID. A name with no node is not in the graph at
// all — neither declared by a fact nor targeted by an edge.
func (g *Graph) lookup(name string) (uint32, bool) {
	id, ok := g.ids[name]
	return id, ok
}

// adjOf returns node id's edges as parallel target/relation-ID slices, sub-slices of
// the flat CSR arrays. They alias the graph's storage and must not be modified.
func (g *Graph) adjOf(id uint32, reverse bool) ([]uint32, []uint16) {
	off, tgt, rel := g.fwdOff, g.fwdTgt, g.fwdRel
	if reverse {
		off, tgt, rel = g.revOff, g.revTgt, g.revRel
	}
	if id >= uint32(len(off)-1) {
		return nil, nil
	}
	lo, hi := off[id], off[id+1]
	return tgt[lo:hi], rel[lo:hi]
}

// degreeOf returns how many edges node id has in the given direction, without
// materializing them.
func (g *Graph) degreeOf(id uint32, reverse bool) int {
	off := g.fwdOff
	if reverse {
		off = g.revOff
	}
	if id >= uint32(len(off)-1) {
		return 0
	}
	return int(off[id+1] - off[id])
}

// relName maps a relation-kind ID back to its string.
func (g *Graph) relName(id uint16) string {
	if int(id) >= len(g.relKinds) {
		return ""
	}
	return g.relKinds[id]
}

// factIdxsOf returns the indices in g.facts of every fact declaring node id.
func (g *Graph) factIdxsOf(id uint32) []int32 {
	if id >= g.declaredNodes {
		return nil
	}
	return g.factIdxs[g.factOff[id]:g.factOff[id+1]]
}

// methodOwner returns the owner type name for a method fact name of the form
// "<owner>.<method>" or "<owner>#<method>", but only when <owner> is itself a
// known symbol fact whose symbol_kind is a type (struct/interface/class/type).
// Returns "" otherwise.
//
// Ruby names an instance method "Analytics::GoogleTagManager#render?" and a
// singleton method "Analytics::GoogleTagManager.configure", so "#" is tried
// first: where it does separate a method from its owner, a "." after it could
// only be part of the method's own name.
//
// It falls back to the last "." because "#" is not always a separator, and the
// two languages that put one elsewhere both reach this function. Rust escapes a
// keyword identifier as "r#async", so a method fact is named "src.Reader.r#async"
// and the "#"-split owner is "src.Reader.r". Every directory-prefixed name —
// TypeScript, Python, Kotlin, Swift, Java — carries a "#" in a directory name
// down into every symbol beneath it, so one directory named "v1#legacy" would
// otherwise unwire that whole subtree. In both the "#"-derived owner names
// nothing the graph declares, and the last "." is the real separator; the type
// guard below is what makes trying the second candidate safe, since an owner no
// fact declares as a type yields no edge either way.
func (g *Graph) methodOwner(name string) string {
	if hash := strings.LastIndex(name, "#"); hash > 0 {
		if owner := g.declaredTypeNamed(name[:hash]); owner != "" {
			return owner
		}
	}
	dot := strings.LastIndex(name, ".")
	if dot <= 0 {
		return ""
	}
	return g.declaredTypeNamed(name[:dot])
}

// declaredTypeNamed returns name when a symbol fact declares it with a type
// symbol_kind (struct/interface/class/type), and "" otherwise.
func (g *Graph) declaredTypeNamed(name string) string {
	// A method's owner is a symbol, so resolve with that context rather than the
	// bare name — a type sharing a name with a module must still find the type.
	idx, ok := g.factIndexFor(name, RelHasMethod)
	if !ok {
		return ""
	}
	of := g.facts[idx]
	if of.Kind != KindSymbol {
		return ""
	}
	switch sk, _ := of.PropAny("symbol_kind").(string); sk {
	case SymbolStruct, SymbolInterface, SymbolClass, SymbolType:
		return name
	}
	return ""
}

// Traverse performs a BFS traversal from the given start node.
// direction is "forward" or "reverse".
// relKinds filters to specific relation types (nil = all).
// nodeKinds filters result nodes to specific fact kinds (nil = all).
// maxDepth limits traversal depth (0 = use default 5).
// maxNodes limits total returned nodes (0 = use default 100).
func (g *Graph) Traverse(start, direction string, relKinds, nodeKinds []string, maxDepth, maxNodes int) TraversalResult {
	return g.traverseFrom([]string{start}, direction, relKinds, nodeKinds, maxDepth, maxNodes)
}

// TraverseFrom performs a BFS traversal seeded at every name in starts (see
// Traverse for the parameters). Use with RollupSeeds so a reverse traversal of a
// type also covers its methods and constructor — otherwise callers that reference
// the type only through its methods are missed (the bug impact_analysis avoids).
func (g *Graph) TraverseFrom(starts []string, direction string, relKinds, nodeKinds []string, maxDepth, maxNodes int) TraversalResult {
	return g.traverseFrom(starts, direction, relKinds, nodeKinds, maxDepth, maxNodes)
}

// RollupSeeds returns name plus, when name is a type symbol
// (struct/class/interface/type), its methods (via has_method edges) and its
// constructor (New<Type> in the same package). For any other node it returns just
// name. This is the seed set reverse traversal needs to find everything that
// references the entity through any of its members.
func (g *Graph) RollupSeeds(name string) []string {
	return g.impactSeeds(name)
}

// traverseFrom is the multi-source BFS underlying Traverse. Every name in starts
// is seeded at depth 0 (deduplicated), so a logical entity spread across several
// fact nodes — e.g. a type plus its methods and constructor — can be traversed as
// one origin. Single-source callers pass a one-element slice.
func (g *Graph) traverseFrom(starts []string, direction string, relKinds, nodeKinds []string, maxDepth, maxNodes int) TraversalResult {
	g.mu.RLock()
	defer g.mu.RUnlock()

	if maxDepth <= 0 {
		maxDepth = 5
	}
	if maxDepth > 20 {
		maxDepth = 20
	}
	if maxNodes <= 0 {
		maxNodes = 100
	}
	if maxNodes > 500 {
		maxNodes = 500
	}

	reverse := direction == "reverse"

	relSet := g.relIDSet(relKinds)
	kindSet := toSet(nodeKinds)

	var result TraversalResult
	// Keyed by node ID rather than name: same asymptotics, but hashing a uint32
	// instead of a string, and no key strings retained for the walk's duration.
	visited := make(map[uint32]struct{})

	type queueItem struct {
		id    uint32
		depth int
	}

	// Seed every start node at depth 0. A start name with no node at all still
	// produces a node in the result — an unresolved one — because callers ask about
	// names that may not be in the graph and an empty answer would not say so.
	var queue []queueItem
	for _, start := range starts {
		id, ok := g.lookup(start)
		if !ok {
			result.Nodes = append(result.Nodes, TraversalNode{Name: start, Depth: 0, Unresolved: true})
			continue
		}
		if _, seen := visited[id]; seen {
			continue
		}
		visited[id] = struct{}{}
		queue = append(queue, queueItem{id: id, depth: 0})
		result.Nodes = append(result.Nodes, g.nodeForID(id, 0, ""))
	}

	truncated := false
	maxDepthReached := 0

	// Use an index pointer instead of re-slicing to avoid keeping the full
	// backing array alive for the duration of traversal.
	for qi := 0; qi < len(queue); qi++ {
		item := queue[qi]

		if item.depth >= maxDepth {
			continue
		}

		tgts, rels := g.adjOf(item.id, reverse)
		for i, tid := range tgts {
			relID := rels[i]
			if relSet != nil {
				if _, ok := relSet[relID]; !ok {
					continue
				}
			}
			relKind := g.relName(relID)

			result.Stats.EdgesTraversed++

			// Record the edge
			if reverse {
				result.Edges = append(result.Edges, TraversalEdge{
					Source: g.names[tid],
					Target: g.names[item.id],
					Kind:   relKind,
				})
			} else {
				result.Edges = append(result.Edges, TraversalEdge{
					Source: g.names[item.id],
					Target: g.names[tid],
					Kind:   relKind,
				})
			}

			if _, seen := visited[tid]; seen {
				continue
			}
			visited[tid] = struct{}{}

			newDepth := item.depth + 1
			if newDepth > maxDepthReached {
				maxDepthReached = newDepth
			}

			node := g.nodeForID(tid, newDepth, relKind)

			// Apply node kind filter
			if kindSet != nil {
				if _, ok := kindSet[node.Kind]; !ok {
					// Still traverse through this node but don't include it in results
					queue = append(queue, queueItem{id: tid, depth: newDepth})
					continue
				}
			}

			if len(result.Nodes) >= maxNodes {
				// Still traverse through this node but don't include it in
				// results — same contract as the node-kind filter above.
				// maxNodes bounds the returned set, not the walk: dropping the
				// node from the queue would hide everything reachable only
				// through it and make NodesVisited/MaxDepthReached describe the
				// truncated walk rather than the graph. Nodes already appended
				// are unaffected, so the returned set stays the BFS-order prefix
				// of an uncapped traversal.
				truncated = true
				queue = append(queue, queueItem{id: tid, depth: newDepth})
				continue
			}

			result.Nodes = append(result.Nodes, node)
			queue = append(queue, queueItem{id: tid, depth: newDepth})
		}
	}

	result.Stats.NodesVisited = len(visited)
	result.Stats.MaxDepthReached = maxDepthReached
	result.Stats.Truncated = truncated

	// Edges are recorded for every relation walked, but the max_nodes cap and the
	// node-kind filter can exclude some destinations from result.Nodes. Drop edges
	// that reference an excluded node so the returned graph is self-consistent
	// (every edge endpoint appears in Nodes). Only needed when something was
	// excluded; otherwise every visited node is already in Nodes.
	if truncated || kindSet != nil {
		inSet := make(map[string]bool, len(result.Nodes))
		for _, n := range result.Nodes {
			inSet[n.Name] = true
		}
		kept := result.Edges[:0]
		for _, e := range result.Edges {
			if inSet[e.Source] && inSet[e.Target] {
				kept = append(kept, e)
			}
		}
		result.Edges = kept
	}

	return result
}

// FindPath finds the shortest path between two nodes using BFS.
// relKinds filters to specific relation types (nil = all).
// maxDepth limits search depth (0 = use default 10).
func (g *Graph) FindPath(from, to string, relKinds []string, maxDepth int) PathResult {
	g.mu.RLock()
	defer g.mu.RUnlock()

	if maxDepth <= 0 {
		maxDepth = 10
	}
	if maxDepth > 20 {
		maxDepth = 20
	}

	// Trivial case: same node
	if from == to {
		return PathResult{
			From:  from,
			To:    to,
			Found: true,
			Path:  []TraversalNode{g.nodeFor(from, 0, "")},
		}
	}

	relSet := g.relIDSet(relKinds)

	fromID, fromOK := g.lookup(from)
	toID, toOK := g.lookup(to)
	if !fromOK || !toOK {
		// One of the endpoints is not in the graph at all, so no path can exist.
		return PathResult{From: from, To: to, Found: false}
	}

	type queueItem struct {
		id    uint32
		depth int
	}

	visited := make(map[uint32]struct{})
	parent := make(map[uint32]uint32)    // child → parent
	parentRel := make(map[uint32]uint16) // child → relation kind of the edge from parent

	visited[fromID] = struct{}{}
	queue := []queueItem{{id: fromID, depth: 0}}

	found := false
	// Use an index pointer to avoid keeping the full backing array alive.
	for qi := 0; qi < len(queue) && !found; qi++ {
		item := queue[qi]

		if item.depth >= maxDepth {
			continue
		}

		tgts, rels := g.adjOf(item.id, false)
		for i, tid := range tgts {
			if relSet != nil {
				if _, ok := relSet[rels[i]]; !ok {
					continue
				}
			}
			if _, seen := visited[tid]; seen {
				continue
			}
			visited[tid] = struct{}{}
			parent[tid] = item.id
			parentRel[tid] = rels[i]

			if tid == toID {
				found = true
				break
			}
			queue = append(queue, queueItem{id: tid, depth: item.depth + 1})
		}
	}

	result := PathResult{From: from, To: to, Found: found}
	if !found {
		return result
	}

	// Reconstruct path
	var path []uint32
	for cur := toID; cur != fromID; cur = parent[cur] {
		path = append(path, cur)
	}
	path = append(path, fromID)

	// Reverse to get from → to order
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}

	for i, id := range path {
		// The origin has no incoming edge; every later hop is identified by the
		// relation that reached it.
		via := ""
		if i > 0 {
			via = g.relName(parentRel[id])
		}
		result.Path = append(result.Path, g.nodeForID(id, i, via))
	}

	// Reconstruct edges along the path
	for i := 1; i < len(path); i++ {
		result.Edges = append(result.Edges, TraversalEdge{
			Source: g.names[path[i-1]],
			Target: g.names[path[i]],
			Kind:   g.relName(parentRel[path[i]]),
		})
	}

	return result
}

// ImpactSet computes the transitive set of nodes affected by changing the target.
// It performs a reverse BFS and groups results by depth.
// If includeForward is true, it also includes what the target depends on.
func (g *Graph) ImpactSet(target string, maxDepth, maxNodes int, includeForward bool) ImpactResult {
	if maxDepth <= 0 {
		maxDepth = 3
	}
	if maxDepth > 10 {
		maxDepth = 10
	}
	if maxNodes <= 0 {
		maxNodes = 200
	}
	if maxNodes > 500 {
		maxNodes = 500
	}

	// Reverse traversal: who depends on target? When the target is a type, seed
	// from its methods (via has_method) and constructor too, so callers that
	// reference the type through those — not the bare type node — are included.
	seeds := g.impactSeeds(target)
	rev := g.traverseFrom(seeds, "reverse", nil, nil, maxDepth, maxNodes)

	// max_nodes caps rev.Nodes, so len(rev.Nodes) is not the dependent count.
	// (The cap bounds the returned set only — the BFS itself walks the full
	// reachable set, so rev.Stats.NodesVisited/MaxDepthReached do describe the
	// graph.) Count with a cheap count-only pass that excludes the seeds
	// themselves, so the summary is accurate even when the display is truncated.
	totalDependents := g.reachableCount(seeds, "reverse", maxDepth)

	result := ImpactResult{
		Target:          target,
		ByDepth:         make(map[int][]TraversalNode),
		Edges:           rev.Edges,
		TotalDependents: totalDependents,
		Stats:           rev.Stats,
	}

	// Bucket nodes by depth (skip depth 0, which holds the target entity's own
	// seed nodes) and roll up which other repos contain a dependent.
	targetRepo := g.repoOf(target)
	repoSet := map[string]bool{}
	for _, n := range rev.Nodes {
		if n.Depth > 0 {
			result.ByDepth[n.Depth] = append(result.ByDepth[n.Depth], n)
			if n.Repo != "" && n.Repo != targetRepo {
				repoSet[n.Repo] = true
			}
		}
	}
	if len(repoSet) > 0 {
		repos := make([]string, 0, len(repoSet))
		for r := range repoSet {
			repos = append(repos, r)
		}
		sort.Strings(repos)
		result.CrossRepoImpact = repos
	}

	// Build summary
	result.Summary = g.buildImpactSummary(result.ByDepth, totalDependents)
	if len(result.CrossRepoImpact) > 0 {
		result.Summary += " — spans repos: " + strings.Join(result.CrossRepoImpact, ", ")
	}

	result.GoverningIntent = g.GoverningIntent(target)
	if n := len(result.GoverningIntent); n > 0 {
		result.Summary += fmt.Sprintf(" — governed by %d intent page(s)", n)
	}

	// Optionally include forward dependencies
	if includeForward {
		fwd := g.Traverse(target, "forward", nil, nil, maxDepth, maxNodes)
		result.Forward = &fwd
	}

	return result
}

// GoverningIntent reports the knowledge pages whose declared anchors cover
// the file the named fact lives in — exactly (a file anchor) or as a
// directory prefix. This is the reverse of the anchor declaration: the page
// pins itself to code, and the traversal answers "which decisions govern
// this code?" for any node under analysis. Page type and status join from
// the page's own declaration when it carries one.
func (g *Graph) GoverningIntent(target string) []GoverningPage {
	g.mu.RLock()
	defer g.mu.RUnlock()
	idx, ok := g.factIndexFor(target, "")
	if !ok {
		return nil
	}
	f := g.facts[idx]
	if f.File == "" || f.Repo == "" {
		return nil
	}
	return g.governingForFile(f.Repo, f.File)
}

// GoverningIntentForFile answers the same reverse query for a file named
// directly — repo label plus the file in either the label-prefixed or
// repo-relative form — without resolving a fact first.
func (g *Graph) GoverningIntentForFile(repo, file string) []GoverningPage {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if repo == "" || file == "" {
		return nil
	}
	return g.governingForFile(repo, file)
}

// governingForFile joins the file against every declared anchor of its repo
// and resolves the matching pages with their declarations and outgoing
// relations. Caller holds g.mu.
func (g *Graph) governingForFile(repo, file string) []GoverningPage {
	// Join in both the label-prefixed and repo-relative file forms, same as
	// every intent join — a single trimmed form mis-fires when a real path
	// starts with the repo's own name.
	forms := []string{file}
	if trimmed := strings.TrimPrefix(file, repo+"/"); trimmed != file {
		forms = append(forms, trimmed)
	}
	pages := map[string]bool{}
	pageInfo := map[string]*Fact{}
	pageByForm := map[string]*Fact{}
	relations := map[string][]*Fact{}
	for i := range g.facts {
		af := &g.facts[i]
		if af.Kind != KindIntent {
			continue
		}
		switch af.PropString("intent_kind") {
		case "anchor":
			if af.PropString("intent_owner") != repo {
				continue
			}
			path := af.PropString("path")
			if path == "" {
				continue
			}
			for _, form := range forms {
				if path == form || strings.HasPrefix(form, path+"/") {
					pages[af.File] = true
					break
				}
			}
		case "page":
			pageInfo[af.File] = af
			pageByForm[af.Repo+"\x00"+af.File] = af
			pageByForm[af.Repo+"\x00"+strings.TrimPrefix(af.File, af.Repo+"/")] = af
		case "relation":
			relations[af.Repo+"\x00"+af.File] = append(relations[af.Repo+"\x00"+af.File], af)
		}
	}
	if len(pages) == 0 {
		return nil
	}
	out := make([]GoverningPage, 0, len(pages))
	for page := range pages {
		gp := GoverningPage{Page: page}
		var pageRepo string
		if pf, ok := pageInfo[page]; ok {
			gp.Type = pf.PropString("page_type")
			gp.Status = pf.PropString("status")
			pageRepo = pf.Repo
		}
		for _, rf := range relations[pageRepo+"\x00"+page] {
			gr := GoverningRelation{Rel: rf.PropString("rel"), To: rf.PropString("to")}
			if tf, ok := pageByForm[rf.Repo+"\x00"+gr.To]; ok {
				gr.ToType = tf.PropString("page_type")
				gr.ToStatus = tf.PropString("status")
			}
			gp.Relations = append(gp.Relations, gr)
		}
		sort.Slice(gp.Relations, func(i, j int) bool {
			if gp.Relations[i].Rel != gp.Relations[j].Rel {
				return gp.Relations[i].Rel < gp.Relations[j].Rel
			}
			return gp.Relations[i].To < gp.Relations[j].To
		})
		out = append(out, gp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Page < out[j].Page })
	return out
}

// AnchorCoverage is one declared anchor of a page joined against the
// measured graph — the forward half of the reverse query: what code does
// this page govern, and does the graph actually measure it.
type AnchorCoverage struct {
	Repo          string `json:"repo"`
	Path          string `json:"path"`
	MeasuredFiles int    `json:"measured_files"`
	MeasuredFacts int    `json:"measured_facts"`
}

// GovernedByPage resolves a compiled page — by label-prefixed or
// repo-relative path — and reports every anchor it declares with the
// measured coverage under it. The bool reports whether the page compiles at
// all, so a caller can keep "no such page" and "a page anchored to nothing"
// apart.
func (g *Graph) GovernedByPage(page string) ([]AnchorCoverage, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	var pageFile string
	for i := range g.facts {
		pf := &g.facts[i]
		if pf.Kind != KindIntent || pf.PropString("intent_kind") != "page" {
			continue
		}
		if pf.File == page || strings.TrimPrefix(pf.File, pf.Repo+"/") == page {
			pageFile = pf.File
			break
		}
	}
	if pageFile == "" {
		return nil, false
	}
	var anchors []*Fact
	for i := range g.facts {
		af := &g.facts[i]
		if af.Kind == KindIntent && af.PropString("intent_kind") == "anchor" && af.File == pageFile {
			anchors = append(anchors, af)
		}
	}
	out := make([]AnchorCoverage, len(anchors))
	files := make([]map[string]bool, len(anchors))
	for i, af := range anchors {
		out[i] = AnchorCoverage{Repo: af.PropString("intent_owner"), Path: af.PropString("path")}
		files[i] = map[string]bool{}
	}
	for i := range g.facts {
		mf := &g.facts[i]
		if mf.Kind == KindIntent || mf.Repo == "" || mf.File == "" {
			continue
		}
		forms := []string{mf.File}
		if trimmed := strings.TrimPrefix(mf.File, mf.Repo+"/"); trimmed != mf.File {
			forms = append(forms, trimmed)
		}
		for a := range out {
			if out[a].Repo != mf.Repo {
				continue
			}
			for _, form := range forms {
				if out[a].Path == form || strings.HasPrefix(form, out[a].Path+"/") {
					out[a].MeasuredFacts++
					files[a][mf.File] = true
					break
				}
			}
		}
	}
	for a := range out {
		out[a].MeasuredFiles = len(files[a])
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		return out[i].Path < out[j].Path
	})
	return out, true
}

// HasCompiledPages reports whether any knowledge page compiles into this
// graph at all — the counterparty question a reverse-query surface must
// answer before rendering an empty result, so "no knowledge layer loaded"
// and "asked, nothing governs this file" never look the same.
func (g *Graph) HasCompiledPages() bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	for i := range g.facts {
		if g.facts[i].Kind == KindIntent && g.facts[i].PropString("intent_kind") == "page" {
			return true
		}
	}
	return false
}

// repoOf returns the repo label of the fact named name, or "" if absent.
func (g *Graph) repoOf(name string) string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if idx, ok := g.factIndexFor(name, ""); ok {
		return g.facts[idx].Repo
	}
	return ""
}

// impactSeeds returns the set of fact names that together represent the target
// entity for impact analysis. For a type symbol (struct/class/interface/type)
// this is the type plus its methods (via has_method edges) and its constructor
// (the New<Type> function in the same package, when present), so reverse
// traversal finds callers that reference the type through any of them. For any
// other target it is just the target itself.
func (g *Graph) impactSeeds(target string) []string {
	g.mu.RLock()
	defer g.mu.RUnlock()

	seeds := []string{target}
	// Seed expansion only applies to type symbols, so resolve with symbol context:
	// a type sharing a name with a module or service must still expand its methods.
	// When no symbol declares the name the kind check below returns the bare target.
	idx, ok := g.factIndexFor(target, RelHasMethod)
	if !ok {
		return seeds
	}
	if g.facts[idx].Kind != KindSymbol {
		return seeds
	}
	switch sk, _ := g.facts[idx].PropAny("symbol_kind").(string); sk {
	case SymbolStruct, SymbolClass, SymbolInterface, SymbolType:
	default:
		return seeds
	}

	// Methods: has_method edges point from the type to each of its methods.
	if id, ok := g.lookup(target); ok {
		tgts, rels := g.adjOf(id, false)
		for i, tid := range tgts {
			if g.relName(rels[i]) == RelHasMethod {
				seeds = append(seeds, g.names[tid])
			}
		}
	}
	// Constructor: "<pkg>.New<Type>" in the same package, when it exists.
	if dot := strings.LastIndex(target, "."); dot >= 0 {
		ctor := target[:dot+1] + "New" + target[dot+1:]
		if g.declares(ctor) {
			seeds = append(seeds, ctor)
		}
	}
	return seeds
}

// normalizeExternalTarget strips a known Go module path prefix from a call
// target that doesn't match any fact. This bridges cross-repo call edges where
// the consumer emits the full import path (e.g. "github.com/x/go-auth/adapters.Handler.Login")
// but the provider's facts use the repo-relative path (e.g. "adapters.Handler.Login").
//
//	subpackage: "github.com/x/go-auth/adapters.Handler.Login" → "adapters.Handler.Login"
//	root pkg:   "github.com/x/go-auth.SecurityHeaders"        → "..SecurityHeaders"
func normalizeExternalTarget(target string, modulePaths map[string]struct{}) string {
	for modulePath := range modulePaths {
		if !strings.HasPrefix(target, modulePath) {
			continue
		}
		after := target[len(modulePath):]
		switch {
		case strings.HasPrefix(after, "/"):
			return after[1:] // subpackage: strip leading "/"
		case strings.HasPrefix(after, "."):
			return "." + after // root pkg: ".Sym" → "..Sym" (pkgDir="." naming)
		}
	}
	return ""
}

// resolveToModule finds the closest matching module for a target by trying
// the target itself, then walking up parent directories until a match is found.
func resolveToModule(target string, moduleNames map[string]bool) string {
	cur := target
	for {
		if moduleNames[cur] {
			return cur
		}
		parent := fileDirectory(cur)
		if parent == cur || parent == "." {
			break
		}
		cur = parent
	}
	return ""
}

func fileDirectory(file string) string {
	if i := strings.LastIndex(file, "/"); i >= 0 {
		return file[:i]
	}
	return "."
}

// ReverseFacts returns all facts that have a relation targeting targetName.
// When relKind is non-empty only edges of that kind are considered.
// It uses the reverse adjacency index (O(1) lookup) instead of scanning all facts.
func (g *Graph) ReverseFacts(targetName, relKind string) []Fact {
	g.mu.RLock()
	defer g.mu.RUnlock()

	id, ok := g.lookup(targetName)
	if !ok {
		return nil
	}
	srcs, rels := g.adjOf(id, true)
	if len(srcs) == 0 {
		return nil
	}

	result := make([]Fact, 0, len(srcs))
	seen := make(map[uint32]struct{}, len(srcs))
	for i, sid := range srcs {
		rk := g.relName(rels[i])
		if relKind != "" && rk != relKind {
			continue
		}
		if _, already := seen[sid]; already {
			continue
		}
		seen[sid] = struct{}{}
		if idx, ok := g.factIndexForID(sid, rk); ok {
			result = append(result, g.facts[idx])
		}
	}
	return result
}

// ForwardEdges returns the outgoing edges of name as a fresh slice, or nil when the
// name has no node. Materializing one node's edges on demand is what replaced
// Forward(), which handed out the whole adjacency map — a shape that cannot exist
// once the adjacency is CSR, and that callers only ever indexed by name anyway.
func (g *Graph) ForwardEdges(name string) []Edge {
	g.mu.RLock()
	defer g.mu.RUnlock()
	id, ok := g.lookup(name)
	if !ok {
		return nil
	}
	return g.edgesOf(id, false)
}

// ReverseEdges returns the incoming edges of name as a fresh slice — each Edge's
// Target holds the SOURCE of that edge, matching the reverse index's convention.
// Unlike ArchitecturalReverseEdges it applies no coupling filter, so it is what
// traversal-style questions want: every reference, including from tests and routes.
func (g *Graph) ReverseEdges(name string) []Edge {
	g.mu.RLock()
	defer g.mu.RUnlock()
	id, ok := g.lookup(name)
	if !ok {
		return nil
	}
	return g.edgesOf(id, true)
}

// FanOut returns the number of outgoing edges of name, without materializing them.
func (g *Graph) FanOut(name string) int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	id, ok := g.lookup(name)
	if !ok {
		return 0
	}
	return g.degreeOf(id, false)
}

// edgesOf materializes node id's edges in the given direction. Callers hold the lock.
func (g *Graph) edgesOf(id uint32, reverse bool) []Edge {
	tgts, rels := g.adjOf(id, reverse)
	if len(tgts) == 0 {
		return nil
	}
	out := make([]Edge, len(tgts))
	for i, tid := range tgts {
		out[i] = Edge{RelKind: g.relName(rels[i]), Target: g.names[tid]}
	}
	return out
}

// isReferenceOnlyKind reports whether a fact kind carries only reference
// (RelCalls) edges into production code — test_ref and file_ref. They exist so
// the dead-code detector can see a production symbol is used from a test/spec or
// a file-scope block; by contract "no other explainer is affected"
// (pkg/plugin/plugin.go). They are not part of the architectural coupling graph,
// so counting them as dependents inflates god-class fan-in and hotspots
// centrality and drifts the outlier threshold (GAP-XL-15).
// A route joins them at v111. bindHTTPHandlers gives every HTTP route a handled_by
// edge into the symbol that serves it — which is exactly what impact_analysis, orphans
// and the performance analyzer need — but it also made a route a SOURCE of fan-in. On
// fairwayhub/golf that added an edge to each of 1254 handlers at once, and 13 were
// immediately reported as new "call-graph hotspots" whose fan-in was nothing but routes
// and a test file. A handler ranked as a change-risk concentrator purely for being a
// handler is a fabricated finding, and the uniform +1 also drifts the outlier threshold.
//
// A route is an entry-point declaration, not a symbol, so it is not architectural
// coupling — the same reasoning that excludes test_ref and file_ref.
func isReferenceOnlyKind(kind string) bool {
	return kind == KindTestRef || kind == KindFileRef || kind == KindRoute
}

// ArchitecturalReverse returns a reverse adjacency map restricted to edges that
// represent architectural coupling — excluding reference-only SOURCE kinds
// (test_ref/file_ref/route) and RelInstantiates edges (struct construction /
// field-type usage). The outlier explainers (god-class, hotspots) use this
// instead of Reverse() so their fan-in/centrality and the distribution they
// threshold over count only real symbol coupling. orphans, impact_analysis,
// traverse and find_path keep using the unfiltered Reverse() index — they
// intentionally surface those references. (GAP-XL-15)
// It is answered per node rather than as a whole map. The map form built a filtered
// copy of the ENTIRE reverse index on every call — two full copies per snapshot, since
// both outlier explainers ask — to read the length of a few thousand buckets.
func (g *Graph) ArchitecturalReverseEdges(name string) []Edge {
	g.mu.RLock()
	defer g.mu.RUnlock()
	id, ok := g.lookup(name)
	if !ok {
		return nil
	}
	srcs, rels := g.adjOf(id, true)
	var out []Edge
	for i, sid := range srcs {
		if !g.isArchitecturalEdge(sid, rels[i]) {
			continue
		}
		out = append(out, Edge{RelKind: g.relName(rels[i]), Target: g.names[sid]})
	}
	return out
}

// ArchitecturalFanIn counts the incoming architectural-coupling edges of name — the
// same filter as ArchitecturalReverseEdges, without building the slice.
func (g *Graph) ArchitecturalFanIn(name string) int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	id, ok := g.lookup(name)
	if !ok {
		return 0
	}
	srcs, rels := g.adjOf(id, true)
	n := 0
	for i, sid := range srcs {
		if g.isArchitecturalEdge(sid, rels[i]) {
			n++
		}
	}
	return n
}

// ArchitecturalForwardEdges returns the outgoing edges of name restricted to what
// the symbol reaches OUT to — the forward counterpart of ArchitecturalReverseEdges,
// and what an explainer wants when it means "the things this symbol depends on".
//
// It drops RelHasMethod, because containment in either direction is not outgoing
// coupling: a type does not depend on its methods, it contains them, and their
// outgoing calls are already counted as the methods' own fan-out. Orientation is
// not something this filter may assume. NewGraph synthesizes the edge owner -> method,
// but grpcextractor emits it the other way, from an RPC method OUT to its service
// (grpc.go), and providers.go admits the kind in whichever orientation a provider
// sends. Counting containment as outgoing coupling makes raw out-degree a size
// measure, which is the god-class signal rather than the pinch-point one — on a large
// Rails monolith it puts an importer base class (449 lines, 102 one-line delegations,
// exactly ONE call out) at out-degree 104.
//
// Nothing else is filtered. The reverse filter's other two arms cannot fire here:
// a symbol never points at a reference-only fact (test_ref/file_ref/route), and
// RelInstantiates in this direction is the symbol really constructing another type.
func (g *Graph) ArchitecturalForwardEdges(name string) []Edge {
	g.mu.RLock()
	defer g.mu.RUnlock()
	id, ok := g.lookup(name)
	if !ok {
		return nil
	}
	tgts, rels := g.adjOf(id, false)
	var out []Edge
	for i, tid := range tgts {
		if !g.isArchitecturalForwardEdge(rels[i]) {
			continue
		}
		out = append(out, Edge{RelKind: g.relName(rels[i]), Target: g.names[tid]})
	}
	return out
}

// ArchitecturalFanOut counts the outgoing architectural-coupling edges of name — the
// same filter as ArchitecturalForwardEdges, without building the slice.
func (g *Graph) ArchitecturalFanOut(name string) int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	id, ok := g.lookup(name)
	if !ok {
		return 0
	}
	_, rels := g.adjOf(id, false)
	n := 0
	for _, rel := range rels {
		if g.isArchitecturalForwardEdge(rel) {
			n++
		}
	}
	return n
}

// isArchitecturalForwardEdge applies the coupling filter to one outgoing edge, given
// its relation kind. Callers hold the lock.
func (g *Graph) isArchitecturalForwardEdge(relID uint16) bool {
	return g.relName(relID) != RelHasMethod
}

// isArchitecturalEdge applies the coupling filter to one incoming edge, given its
// SOURCE node and relation kind. Callers hold the lock.
func (g *Graph) isArchitecturalEdge(srcID uint32, relID uint16) bool {
	rk := g.relName(relID)
	// RelInstantiates keeps ubiquitous DATA structs out of the dead-code report, but
	// it is not change-risk coupling: a data struct built at many sites is not a god
	// class or a call-graph hotspot. Exclude it from fan-in / centrality (and the
	// outlier distribution). Traversal, impact_analysis, find_path and orphans read
	// the unfiltered index and still see it.
	if rk == RelInstantiates || rk == RelImplementedBy {
		return false
	}
	if idx, ok := g.factIndexForID(srcID, rk); ok && isReferenceOnlyKind(g.facts[idx].Kind) {
		return false
	}
	return true
}

// NodeCount returns the number of unique nodes in the graph.
//
// "Unique nodes" means names some fact DECLARES, which is what the fact-index map it
// used to measure contained. Dangling edge targets have IDs too, but they are not
// nodes the graph knows anything about.
func (g *Graph) NodeCount() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return int(g.declaredNodes)
}

// EdgeCount returns the total number of edges in the graph.
func (g *Graph) EdgeCount() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.fwdTgt)
}

// kindForRel maps the relation that reached a node to the fact kind that relation
// naturally targets. A node's name alone cannot identify it when several facts share
// that name — module names are repo-root-relative, so any top-level directory whose
// name matches a repo label collides with that repo's service node — but the edge used
// to arrive says which one was meant: only a service is the target of depends_on, only
// a module of imports.
var kindForRel = map[string]string{
	RelDependsOn:     KindService,
	RelImports:       KindModule,
	RelCalls:         KindSymbol,
	RelImplements:    KindSymbol,
	RelHasMethod:     KindSymbol,
	RelInstantiates:  KindSymbol,
	RelInjects:       KindSymbol,
	RelDeclares:      KindSymbol,
	RelHandledBy:     KindRoute,
	RelImplementedBy: KindSymbol,
}

// kindRank orders fact kinds for picking among same-named facts when there is no edge
// context — a traversal origin, or a relation whose natural kind is absent. Lower wins.
// Services rank first because they are synthetic whole-repo nodes: if a name is both a
// repo label and something else, the repo is the more meaningful node in the graph
// queries (traverse/find_path/impact) that reach it.
var kindRank = map[string]int{
	KindService:    0,
	KindModule:     1,
	KindSymbol:     2,
	KindRoute:      3,
	KindStorage:    4,
	KindDependency: 5,
}

func rankOf(kind string) int {
	if r, ok := kindRank[kind]; ok {
		return r
	}
	return len(kindRank) // unknown kinds sort last, deterministically
}

// factIndexFor picks which of the facts named name represents the node, preferring the
// kind natural for viaRel (the relation that reached the node; "" when there is none)
// and otherwise falling back to kindRank. Returns false when no fact backs the name.
func (g *Graph) factIndexFor(name, viaRel string) (int, bool) {
	id, ok := g.lookup(name)
	if !ok {
		return 0, false
	}
	return g.factIndexForID(id, viaRel)
}

// factIndexForID is factIndexFor for a node whose ID is already known — every caller
// on a traversal path, which reaches nodes by edge rather than by name.
func (g *Graph) factIndexForID(id uint32, viaRel string) (int, bool) {
	idxs := g.factIdxsOf(id)
	if len(idxs) == 0 {
		return 0, false
	}
	if len(idxs) == 1 {
		if int(idxs[0]) >= len(g.facts) {
			return 0, false
		}
		return int(idxs[0]), true
	}

	want := kindForRel[viaRel]
	best, found := 0, false
	for _, i32 := range idxs {
		idx := int(i32)
		if idx >= len(g.facts) {
			continue
		}
		if want != "" && g.facts[idx].Kind == want {
			return idx, true // exact edge-context match: nothing can beat it
		}
		if !found || rankOf(g.facts[idx].Kind) < rankOf(g.facts[best].Kind) {
			best, found = idx, true
		}
	}
	return best, found
}

// conflatedKinds returns the distinct kinds sharing name, sorted, when more than one
// fact declares it — the honest signal that this graph node merges several facts and
// their edges. Returns nil for the common single-fact case.
func (g *Graph) conflatedKindsOf(id uint32) []string {
	idxs := g.factIdxsOf(id)
	if len(idxs) < 2 {
		return nil
	}
	seen := map[string]bool{}
	for _, idx := range idxs {
		if int(idx) < len(g.facts) {
			seen[g.facts[idx].Kind] = true
		}
	}
	if len(seen) < 2 {
		return nil // several facts, but all the same kind — not a cross-kind conflation
	}
	kinds := make([]string, 0, len(seen))
	for k := range seen {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	return kinds
}

// nodeFor builds the traversal node for name. viaRel is the relation kind that reached
// it, or "" for a traversal/path origin; it disambiguates which fact the node means
// when a name is shared.
func (g *Graph) nodeFor(name string, depth int, viaRel string) TraversalNode {
	if id, ok := g.lookup(name); ok {
		return g.nodeForID(id, depth, viaRel)
	}
	return TraversalNode{Name: name, Depth: depth, Unresolved: true}
}

// nodeForID is nodeFor for a node reached by edge, where the ID is already in hand.
func (g *Graph) nodeForID(id uint32, depth int, viaRel string) TraversalNode {
	node := TraversalNode{Name: g.names[id], Depth: depth}
	if idx, ok := g.factIndexForID(id, viaRel); ok {
		f := g.facts[idx]
		node.Kind = f.Kind
		node.File = f.File
		node.Line = f.Line
		node.Repo = f.Repo
		node.Conflated = g.conflatedKindsOf(id)
	} else {
		// No backing fact: this is a dangling edge target (e.g. an inferred call
		// into an unanalyzed package or an interface method). Mark it honestly
		// rather than emitting a silent kind-less node.
		node.Unresolved = true
	}
	return node
}

// buildImpactSummary renders the per-depth breakdown of the displayed dependents.
// total is the true dependent count within max_depth; when it exceeds the shown
// count (because the max_nodes cap truncated the display), the summary notes how
// many are shown.
func (g *Graph) buildImpactSummary(byDepth map[int][]TraversalNode, total int) string {
	if total == 0 {
		return "No dependents found."
	}

	shown := 0
	for _, nodes := range byDepth {
		shown += len(nodes)
	}

	summary := ""
	for d := 1; d <= 10; d++ {
		nodes := byDepth[d]
		if len(nodes) == 0 {
			continue
		}
		// Count by kind
		kindCount := make(map[string]int)
		for _, n := range nodes {
			k := n.Kind
			if k == "" {
				k = "unknown"
			}
			kindCount[k]++
		}
		if summary != "" {
			summary += "; "
		}
		summary += "depth " + itoa(d) + ": "
		first := true
		for kind, count := range kindCount {
			if !first {
				summary += ", "
			}
			summary += itoa(count) + " " + kind
			if count > 1 {
				summary += "s"
			}
			first = false
		}
	}

	prefix := itoa(total) + " total dependents"
	if shown < total {
		prefix += " (showing " + itoa(shown) + ")"
	}
	return prefix + " — " + summary
}

// reachableCount counts the distinct nodes reachable from seeds within maxDepth
// (following all relation kinds), excluding the seeds themselves. Unlike
// traverseFrom it materializes nothing and applies no node cap, so it yields the
// true dependent/dependency count even when the displayed set is truncated. It
// terminates because the graph is finite and the BFS is depth-bounded.
func (g *Graph) reachableCount(seeds []string, direction string, maxDepth int) int {
	g.mu.RLock()
	defer g.mu.RUnlock()

	if maxDepth <= 0 {
		maxDepth = 3
	}
	reverse := direction == "reverse"

	visited := make(map[uint32]struct{})
	type queueItem struct {
		id    uint32
		depth int
	}
	var queue []queueItem
	for _, s := range seeds {
		id, ok := g.lookup(s)
		if !ok {
			continue
		}
		if _, seen := visited[id]; !seen {
			visited[id] = struct{}{}
			queue = append(queue, queueItem{id: id, depth: 0})
		}
	}
	seedCount := len(visited)

	for qi := 0; qi < len(queue); qi++ {
		item := queue[qi]
		if item.depth >= maxDepth {
			continue
		}
		tgts, _ := g.adjOf(item.id, reverse)
		for _, tid := range tgts {
			if _, seen := visited[tid]; seen {
				continue
			}
			visited[tid] = struct{}{}
			queue = append(queue, queueItem{id: tid, depth: item.depth + 1})
		}
	}

	return len(visited) - seedCount
}

// relIDSet maps a relation-kind filter to the IDs the adjacency arrays actually hold,
// so the hot loop compares uint16s instead of strings. A kind the graph never saw has
// no ID and is simply absent, which correctly matches nothing. Returns nil for "no
// filter", matching toSet.
func (g *Graph) relIDSet(kinds []string) map[uint16]struct{} {
	if len(kinds) == 0 {
		return nil
	}
	set := make(map[uint16]struct{}, len(kinds))
	for _, k := range kinds {
		if k == "" {
			continue
		}
		if id, ok := g.relIDs[k]; ok {
			set[id] = struct{}{}
		}
	}
	if len(set) == 0 {
		// Every requested kind is absent from this graph. Returning nil would mean
		// "no filter" and match everything, which is the opposite of what was asked.
		return map[uint16]struct{}{}
	}
	return set
}

func toSet(ss []string) map[string]struct{} {
	if len(ss) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(ss))
	for _, s := range ss {
		if s != "" {
			set[s] = struct{}{}
		}
	}
	if len(set) == 0 {
		return nil
	}
	return set
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
