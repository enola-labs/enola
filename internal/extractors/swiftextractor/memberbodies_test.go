package swiftextractor

import (
	"slices"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// An init declared in an extension has no type to own its calls, so it is a member
// of its own, Base.init. A subscript's and a deinit's body call in the name of the
// type that declares them, as an init's does.
func TestSwiftExtensionInitSubscriptAndDeinitRecordCalls(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"Sources/App/Parse.swift": `
func parseA(_ s: String) -> Bool { true }
func parseB(_ i: Int) -> Int { i }
func cleanupC() {}
func readD(_ k: String) -> Int { 0 }
`,
		"Sources/App/Params.swift": `
struct Params {
    let flag: Bool
}
extension Params: Decodable {
    init(from decoder: Decoder) throws {
        self.flag = parseA("x")
    }
}
extension String {
    init(html: String) {
        self.init(parseB(1))
    }
}
final class Store {
    subscript(i: Int) -> Int { parseB(i) }
    subscript(k: String) -> Int {
        get { readD(k) }
    }
    deinit { cleanupC() }
}
`,
	})
	if calls := swCalls(t, ff, "Sources/App.Params.init"); !slices.Contains(calls, "Sources/App.parseA") {
		t.Errorf("Params.init calls %v, want parseA", calls)
	}
	if calls := swCalls(t, ff, "Sources/App.String.init"); !slices.Contains(calls, "Sources/App.parseB") {
		t.Errorf("String.init calls %v, want parseB", calls)
	}
	calls := swCalls(t, ff, "Sources/App.Store")
	for _, want := range []string{"Sources/App.parseB", "Sources/App.readD", "Sources/App.cleanupC"} {
		if !slices.Contains(calls, want) {
			t.Errorf("Store calls %v, want %s", calls, want)
		}
	}
	for _, f := range ff {
		if f.Name != "Sources/App.Params.init" {
			continue
		}
		if sk, _ := f.PropAny("symbol_kind").(string); sk != facts.SymbolMethod {
			t.Errorf("Params.init is a %q, want a method", sk)
		}
		if recv, _ := f.PropAny("receiver").(string); recv != "Params" {
			t.Errorf("Params.init receiver %q, want Params", recv)
		}
	}
}
