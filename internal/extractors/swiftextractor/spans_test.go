package swiftextractor

import (
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A symbol's span is what maps a changed line or a reported location to the
// declaration it sits in. Each declaration form ends on the line its last token is on.
func TestSymbolSpans(t *testing.T) {
	checkSpans(t, extractAST(t, `import Foundation

public class Shop {
    let name: String = "shop"

    func run() -> Int {
        return name.count
    }
}

protocol Store {
    func open()
}

struct Point {
    var x: Int
    var y: Int
}

enum Kind {
    case small
    case large
}

func helper(_ a: Int) -> Int {
    return a + 1
}
`, false), map[string][2]int{
		"Shop":     {3, 9},
		"Shop.run": {6, 8},
		"Store":    {11, 13},
		"Point":    {15, 18},
		"Kind":     {20, 23},
		"helper":   {25, 27},
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
