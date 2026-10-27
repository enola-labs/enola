package tsextractor

import (
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A symbol's span is what maps a changed line or a reported location to the
// declaration it sits in. Each declaration form ends on the line its last token is on.
func TestSymbolSpans(t *testing.T) {
	ff := extractTSFiles(t, map[string]string{
		"src/spans.ts": `import { x } from './x';

export function alpha(a: number) {
	return a + x;
}

export const beta = (b: number) => {
	const c = b * 2;
	return c;
};

export class Gamma {
	run(): void {
		alpha(1);
	}

	get size(): number {
		return 1;
	}
}

export interface Delta {
	id: string;
}

export const handlers = {
	onOpen() {
		return beta(2);
	},
};
`,
		"src/Panel.vue": `<template><div /></template>
<script setup lang="ts">
function open() {
	return 1;
}
</script>
`,
	})

	want := map[string][2]int{
		"src.alpha":           {3, 5},
		"src.beta":            {7, 10},
		"src.Gamma":           {12, 20},
		"src.Gamma.run":       {13, 15},
		"src.Gamma.size":      {17, 19},
		"src.Delta":           {22, 24},
		"src.Delta.id":        {23, 23},
		"src.handlers":        {26, 30},
		"src.handlers.onOpen": {27, 29},
		"src.open":            {3, 5},
	}
	got := map[string][2]int{}
	for _, f := range ff {
		if f.Kind != facts.KindSymbol {
			continue
		}
		if f.EndLine < f.Line || f.Line <= 0 {
			t.Errorf("%s (%s): span %d-%d", f.Name, f.File, f.Line, f.EndLine)
		}
		for name := range want {
			if f.Name == name || strings.HasSuffix(f.Name, "/"+name) {
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
