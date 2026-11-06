package tsutil

import (
	"sync"

	sitter "github.com/enola-labs/enola/internal/treesitter"
)

// queryCache holds compiled queries, keyed by grammar name and pattern. Compiling is
// the expensive half and a query is immutable once built, so one compile serves every
// file. The CURSOR is never shared: it carries per-execution state and extraction runs
// files in parallel.
var queryCache sync.Map // "grammar\x00pattern" -> *sitter.Query (nil when it failed to compile)

// QueryNodes returns the nodes under root that match a single-capture pattern.
//
// It exists for the same reason KindTable does, one level up. go-tree-sitter hands
// Go a heap-allocated object for every node it returns — `newNode` is
// `&Node{_inner: node}` — so a recursive Go walk that inspects every node to find the
// few that matter pays an allocation per node visited, whatever traversal idiom it
// uses. `Node.Child`, `Node.Children(cursor)` and driving a `TreeCursor` directly all
// allocate identically once the node is actually needed; measured on a 92 KB source
// file, Children(cursor) is WORSE than Child.
//
// Matching inside tree-sitter avoids the question: the scan runs in C and only the
// matches cross into Go. On that same file, finding every declaration costs 845
// allocations instead of 32,150, and runs 3.2x faster.
//
// grammar names the language for cache purposes ("cpp", "typescript"): extractors
// build a fresh *sitter.Language per file, so the pointer cannot be the key.
func QueryNodes(grammar string, lang *sitter.Language, pattern string, root *sitter.Node) []*sitter.Node {
	if root == nil || lang == nil {
		return nil
	}
	key := grammar + "\x00" + pattern
	cached, ok := queryCache.Load(key)
	if !ok {
		q, err := sitter.NewQuery(lang, pattern)
		if err != nil {
			q = nil // cache the failure too, so a bad pattern is not recompiled per file
		}
		cached, _ = queryCache.LoadOrStore(key, q)
	}
	q, _ := cached.(*sitter.Query)
	if q == nil {
		return nil
	}
	qc := sitter.NewQueryCursor()
	defer qc.Close()
	var out []*sitter.Node
	matches := qc.Matches(q, root, nil)
	for m := matches.Next(); m != nil; m = matches.Next() {
		for i := range m.Captures {
			n := m.Captures[i].Node
			out = append(out, &n)
		}
	}
	return out
}
