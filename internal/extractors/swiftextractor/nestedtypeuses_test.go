package swiftextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A static path through a lower-case nested namespace (`Config.lens.features`) names
// the namespace, when a type of that name is declared in the type before it. A
// lower-case component that is a static member (`Router.shared.value`) names only the
// type before it, as before, and a namespace no path reaches is named by nothing.
func TestSwiftStaticPathThroughLowercaseNamespace(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"Sources/Lib/Lenses.swift": `
struct Config { let features: Int }
extension Config {
    enum lens { static let features = 1 }
}
struct Unused { let a: Int }
extension Unused {
    enum lens { static let a = 1 }
}
enum Router { static let shared = Router() }
struct Screen {
    func make() -> Int { Config.lens.features }
    func route() -> Int { Router.shared.hashValue }
}
`,
	})
	names := namesOf(ff)
	if !hasName(names, "Sources/Lib.Screen.make", "Sources/Lib.Config.lens") {
		t.Errorf("Config.lens.features names no Config.lens: %v", names["Sources/Lib.Screen.make"])
	}
	if !hasName(names, "Sources/Lib.Screen.route", "Sources/Lib.Router") {
		t.Errorf("Router.shared.hashValue no longer names Router: %v", names["Sources/Lib.Screen.route"])
	}
	for from, targets := range names {
		for _, to := range targets {
			if to == "Sources/Lib.Unused.lens" {
				t.Errorf("%s names Unused.lens, which nothing uses", from)
			}
		}
	}
}

// A type whose name starts with an underscore (`_CodingKey`) is a type: building it
// constructs the nested type the scope sees, not a top-level function of the name.
func TestSwiftUnderscoreTypeConstructorBindsThroughScope(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"Sources/Lib/Serializer.swift": `
struct Serializer {
    func keys(_ s: [String]) -> [_CodingKey] { s.map { _CodingKey(stringValue: $0) } }
    struct _CodingKey: CodingKey {
        var stringValue: String
        init(stringValue: String) { self.stringValue = stringValue }
        var intValue: Int? { nil }
        init?(intValue: Int) { nil }
    }
}
`,
	})
	if !ctorRelation(ff, "Sources/Lib.Serializer.keys", facts.RelInstantiates, "Sources/Lib.Serializer._CodingKey") {
		for _, f := range ff {
			if f.Name == "Sources/Lib.Serializer.keys" {
				t.Logf("relations: %v", f.Relations)
			}
		}
		t.Errorf("_CodingKey(...) does not construct the nested _CodingKey")
	}
}

// A conformance written through a qualified nested protocol
// (`extension Card: Dashboard.HasModel`) names that nested protocol.
func TestSwiftQualifiedConformanceNamesNestedProtocol(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"Sources/Lib/Dashboard.swift": `
final class Dashboard {
    protocol HasModel { var model: Int { get } }
    protocol Unconformed {}
}
struct Card { let model: Int }
extension Card: Dashboard.HasModel {}
`,
	})
	named, unconformed := false, false
	for _, f := range ff {
		for _, r := range f.Relations {
			if r.Kind != facts.RelNames {
				continue
			}
			named = named || r.Target == "Sources/Lib.Dashboard.HasModel"
			unconformed = unconformed || r.Target == "Sources/Lib.Dashboard.Unconformed"
		}
	}
	if !named {
		t.Errorf("the qualified conformance names no Dashboard.HasModel")
	}
	if unconformed {
		t.Errorf("Dashboard.Unconformed is named, though nothing conforms to it")
	}
}

// A type nested under the name of an associated type of a protocol the repository
// declares witnesses it, when its outer type conforms to that protocol directly or by
// an extension: the conformance names it. A nested type of that name in a type that
// does not conform, or of another name, is named by nothing.
func TestSwiftProjectAssociatedTypeWitnessIsNamed(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"Sources/Lib/Experiments.swift": `
protocol ExperimentProtocol {
    associatedtype Parameters: CaseIterable
    associatedtype Failure: Error = any Error
}
enum NullParameters: String, CaseIterable { case a }
struct NullExperiment: ExperimentProtocol {
    typealias Parameters = NullParameters
    typealias Extra = Int
}
struct Decoder {}
extension Decoder: ExperimentProtocol {
    enum Parameters: String, CaseIterable { case b }
    typealias Failure = Never
}
struct Plain {
    typealias Parameters = Int
}
`,
	})
	named := map[string]bool{}
	for _, f := range ff {
		for _, r := range f.Relations {
			if r.Kind == facts.RelNames {
				named[r.Target] = true
			}
		}
	}
	for _, n := range []string{"Sources/Lib.NullExperiment.Parameters", "Sources/Lib.Decoder.Parameters", "Sources/Lib.Decoder.Failure"} {
		if !named[n] {
			t.Errorf("witness %s is named by nothing", n)
		}
	}
	for _, n := range []string{"Sources/Lib.Plain.Parameters", "Sources/Lib.NullExperiment.Extra"} {
		if named[n] {
			t.Errorf("%s witnesses nothing but is named", n)
		}
	}
}

// A type nested in an extension of a type the project does not declare
// (`extension Date { struct Style {} }`) is named through that type's path:
// `Date.Style`, as a property type, a static read and a same-type constraint.
func TestSwiftTypeInExternalExtensionIsNamedThroughItsPath(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"Sources/Lib/Date+Style.swift": `
import Foundation
extension Date {
    struct Style { static let calendar = 1 }
    struct Strategy {}
    struct Unused {}
}
extension Date.Style {
    var parseStrategy: Date.Strategy { .init() }
}
extension FormatStyle where Self == Date.Style {
    static var style: Self { .init() }
}
func render() -> Int { Date.Style.calendar }
`,
	})
	names := namesOf(ff)
	if !hasName(names, "Sources/Lib.render", "Sources/Lib.Date.Style") {
		t.Errorf("render names %v, want Sources/Lib.Date.Style", names["Sources/Lib.render"])
	}
	found := false
	for from, targets := range names {
		for _, to := range targets {
			if to == "Sources/Lib.Date.Strategy" {
				found = true
			}
			if to == "Sources/Lib.Date.Unused" {
				t.Errorf("%s names Date.Unused, which nothing uses", from)
			}
		}
	}
	if !found {
		t.Errorf("nothing names Sources/Lib.Date.Strategy: %v", names)
	}
}
