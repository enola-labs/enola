package dartextractor

import (
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// TestDartUnnamedExtensionIsExtracted pins `extension on T { ... }`: it gets a
// library-private synthetic name, its members are symbols, its bodies are walked, and
// a call on T in the same library binds to its member.
func TestDartUnnamedExtensionIsExtracted(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{
		"lib/a.dart": `Object outline() => 1; class T { void ins(List l) {} }`,
		"lib/b.dart": `import 'a.dart';
extension on T { void a() { outline(); } }
void run(T t) { t.a(); }`,
	})
	ext := findFact(all, facts.KindSymbol, "lib._on_T@b")
	if ext == nil {
		t.Fatal("unnamed extension emitted no symbol")
	}
	if !ext.PropBool("anonymous") || ext.PropString("extension_on") != "T" || ext.PropBool("exported") {
		t.Fatalf("unnamed extension props: %+v", ext.Props)
	}
	if !hasRelation(ext, facts.RelDeclares, "lib._on_T@b.a") {
		t.Fatalf("container does not declare its member: %+v", ext.Relations)
	}
	if m := findFact(all, facts.KindSymbol, "lib._on_T@b.a"); !hasRelation(m, facts.RelCalls, "lib.outline") {
		t.Fatalf("member body not walked: %+v", m)
	}
	if run := findFact(all, facts.KindSymbol, "lib.run"); !hasRelation(run, facts.RelCalls, "lib._on_T@b.a") {
		t.Fatalf("t.a() did not bind to the unnamed extension: %+v", run)
	}
}

// TestDartUnnamedExtensionNames pins the synthetic names: the type's own name without
// type arguments or prefix, `#2` for a second one on the same type in source order, and
// nothing for a file whose base name has a dot, where the name would read as a member.
func TestDartUnnamedExtensionNames(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{
		"lib/c.dart": `import 'p.dart' as p;
extension on T { void x() {} }
extension on List<int> {}
extension on T { void y() {} }
extension<E> on Foo<E> {}
extension on p.Bar {}
extension on void Function() {}
extension on (int, String) {}
extension on T? {}`,
		"lib/user.repository.dart": `extension on T { void z() {} }`,
	})
	for _, name := range []string{
		"lib._on_T@c", "lib._on_T@c.x", "lib._on_List@c", "lib._on_T@c#2", "lib._on_T@c#2.y",
		"lib._on_Foo@c", "lib._on_Bar@c", "lib._on_Function@c", "lib._on_Record@c", "lib._on_T@c#3",
	} {
		if findFact(all, facts.KindSymbol, name) == nil {
			t.Errorf("missing %s", name)
		}
	}
	for _, f := range all {
		if f.Kind == facts.KindSymbol && strings.Contains(f.File, "user.repository") {
			t.Errorf("dotted file base name produced %s", f.Name)
		}
	}
}

// TestDartUnnamedExtensionIsLibraryPrivate: Dart cannot apply an unnamed extension
// outside its library, so a call there must not bind to it.
func TestDartUnnamedExtensionIsLibraryPrivate(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{
		"lib/b.dart":   `class T {} extension on T { void a() {} }`,
		"lib/use.dart": `import 'b.dart'; void run(T t) { t.a(); }`,
	})
	if run := findFact(all, facts.KindSymbol, "lib.run"); hasRelation(run, facts.RelCalls, "lib._on_T@b.a") {
		t.Fatalf("unnamed extension applied outside its library: %+v", run.Relations)
	}
}
