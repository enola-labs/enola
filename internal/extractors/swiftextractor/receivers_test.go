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

// A type handed on as `Type.self` is named by the code that hands it on; a system
// type and a type the project does not declare add nothing.
func TestSwiftMetatypeIsNamed(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"Sources/App/Payload.swift": "struct Payload: Decodable {\n    let id: Int\n}\n",
		"Sources/App/Loader.swift": `
class Loader {
    func load() {
        decode(Payload.self)
        decode(String.self)
        decode(Outside.self)
    }
    func decode<T>(_ type: T.Type) {}
}
`,
	})
	var load facts.Fact
	for _, f := range ff {
		if f.Kind == facts.KindSymbol && f.Name == "Sources/App.Loader.load" {
			load = f
		}
	}
	if !load.HasRelation(facts.RelNames, "Sources/App.Payload") {
		t.Errorf("load relations = %v, want names Sources/App.Payload", load.Relations)
	}
	for _, r := range load.Relations {
		if r.Kind == facts.RelNames && r.Target != "Sources/App.Payload" {
			t.Errorf("load names %s, which the project does not declare", r.Target)
		}
	}
}

// A type's stored property states its type for the calls the type's methods make on
// it, bare or through self, inside a closure as well; a local of no stated type
// shadows it. A property typed as a protocol, whose requirements are no facts, binds
// to the one type outside the tests that conforms and declares the method, whatever
// mocks the tests add.
func TestSwiftCallOnAStoredPropertyResolves(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"Sources/App/UseCases.swift": `
final class LoadOrders {
    func execute(id: Int) async {}
}
final class SaveOrder {
    func execute(id: Int) async {}
}
protocol OrdersService {
    func list() async
}
final class OrdersAPI: OrdersService {
    func list() async {}
}
`,
		"Tests/AppTests/Mocks.swift": `
final class MockOrdersAPI: OrdersService {
    func list() async {}
}
`,
		"Sources/App/OrdersState.swift": `
final class OrdersState {
    private let loadOrders: LoadOrders
    private let service: OrdersService
    init(loadOrders: LoadOrders, service: OrdersService) {
        self.loadOrders = loadOrders
        self.service = service
    }
    func refresh() {
        Task {
            await loadOrders.execute(id: 1)
            await self.service.list()
        }
    }
    func shadowed() {
        let loadOrders = makeSomething()
        loadOrders.execute(id: 2)
    }
}
`,
	})
	refresh := swCalls(t, ff, "Sources/App.OrdersState.refresh")
	for _, want := range []string{"Sources/App.LoadOrders.execute", "Sources/App.OrdersAPI.list"} {
		if !slices.Contains(refresh, want) {
			t.Errorf("refresh calls %v, want %s", refresh, want)
		}
	}
	if slices.Contains(refresh, "Sources/App.SaveOrder.execute") {
		t.Errorf("refresh calls %v: the property is a LoadOrders", refresh)
	}
	if shadowed := swCalls(t, ff, "Sources/App.OrdersState.shadowed"); slices.Contains(shadowed, "Sources/App.LoadOrders.execute") {
		t.Errorf("shadowed calls %v: the untyped local shadows the property", shadowed)
	}
}
