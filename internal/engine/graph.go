package engine

import (
	"os"
	"path/filepath"
	"sort"

	"github.com/enola-labs/enola/internal/facts"
)

// Graph is one published graph, shareable between engines: the immutable bundle a
// generate or a restore published. Nothing reachable from it is ever mutated (append
// builds a fresh store), so any number of engines can serve it at once. A served
// instance keeps one engine per session and shares graphs between them through
// this handle instead of loading the same repositories twice.
type Graph struct {
	b *snapshotBundle
}

// Graph returns the graph this engine currently serves, or nil when none is loaded.
func (e *Engine) Graph() *Graph {
	b := e.current.Load()
	if b == nil || b.snapshot == nil {
		return nil
	}
	return &Graph{b: b}
}

// Adopt makes this engine serve g, published by this engine or another. A nil g
// drops whatever the engine held, so the memory it pinned can be reclaimed once no
// other engine serves it either.
//
// The published bundle is not the only reference an engine keeps: after a build its
// scratch store IS the published store, and the last facts.jsonl write remembers it
// too. Both are let go here, or unloading a graph would free nothing.
func (e *Engine) Adopt(g *Graph) {
	e.mu.Lock()
	e.store = facts.NewStore()
	e.lastFacts = lastFactsWrite{}
	e.mu.Unlock()
	if g == nil {
		e.current.Store(&snapshotBundle{store: facts.NewStore()})
		return
	}
	e.current.Store(g.b)
}

// Key identifies the published graph g is a handle to: two handles to one graph have
// the same key. For a map keyed by graph.
func (g *Graph) Key() any { return g.b }

// Same reports whether g and other are one published graph.
func (g *Graph) Same(other *Graph) bool {
	return g != nil && other != nil && g.b == other.b
}

// RepoPaths returns the absolute paths of the repositories in g, sorted.
func (g *Graph) RepoPaths() []string {
	if g == nil {
		return nil
	}
	var out []string
	for _, p := range g.b.repoPaths {
		out = append(out, p)
	}
	if len(out) == 0 && g.b.snapshot != nil && g.b.snapshot.Meta.RepoPath != "" {
		out = append(out, g.b.snapshot.Meta.RepoPath)
	}
	sort.Strings(out)
	return out
}

// ExtractorVersion is the extraction behaviour g was built by. A graph built by
// another one is never adopted for a request: its facts follow other rules.
func (g *Graph) ExtractorVersion() string {
	if g == nil || g.b.snapshot == nil {
		return ""
	}
	return g.b.snapshot.Meta.ExtractorVersion
}

// SnapshotID is the content fingerprint of g's snapshot, as its snapshot.meta.json
// records it: two graphs with one ID hold the same facts.
func (g *Graph) SnapshotID() string {
	if g == nil || g.b.snapshot == nil {
		return ""
	}
	return g.b.snapshot.Meta.SnapshotID
}

// FactCount is how many facts g holds.
func (g *Graph) FactCount() int {
	if g == nil || g.b.store == nil {
		return 0
	}
	return g.b.store.Count()
}

// EstimatedBytes is a rough measure of the memory g holds, for a budget that has to
// choose which graph to drop: the size of its facts.jsonl in the first repository's
// output directory, times bytesPerJSONLByte. It is an estimate by design: measuring
// the heap a store holds would mean walking it, on every publish.
func (g *Graph) EstimatedBytes(outputDirFor func(string) string) int64 {
	paths := g.RepoPaths()
	if len(paths) == 0 {
		return 0
	}
	var best int64
	for _, p := range paths {
		if info, err := os.Stat(filepath.Join(outputDirFor(p), "facts.jsonl")); err == nil && info.Size() > best {
			best = info.Size()
		}
	}
	return best * bytesPerJSONLByte
}

// bytesPerJSONLByte is the heap a loaded graph holds per byte of its facts.jsonl,
// rounded up. Measured as the heap retained after a restore, on nine repositories
// from 0.5 MB to 1.2 GB of JSONL: 2.5 on the smallest, 1.1 to 1.6 on everything past
// 100 MB (the Linux kernel holds 1.4 GB for 1.16 GB). Rounding up makes a budget
// unload early, never late. The table is in docs/SERVING.md.
const bytesPerJSONLByte = 2

// Current reports whether every repository in g still has the tree g was built from,
// as this engine walks it. A comparison it cannot make (no recorded hashes, another
// ignore set) counts as not current: adopting a graph is only ever a shortcut for
// building one, and a wrong shortcut serves stale facts.
func (e *Engine) Current(g *Graph) bool {
	if g == nil || g.b.snapshot == nil || g.ExtractorVersion() != cacheVersion {
		return false
	}
	paths := g.RepoPaths()
	if len(paths) == 0 {
		return false
	}
	for _, p := range paths {
		meta, ok := g.b.members[p]
		if !ok {
			if len(paths) > 1 {
				return false
			}
			meta = g.b.snapshot.Meta
		}
		d, err := e.DriftFromMeta(p, meta)
		if err != nil || d.Unknown || d.Any() {
			return false
		}
	}
	return true
}
