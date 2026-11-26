package swiftextractor

import (
	"sort"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// namesOf returns the names edges of each fact, keyed by fact name.
func namesOf(ff []facts.Fact) map[string][]string {
	out := map[string][]string{}
	for _, f := range ff {
		for _, r := range f.Relations {
			if r.Kind == facts.RelNames {
				out[f.Name] = append(out[f.Name], r.Target)
			}
		}
	}
	for k := range out {
		sort.Strings(out[k])
	}
	return out
}

func hasName(names map[string][]string, from, to string) bool {
	for _, t := range names[from] {
		if t == to {
			return true
		}
	}
	return false
}

// A type written in a declaration or body is a use: an associated value, a
// constraint, an associatedtype's constraint, a closure parameter, a return,
// parameter or stored property type, a local annotation, a cast target, a protocol
// composition, a typealias's closure type, and the type path of a static read or of
// a nested type built through its owner.
func TestSwiftTypeUsesInDeclarationsAreNames(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"Sources/Lib/Types.swift": `
protocol Credential {}
protocol Inputs {}
protocol Outputs {}
struct Thing {}
struct Payload {}
struct Disposition {}
struct Format {}
struct Mode {}
struct Holder {}
struct Info { static let version = "1" }
struct Layer {}
enum Failure: Error {
    case reason(Payload)
    struct Unexpected: Error { init(_ n: Int) {} }
}
enum Colors { enum Accent { enum Green { static let pressed = 1 } } }
`,
		"Sources/Lib/Uses.swift": `
typealias LayerStyle = (Layer) -> Void

enum AuthState {
    case failed(Credential)
}

protocol Store {
    associatedtype Item: Thing
}

final class Client {
    var mode: Mode
    init(mode: Mode) { self.mode = mode }

    func refresh<C>(_ c: C, done: (Disposition) -> Void) -> Format where C: Credential {
        let h: Holder? = nil
        let p = h as? Payload
        _ = "\(Info.version)"
        _ = Colors.Accent.Green.pressed
        _ = Failure.Unexpected(1)
        let sort: Inputs & Outputs
        return Format()
    }

    func style(_ s: LayerStyle) {}

    func fire(config: Int = Defaults.current) {}
}

enum Defaults { static let current = 1 }
`,
	})
	names := namesOf(ff)
	for _, c := range []struct{ from, to string }{
		{"Sources/Lib.AuthState", "Sources/Lib.Credential"},
		{"Sources/Lib.Store", "Sources/Lib.Thing"},
		{"Sources/Lib.Client", "Sources/Lib.Mode"},
		{"Sources/Lib.Client.refresh", "Sources/Lib.Disposition"},
		{"Sources/Lib.Client.refresh", "Sources/Lib.Format"},
		{"Sources/Lib.Client.refresh", "Sources/Lib.Credential"},
		{"Sources/Lib.Client.refresh", "Sources/Lib.Holder"},
		{"Sources/Lib.Client.refresh", "Sources/Lib.Payload"},
		{"Sources/Lib.Client.refresh", "Sources/Lib.Info"},
		{"Sources/Lib.Client.refresh", "Sources/Lib.Colors.Accent.Green"},
		{"Sources/Lib.Client.refresh", "Sources/Lib.Failure.Unexpected"},
		{"Sources/Lib.Client.refresh", "Sources/Lib.Inputs"},
		{"Sources/Lib.Client.refresh", "Sources/Lib.Outputs"},
		{"Sources/Lib.Client.style", "Sources/Lib.LayerStyle"},
		{"Sources/Lib.Client.fire", "Sources/Lib.Defaults"},
		{"Sources/Lib.LayerStyle", "Sources/Lib.Layer"},
		{"Sources/Lib.Failure", "Sources/Lib.Payload"},
	} {
		if !hasName(names, c.from, c.to) {
			t.Errorf("%s: no names edge to %s; has %v", c.from, c.to, names[c.from])
		}
	}
}

// The name binds as Swift looks it up: a type nested in an enclosing type shadows a
// top-level one, a subclass sees its superclass's nested types, a generic parameter
// or associated type names no project type, and a type nested in an unrelated type
// is not visible by its bare name.
func TestSwiftTypeUseBindsThroughScope(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"Sources/Lib/Model.swift": `
struct Category {}
struct Element {}
struct Value {}
struct Other { struct Hidden {} }

struct Project {
    struct Category {}
    var category: Category
}

struct Box<Element> {
    func first() -> Element? { nil }
}

protocol Source {
    associatedtype Value
    func read() -> Value
}

struct Reader {
    func look(_ h: Hidden) {}
}

class Request {
    enum Disposition { case allow }
}

final class StreamRequest: Request {
    func respond(_ handler: (Disposition) -> Void) {}
}
`,
	})
	names := namesOf(ff)
	if !hasName(names, "Sources/Lib.Project", "Sources/Lib.Project.Category") ||
		hasName(names, "Sources/Lib.Project", "Sources/Lib.Category") {
		t.Errorf("Project: want the nested Category only, got %v", names["Sources/Lib.Project"])
	}
	if hasName(names, "Sources/Lib.Box.first", "Sources/Lib.Element") {
		t.Errorf("a generic parameter bound to the project type: %v", names["Sources/Lib.Box.first"])
	}
	if hasName(names, "Sources/Lib.Source", "Sources/Lib.Value") {
		t.Errorf("an associated type bound to the project type: %v", names["Sources/Lib.Source"])
	}
	if !hasName(names, "Sources/Lib.StreamRequest.respond", "Sources/Lib.Request.Disposition") {
		t.Errorf("a superclass's nested type is not seen from the subclass: %v", names["Sources/Lib.StreamRequest.respond"])
	}
	if len(names["Sources/Lib.Reader.look"]) != 0 {
		t.Errorf("a nested type of another type bound by its bare name: %v", names["Sources/Lib.Reader.look"])
	}
}

// The compiler's synthesized Codable conformance reads CodingKeys, whether the type
// conforms itself, through an extension or through a protocol refining Codable.
func TestSwiftSynthesizedCodingKeysAreRegistered(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"Sources/Lib/Models.swift": `
protocol Model: Decodable {}

struct Direct: Codable {
    enum CodingKeys: String, CodingKey { case a }
    let a: Int
}

struct ViaExtension {
    let b: Int
}

extension ViaExtension: Encodable {
    enum CodingKeys: String, CodingKey { case b }
}

struct ViaProtocol: Model {
    enum CodingKeys: String, CodingKey { case c }
    let c: Int
}

struct Plain {
    enum CodingKeys: String, CodingKey { case d }
}
`,
	})
	want := map[string]bool{
		"Sources/Lib.Direct.CodingKeys":       true,
		"Sources/Lib.ViaExtension.CodingKeys": true,
		"Sources/Lib.ViaProtocol.CodingKeys":  true,
		"Sources/Lib.Plain.CodingKeys":        false,
	}
	seen := map[string]bool{}
	for _, f := range ff {
		w, ok := want[f.Name]
		if !ok || f.Kind != facts.KindSymbol {
			continue
		}
		seen[f.Name] = true
		if got := f.PropBool("framework_registered"); got != w {
			t.Errorf("%s: framework_registered=%v, want %v", f.Name, got, w)
		}
	}
	for n := range want {
		if !seen[n] {
			t.Errorf("no fact %s", n)
		}
	}
}

// A constructor names the type the scope sees: a nested type before a top-level one,
// a nested type of a framework type's name (`State()`) too, and a member of an
// extension of a qualified type sees that type's nested types. A @Reducer type
// conforms to Reducer, as the macro makes it.
func TestSwiftConstructorBindsThroughScope(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"Sources/Lib/Ctor.swift": `
struct Failure: Error {}

struct Parser {
    struct Failure: Error {}
    func parse() throws { throw Failure() }
}

final class Handoff {
    struct State {}
    private let state = Box(State())
}

enum Formats {
    struct Strategy {
        enum ParseError: Error { case bad }
    }
}

extension Formats.Strategy {
    func parse() throws { throw ParseError.bad }
}

@Reducer
struct Feature {
    enum Action {}
}
`,
	})
	if !ctorRelation(ff, "Sources/Lib.Parser.parse", facts.RelInstantiates, "Sources/Lib.Parser.Failure") {
		t.Errorf("Parser.parse: want the nested Failure constructed")
	}
	names := namesOf(ff)
	if !hasName(names, "Sources/Lib.Handoff", "Sources/Lib.Handoff.State") {
		t.Errorf("Handoff: State() names no nested State: %v", names["Sources/Lib.Handoff"])
	}
	if !hasName(names, "Sources/Lib.Formats.Strategy.parse", "Sources/Lib.Formats.Strategy.ParseError") {
		t.Errorf("a qualified extension's member does not see the nested type: %v", names)
	}
	found := false
	for _, f := range ff {
		if f.Name == "Sources/Lib.Feature" && f.HasRelation(facts.RelImplements, "Reducer") {
			found = true
		}
	}
	if !found {
		t.Errorf("@Reducer type does not conform to Reducer")
	}
}

func ctorRelation(ff []facts.Fact, from, kind, to string) bool {
	for _, f := range ff {
		if f.Name == from && f.HasRelation(kind, to) {
			return true
		}
	}
	return false
}
