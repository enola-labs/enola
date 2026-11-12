package tree_sitter

/*
#include <tree_sitter/api.h>
*/
import "C"

import "sync"

// This file is enola's, not upstream's (see ATTRIBUTION.md).
//
// Child and NamedChild return a *Node, and newNode heap-allocates one per call: on the
// linux kernel that was 573M allocations, 58% of every allocation a cold index made,
// each 32 bytes discarded almost at once. The value forms below return the node itself,
// so a walker that hands its children on by pointer to a static call keeps them on its
// stack. A walker must not retain such a pointer past the call it was handed to.

// ChildAt is Child returning the node by value, and false when there is no child i.
func (n *Node) ChildAt(i uint) (Node, bool) {
	c := C.ts_node_child(n._inner, C.uint(i))
	return Node{_inner: c}, c.id != nil
}

// NamedChildAt is NamedChild returning the node by value, and false when there is no
// named child i.
func (n *Node) NamedChildAt(i uint) (Node, bool) {
	c := C.ts_node_named_child(n._inner, C.uint(i))
	return Node{_inner: c}, c.id != nil
}

// nodeSlabSize nodes are allocated at once: 255 nodes of 32 bytes and the counter fill the
// 8 KiB size class exactly. 256 came to 8,200 bytes, which Go rounds up to 9,472, and that
// waste showed as 7% more bytes allocated for the same nodes.
const nodeSlabSize = 255

type nodeSlab struct {
	nodes [nodeSlabSize]Node
	next  int
}

// nodeSlabs hands each goroutine a slab to carve nodes from. A slab is held only between
// Get and Put, so two goroutines never share one; a full slab is dropped, and lives on
// only as long as a node carved from it.
var nodeSlabs = sync.Pool{New: func() any { return new(nodeSlab) }}

// allocNode is how newNode allocates. Every *Node the binding returns came from a heap
// allocation of its own, which on a cold index was most of every allocation the process
// made (80% on airflow, 58% on the linux kernel). From a slab it is one per 255.
//
// A slab is freed once every node carved from it is unreachable, so a node kept alive
// keeps 8 KiB alive with it. That is bounded by the tree: a node is valid only until its
// tree is closed, and every extractor closes its tree when it is done with the file.
func allocNode(inner C.TSNode) *Node {
	s := nodeSlabs.Get().(*nodeSlab)
	if s.next == nodeSlabSize {
		s = new(nodeSlab)
	}
	n := &s.nodes[s.next]
	s.next++
	n._inner = inner
	nodeSlabs.Put(s)
	return n
}
