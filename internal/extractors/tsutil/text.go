package tsutil

import sitter "github.com/tree-sitter/go-tree-sitter"

// Text returns the source text a node covers, or "" for a nil node.
//
// Optional children (ChildByFieldName, a missing name) come back nil, so every
// extractor needs the guard. Keeping one copy here stops the copies drifting: the
// TypeScript extractor's own version once lacked it and panicked on a nil child.
func Text(node *sitter.Node, src []byte) string {
	if node == nil {
		return ""
	}
	return string(src[node.StartByte():node.EndByte()])
}
