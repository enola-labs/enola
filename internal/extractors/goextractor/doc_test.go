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

func TestGoDoc_PackageComment(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"go.mod": "module example.com/shop\n\ngo 1.21\n",
		// doc.go wins over a file that sorts first.
		"order/a.go":      "// Package order is not described here.\npackage order\n",
		"order/doc.go":    "// Package order takes and charges orders.\n//\n// More detail.\npackage order\n",
		"order/x_test.go": "// Package order is from a test.\npackage order\n",
		// Without doc.go, the first file by name that has one.
		"cart/b.go": "// Package cart holds picks.\npackage cart\n",
		"cart/a.go": "package cart\n",
		// A licence directly above the clause is no description.
		"lic/l.go": "// Copyright 2026 Someone. All rights reserved.\npackage lic\n",
		// A comment separated by a blank line is not the package comment.
		"loose/l.go": "// Loose words.\n\npackage loose\n",
	})
	for dir, want := range map[string]string{
		"order": "Package order takes and charges orders.",
		"cart":  "Package cart holds picks.",
		"lic":   "",
		"loose": "",
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
			t.Errorf("%s: an undocumented package must carry no doc prop", dir)
		}
	}
}
