package kotlinextractor

import (
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A symbol's span is what maps a changed line or a reported location to the
// declaration it sits in. Each declaration form ends on the line its last token is on.
func TestSymbolSpans(t *testing.T) {
	checkSpans(t, extractAST(t, `package com.acme

class Shop(val name: String) {
    fun run(): Int {
        return name.length
    }

    val size: Int
        get() = 1
}

object Registry {
    fun open() = Unit
}

interface Store {
    fun close()
}

fun topLevel(a: Int): Int {
    return a + 1
}

val handler = {
    topLevel(2)
}
`, false), map[string][2]int{
		"Shop":          {3, 10},
		"Shop.run":      {4, 6},
		"Registry":      {12, 14},
		"Registry.open": {13, 13},
		"Store":         {16, 18},
		"topLevel":      {20, 22},
	})
}

// A declaration the grammar misparses (an annotated interface or class, which tree-sitter
// reads as an expression) is recovered by hand; its span still reaches its closing brace.
func TestSymbolSpans_RecoveredDeclarations(t *testing.T) {
	ff := extractAST(t, `package com.acme.di

@EntryPoint
@InstallIn(SingletonComponent::class)
interface WorkerEntryPoint {
    fun factory(): Factory
}

@Module
@InstallIn(SingletonComponent::class)
abstract class DataModule {
    fun bind(): Int = helper()
}
`, true)
	ends := map[string]int{"WorkerEntryPoint": 7, "DataModule": 13}
	for _, f := range ff {
		for name, end := range ends {
			if f.Kind == facts.KindSymbol && strings.HasSuffix(f.Name, "."+name) {
				if f.EndLine != end || f.Line <= 0 || f.Line > end {
					t.Errorf("%s: span %d-%d, want it to end on %d", name, f.Line, f.EndLine, end)
				}
				delete(ends, name)
			}
		}
	}
	for name := range ends {
		t.Errorf("%s: not extracted", name)
	}
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
