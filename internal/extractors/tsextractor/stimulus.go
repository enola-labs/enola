package tsextractor

import (
	"path/filepath"
	"strings"

	sitter "github.com/enola-labs/enola/internal/treesitter"

	"github.com/enola-labs/enola/internal/extractors/tsutil"
)

// Stimulus declares its magic accessors through two static class fields —
// `static targets = [...]` and `static values = {...}` — which the member
// pass already parses as public_field_definition symbols (finding 0007: they
// classify as methods and carry no framework identity). This tags exactly
// those symbols with classification props, so a consumer can ask "which
// Stimulus controllers declare targets" without any new parsing: the field
// must literally be named targets or values, must carry the static modifier,
// and must live in a conventionally-placed controller file. Anything else is
// left untouched — a non-static field named targets, or the same shape
// outside a controller file, says nothing about Stimulus, fail closed.

// stimulusStaticField reports whether a class member is one of Stimulus's
// static declaration fields on a controller file.
func stimulusStaticField(kinds *tsutil.KindTable, member *sitter.Node, name, relFile string) bool {
	if name != "targets" && name != "values" {
		return false
	}
	if kindOf(kinds, member) != "public_field_definition" {
		return false
	}
	if !stimulusControllerFile(relFile) {
		return false
	}
	for i := uint(0); i < member.ChildCount(); i++ {
		if kindOf(kinds, member.Child(i)) == "static" {
			return true
		}
	}
	return false
}

// stimulusControllerFile matches the Stimulus naming convention the Ruby
// extractor's markup pass resolves against: a *_controller.js/.ts file under
// a controllers/ directory. Only the convention is honored — a relocated
// controller gets no classification, which is the honest miss.
func stimulusControllerFile(relFile string) bool {
	slash := filepath.ToSlash(strings.ToLower(relFile))
	if !strings.Contains(slash, "controllers/") {
		return false
	}
	base := filepath.Base(slash)
	return strings.HasSuffix(base, "_controller.js") || strings.HasSuffix(base, "_controller.ts")
}
