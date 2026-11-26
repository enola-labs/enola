package swiftextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func callsFrom(ff []facts.Fact, from string) []string {
	var out []string
	for _, f := range ff {
		if f.Name != from {
			continue
		}
		for _, r := range f.Relations {
			if r.Kind == facts.RelCalls {
				out = append(out, r.Target)
			}
		}
	}
	return out
}

// A declaration in an extension of a qualified type is named through the path the
// extension writes, as if declared inside that type, so a use written through the
// full path reaches it. A leading module, imported or the file's own, is no part of the path, unless a
// type of its name shadows it.
func TestSwiftExtensionOfNestedTypeNamesMembersByFullPath(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"Sources/Lib/Fields.swift": `
import Foundation
extension Fields {
    struct Range {
        let ranges: [Fields.Range.Value]
        func first() -> Fields.Range.Value? { Fields.Range.Value.from("x") }
    }
}
extension Fields.Range {
    enum Value {
        case tail(Int)
        static func from(_ s: String) -> Fields.Range.Value? { nil }
    }
    func count() -> Int { 0 }
}
extension Foundation.URLSessionWebSocketTask.Message {
    func text() -> String { "" }
}
extension Lib.Fields {
    func own() {}
}
`,
		"Sources/Player/Player.swift": `
struct Player {}
extension Player.Sound {
    func play() {}
}
`,
		"Sources/Lib/Base.swift": `
struct Fields {}
`,
	})
	for _, want := range []string{
		"Sources/Lib.Fields.Range.Value",
		"Sources/Lib.Fields.Range.Value.from",
		"Sources/Lib.Fields.Range.count",
		"Sources/Lib.URLSessionWebSocketTask.Message.text",
		"Sources/Lib.Fields.own",
		"Sources/Player.Player.Sound.play",
	} {
		if _, ok := findFact(ff, want); !ok {
			t.Errorf("no fact %s", want)
		}
	}
	for _, gone := range []string{"Sources/Lib.Range.Value", "Sources/Lib.Range.count", "Sources/Lib.Foundation.URLSessionWebSocketTask.Message.text", "Sources/Lib.Lib.Fields.own", "Sources/Player.Sound.play"} {
		if _, ok := findFact(ff, gone); ok {
			t.Errorf("fact %s still named by the last component", gone)
		}
	}
	names := namesOf(ff)
	if !hasName(names, "Sources/Lib.Fields.Range", "Sources/Lib.Fields.Range.Value") {
		t.Errorf("Fields.Range does not name Fields.Range.Value: %v", names["Sources/Lib.Fields.Range"])
	}
	found := false
	for _, c := range callsFrom(ff, "Sources/Lib.Fields.Range.first") {
		found = found || c == "Sources/Lib.Fields.Range.Value.from"
	}
	if !found {
		t.Errorf("Fields.Range.first calls %v, want Fields.Range.Value.from", callsFrom(ff, "Sources/Lib.Fields.Range.first"))
	}
}

// The types a where-clause of an extension states are names of the members the
// extension declares, and of its conformance facts: an extension of a protocol the
// project does not declare has no other fact to carry them. The constrained names
// (Self, generic parameters, associated types) name nothing.
func TestSwiftExtensionWhereClauseNamesItsMembers(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"Sources/Lib/Style.swift": `
extension Date {
    struct RFCStyle {}
}
struct Element {}
protocol Marker {}
extension FormatStyle where Self == Date.RFCStyle {
    static var rfc: Self { .init() }
}
extension Array where Element: Marker {
    func marked() -> Int { 0 }
}
extension Collection: Codable where Element == Date.RFCStyle {}
`,
	})
	names := namesOf(ff)
	if !hasName(names, "Sources/Lib.FormatStyle.rfc", "Sources/Lib.Date.RFCStyle") {
		t.Errorf("FormatStyle.rfc names %v, want Date.RFCStyle", names["Sources/Lib.FormatStyle.rfc"])
	}
	if !hasName(names, "Sources/Lib.Array.marked", "Sources/Lib.Marker") {
		t.Errorf("Array.marked names %v, want Marker", names["Sources/Lib.Array.marked"])
	}
	if hasName(names, "Sources/Lib.Array.marked", "Sources/Lib.Element") {
		t.Errorf("the constrained associated type Element bound to the project's Element")
	}
	if !hasName(names, "Sources/Lib.Collection+Codable", "Sources/Lib.Date.RFCStyle") {
		t.Errorf("Collection+Codable names %v, want Date.RFCStyle", names["Sources/Lib.Collection+Codable"])
	}
}
