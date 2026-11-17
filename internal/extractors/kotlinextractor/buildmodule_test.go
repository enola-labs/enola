package kotlinextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A package directory names the Gradle module it compiles into, so a cycle
// between two packages of one module reads as coupling, not a build defect.
func TestModulesNameTheirBuildModule(t *testing.T) {
	ff := extractKotlinRepo(t, map[string]string{
		"feature/cart/src/main/kotlin/com/shop/cart/Cart.kt":    "package com.shop.cart\n\nclass Cart\n",
		"feature/cart/src/main/kotlin/com/shop/cart/ui/View.kt": "package com.shop.cart.ui\n\nclass View\n",
		"scripts/Gen.kt": "package gen\n\nclass Gen\n",
	})
	got := map[string]any{}
	for _, f := range ff {
		if f.Kind == facts.KindModule {
			got[f.Name] = f.PropAny("jvm_module")
		}
	}
	for dir, want := range map[string]any{
		"feature/cart/src/main/kotlin/com/shop/cart":    "feature/cart",
		"feature/cart/src/main/kotlin/com/shop/cart/ui": "feature/cart",
		"scripts": nil,
	} {
		if got[dir] != want {
			t.Errorf("%s jvm_module = %v, want %v (modules %v)", dir, got[dir], want, got)
		}
	}
}
