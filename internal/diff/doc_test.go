package diff

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A reworded comment is not a change to the architecture; a changed metric
// beside it still is.
func TestPropsChanged_IgnoresTheDocProp(t *testing.T) {
	fact := func(doc string, cyclomatic int) facts.Fact {
		props := map[string]any{"symbol_kind": "function", "cyclomatic": cyclomatic}
		if doc != "" {
			props[facts.PropDoc] = doc
		}
		return facts.Fact{Kind: facts.KindSymbol, Name: "pkg.F", Props: props}
	}
	for name, tc := range map[string]struct {
		a, b facts.Fact
		want bool
	}{
		"reworded":        {fact("Does a thing.", 2), fact("Does the thing.", 2), false},
		"comment added":   {fact("", 2), fact("Does a thing.", 2), false},
		"comment removed": {fact("Does a thing.", 2), fact("", 2), false},
		"metric moved":    {fact("Does a thing.", 2), fact("Does a thing.", 3), true},
		"both moved":      {fact("Does a thing.", 2), fact("Does the thing.", 3), true},
		"no doc, same":    {fact("", 2), fact("", 2), false},
		"no doc, metric":  {fact("", 2), fact("", 3), true},
	} {
		if got := propsChanged(tc.a, tc.b); got != tc.want {
			t.Errorf("%s: propsChanged = %v, want %v", name, got, tc.want)
		}
	}
}

// An edited function body changes the symbol's version and nothing about the
// architecture.
func TestPropsChanged_IgnoresTheBodyHash(t *testing.T) {
	fact := func(body string, cyclomatic int) facts.Fact {
		props := map[string]any{"symbol_kind": "function", "cyclomatic": cyclomatic}
		if body != "" {
			props[facts.PropBodyHash] = body
		}
		return facts.Fact{Kind: facts.KindSymbol, Name: "pkg.F", Props: props}
	}
	for name, tc := range map[string]struct {
		a, b facts.Fact
		want bool
	}{
		"body edited":   {fact("aa", 2), fact("bb", 2), false},
		"version added": {fact("", 2), fact("aa", 2), false},
		"metric moved":  {fact("aa", 2), fact("bb", 3), true},
	} {
		if got := propsChanged(tc.a, tc.b); got != tc.want {
			t.Errorf("%s: propsChanged = %v, want %v", name, got, tc.want)
		}
	}
}
