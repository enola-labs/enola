package dartextractor

import (
	"github.com/enola-labs/enola/internal/facts"

	"reflect"
	"slices"
	"testing"
)

func TestDartEdgeReviewNullAssertion(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{"lib/a.dart": `import 'package:http/http.dart'; class A { void m() { httpClient.get('/'); } } void run(A? a,rows) {for(final row in rows) {a!.m();}}`})
	f := findFact(all, facts.KindSymbol, "lib.run")
	if !hasReviewCall(f, "lib.A.m") || !f.PropBool("performs_io") || !slices.Contains(f.Props["calls_in_loop"].([]string), "lib.A.m") {
		t.Fatalf("null assertion lost: %+v", f)
	}
}
func TestDartEdgeReviewPrefixedFunction(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{"lib/run.dart": `import 'barrel.dart' as u; void run() {u.helper();}`, "lib/barrel.dart": `export 'util.dart' show helper;`, "lib/util.dart": `import 'package:http/http.dart'; void helper() {httpClient.get('/');}`})
	f := findFact(all, facts.KindSymbol, "lib.run")
	if !hasReviewCall(f, "lib.helper") || !f.PropBool("performs_io") {
		t.Fatalf("prefixed function lost: %+v", f)
	}
}
func TestDartEdgeReviewTypedForIn(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{"lib/a.dart": `import 'package:http/http.dart'; class A {void m() {httpClient.get('/');}} void run(xs) {for(final A a in xs) {a.m();}}`})
	f := findFact(all, facts.KindSymbol, "lib.run")
	if !hasReviewCall(f, "lib.A.m") || !f.PropBool("performs_io") || !slices.Contains(f.Props["calls_in_loop"].([]string), "lib.A.m") {
		t.Fatalf("loop type lost: %+v", f)
	}
}
func TestDartEdgeReviewGetterOrder(t *testing.T) {
	var previous []facts.Relation
	for i := 0; i < 40; i++ {
		all := dartReviewFacts(t, map[string]string{"lib/a.dart": `class A { get a=>1;get b=>2;get c=>3;get d=>4; void run() { consume(a,b,c,d); } }`})
		f := findFact(all, facts.KindSymbol, "lib.A.run")
		if i > 0 && !reflect.DeepEqual(previous, f.Relations) {
			t.Fatalf("unstable relations: %v / %v", previous, f.Relations)
		}
		previous = f.Relations
	}
}
func TestDartEdgeReviewInferredConstruction(t *testing.T) {
	for _, expr := range []string{`A()`, `A.named()`, `new A()`, `const A()`} {
		all := dartReviewFacts(t, map[string]string{"lib/a.dart": `class A {const A(); A.named(); void m() {}} void run() {final a=` + expr + `;a.m();}`})
		if f := findFact(all, facts.KindSymbol, "lib.run"); !hasReviewCall(f, "lib.A.m") {
			t.Errorf("inference %s: %+v", expr, f)
		}
	}
}
func TestDartEdgeReviewExtension(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{"lib/run.dart": `import 'words.dart'; void run(String s) {s.shout();}`, "lib/words.dart": `extension Words on String {void shout() {}}`})
	if f := findFact(all, facts.KindSymbol, "lib.run"); !hasReviewCall(f, "lib.Words.shout") {
		t.Fatalf("extension lost: %+v", f)
	}
}
func TestDartEdgeReviewLexicalOwnerPrefix(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{"lib/run.dart": `import '../other/api.dart' as lib; class A {void m() {}} class B extends A {void run() {m();}}`, "other/api.dart": `class B {void m() {}}`})
	if f := findFact(all, facts.KindSymbol, "lib.B.run"); !hasReviewCall(f, "lib.A.m") {
		t.Fatalf("lexical owner treated as alias: %+v", f)
	}
}

func TestDartEdgeReviewConditionalTypeRelations(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{
		"foo.dart":    `class Foo {Foo();}`,
		"barrel.dart": `export 'foo.dart';`, "alt.dart": `export 'foo.dart';`,
		"lib/use.dart": `import '../barrel.dart' if (dart.library.io) '../alt.dart'; class Use implements Foo {Use(Foo foo); void run() {Foo();}}`,
	})
	for _, tc := range []struct{ source, kind string }{{"lib.Use", facts.RelImplements}, {"lib.Use.Use", facts.RelInjects}, {"lib.Use.run", facts.RelInstantiates}} {
		f := findFact(all, facts.KindSymbol, tc.source)
		found := false
		if f != nil {
			for _, rel := range f.Relations {
				if rel.Kind == tc.kind && rel.Target == "Foo" {
					found = true
				}
			}
		}
		if !found {
			t.Errorf("conditional type relation lost: %s %s %+v", tc.source, tc.kind, f)
		}
	}
}
func TestDartEdgeReviewUnresolvedPackageURI(t *testing.T) {
	all := append(walkSource(t, "lib/foo.dart", `class Foo {void m() {}}`), walkSource(t, "lib/use.dart", `import 'package:review_app/foo.dart'; class Use implements Foo {Use(Foo foo); void run(Foo foo) {foo.m();}}`)...)
	// Simulate cached dependency metadata without a resolved_file. Declaration
	// package identity is sufficient to recover this explicit package URI.
	for i := range all {
		if all[i].Name == "lib.Foo" {
			all[i].SetProp("pub_package", "review_app")
		}
	}
	all = resolveCallTargets(all)
	if f := findFact(all, facts.KindSymbol, "lib.Use.run"); !hasReviewCall(f, "lib.Foo.m") {
		t.Fatalf("package URI not recovered: %+v", f)
	}
}
func TestDartEdgeReviewPrefixedFunctionVisibility(t *testing.T) {
	for _, spec := range []string{`as u hide helper`, `as u show other`} {
		all := dartReviewFacts(t, map[string]string{"lib/use.dart": `import 'util.dart' ` + spec + `;void run() {u.helper();}`, "lib/util.dart": `void helper() {}`})
		if f := findFact(all, facts.KindSymbol, "lib.run"); hasReviewCall(f, "lib.helper") {
			t.Fatalf("hidden function resolved: %+v", f)
		}
	}
	all := dartReviewFacts(t, map[string]string{"lib/use.dart": `import '../other/util.dart' as lib; void helper() {} void run() {helper(); lib.helper();}`, "other/util.dart": `void helper() {}`})
	f := findFact(all, facts.KindSymbol, "lib.run")
	if !hasReviewCall(f, "lib.helper") || !hasReviewCall(f, "other.helper") {
		t.Fatalf("function canonical name mistaken for prefix: %+v", f)
	}
}
func TestDartEdgeReviewInferredFactoryIsNotConstructor(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{"lib/a.dart": `class A {static dynamic make()=>external;void m() {}} void run() {final a=A.make(); a.m();}`})
	if f := findFact(all, facts.KindSymbol, "lib.run"); hasReviewCall(f, "lib.A.m") {
		t.Fatalf("unknown factory result inferred as A: %+v", f)
	}
}
func TestDartEdgeReviewExtensionVisibility(t *testing.T) {
	for _, tc := range []struct{ imports, typ string }{{"", "String"}, {`import 'words.dart' hide Words;`, "String"}, {`import 'words.dart';`, "int"}, {`import 'words.dart';import 'other.dart';`, "String"}} {
		all := dartReviewFacts(t, map[string]string{"lib/use.dart": tc.imports + `void run(` + tc.typ + ` s) {s.shout();}`, "lib/words.dart": `extension Words on String {void shout() {}}`, "other/other.dart": `extension Other on String {void shout() {}}`, "lib/other.dart": `export '../other/other.dart';`})
		if f := findFact(all, facts.KindSymbol, "lib.run"); hasReviewCall(f, "lib.Words.shout") || hasReviewCall(f, "other.Other.shout") {
			t.Fatalf("inapplicable/ambiguous extension resolved: %+v", f)
		}
	}
}

func TestDartEdgeReviewInferredExtension(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{"lib/a.dart": `class A {A();} extension Helpers on A {void extra() {}} void run() {final a=A();a.extra();}`})
	if f := findFact(all, facts.KindSymbol, "lib.run"); !hasReviewCall(f, "lib.Helpers.extra") {
		t.Fatalf("constructor extension did not resolve after verification: %+v", f)
	}
}

func TestDartEdgeReviewExportCombinatorsAndPrecedence(t *testing.T) {
	for _, barrel := range []string{`export '../other/util.dart' show other; void helper() {}`, `export '../other/util.dart'; void helper() {}`, `export '../other/util.dart' hide helper; export 'own.dart' show helper;`} {
		all := dartReviewFacts(t, map[string]string{"lib/use.dart": `import 'barrel.dart' as u; void run() {u.helper();}`, "lib/barrel.dart": barrel, "lib/own.dart": `void helper() {}`, "other/util.dart": `void helper() {} void other() {}`})
		f := findFact(all, facts.KindSymbol, "lib.run")
		if !hasReviewCall(f, "lib.helper") || hasReviewCall(f, "other.helper") {
			t.Fatalf("export filter/precedence failed: %+v", f)
		}
	}
}
func TestDartEdgeReviewGeneratedOwnConstructor(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{"lib/a.dart": `class $Table {$Table(); $Table alias() {return $Table();}}`})
	if f := findFact(all, facts.KindSymbol, "lib.$Table.alias"); !hasReviewCall(f, "lib.$Table.$Table") {
		t.Fatalf("verified own constructor lost: %+v", f)
	}
}
