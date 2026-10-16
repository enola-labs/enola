package dartextractor

import (
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A Dart declaration's doc is its `///` lines or `/** */` block, read above its
// annotations. A plain `//` comment, a comment trailing code on its line and one
// separated by a blank line are none.
func TestDartDoc_Declarations(t *testing.T) {
	ff := walkSource(t, "lib/shop/orders.dart", `/// Places and cancels orders.
///
/// More detail.
@immutable
class OrderService {
  /** Places an order and charges the card. */
  @override
  void place(int id) {}

  // Not a doc.
  void plain() {}

  /// The most a cart may hold.
  final int max = 10;
  final int min = 1; /// trails min
  final int step = 2;

  /// Makes a service.
  OrderService();

  /// Refunds an order.
  void refund(int id);
}

/// What can be bought.
enum Item {
  /// A red one.
  red
}

/// Money in cents.
typedef Cents = int;

/// Separated.

void loose() {}

/// Starts the shop.
void main() {}
`)
	want := map[string]string{
		"OrderService":              "Places and cancels orders.",
		"OrderService.place":        "Places an order and charges the card.",
		"OrderService.plain":        "",
		"OrderService.max":          "The most a cart may hold.",
		"OrderService.step":         "",
		"OrderService.OrderService": "Makes a service.",
		"OrderService.refund":       "Refunds an order.",
		"Item":                      "What can be bought.",
		"Item.red":                  "A red one.",
		"Cents":                     "Money in cents.",
		"loose":                     "",
		"main":                      "Starts the shop.",
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
