package cppextractor

import (
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A symbol's span is what maps a changed line or a reported location to the
// declaration it sits in. A header prototype and its definition merge into one
// symbol, and it takes the definition's file, start and span together: the body.
func TestSymbolSpans(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/shop.h": `#pragma once

class Shop {
public:
    void run();
    int size() const { return 1; }
private:
    int count;
};

int helper(int a);
`,
		"src/shop.cpp": `#include "shop.h"

void Shop::run() {
    count++;
    helper(count);
}

int helper(int a) {
    return a + 1;
}
`,
		"src/point.c": `struct point {
    int x;
    int y;
};
`,
	})
	for _, f := range ff {
		if f.Kind == facts.KindSymbol && strings.HasSuffix(f.Name, "run") && !strings.HasSuffix(f.File, "shop.cpp") {
			t.Errorf("%s: merged into %s, want the definition's file", f.Name, f.File)
		}
	}
	checkSpans(t, ff, map[string][2]int{
		"Shop":      {3, 9},
		"Shop::run": {3, 6},
		"helper":    {8, 10},
		"point":     {1, 4},
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
