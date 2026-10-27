package dartextractor

import (
	"context"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A symbol's span is what maps a changed line or a reported location to the
// declaration it sits in. Each declaration form ends on the line its last token is on;
// a method whose body is a sibling of its signature ends with the body.
func TestSymbolSpans(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"pubspec.yaml": "name: app\n",
		"lib/shop.dart": `import 'dart:core';

class Shop {
  final String name = 'shop';

  int run() {
    return name.length;
  }

  int get size => 1;
}

enum Kind {
  small,
  large,
}

typedef Handler = void Function();

int helper(int a) {
  return a + 1;
}
`,
	})
	ff, err := New().Extract(context.Background(), root, []string{"lib/shop.dart"})
	if err != nil {
		t.Fatal(err)
	}
	checkSpans(t, ff, map[string][2]int{
		"Shop":       {3, 11},
		"Shop.name":  {4, 4},
		"Shop.run":   {6, 8},
		"Shop.size":  {10, 10},
		"Kind":       {13, 16},
		"Kind.small": {14, 14},
		"Handler":    {18, 18},
		"helper":     {20, 22},
	})
}

// checkSpans asserts each wanted symbol (matched by the end of its name) has the span,
// and every symbol has a well-formed one.
func checkSpans(t *testing.T, ff []facts.Fact, want map[string][2]int) {
	t.Helper()
	got := map[string][2]int{}
	for _, f := range ff {
		if f.Kind != facts.KindSymbol {
			continue
		}
		t.Logf("%s %d-%d", f.Name, f.Line, f.EndLine)
		if f.EndLine < f.Line || f.Line <= 0 {
			t.Errorf("%s (%s): span %d-%d", f.Name, f.File, f.Line, f.EndLine)
		}
		for name := range want {
			if f.Name == name || strings.HasSuffix(f.Name, "."+name) || strings.HasSuffix(f.Name, "/"+name) {
				got[name] = [2]int{f.Line, f.EndLine}
			}
		}
	}
	for name, span := range want {
		if got[name] != span {
			t.Errorf("%s: span %v, want %v", name, got[name], span)
		}
	}
}
