package swiftextractor

import (
	"slices"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func swCalls(t *testing.T, ff []facts.Fact, name string) []string {
	t.Helper()
	for _, f := range ff {
		if f.Kind == facts.KindSymbol && f.Name == name {
			var out []string
			for _, r := range f.Relations {
				if r.Kind == facts.RelCalls {
					out = append(out, r.Target)
				}
			}
			return out
		}
	}
	t.Fatalf("no symbol %s", name)
	return nil
}

// Two types declare didCreate and task, so the name alone binds nothing. A call on
// a typed parameter, a typed local, a constructed local and self binds to the
// receiver type's method, an extension's included; an untyped receiver keeps the
// bare name.
func TestSwiftCallOnAStatedReceiverResolves(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"Sources/Net/Request.swift": `
class Request {
    func didCreate(_ r: URLRequest) {}
    func task(for r: URLRequest) {}
}
`,
		"Sources/Net/Ledger.swift": `
class Ledger {
    func didCreate(_ r: URLRequest) {}
    func task(for r: URLRequest) {}
    func settle() {}
}
`,
		"Sources/Net/URLRequest+Ext.swift": `
extension URLRequest {
    func validate() throws {}
}
`,
		"Sources/Net/Session.swift": `
class Session {
    func setup(for request: Request?, other: AnyObject) {
        let initial: URLRequest = make()
        try initial.validate()
        request.didCreate(initial)
        request.task(for: initial)
        let ledger = Ledger()
        ledger.settle()
        self.didCreate(initial, for: request)
        other.didCreate(initial)
    }
}

extension Session {
    func didCreate(_ r: URLRequest, for request: Request?) {}
}
`,
	})
	calls := swCalls(t, ff, "Sources/Net.Session.setup")
	for _, want := range []string{
		"Sources/Net.Request.didCreate",
		"Sources/Net.Request.task",
		"Sources/Net.URLRequest.validate",
		"Sources/Net.Ledger.settle",
		"Sources/Net.Session.didCreate",
	} {
		if !slices.Contains(calls, want) {
			t.Errorf("setup calls %v, want %s", calls, want)
		}
	}
	if slices.Contains(calls, "Sources/Net.Ledger.didCreate") {
		t.Errorf("setup calls %v: no receiver is a Ledger when didCreate is called", calls)
	}
	for _, f := range ff {
		if _, kept := f.Prop(propTypedCalls); kept {
			t.Errorf("%s: the walker's note leaked into the facts", f.Name)
		}
	}
}
