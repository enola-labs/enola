package tree_sitter

/*
#include <tree_sitter/api.h>
*/
import "C"

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
