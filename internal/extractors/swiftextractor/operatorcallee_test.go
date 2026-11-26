package swiftextractor

import (
	"slices"
	"testing"
)

// The grammar parses `a + f(x)` as a call whose callee is `a + f`, and `!f(x)` as a
// call whose callee is `!f`. The call is the right operand's, or the prefix
// operator's target's.
func TestSwiftCallUnderAnOperatorIsACall(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"Sources/App/Calc.swift": `
func g1(_ a: Int) -> Int { a }
func g2(_ a: Int) -> Int { a }
func g3(_ a: Int) -> Bool { true }
func g5(_ a: Int) -> Int { a }
func suffix(_ a: Int) -> String { "" }

final class Calc {
    func ok() -> Bool { true }
    func neg() -> Int { 1 }
    func p1(_ a: Int) -> Int { a + g1(a) }
    func p2(_ a: Int) -> Int { a * g2(a) }
    func p3(_ a: Int) -> Bool { !g3(a) }
    func p4(_ a: Int) -> Bool { !self.ok() }
    func p5(_ a: Int) -> String { "k" + "." + suffix(a) }
    func p6(_ a: Int) -> Int { -self.neg() - g5(a) }
}
`,
	})
	for fn, want := range map[string]string{
		"Sources/App.Calc.p1": "Sources/App.g1",
		"Sources/App.Calc.p2": "Sources/App.g2",
		"Sources/App.Calc.p3": "Sources/App.g3",
		"Sources/App.Calc.p4": "Sources/App.Calc.ok",
		"Sources/App.Calc.p5": "Sources/App.suffix",
		"Sources/App.Calc.p6": "Sources/App.g5",
	} {
		if calls := swCalls(t, ff, fn); !slices.Contains(calls, want) {
			t.Errorf("%s calls %v, want %s", fn, calls, want)
		}
	}
	if calls := swCalls(t, ff, "Sources/App.Calc.p6"); !slices.Contains(calls, "Sources/App.Calc.neg") {
		t.Errorf("p6 calls %v, want the negated self call too", calls)
	}
}

// `.member(...)` calls a static member of the type the context infers, or makes an
// enum case. Where the source states that type (a declared result, an annotation,
// self, a property's or a parameter's type), the call binds to that type's static
// method whose parameters take the labels the call writes. Nowhere else: not by
// name alone, not to an instance method, not to a method the labels do not fit,
// and not to the static factory the call is written in, which wraps a backing
// enum's case of the same name.
func TestSwiftImplicitMemberCallIsAUse(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"Sources/App/Types.swift": `
struct TLSConfiguration {
    static func inMemory(chain: [String], key: String? = nil) -> TLSConfiguration { TLSConfiguration() }
    static func pemFile(path: String) -> TLSConfiguration { TLSConfiguration() }
}
struct Address {
    enum Base { case ipv4(host: String), unix(path: String) }
    let base: Base
    static func ipv4(host: String) -> Address { Address(base: .ipv4(host: host)) }
    static func unix(path: String) -> Self { Self(base: .unix(path: path)) }
    static var local: Address { .unix(path: "/tmp/s") }
    static func loopback() -> Self { return .ipv4(host: "127.0.0.1") }
}
struct Auth {
    static func basic(realm: String) -> Auth { Auth() }
    static func bearer(realm: String) -> Auth { Auth() }
}
struct ServerConfig {
    var tls: TLSConfiguration?
    var address: Address
}
final class App {
    func start(value: Int) {}
}
func capture() -> Int { 1 }
`,
		"Sources/App/Server.swift": `
final class Server {
    var config: ServerConfig
    var tls: TLSConfiguration?
    func respond(to r: String, advertising auth: Auth) {}
    func configure(app: Server) {
        app.config.tls = .inMemory(chain: [])
        tls = .pemFile(path: "/k")
        let fallback: Address = .ipv4(host: "0.0.0.0")
        respond(to: "x", advertising: .basic(realm: "r"))
        let n = .capture()
        let r: Range = .start(value: 1)
        other.insert(.bearer(realm: "r"))
        let c: TLSConfiguration = .inMemory(certificates: [])
    }
}
`,
	})
	calls := swCalls(t, ff, "Sources/App.Server.configure")
	for _, want := range []string{
		"Sources/App.TLSConfiguration.inMemory", // a property's type, through another receiver
		"Sources/App.TLSConfiguration.pemFile",  // the enclosing type's property
		"Sources/App.Address.ipv4",              // an annotation
		"Sources/App.Auth.basic",                // a parameter's type
	} {
		if !slices.Contains(calls, want) {
			t.Errorf("configure calls %v, want %s", calls, want)
		}
	}
	for _, bad := range []string{"Sources/App.capture", "capture", "Sources/App.App.start", "start", "Sources/App.Auth.bearer", "bearer"} {
		if slices.Contains(calls, bad) {
			t.Errorf("configure calls %v: %s is not what its implicit member call names", calls, bad)
		}
	}
	// A property's getter calls in the name of the type that declares it.
	if calls := swCalls(t, ff, "Sources/App.Address"); !slices.Contains(calls, "Sources/App.Address.unix") {
		t.Errorf("Address calls %v, want Address.unix through local's declared type", calls)
	}
	if calls := swCalls(t, ff, "Sources/App.Address.loopback"); !slices.Contains(calls, "Sources/App.Address.ipv4") {
		t.Errorf("loopback calls %v, want Address.ipv4 through Self", calls)
	}
	for _, fn := range []string{"Sources/App.Address.ipv4", "Sources/App.Address.unix"} {
		if calls := swCalls(t, ff, fn); slices.Contains(calls, fn) {
			t.Errorf("%s calls %v: its implicit member is the backing enum's case", fn, calls)
		}
	}
	for _, f := range ff {
		for _, p := range []string{propImplicitCalls, propStaticLabels, propStatedType, propParamTypes} {
			if _, kept := f.Prop(p); kept {
				t.Errorf("%s: the walker's %s leaked into the facts", f.Name, p)
			}
		}
	}
}

// The type an implicit member call's context infers is stated through a tuple
// assignment's matching target, and through the right operand of `??`, whose type is
// that of the whole expression.
func TestSwiftImplicitMemberCallInTupleAndNilCoalescing(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"Sources/App/Source.swift": `
struct Source {
    static func capture(file: String = #file) -> Source { Source() }
    static func other() -> Source { Source() }
}
struct Status {
    static func ok() -> Status { Status() }
}
`,
		"Sources/App/Handler.swift": `
final class Handler {
    func handle(given: Source?) {
        let status: Status
        let source: Source
        (status, source) = (.ok(), given ?? .capture())
    }
    func plain() {
        let source: Source
        (_, source) = (1, .capture())
    }
}
`,
	})
	calls := swCalls(t, ff, "Sources/App.Handler.handle")
	for _, want := range []string{"Sources/App.Source.capture", "Sources/App.Status.ok"} {
		if !slices.Contains(calls, want) {
			t.Errorf("handle calls %v, want %s", calls, want)
		}
	}
	if slices.Contains(calls, "Sources/App.Source.other") {
		t.Errorf("handle calls %v: nothing calls other", calls)
	}
	if calls := swCalls(t, ff, "Sources/App.Handler.plain"); !slices.Contains(calls, "Sources/App.Source.capture") {
		t.Errorf("plain calls %v, want Sources/App.Source.capture", calls)
	}
}
