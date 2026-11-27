package swiftextractor

import (
	"slices"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func symbolNames(ff []facts.Fact) []string {
	var out []string
	for _, f := range ff {
		if f.Kind == facts.KindSymbol {
			out = append(out, f.Name)
		}
	}
	return out
}

func targetsFrom(ff []facts.Fact, from string, kind string) []string {
	var out []string
	for _, f := range ff {
		if f.Name != from {
			continue
		}
		for _, r := range f.Relations {
			if r.Kind == kind {
				out = append(out, r.Target)
			}
		}
	}
	return out
}

// A member of a constrained extension is named with the base name of its file, so
// the same member name declared under different constraints in different files is
// one fact per declaration. Members of plain extensions and of the type keep their
// names, and a second constrained extension in one file declaring the name again
// counts on (`@2`), while overloads in one extension share their name.
func TestSwiftConstrainedExtensionMembersAreFileQualified(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"Sources/Lib/Lens.swift": `
struct Lens<Whole, Part> {
    let get: (Whole) -> Part
    var name: String { "" }
}
extension Lens {
    var plain: Int { 0 }
}
`,
		"Sources/Lib/Project.Lenses.swift": `
struct Project {}
struct User {}
extension Lens where Whole == Project, Part == Int {
    var id: Int { 0 }
    func make(a: Int) {}
    func make(b: Int) {}
}
extension Lens where Whole == User, Part == Int {
    var id: Int { 0 }
}
`,
		"Sources/Lib/Draft.swift": `
struct Draft {}
extension Lens where Whole == Draft {
    var id: Int { 0 }
}
extension Array<Draft> {
    func total() -> Int { 0 }
}
`,
	})
	names := symbolNames(ff)
	for _, want := range []string{
		"Sources/Lib.Lens.id@Project_Lenses_swift",
		"Sources/Lib.Lens.id@Project_Lenses_swift@2",
		"Sources/Lib.Lens.id@Draft_swift",
		"Sources/Lib.Lens.make@Project_Lenses_swift",
		"Sources/Lib.Array.total@Draft_swift",
		"Sources/Lib.Lens.plain",
		"Sources/Lib.Lens.name",
	} {
		if !slices.Contains(names, want) {
			t.Errorf("missing %s in %v", want, names)
		}
	}
	for _, bad := range []string{"Sources/Lib.Lens.id", "Sources/Lib.Lens.make", "Sources/Lib.Lens.make@Project_Lenses_swift@2"} {
		if slices.Contains(names, bad) {
			t.Errorf("unexpected %s", bad)
		}
	}
	n := 0
	for _, name := range names {
		if name == "Sources/Lib.Lens.make@Project_Lenses_swift" {
			n++
		}
	}
	if n != 2 {
		t.Errorf("overloads of make in one extension: %d facts, want 2 sharing a name", n)
	}
}

// A use of a constrained-extension member binds to the declaration in its own
// extension, else its own file, else the only declaration of the name; a use the
// name leaves ambiguous across files draws no edge rather than an arbitrary one.
func TestSwiftConstrainedExtensionMemberUsesBindByFile(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"Sources/Lib/Lens.swift": `
struct Lens<Whole, Part> {}
struct Project {}
struct User {}
`,
		"Sources/Lib/A.swift": `
extension Lens where Whole == Project {
    static func shared() -> Int { 0 }
    static func onlyA() -> Int { 0 }
    func run() -> Int { Lens.shared() }
}
func useA() -> Int { Lens.shared() }
`,
		"Sources/Lib/B.swift": `
extension Lens where Whole == User {
    static func shared() -> Int { 0 }
}
`,
		"Sources/Lib/C.swift": `
func useC() -> Int { Lens.shared() + Lens.onlyA() }
func useP(p: Pager<Int>) { p.first(params: 1) }
`,
		"Sources/Lib/Pager.swift": `
struct Pager<P> {
    func first(params: P) {}
}
extension Pager where P == Void {
    func first() { self.first(params: ()) }
}
`,
	})
	// An overload a constrained extension adds beside the type's own member leaves
	// the edge on the type's member, from its own extension too.
	if got := targetsFrom(ff, "Sources/Lib.Pager.first@Pager_swift", facts.RelCalls); !slices.Contains(got, "Sources/Lib.Pager.first") {
		t.Errorf("the Void overload calls %v, want the type's own first", got)
	}
	if got := targetsFrom(ff, "Sources/Lib.useP", facts.RelCalls); !slices.Contains(got, "Sources/Lib.Pager.first") {
		t.Errorf("useP calls %v, want the type's own first", got)
	}
	if got := targetsFrom(ff, "Sources/Lib.Lens.run@A_swift", facts.RelCalls); !slices.Contains(got, "Sources/Lib.Lens.shared@A_swift") {
		t.Errorf("run calls %v, want its own extension's shared", got)
	}
	if got := targetsFrom(ff, "Sources/Lib.useA", facts.RelCalls); !slices.Contains(got, "Sources/Lib.Lens.shared@A_swift") {
		t.Errorf("useA calls %v, want the shared of its own file", got)
	}
	got := targetsFrom(ff, "Sources/Lib.useC", facts.RelCalls)
	if !slices.Contains(got, "Sources/Lib.Lens.onlyA@A_swift") {
		t.Errorf("useC calls %v, want the only onlyA", got)
	}
	for _, g := range got {
		if g == "Sources/Lib.Lens.shared" || g == "Sources/Lib.Lens.shared@A_swift" || g == "Sources/Lib.Lens.shared@B_swift" {
			t.Errorf("useC calls %s: shared is declared in two files, so no edge", g)
		}
	}
}
