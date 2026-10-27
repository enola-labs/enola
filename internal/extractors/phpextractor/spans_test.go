package phpextractor

import (
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A symbol's span is what maps a changed line or a reported location to the
// declaration it sits in. Each declaration form ends on the line its last token is on.
func TestSymbolSpans(t *testing.T) {
	checkSpans(t, extractFileAST([]byte(`<?php
namespace App\Shop;

class Shop
{
    const NAME = 'shop';

    private $size = 1;

    public function run(): int
    {
        return $this->size;
    }
}

interface Store
{
    public function open(): void;
}

function helper($a)
{
    return $a + 1;
}
`), "app/Shop.php"), map[string][2]int{
		`App\Shop\Shop`:        {4, 14},
		`App\Shop\Shop::NAME`:  {6, 6},
		`App\Shop\Shop::$size`: {8, 8},
		`App\Shop\Shop::run`:   {10, 13},
		`App\Shop\Store`:       {16, 19},
		`App\Shop\Store::open`: {18, 18},
		`App\Shop\helper`:      {21, 24},
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
