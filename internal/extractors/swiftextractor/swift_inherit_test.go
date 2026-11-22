package swiftextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// classFactI builds a class symbol fact with implements edges to supertypes.
func classFactI(name string, supers ...string) facts.Fact {
	f := facts.Fact{Kind: facts.KindSymbol, Name: name, Props: map[string]any{"symbol_kind": facts.SymbolClass}}
	for _, s := range supers {
		f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelImplements, Target: s})
	}
	return f
}

// methodFactI builds a method symbol fact (with a receiver) that calls the given targets.
func methodFactI(name, receiver string, callTargets ...string) facts.Fact {
	f := facts.Fact{Kind: facts.KindSymbol, Name: name, Props: map[string]any{"symbol_kind": facts.SymbolMethod, "receiver": receiver}}
	for _, t := range callTargets {
		f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelCalls, Target: t})
	}
	return f
}

func callTargets(f facts.Fact) []string {
	var out []string
	for _, r := range f.Relations {
		if r.Kind == facts.RelCalls {
			out = append(out, r.Target)
		}
	}
	return out
}

func containsStr(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// End-to-end over real extractor output: a subclass calling an inherited base
// method leaves a dangling `pkg.foo` edge that resolveInheritedCalls binds to
// `pkg.A.foo`.
func TestResolveInheritedCalls_SubclassBaseMethod(t *testing.T) {
	src := "class A {\n  func foo() {}\n}\n" +
		"class B: A {\n  func bar() { foo() }\n}"
	ff := extractAST(t, src, false)
	// Before the pass, B.bar's call to inherited foo is a dangling top-level shape.
	b, ok := findFact(ff, "pkg.B.bar")
	if !ok {
		t.Fatalf("missing pkg.B.bar; got %v", ff)
	}
	if !containsStr(callTargets(b), "pkg.foo") {
		t.Fatalf("expected dangling pkg.foo before resolution; got %v", callTargets(b))
	}

	resolveInheritedCalls(ff, &swiftHierarchies{facts: ff})

	b, _ = findFact(ff, "pkg.B.bar")
	if !containsStr(callTargets(b), "pkg.A.foo") {
		t.Errorf("inherited foo() should resolve to pkg.A.foo; got %v", callTargets(b))
	}
	if containsStr(callTargets(b), "pkg.foo") {
		t.Errorf("dangling pkg.foo should be gone; got %v", callTargets(b))
	}
}

// A protocol-extension method called by a conformer resolves to dir.Proto.method.
func TestResolveInheritedCalls_ProtocolExtensionMethod(t *testing.T) {
	src := "protocol P {}\n" +
		"extension P {\n  func px() {}\n}\n" +
		"struct S: P {\n  func use() { px() }\n}"
	ff := extractAST(t, src, false)
	resolveInheritedCalls(ff, &swiftHierarchies{facts: ff})

	s, ok := findFact(ff, "pkg.S.use")
	if !ok {
		t.Fatalf("missing pkg.S.use; got %v", ff)
	}
	if !containsStr(callTargets(s), "pkg.P.px") {
		t.Errorf("protocol-extension px() should resolve to pkg.P.px; got %v", callTargets(s))
	}
}

func TestResolveInheritedCalls_NearestAncestorWins(t *testing.T) {
	// C : B : A; both A and B declare foo. C's dangling `foo` must bind to the
	// nearest ancestor B, not A.
	ff := []facts.Fact{
		classFactI("d.C", "B"),
		classFactI("d.B", "A"),
		classFactI("d.A"),
		methodFactI("d.A.foo", "A"),
		methodFactI("d.B.foo", "B"),
		methodFactI("d.C.run", "C", "foo"),
	}
	resolveInheritedCalls(ff, &swiftHierarchies{facts: ff})
	if got := callTargets(ff[5]); !containsStr(got, "d.B.foo") || containsStr(got, "foo") {
		t.Errorf("nearest-ancestor resolution: want d.B.foo, got %v", got)
	}
}

func TestResolveInheritedCalls_NoAncestorLeftAlone(t *testing.T) {
	// A dangling call whose short name no ancestor declares is left unchanged
	// (external/framework call).
	ff := []facts.Fact{
		classFactI("d.B", "A"),
		classFactI("d.A"),
		methodFactI("d.B.run", "B", "someFrameworkThing"),
	}
	resolveInheritedCalls(ff, &swiftHierarchies{facts: ff})
	if got := callTargets(ff[2]); !containsStr(got, "someFrameworkThing") {
		t.Errorf("unresolvable dangling call must be left as-is; got %v", got)
	}
}

func TestResolveInheritedCalls_SupertypeCycleTerminates(t *testing.T) {
	// A <-> B supertype cycle must not loop forever; foo declared on A resolves.
	ff := []facts.Fact{
		classFactI("d.A", "B"),
		classFactI("d.B", "A"),
		methodFactI("d.A.foo", "A"),
		methodFactI("d.B.run", "B", "foo"),
	}
	resolveInheritedCalls(ff, &swiftHierarchies{facts: ff}) // must terminate
	if got := callTargets(ff[3]); !containsStr(got, "d.A.foo") {
		t.Errorf("cycle-safe resolution: want d.A.foo, got %v", got)
	}
}

func TestResolveInheritedCalls_ResolvedEdgesUntouched(t *testing.T) {
	// An already-resolved edge (target is a real fact) must not be rewritten even if
	// an ancestor also declares that short name.
	ff := []facts.Fact{
		classFactI("d.B", "A"),
		classFactI("d.A"),
		methodFactI("d.A.foo", "A"),
		methodFactI("d.Other.foo", "Other"),
		methodFactI("d.B.run", "B", "d.Other.foo"), // already resolved to a real fact
	}
	resolveInheritedCalls(ff, &swiftHierarchies{facts: ff})
	if got := callTargets(ff[4]); !containsStr(got, "d.Other.foo") || containsStr(got, "d.A.foo") {
		t.Errorf("resolved edge must be untouched; want d.Other.foo, got %v", got)
	}
}
