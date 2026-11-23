package swiftextractor

import (
	"strings"

	sitter "github.com/enola-labs/enola/internal/treesitter"
)

// A Swift declaration is exported when code outside its module can name it: it is
// `public` or `open`, and so is every type it is declared in. The default access is
// `internal`, visible only inside the module, and `package` stays inside the
// package; neither is exported. A member of an extension takes the extension's
// access when it states none (`public extension Foo { func a() }`); the extended
// type is usually declared in another file, so an extension's member is judged by
// what it and the extension state.

// swiftContainer is a type or extension whose body is being walked.
type swiftContainer struct {
	exported      bool   // a type: whether it is exported; an extension: true
	memberDefault string // the access a member that states none gets
}

// accessLevel returns the access a declaration's modifiers state (`public`, `open`,
// `package`, `internal`, `fileprivate`, `private`), or "" when they state none. A
// setter's access (`private(set)`) is not the declaration's.
func accessLevel(modifiers *sitter.Node, src []byte) string {
	if modifiers == nil {
		return ""
	}
	for i := uint(0); i < modifiers.ChildCount(); i++ {
		c := modifiers.Child(i)
		if kindOf(c) == "attribute" {
			continue
		}
		text := strings.TrimSpace(nodeText(c, src))
		if strings.Contains(text, "(") {
			continue // private(set), fileprivate(set), public(set)
		}
		switch text {
		case "public", "open", "package", "internal", "fileprivate", "private":
			return text
		}
	}
	return ""
}

// exportedFor reports whether a declaration with these modifiers, in the containers
// being walked, is exported.
func (w *astWalker) exportedFor(modifiers *sitter.Node) bool {
	level := accessLevel(modifiers, w.src)
	if level == "" && len(w.containers) > 0 {
		level = w.containers[len(w.containers)-1].memberDefault
	}
	if level != "public" && level != "open" {
		return false
	}
	for _, c := range w.containers {
		if !c.exported {
			return false
		}
	}
	return true
}

func (w *astWalker) pushContainer(c swiftContainer) { w.containers = append(w.containers, c) }
func (w *astWalker) popContainer()                  { w.containers = w.containers[:len(w.containers)-1] }
