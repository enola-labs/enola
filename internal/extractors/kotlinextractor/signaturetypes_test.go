package kotlinextractor

import (
	"slices"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func kotlinNames(ff []facts.Fact, suffix string) []string {
	var out []string
	for _, f := range ff {
		if f.Kind == facts.KindSymbol && strings.HasSuffix(f.Name, suffix) {
			for _, r := range f.Relations {
				if r.Kind == facts.RelNames {
					out = append(out, r.Target)
				}
			}
		}
	}
	slices.Sort(out)
	return out
}

// A type a signature, a property or a constructor parameter names is used by the
// declaration, though nothing constructs it there: a data class a function takes or
// returns, the type of a field.
func TestSignatureTypes_AreUsedByTheirDeclaration(t *testing.T) {
	ff := extractKotlinRepo(t, map[string]string{
		"src/main/kotlin/shop/Models.kt": `package shop

data class Line(val sku: String)
data class Cart(val lines: List<Line>)
class Receipt
`,
		"src/main/kotlin/shop/Checkout.kt": `package shop

class Checkout(private val cart: Cart) {
    fun total(lines: List<Line>): Receipt? = null
}
`,
	})
	if got := kotlinNames(ff, "Checkout.total"); !slices.ContainsFunc(got, func(s string) bool { return strings.HasSuffix(s, ".Line") }) ||
		!slices.ContainsFunc(got, func(s string) bool { return strings.HasSuffix(s, ".Receipt") }) {
		t.Errorf("total names %v, want Line and Receipt", got)
	}
	if got := kotlinNames(ff, "shop.Checkout"); !slices.ContainsFunc(got, func(s string) bool { return strings.HasSuffix(s, ".Cart") }) {
		t.Errorf("Checkout names %v, want Cart", got)
	}
	if got := kotlinNames(ff, "shop.Cart"); !slices.ContainsFunc(got, func(s string) bool { return strings.HasSuffix(s, ".Line") }) {
		t.Errorf("Cart names %v, want Line", got)
	}
}
