package orphans

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// swiftStore builds Swift symbols the way the Swift extractor emits them: a type's
// conformances are implements edges, a conformance extension is its own fact
// named Base+Proto, and a method carries its type in the receiver prop.
type swiftStore struct{ facts []facts.Fact }

const swiftDir = "Sources/App"

func (s *swiftStore) add(name, kind string, props map[string]any, rels ...facts.Relation) {
	p := map[string]any{"symbol_kind": kind, "language": "swift", "exported": true}
	for k, v := range props {
		p[k] = v
	}
	s.facts = append(s.facts, facts.Fact{Kind: facts.KindSymbol, Name: swiftDir + "." + name,
		File: swiftDir + "/a.swift", Props: p,
		Relations: append([]facts.Relation{{Kind: facts.RelDeclares, Target: swiftDir}}, rels...)})
}

func (s *swiftStore) typ(name string, conforms ...string) {
	var rels []facts.Relation
	for _, c := range conforms {
		rels = append(rels, implements(c))
	}
	s.add(name, facts.SymbolStruct, nil, rels...)
}

func (s *swiftStore) method(receiver, name string) {
	s.add(receiver+"."+name, facts.SymbolMethod, map[string]any{"receiver": receiver})
}

func (s *swiftStore) reported() map[string]bool {
	st := facts.NewStore()
	st.Add(s.facts...)
	st.BuildGraph()
	out := map[string]bool{}
	for _, o := range Detect(st) {
		out[o.Name[len(swiftDir)+1:]] = true
	}
	return out
}

// A member that satisfies a requirement of a protocol its type conforms to is
// called through the protocol by the standard library, a framework or a
// package. It is a root only when the type names that protocol and the protocol
// declares a requirement of that name; any other member of the same type is
// judged by its references.
func TestSwiftProtocolWitnessesAreRoots(t *testing.T) {
	var s swiftStore
	s.typ("Encoder.KeyedContainer", "KeyedEncodingContainerProtocol", "Sendable")
	s.method("Encoder.KeyedContainer", "encodeNil")
	s.method("Encoder.KeyedContainer", "superEncoder")
	s.method("Encoder.KeyedContainer", "helper")
	// A conformance declared by an extension covers the members of every
	// extension of the type.
	s.typ("Date.RFC1123FormatStyle", "Sendable")
	s.add("RFC1123FormatStyle+FormatStyle", facts.SymbolExtension, nil, implements("FormatStyle"))
	s.method("RFC1123FormatStyle", "format")
	s.method("RFC1123FormatStyle", "pad")
	// Sendable and Equatable declare no method that is called by name here.
	s.typ("Validator", "Sendable")
	s.method("Validator", "count")
	s.method("Validator", "format")
	// A Cocoa delegate's requirements are named after its delegating object.
	s.typ("AppDelegate", "UIResponder", "UIApplicationDelegate", "UNUserNotificationCenterDelegate")
	s.method("AppDelegate", "application")
	s.method("AppDelegate", "applicationDidBecomeActive")
	s.method("AppDelegate", "userNotificationCenter")
	s.method("AppDelegate", "configure")
	s.method("AppDelegate", "applicationsList")
	s.typ("SessionClient", "URLSessionDelegate")
	s.method("SessionClient", "urlSession")
	s.typ("Picker", "UITableViewDelegate")
	s.method("Picker", "tableView")
	s.method("Picker", "scrollViewDidScroll")
	// Xcode previews find a PreviewProvider by its conformance.
	s.typ("ContentView_Previews", "PreviewProvider")
	s.typ("Unused")
	wantReported(t, s.reported(), map[string]bool{
		"Encoder.KeyedContainer.encodeNil":       false,
		"Encoder.KeyedContainer.superEncoder":    false,
		"Encoder.KeyedContainer.helper":          true,
		"RFC1123FormatStyle.format":              false,
		"RFC1123FormatStyle.pad":                 true,
		"Validator.count":                        true,
		"Validator.format":                       true,
		"AppDelegate.application":                false,
		"AppDelegate.applicationDidBecomeActive": false,
		"AppDelegate.userNotificationCenter":     false,
		"AppDelegate.configure":                  true,
		"AppDelegate.applicationsList":           true,
		"SessionClient.urlSession":               false,
		"Picker.tableView":                       false,
		"Picker.scrollViewDidScroll":             false,
		"ContentView_Previews":                   false,
		"Unused":                                 true,
	})
}

// A protocol the repository declares is its own, whatever its name: calls through
// it are recorded, so the table does not apply and its conformers' members are
// judged by their references.
func TestSwiftLocalProtocolIsNotTheFrameworkOne(t *testing.T) {
	var s swiftStore
	s.add("FormatStyle", facts.SymbolInterface, nil)
	s.typ("Local", "FormatStyle")
	s.method("Local", "format")
	s.typ("Delegated", "UIApplicationDelegate")
	s.add("UIApplicationDelegate", facts.SymbolInterface, nil)
	s.method("Delegated", "application")
	wantReported(t, s.reported(), map[string]bool{
		"Local.format":          true,
		"Delegated.application": true,
	})
}
