package config

import (
	"regexp"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// TestDefaultIgnoresMatchGeneratedDartSuffixes keeps the walker's ignore list and
// the Dart extractor's own filter on one list. A suffix only the extractor knows
// leaves the file in the walk, where it lands in the shared cache partition and
// invalidates every extractor's cache each time it is regenerated. The bundled
// mcp-arch.yaml is held to Default().Ignore by TestBundledConfigCoversDefaultIgnores.
func TestDefaultIgnoresMatchGeneratedDartSuffixes(t *testing.T) {
	ignore := map[string]bool{}
	for _, g := range Default().Ignore {
		ignore[g] = true
	}
	want := map[string]bool{}
	for _, suf := range facts.DartGeneratedSuffixes {
		glob := "**/*" + suf
		want[glob] = true
		if !ignore[glob] {
			t.Errorf("Default().Ignore lacks %q from facts.DartGeneratedSuffixes", glob)
		}
	}
	codegen := regexp.MustCompile(`^\*\*/\*\.[a-z]+\.dart$`)
	for g := range ignore {
		if codegen.MatchString(g) && !want[g] {
			t.Errorf("Default().Ignore has %q, which facts.DartGeneratedSuffixes does not list", g)
		}
	}
}
