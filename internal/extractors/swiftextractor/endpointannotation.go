package swiftextractor

import (
	"regexp"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// swiftEndpointAnnotation matches a `// @endpoint VERB /path` comment, the form a
// codebase writes above each request builder to say which backend route it calls.
var swiftEndpointAnnotation = regexp.MustCompile(`^\s*//+\s*@endpoint\s+([A-Z]+)\s+(/\S*)`)

// swiftDeclLine matches a line that declares a function, initializer or property.
var swiftDeclLine = regexp.MustCompile(`^\s*(?:@\w+\s+)*(?:(?:public|private|fileprivate|internal|static|class|override|final|mutating)\s+)*(?:func|init|var|let)\b`)

// applyEndpointAnnotations replaces the path of a client route that holds an
// unresolved interpolation with the path its builder's `// @endpoint` comment
// states. `"\(endpoint)/order"` builds on a base defined elsewhere, so the path
// read from the literal is `{}/order` and matches nothing; the comment above the
// builder names the whole route.
//
// A comment governs the routes inside the one declaration that follows it. A
// route whose path resolved on its own keeps it.
func applyEndpointAnnotations(src []byte, clients []facts.Fact) {
	if len(clients) == 0 || !strings.Contains(string(src), "@endpoint") {
		return
	}
	lines := strings.Split(string(src), "\n")
	type note struct{ verb, path string }
	// governing[i] is the annotation covering line i+1, if any.
	governing := make([]*note, len(lines))
	var cur *note
	decls := 0
	for i, line := range lines {
		if m := swiftEndpointAnnotation.FindStringSubmatch(line); m != nil {
			if v := mapSwiftMethod(strings.ToLower(m[1])); v != "" {
				cur, decls = &note{verb: v, path: m[2]}, 0
			} else {
				cur = nil
			}
			continue
		}
		if cur != nil && swiftDeclLine.MatchString(line) {
			decls++
			if decls > 1 {
				cur = nil
			}
		}
		governing[i] = cur
	}
	for i := range clients {
		f := &clients[i]
		if f.Kind != facts.KindRoute || !strings.Contains(f.Name, "{}") || f.Line < 1 || f.Line > len(lines) {
			continue
		}
		n := governing[f.Line-1]
		if n == nil {
			continue
		}
		f.Name = strings.TrimPrefix(n.path, "/")
		f.Props["method"] = n.verb
		f.Props["annotated"] = true
	}
}
