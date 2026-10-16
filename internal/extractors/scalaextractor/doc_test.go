package scalaextractor

import (
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A Scala definition's doc is its Scaladoc up to the first tag, or the run of `//`
// lines above it, read above its annotations. A plain block comment, a tool
// directive and a comment separated by a blank line are none.
func TestScalaDoc_Definitions(t *testing.T) {
	ff := extractAST(t, "src/shop/Orders.scala", `package shop

/** Places and cancels orders.
  *
  * More detail.
  * @param repo where orders live
  */
@deprecated
class OrderService {
  /** Places an order and charges the card. */
  @inline def place(id: Long): Unit = ()

  def total(id: Long): Long =
    id + 1

  /** Refunds an order. */
  def refund(id: Long): Unit

  // Cancels an order
  // that was not shipped.
  def cancel(id: Long): Unit = ()

  // scalastyle:off
  def directive(): Unit = ()

  /* not a doc */
  def plain(): Unit = ()

  /** The most a cart may hold. */
  val max = 10
  val min = 1 // the least
  val step = 2

  /** Money in cents. */
  type Cents = Long
}

/** What can be bought. */
enum Item {
  /** A red one. */
  case Red
}

/** Separated. */

def loose(): Unit = ()
`)
	want := map[string]string{
		"OrderService":           "Places and cancels orders.",
		"OrderService.place":     "Places an order and charges the card.",
		"OrderService.cancel":    "Cancels an order that was not shipped.",
		"OrderService.refund":    "Refunds an order.",
		"OrderService.directive": "",
		"OrderService.plain":     "",
		"OrderService.max":       "The most a cart may hold.",
		"OrderService.step":      "",
		"OrderService.Cents":     "Money in cents.",
		"Item":                   "What can be bought.",
		"Item.Red":               "A red one.",
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
			t.Errorf("%s: an undocumented definition must carry no doc prop", suffix)
		}
	}
}
