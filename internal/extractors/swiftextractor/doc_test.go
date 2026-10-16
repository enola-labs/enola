package swiftextractor

import (
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A Swift declaration's doc is its `///` lines or `/** */` block, read above its
// attributes. A plain `//` comment, a `////` one, a comment trailing code on its
// line and one separated by a blank line are none.
func TestSwiftDoc_Declarations(t *testing.T) {
	ff := extractAST(t, `import Foundation

/// Places and cancels orders.
///
/// More detail.
@MainActor
public final class OrderService {
    /** Places an order and charges the card. */
    @discardableResult
    func place(id: Int) -> Bool { true }

    // Not a doc.
    func plain() {}

    //// Not a doc either.
    func four() {}

    /// The most a cart may hold.
    let max = 10
    let min = 1 /// trails min
    var step = 2
}

/// Something that is priced.
protocol Priced {}

/// Money in cents.
typealias Cents = Int

/// Separated.

func loose() {}
`, false)
	want := map[string]string{
		"OrderService":       "Places and cancels orders.",
		"OrderService.place": "Places an order and charges the card.",
		"OrderService.plain": "",
		"OrderService.four":  "",
		"OrderService.max":   "The most a cart may hold.",
		"OrderService.step":  "",
		"Priced":             "Something that is priced.",
		"Cents":              "Money in cents.",
		"loose":              "",
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
