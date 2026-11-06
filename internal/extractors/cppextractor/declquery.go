package cppextractor

import (
	sitter "github.com/enola-labs/enola/internal/treesitter"
	c "github.com/tree-sitter/tree-sitter-c/bindings/go"
	cpp "github.com/tree-sitter/tree-sitter-cpp/bindings/go"

	"github.com/enola-labs/enola/internal/extractors/tsutil"
)

// queryDeclarations returns the declaration nodes inside body, matched by
// tree-sitter rather than by a Go walk of the subtree. See tsutil.QueryNodes for
// why the walk it replaced was expensive.
func queryDeclarations(lang string, body *sitter.Node) []*sitter.Node {
	grammar := cpp.Language()
	if lang == langC {
		grammar = c.Language()
	}
	return tsutil.QueryNodes(lang, sitter.NewLanguage(grammar), "(declaration) @d", body)
}
