package tsutil

import (
	"bytes"

	"github.com/enola-labs/enola/internal/facts"
)

// SpanFileSymbols gives the symbols that stand for their whole file the file as their
// span: a component synthesized at line 1 with no declaration node of its own (a Vue,
// Svelte or Ember component, a XAML view, a Razor component). A declaration's own span
// is set where it is built; only these are left at 0.
func SpanFileSymbols(ff []facts.Fact, relFile string, src []byte) {
	last := bytes.Count(src, []byte("\n"))
	if len(src) > 0 && src[len(src)-1] != '\n' {
		last++
	}
	for i := range ff {
		if ff[i].Kind == facts.KindSymbol && ff[i].File == relFile && ff[i].Line == 1 && ff[i].EndLine == 0 {
			ff[i].EndLine = max(last, 1)
		}
	}
}
