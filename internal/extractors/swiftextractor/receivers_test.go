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

// The grammar parses a generic metatype, `Box<T>.self`, and a static call through a
// specialized type, `Box<T>.make(for:)`, as two comparisons. Both name the type, the
// call binds to its static method, and a generic parameter names nothing.
func TestSwiftGenericMetatypeIsNamed(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"Sources/App/Box.swift": `
struct Payload: Decodable {}
struct Box<D: Decodable>: Decodable {
    static func info(for keys: [String]) -> [String: String] { [:] }
}
`,
		"Sources/App/Loader.swift": `
final class Loader {
    func load<D: Decodable>(_: D.Type) {
        decode(Box<D>.self, info: Box<D>.info(for: []))
        decode(Box<Payload>.self, info: [:])
    }
    func decode<T>(_ type: T.Type, info: [String: String]) {}
}
`,
	})
	names := namesOf(ff)
	if !hasName(names, "Sources/App.Loader.load", "Sources/App.Box") {
		t.Errorf("load names %v, want Sources/App.Box", names["Sources/App.Loader.load"])
	}
	if !hasName(names, "Sources/App.Loader.load", "Sources/App.Payload") {
		t.Errorf("load names %v, want Sources/App.Payload (a type argument)", names["Sources/App.Loader.load"])
	}
	if calls := swCalls(t, ff, "Sources/App.Loader.load"); !slices.Contains(calls, "Sources/App.Box.info") {
		t.Errorf("load calls %v, want Sources/App.Box.info", calls)
	}
}

// A call on a parameter, local or stored property whose type the repository does
// not declare (an imported ConfigReader) is the framework's, so the one in-repo
// method of that name a struct owns does not capture it. A method the repository
// adds to the type or a protocol still binds; where an extension of another type
// declares the name too, the call keeps its bare name. A receiver typed by a generic
// parameter, an associated type, AnyObject or a type the repository declares
// binds by name as before. A name an if or guard condition binds by a cast has the
// cast's type, not the type of the parameter it shadows.
func TestSwiftCallOnAnExternalTypedReceiverDoesNotBindByName(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"Sources/App/Strategy.swift": `
struct Strategy {
    private static func int(_ s: String) -> Int? { nil }
    func render() {}
    func lookup() {}
    func show() {}
    func run() {}
}

extension TaskRunner {
    func run() {}
}

extension ConfigReader {
    func port() -> Int { 0 }
}

protocol Keyed {
    associatedtype Value
}

protocol Shiny {}

extension Shiny {
    func gleam() {}
}
`,
		"Sources/App/Address.swift": `
import Configuration

struct Address {
    let reader: ConfigReader
    init(from config: ConfigReader) {
        _ = config.int(forKey: "port")
        _ = config.port()
        let other: ConfigReader = config
        other.render()
        reader.gleam()
        config.run()
    }
    func generic<T>(_ value: T) {
        value.lookup()
    }
    func any(_ target: AnyObject) {
        target.show()
    }
    func display(cell: UITableViewCell, other: UITableViewCell, expand: ExpandCell?) {
        if let expand = expand {
            expand.collapse()
        }
        if let cell = cell as? ExpandCell {
            cell.willDisplay()
        } else if let cell = cell as? SelectCell {
            cell.willDisplay()
        }
        guard let row = other as? RowCell, let expand = expand?.sibling() else { return }
        row.willDisplay()
        expand.collapse()
    }
}

class ExpandCell {
    func willDisplay() {}
    func collapse() {}
    func sibling() -> SelectCell? { nil }
}

class SelectCell {
    func willDisplay() {}
    func collapse() {}
    func sibling() -> SelectCell? { nil }
}

class RowCell {
    func willDisplay() {}
}

extension Keyed {
    func read(_ v: Value) {
        v.render()
    }
}
`,
	})
	// A type's own init records its calls on the type.
	calls := swCalls(t, ff, "Sources/App.Address")
	for _, bad := range []string{"Sources/App.Strategy.int", "Sources/App.Strategy.render", "int", "render"} {
		if slices.Contains(calls, bad) {
			t.Errorf("Address calls %v, must not call %s", calls, bad)
		}
	}
	if slices.Contains(calls, "Sources/App.TaskRunner.run") {
		t.Errorf("Address calls %v, must not bind run to an extension of another type", calls)
	}
	for _, want := range []string{"Sources/App.ConfigReader.port", "Sources/App.Shiny.gleam", "run"} {
		if !slices.Contains(calls, want) {
			t.Errorf("Address calls %v, want %s", calls, want)
		}
	}
	for fn, want := range map[string]string{
		"Sources/App.Address.generic": "Sources/App.Strategy.lookup",
		"Sources/App.Address.any":     "Sources/App.Strategy.show",
		"Sources/App.Keyed.read":      "Sources/App.Strategy.render",
	} {
		if got := swCalls(t, ff, fn); !slices.Contains(got, want) {
			t.Errorf("%s calls %v, want %s", fn, got, want)
		}
	}
	// A name an if or guard condition casts is a value of the cast's type, and one
	// it unwraps keeps the optional's; the condition itself sees the name it shadows.
	for _, want := range []string{"Sources/App.ExpandCell.collapse", "Sources/App.ExpandCell.sibling", "Sources/App.ExpandCell.willDisplay", "Sources/App.SelectCell.willDisplay", "Sources/App.RowCell.willDisplay"} {
		if got := swCalls(t, ff, "Sources/App.Address.display"); !slices.Contains(got, want) {
			t.Errorf("Address.display calls %v, want %s", got, want)
		}
	}
}
