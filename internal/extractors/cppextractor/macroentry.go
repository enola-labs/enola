package cppextractor

import (
	sitter "github.com/enola-labs/enola/internal/treesitter"
)

// rootMacro marks a definition whose signature a repository macro writes
// (facts.PropFrameworkRoot).
//
//	#define EXT_ENTRY(NAME, LOADER) void NAME##_init(Loader &LOADER)
//	EXT_ENTRY(demo, loader) { … }
//
// tree-sitter reads the invocation as a function named after the macro. The
// function the preprocessor makes is named by token pasting and reached by that
// pasted name, typically through a symbol lookup in a loader (an extension entry
// point), never by the macro's name, so nothing in the code can reference it. The
// definition is recorded as an entry point instead of dead code; its body's calls
// stay its own. Only a definition with no return type whose name is a macro the
// repository defines qualifies: a real function always states its return type.
const rootMacro = "macro"

// isMacroWrittenDefinition reports whether a function definition is a repository
// macro invocation with a body.
func (w *astWalker) isMacroWrittenDefinition(node, nameNode *sitter.Node) bool {
	if w.macros == nil || node.ChildByFieldName("type") != nil || kindOf(w.kinds, nameNode) != "identifier" {
		return false
	}
	_, ok := w.macros[nodeText(nameNode, w.src)]
	return ok
}
