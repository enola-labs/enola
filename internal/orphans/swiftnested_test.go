package orphans

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func swiftSym(name, kind string, rels ...facts.Relation) facts.Fact {
	f := symRel(name, kind, rels...)
	f.File = "Sources/App/a.swift"
	f.SetProp(propLanguage, "swift")
	return f
}

// A Swift nested type is used only through its full name: a use of
// Colors.Background.Surface is not a use of Colors.Surface, and a use of one
// type's nested Entry does not make another type that declares an Entry used.
// Elsewhere the last-segment match stays.
func TestSwiftNestedTypeMatchesOnlyItsFullName(t *testing.T) {
	got := orphanNames(storeOf(
		swiftSym("App.Colors", facts.SymbolClass),
		swiftSym("App.Colors.Background", facts.SymbolStruct),
		swiftSym("App.Colors.Background.Surface", facts.SymbolStruct),
		swiftSym("App.Colors.Surface", facts.SymbolStruct),
		swiftSym("App.Envelope", facts.SymbolStruct),
		swiftSym("App.Envelope.Entry", facts.SymbolStruct),
		swiftSym("App.Board", facts.SymbolStruct),
		swiftSym("App.Board.Entry", facts.SymbolStruct),
		swiftSym("App.Entry", facts.SymbolStruct),
		swiftSym("App.View", facts.SymbolStruct,
			facts.Relation{Kind: facts.RelNames, Target: "App.Colors.Background.Surface"},
			facts.Relation{Kind: facts.RelNames, Target: "App.Board.Entry"}),
		symRel("src.Outer", facts.SymbolClass),
		symRel("src.Outer.Inner", facts.SymbolClass),
		symRel("src.User", facts.SymbolClass, facts.Relation{Kind: facts.RelInstantiates, Target: "Inner"}),
	))
	for _, n := range []string{"App.Colors.Surface", "App.Envelope", "App.Envelope.Entry", "App.Entry"} {
		if !got[n] {
			t.Errorf("%s is used by nothing and must be reported", n)
		}
	}
	for _, n := range []string{"App.Colors.Background.Surface", "App.Board.Entry", "App.Board", "src.Outer.Inner"} {
		if got[n] {
			t.Errorf("%s is used and must not be reported", n)
		}
	}
}

// A type nested under the name of an associated type of a protocol its outer type
// conforms to (Sequence's Element, Publisher's Failure) witnesses it.
func TestSwiftAssociatedTypeWitnessIsNotDead(t *testing.T) {
	seq := swiftSym("App.Chunks", facts.SymbolStruct, facts.Relation{Kind: facts.RelImplements, Target: "Sequence"})
	elem := swiftSym("App.Chunks.Element", facts.SymbolType)
	other := swiftSym("App.Plain", facts.SymbolStruct)
	stray := swiftSym("App.Plain.Element", facts.SymbolType)
	for _, f := range []*facts.Fact{&seq, &elem, &other, &stray} {
		f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelDeclares, Target: "App"})
	}
	got := orphanNames(storeOf(seq, elem, other, stray))
	if got["App.Chunks.Element"] {
		t.Error("Sequence's Element witness is reported")
	}
	if !got["App.Plain.Element"] {
		t.Error("an Element of a type that conforms to nothing must be reported")
	}
}

// A multipart part conformance stated by an extension (`extension File:
// MultipartPartConvertible { typealias Body = Data }`) witnesses the package
// protocol's Body.
func TestSwiftMultipartBodyWitnessIsNotDead(t *testing.T) {
	file := swiftSym("App.File", facts.SymbolStruct)
	ext := swiftSym("App.File+MultipartPartConvertible", facts.SymbolExtension, facts.Relation{Kind: facts.RelImplements, Target: "MultipartPartConvertible"})
	body := swiftSym("App.File.Body", facts.SymbolType)
	stray := swiftSym("App.Blob.Body", facts.SymbolType)
	for _, f := range []*facts.Fact{&file, &ext, &body, &stray} {
		f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelDeclares, Target: "App"})
	}
	got := orphanNames(storeOf(file, ext, body, stray))
	if got["App.File.Body"] {
		t.Error("MultipartPartConvertible's Body witness is reported")
	}
	if !got["App.Blob.Body"] {
		t.Error("a Body of a type that conforms to nothing must be reported")
	}
}
