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
// type arguments or prefix, and `@2`, `@3` for further ones on the same type in source
// order. No name carries `#`.
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
	})
	for _, name := range []string{
		"lib._on_T@c", "lib._on_T@c.x", "lib._on_List@c", "lib._on_T@c@2", "lib._on_T@c@2.y",
		"lib._on_Foo@c", "lib._on_Bar@c", "lib._on_Function@c", "lib._on_Record@c", "lib._on_T@c@3",
	} {
		if findFact(all, facts.KindSymbol, name) == nil {
			t.Errorf("missing %s", name)
		}
	}
	for _, f := range all {
		if strings.Contains(f.Name, "#") {
			t.Errorf("name carries #: %s", f.Name)
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

// TestDartUnnamedExtensionInDottedFile: a dot in the file's base name becomes `_`, so
// the synthetic name stays one segment, and the extension is extracted and dispatched.
func TestDartUnnamedExtensionInDottedFile(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{
		"lib/user.repository.dart": `class T {}
extension on T { void z() {} }
extension on T { void w() {} }
void run(T t) { t.z(); }`,
		"lib/foo.g.dart": `class U {} extension on U { void v() {} }`,
	})
	for _, name := range []string{
		"lib._on_T@user_repository", "lib._on_T@user_repository.z",
		"lib._on_T@user_repository@2", "lib._on_T@user_repository@2.w",
		"lib._on_U@foo_g", "lib._on_U@foo_g.v",
	} {
		if findFact(all, facts.KindSymbol, name) == nil {
			t.Errorf("missing %s", name)
		}
	}
	if run := findFact(all, facts.KindSymbol, "lib.run"); !hasRelation(run, facts.RelCalls, "lib._on_T@user_repository.z") {
		t.Fatalf("t.z() did not bind in a dotted file: %+v", run)
	}
}
