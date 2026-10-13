package goextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func TestGoDoc_FunctionsMethodsAndTypes(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"go.mod": "module example.com/shop\n\ngo 1.21\n",
		"order/order.go": `package order

// Place takes an order
// and charges for it.
//
// It returns nothing.
func Place() {}

func bare() {}

//go:noinline
func directiveOnly() {}

// Cart holds what a customer picked.
type Cart struct{}

// Total sums the cart.
func (c Cart) Total() int { return 0 }

type (
	// Line is one row of a cart.
	Line struct{}

	Plain struct{}
)
`,
	})
	for name, want := range map[string]string{
		"order.Place":         "Place takes an order and charges for it.",
		"order.bare":          "",
		"order.directiveOnly": "",
		"order.Cart":          "Cart holds what a customer picked.",
		"order.Cart.Total":    "Total sums the cart.",
		"order.Line":          "Line is one row of a cart.",
		"order.Plain":         "",
	} {
		f, ok := findFact(ff, name)
		if !ok {
			t.Errorf("no fact named %s", name)
			continue
		}
		if got := f.PropString(facts.PropDoc); got != want {
			t.Errorf("%s: doc = %q, want %q", name, got, want)
		}
		if _, has := f.Prop(facts.PropDoc); want == "" && has {
			t.Errorf("%s: an uncommented symbol must carry no doc prop", name)
		}
	}
}
