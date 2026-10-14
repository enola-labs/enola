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
