package swiftextractor

import (
	"context"
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
struct Badge {}
extension Params {
    init(badge: Query.Node.Badge) { self.flag = true }
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
	for _, f := range ff {
		if f.Name == "Sources/App.Params.init" && f.HasRelation(facts.RelInjects, "Sources/App.Badge") {
			t.Errorf("Params.init injects the module's Badge for a parameter typed Query.Node.Badge")
		}
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

// In a newly walked body, a member call on a receiver of no stated repository
// type binds nothing by bare name: `values.decode(...)` is the decoding
// container's, not the one repository method named decode. A call on a receiver
// whose type the source states still binds through that type.
func TestSwiftExtensionInitBindsMemberCallsByStatedTypeOnly(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"Sources/App/Category.swift": `
struct Category {
    static func decode(id: String) -> String { id }
}
final class Parser {
    func parse(_ s: String) -> Int { 0 }
}
`,
		"Sources/App/Params.swift": `
struct Params {
    let n: Int
}
extension Params: Decodable {
    init(from decoder: Decoder) throws {
        let values = try decoder.container(keyedBy: CodingKeys.self)
        let raw = try values.decode(String.self, forKey: .n)
        let parser: Parser = Parser()
        self.n = parser.parse(raw)
    }
}
`,
	})
	calls := swCalls(t, ff, "Sources/App.Params.init")
	if slices.Contains(calls, "Sources/App.Category.decode") || slices.Contains(calls, "decode") {
		t.Errorf("Params.init calls %v: values.decode is not Category.decode", calls)
	}
	if !slices.Contains(calls, "Sources/App.Parser.parse") {
		t.Errorf("Params.init calls %v, want Parser.parse through the stated type", calls)
	}
}

// A type name means the type of that name the file sees: its own module's, else an
// imported module's; never one in a target the file does not import.
func TestSwiftTypeNameResolvesToTheVisibleModule(t *testing.T) {
	manifest := `// swift-tools-version: 5.9
import PackageDescription

let package = Package(
    name: "Mods",
    targets: [
        .target(name: "Api"),
        .target(name: "Lib", dependencies: ["Api"]),
        .target(name: "Other"),
    ]
)
`
	repo := t.TempDir()
	files := map[string]string{
		"Package.swift":            "",
		"Sources/Api/User.swift":   "public struct User: Decodable { let id: Int }\npublic struct Envelope: Decodable {\n    let user: User\n    let note: Note\n    init(from decoder: Decoder) throws {\n        let c = try decoder.container(keyedBy: K.self)\n        self.user = try c.decode(User.self, forKey: .user)\n        self.note = try c.decode(Note.self, forKey: .note)\n    }\n}\n",
		"Sources/Lib/User.swift":   "import Api\npublic struct User { let name: String }\nstruct Shown { func show() { let e = Envelope.self } }\n",
		"Sources/Other/Note.swift": "public struct Note: Decodable { let t: String }\n",
		"Sources/Lib/Either.swift": "import Api\nstruct Either { init(r: Result<Int, Error>) {} }\n",
		"Sources/Api/Board.swift":  "public struct Board { public struct Result {} }\n",
	}
	var names []string
	for rel, content := range files {
		if rel == "Package.swift" {
			content = manifest
		}
		mustWrite(t, repo, rel, content)
		names = append(names, rel)
	}
	slices.Sort(names)
	ff, err := New().Extract(context.Background(), repo, names)
	if err != nil {
		t.Fatal(err)
	}
	var named []string
	for _, f := range ff {
		if f.Name == "Sources/Api.Envelope" {
			for _, r := range f.Relations {
				named = append(named, r.Kind+" "+r.Target)
			}
		}
	}
	if !slices.Contains(named, "names Sources/Api.User") {
		t.Errorf("Envelope relations %v, want its own module's User", named)
	}
	for _, bad := range []string{"names Sources/Lib.User", "names Sources/Other.Note"} {
		if slices.Contains(named, bad) {
			t.Errorf("Envelope relations %v: %s is in a target Api does not import", named, bad)
		}
	}
	for _, f := range ff {
		if f.Name == "Sources/Lib.Either" && f.HasRelation(facts.RelInjects, "Sources/Api.Result") {
			t.Errorf("Either relations %v: Api's Result is nested, not in scope by its bare name", f.Relations)
		}
		if f.Name == "Sources/Lib.Shown.show" && !f.HasRelation(facts.RelNames, "Sources/Api.Envelope") {
			t.Errorf("Shown relations %v, want the imported module's Envelope", f.Relations)
		}
	}
}
