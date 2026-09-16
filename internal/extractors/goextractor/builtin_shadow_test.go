package goextractor

import (
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A package-level function SHADOWS the predeclared identifier of the same name, so a
// bare call to it is a call to the package's own function and must carry an edge.
//
// Repositories written before Go 1.21 carry their own min and max helpers by the
// hundred. Dropping those calls left the helper with no incoming edge at all, and the
// dead-code analyzer reported it at HIGH confidence — the tier documented as the
// safest to delete — for a function called seventeen times.
func TestPackageFuncShadowsBuiltin(t *testing.T) {
	got := extractAll(t, map[string]string{
		"svc/helpers.go": `package svc

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
`,
		"svc/clamp.go": `package svc

func Clamp(v, hi int) int {
	return min(v, hi)
}

func Size(xs []int) int {
	return len(xs)
}
`,
	})

	calls := func(suffix string) []string {
		var out []string
		for _, f := range got {
			if f.Kind != facts.KindSymbol || !strings.HasSuffix(f.Name, suffix) {
				continue
			}
			for _, r := range f.Relations {
				if r.Kind == facts.RelCalls {
					out = append(out, r.Target)
				}
			}
		}
		return out
	}

	// The helper is declared in a DIFFERENT file of the same package, which is why the
	// name set has to be collected per package rather than per file.
	clamp := calls(".Clamp")
	var linked bool
	for _, c := range clamp {
		if strings.HasSuffix(c, ".min") {
			linked = true
		}
	}
	if !linked {
		t.Errorf("Clamp calls the package's own min, but no edge was emitted; got %v", clamp)
	}

	// len is not declared here, so it stays predeclared and emits nothing. Without
	// that half, every len() in every repository becomes a dangling phantom node.
	for _, c := range calls(".Size") {
		if strings.HasSuffix(c, ".len") {
			t.Errorf("len is not declared in this package and must stay a builtin; got %v", c)
		}
	}
}
