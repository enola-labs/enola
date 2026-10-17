package kotlinextractor

import (
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A Kotlin declaration's doc is its KDoc up to the first tag, or the run of `//`
// lines above it, read above its annotations. A plain block comment, a tool
// directive and a comment separated by a blank line are none.
func TestKotlinDoc_Declarations(t *testing.T) {
	ff := extractAST(t, `package shop

/**
 * Places and cancels orders.
 *
 * More detail.
 * @property repo where orders live
 */
@Service
class OrderService {
    /** Places an order and charges the card. */
    @Transactional
    fun place(id: Long) {}

    // Cancels an order
    // that was not shipped.
    fun cancel(id: Long) {}

    // ktlint-disable
    fun directive() {}

    /* not a doc */
    fun plain() {}
}

/** The one cart. */
object Cart

/** The most a cart may hold. */
val MAX = 10

/** Money in cents. */
typealias Cents = Long

/** Separated. */

fun loose() {}
`, false)
	want := map[string]string{
		"OrderService":           "Places and cancels orders.",
		"OrderService.place":     "Places an order and charges the card.",
		"OrderService.cancel":    "Cancels an order that was not shipped.",
		"OrderService.directive": "",
		"OrderService.plain":     "",
		"Cart":                   "The one cart.",
		"MAX":                    "The most a cart may hold.",
		"Cents":                  "Money in cents.",
		"loose":                  "",
	}
	for suffix, w := range want {
		var f *facts.Fact
		for i := range ff {
			if ff[i].Kind == facts.KindSymbol && strings.HasSuffix(ff[i].Name, "."+suffix) {
				f = &ff[i]
			}
		}
		if f == nil {
			t.Errorf("no symbol ending %s", suffix)
			continue
		}
		if got := f.PropString(facts.PropDoc); got != w {
			t.Errorf("%s: doc = %q, want %q", suffix, got, w)
		}
		if _, has := f.Prop(facts.PropDoc); w == "" && has {
			t.Errorf("%s: an undocumented declaration must carry no doc prop", suffix)
		}
	}
}
