package tsextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A symbol's doc is the JSDoc or the run of line comments ending on the line above
// it, or above the export holding it: up to the first tag, without directives, and
// not a comment a blank line separates from it.
func TestDoc_JSDocAndLineComments(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/orders.ts": `/* Copyright the authors. */

/**
 * Places an order and charges the card.
 *
 * Retries once on a timeout.
 * @param id the order
 */
export async function placeOrder(id: string) {}

// Cancels an order that has not shipped.
// The refund is issued asynchronously.
export const cancelOrder = async (id: string) => {}

// eslint-disable-next-line no-console
export function logOrder() {}

// A section header, not about the function below.

export function untouched() {}

/** The order service. */
export class OrderService {
	/** Looks an order up by id. */
	find(id: string) {}

	list() {}

	/** Cancels by id. */
	@Delete(':id')
	@HttpCode(204)
	remove(id: string) {}
}
`,
	}, false)
	got := map[string]string{}
	for _, f := range ff {
		if f.Kind == facts.KindSymbol {
			got[f.Name] = f.PropString(facts.PropDoc)
		}
	}
	want := map[string]string{
		"src.placeOrder":          "Places an order and charges the card.",
		"src.cancelOrder":         "Cancels an order that has not shipped. The refund is issued asynchronously.",
		"src.logOrder":            "",
		"src.untouched":           "",
		"src.OrderService":        "The order service.",
		"src.OrderService.find":   "Looks an order up by id.",
		"src.OrderService.list":   "",
		"src.OrderService.remove": "Cancels by id.",
	}
	for name, doc := range want {
		if got[name] != doc {
			t.Errorf("%s doc = %q, want %q", name, got[name], doc)
		}
	}
}

// A directory's doc is the file-level JSDoc block opening its index file: one marked
// @packageDocumentation, @module or @fileoverview. An unmarked block there is the
// first export's, and index.ts is read before index.js.
func TestDoc_IndexFileBlock(t *testing.T) {
	ff := extractTSRepo(t, map[string]string{
		"shop/index.ts": "// SPDX-License-Identifier: MIT\n/* eslint-disable */\n/**\n * Orders and payments.\n *\n * More detail.\n * @packageDocumentation\n */\nexport const place = () => 1;\n",
		"shop/index.js": "/** @fileoverview Not this one. */\nexport const x = 1;\n",
		"util/index.js": "#!/usr/bin/env node\n/**\n * @fileoverview Small helpers\n * for strings.\n * @author someone\n */\nexport const trim = (s) => s;\n",
		"lib/index.ts":  "/**\n * Places an order.\n */\nexport function place() { return 1; }\n",
		"late/index.ts": "import { a } from './a';\n/** @packageDocumentation Too late. */\nexport const b = a;\n",
		"late/a.ts":     "export const a = 1;\n",
	})
	for dir, want := range map[string]string{
		"shop": "Orders and payments.",
		"util": "Small helpers for strings.",
		"lib":  "",
		"late": "",
	} {
		var mod *facts.Fact
		for i := range ff {
			if ff[i].Kind == facts.KindModule && ff[i].Name == dir {
				mod = &ff[i]
			}
		}
		if mod == nil {
			t.Errorf("no module fact %s", dir)
			continue
		}
		if got := mod.PropString(facts.PropDoc); got != want {
			t.Errorf("%s: doc = %q, want %q", dir, got, want)
		}
		if _, has := mod.Prop(facts.PropDoc); want == "" && has {
			t.Errorf("%s: an undocumented directory must carry no doc prop", dir)
		}
	}
}
