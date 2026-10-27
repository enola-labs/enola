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

// EndLine is the 1-based last line a node covers, or 0 for a nil node. Tree-sitter's
// end point is one past the last byte, so a node whose text ends with a newline ends
// at column 0 of the following row; that row holds none of it.
func EndLine(node *sitter.Node) int {
	if node == nil {
		return 0
	}
	start, end := node.StartPosition(), node.EndPosition()
	if end.Column == 0 && end.Row > start.Row {
		return int(end.Row)
	}
	return int(end.Row) + 1
}
