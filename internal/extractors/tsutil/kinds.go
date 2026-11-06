// Package tsutil holds helpers shared by the tree-sitter-based extractors.
package tsutil

import (
	"sync"
	"unsafe"

	sitter "github.com/enola-labs/enola/internal/treesitter"
)

// KindTable maps a grammar's symbol ids to their names, so a node's kind can be read
// without allocating.
//
// go-tree-sitter's Node.Kind() is `C.GoString(C.ts_node_type(...))`: a fresh Go string
// built from a C string on every single call. The C string it copies is already
// interned — it points into the grammar's static symbol-name table — so the copy buys
// nothing and the allocation is pure loss.
//
// It is not a small loss. Measured on a cold snapshot of dotnet/runtime: 180 M
// allocations and 3.28 GB, 18% of everything the run allocated and 42% of every object
// it created, from this one call. On an isolated AST walk, replacing it halves the
// allocation count outright.
//
// The table is the same strings by a different route: ts_language_symbol_name of the
// node's symbol id. That the two always agree is not obvious from the API — aliased
// nodes resolve their symbol differently — so it is asserted rather than assumed, over
// every node of a real corpus, by each extractor's own kind-table test.
type KindTable struct {
	names []string
}

// KindsFor returns the table for a grammar, building it once per process.
//
// The key is the grammar pointer that tree-sitter's Go bindings expose — the same
// value the caller hands to sitter.NewLanguage — which is a stable address in the
// compiled grammar rather than a per-parse wrapper. Passing the grammar the parser
// was actually configured with is the caller's side of the bargain: a table from one
// grammar read against another's ids returns plausible, wrong kind names, and nothing
// downstream would notice. Extractors that parse with more than one grammar therefore
// carry the table alongside the language rather than in a package-level variable.
func KindsFor(grammar unsafe.Pointer) *KindTable {
	if t, ok := kindCache.Load(grammar); ok {
		return t.(*KindTable)
	}
	lang := sitter.NewLanguage(grammar)
	n := lang.NodeKindCount()
	t := &KindTable{names: make([]string, n)}
	for i := uint32(0); i < n; i++ {
		t.names[i] = lang.NodeKindForId(uint16(i))
	}
	actual, _ := kindCache.LoadOrStore(grammar, t)
	return actual.(*KindTable)
}

var kindCache sync.Map // unsafe.Pointer (grammar) -> *KindTable

// Of returns n's kind, without allocating.
//
// A nil node yields "", which Node.Kind() would panic on. Callers that guard for nil
// keep working unchanged; a caller that did not was already broken, and returning ""
// turns a crash into a miss — the safer failure for an extractor, whose job is to skip
// what it cannot read.
//
// An id past the end of the table falls back to Node.Kind(). That should not happen —
// the table covers every symbol the grammar declares — but a grammar upgraded
// underneath a cached table would otherwise index out of range.
func (t *KindTable) Of(n *sitter.Node) string {
	if n == nil {
		return ""
	}
	id := int(n.KindId())
	if id < len(t.names) {
		return t.names[id]
	}
	return n.Kind()
}
