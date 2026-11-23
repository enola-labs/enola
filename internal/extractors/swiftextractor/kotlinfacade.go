package swiftextractor

import (
	"regexp"

	sitter "github.com/enola-labs/enola/internal/treesitter"

	"github.com/enola-labs/enola/internal/facts"
)

// kotlinFacadeRe matches the name Kotlin gives a file's facade class, which
// Kotlin Multiplatform exports to Swift: `Main_iosKt` for main.ios.kt.
var kotlinFacadeRe = regexp.MustCompile(`^[A-Z][A-Za-z0-9_]*Kt$`)

// noteKotlinFacadeCall records `Main_iosKt.MainViewController()` on the calling
// symbol as facts.PropKotlinFacadeCalls. The walker cannot tell a Kotlin facade
// from a Swift type of the same shape; the kmp-swift binder binds the call only
// through a framework the file imports that exports that facade and member.
func (w *astWalker) noteKotlinFacadeCall(callee *sitter.Node, root, member string) {
	if member == "" || !kotlinFacadeRe.MatchString(root) {
		return
	}
	target := callee.ChildByFieldName("target")
	if target == nil || kindOf(target) != "simple_identifier" || nodeText(target, w.src) != root {
		return
	}
	idx := w.currentOwner()
	if idx < 0 {
		return
	}
	w.out[idx].AddPropStrings(facts.PropKotlinFacadeCalls, root+"."+member)
}
