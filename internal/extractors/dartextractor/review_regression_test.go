package dartextractor

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/perf"
)

func dartReviewFacts(t *testing.T, files map[string]string) []facts.Fact {
	t.Helper()
	var all []facts.Fact
	for file, src := range files {
		all = append(all, walkSource(t, file, src)...)
	}
	all = resolveCallTargets(all)
	computeDartPerformsIO(all)
	return all
}

func TestDartReviewLoopReceiverEvidence(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		{"typed field", `class AppDatabase {} class Work { final AppDatabase _db; Work(this._db); void run(List<int> rows) { for(final row in rows) { _db.select(row); } } }`},
		{"untyped local", `void run(List<int> rows) { final db = external; for(final row in rows) { db.select(row); } }`},
		{"typed dio", `class Dio {} void run(Dio dio,List<int> rows) { for(final row in rows) { dio.get(row); } }`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			all := dartReviewFacts(t, map[string]string{"lib/work.dart": tc.src})
			store := facts.NewStore()
			store.Add(all...)
			for _, roundTrip := range []bool{false, true} {
				if roundTrip {
					var buf bytes.Buffer
					if err := store.WriteJSONL(&buf); err != nil {
						t.Fatal(err)
					}
					store = facts.NewStore()
					if err := store.ReadJSONL(&buf); err != nil {
						t.Fatal(err)
					}
				}
				found := false
				for _, f := range perf.Analyze(store) {
					if f.Kind == "call-in-loop" {
						found = true
					}
				}
				if !found {
					t.Fatalf("extractor-produced loop lost I/O receiver evidence (round trip=%v)", roundTrip)
				}
			}
		})
	}
}

func TestDartReviewChainedIO(t *testing.T) {
	for _, tc := range []struct{ call, uri string }{
		{`_supabase.from('t').select()`, `package:supabase/supabase.dart`},
		{`_db.select(t).get()`, `package:drift/drift.dart`},
		{`this._db.select(t).get()`, `package:drift/drift.dart`},
		{`FirebaseFirestore.instance.collection('t').get()`, `package:cloud_firestore/cloud_firestore.dart`},
	} {
		t.Run(tc.call, func(t *testing.T) {
			all := dartReviewFacts(t, map[string]string{"lib/service.dart": `import '` + tc.uri + `'; class Work { void run(List<int> rows) { for(final row in rows) {` + tc.call + `;} } }`})
			if f := findFact(all, facts.KindSymbol, "lib.Work.run"); f == nil || !f.PropBool("io_direct") || !f.PropBool("performs_io") {
				t.Fatalf("chained I/O lost: %+v", f)
			}
			store := facts.NewStore()
			store.Add(all...)
			found := false
			for _, f := range perf.Analyze(store) {
				if f.Symbol == "lib.Work.run" && f.Kind == "call-in-loop" {
					found = true
				}
			}
			if !found {
				t.Fatalf("chain has no per-iteration I/O finding: %s", tc.call)
			}
		})
	}
}

func TestDartReviewInheritedCalls(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{"lib/repo.dart": `
import 'package:http/http.dart';
class BaseRepo { load() { httpClient.get('/data'); } }
class UserRepo extends BaseRepo { void run() { load(); this.load(); } }
class Unrelated { void load() {} }
void run(UserRepo repo) { repo.load(); }
abstract class ContractOnly implements BaseRepo {}
void notInherited(ContractOnly repo) { repo.load(); }
`})
	for _, name := range []string{"lib.UserRepo.run", "lib.run"} {
		f := findFact(all, facts.KindSymbol, name)
		if !hasRelation(f, facts.RelCalls, "lib.BaseRepo.load") || !f.PropBool("performs_io") {
			t.Errorf("inherited implementation lost: %+v", f)
		}
	}
	if f := findFact(all, facts.KindSymbol, "lib.notInherited"); f.PropBool("performs_io") || hasRelation(f, facts.RelCalls, "lib.BaseRepo.load") {
		t.Errorf("interface conformance inherited an implementation: %+v", f)
	}
}

func TestDartReviewDeduplicateResolvedCalls(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{"lib/user.dart": `class User { foo() {} void run(User other) { for(final x in values) { foo(); other.foo(); } } }`})
	f := findFact(all, facts.KindSymbol, "lib.User.run")
	count := 0
	for _, r := range f.Relations {
		if r.Kind == facts.RelCalls && r.Target == "lib.User.foo" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("resolved call appears %d times: %+v", count, f.Relations)
	}
	calls, _ := f.Props["calls_in_loop"].([]string)
	if len(calls) != 1 || calls[0] != "lib.User.foo" {
		t.Fatalf("loop metrics not deduplicated: %v", calls)
	}
}

func TestDartReviewInitializingFormalKeepsFieldType(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{"lib/repo.dart": `
import 'package:http/http.dart';
class Repo { void load() { httpClient.get('/data'); } }
class Foo { final Repo repo; Foo(this.repo) { repo.load(); } }
`})
	f := findFact(all, facts.KindSymbol, "lib.Foo.Foo")
	if !hasRelation(f, facts.RelCalls, "lib.Repo.load") || !f.PropBool("performs_io") {
		t.Fatalf("initializing formal erased field type: %+v", f)
	}
}

func TestDartReviewGetterSetterIO(t *testing.T) {
	for _, getterIO := range []bool{true, false} {
		getter, setter := "", ""
		if getterIO {
			getter = `httpClient.get('/data');`
		} else {
			setter = `httpClient.post('/data');`
		}
		all := dartReviewFacts(t, map[string]string{"lib/accessor.dart": `import 'package:http/http.dart'; class A { Function get value { ` + getter + ` return () {}; } set value(Function next) { ` + setter + ` } } void run(A a) { a.value(); }`})
		f := findFact(all, facts.KindSymbol, "lib.run")
		if !hasRelation(f, facts.RelCalls, "lib.A.value") || f.PropBool("performs_io") != getterIO {
			t.Fatalf("getter/setter read selection incorrect (getter I/O %v): %+v", getterIO, f)
		}
	}
}

func TestDartReviewInternalPrefixedType(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{
		"lib/client.dart":   `import 'package:http/http.dart'; class Client { fetch() { httpClient.get('/data'); } }`,
		"other/client.dart": `class Client { void fetch() {} }`,
		"lib/barrel.dart":   `export 'client.dart' show Client;`,
		"lib/use.dart":      `import 'barrel.dart' as api show Client; void run(api.Client c) { c.fetch(); }`,
		"lib/hidden.dart":   `import 'client.dart' as api hide Client; void run(api.Client c) { c.fetch(); }`,
	})
	f := findFact(all, facts.KindSymbol, "lib.run") // two same-directory functions; find by file below
	for i := range all {
		if all[i].File == "lib/use.dart" && all[i].Name == "lib.run" {
			f = &all[i]
		}
	}
	if !hasRelation(f, facts.RelCalls, "lib.Client.fetch") || !f.PropBool("performs_io") {
		t.Fatalf("project prefix/barrel unresolved: %+v", f)
	}
	for _, f := range all {
		if f.File == "lib/hidden.dart" && f.Name == "lib.run" && hasRelation(&f, facts.RelCalls, "lib.Client.fetch") {
			t.Fatal("hidden import was resolved")
		}
	}
}

func TestDartReviewGetterReadsAndWrites(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{"lib/accessor.dart": `
import 'package:http/http.dart';
class A {
 Object get value {httpClient.get('/value');return 1;}
 set value(Object next) {}
 Object localRead() => value;
}
Object read(A a) => a.value;
void write(A a) {a.value=1;}
void loop(A a,List<int> rows) {for(final row in rows) {consume(a.value);}}
`})
	for _, name := range []string{"lib.read", "lib.A.localRead", "lib.loop"} {
		f := findFact(all, facts.KindSymbol, name)
		if !hasRelation(f, facts.RelCalls, "lib.A.value") || !f.PropBool("performs_io") {
			t.Errorf("getter read lost I/O: %+v", f)
		}
	}
	if f := findFact(all, facts.KindSymbol, "lib.write"); hasRelation(f, facts.RelCalls, "lib.A.value") || f.PropBool("performs_io") {
		t.Errorf("setter write executed getter: %+v", f)
	}
	store := facts.NewStore()
	store.Add(all...)
	found := false
	for _, f := range perf.Analyze(store) {
		if f.Symbol == "lib.loop" && f.Kind == "call-in-loop" {
			found = true
		}
	}
	if !found {
		t.Fatalf("getter I/O in loop has no N+1 finding: loop=%+v findings=%+v", findFact(all, facts.KindSymbol, "lib.loop"), perf.Analyze(store))
	}
}

func TestDartReviewMixinAndInheritanceAmbiguity(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{"lib/types.dart": `
class Base { void load() {} }
mixin First { void load() {} }
mixin Last { void load() {} }
class User extends Base with First,Last { void run() {load();} }
class CycleA extends CycleB {}
class CycleB extends CycleA {}
void cycle(CycleA a) {a.load();}
`})
	if f := findFact(all, facts.KindSymbol, "lib.User.run"); !hasRelation(f, facts.RelCalls, "lib.Last.load") {
		t.Fatalf("mixin precedence lost: %+v", f)
	}
	if f := findFact(all, facts.KindSymbol, "lib.cycle"); hasRelation(f, facts.RelCalls, "lib.Base.load") {
		t.Fatal("inheritance cycle fell back to an unrelated method")
	}
}

func TestDartReviewAmbiguousGetterDoesNotSupplyLoopIO(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{
		"lib/a.dart": `import 'package:http/http.dart'; class A { Function get value {httpClient.get('/data');return () {};} void run(List<int> rows) {for(final row in rows) {value();}} }`,
		"lib/b.dart": `class A { Function get value => () {}; }`,
	})
	for _, f := range all {
		if f.Name == "lib.A.run" && f.PropBool("performs_io") {
			t.Fatal("ambiguous getter propagated I/O")
		}
	}
	store := facts.NewStore()
	store.Add(all...)
	for _, f := range perf.Analyze(store) {
		if f.Symbol == "lib.A.run" && f.Kind == "call-in-loop" {
			t.Fatalf("analyzer guessed an ambiguous getter: %+v", f)
		}
	}
}

func TestDartReviewPackagePrefixResolution(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"pubspec.yaml":      "name: review_app\n",
		"lib/client.dart":   `import 'package:http/http.dart'; class Client { fetch() { httpClient.get('/data'); } }`,
		"other/client.dart": `class Client { void fetch() {} }`,
		"lib/use.dart":      `import 'package:review_app/client.dart' as api; void run(api.Client c) {c.fetch();}`,
	}
	var paths []string
	for name, src := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), []byte(src), 0644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, name)
	}
	all, err := New().Extract(context.Background(), root, paths)
	if err != nil {
		t.Fatal(err)
	}
	f := findFact(all, facts.KindSymbol, "lib.run")
	if !hasRelation(f, facts.RelCalls, "lib.Client.fetch") || !f.PropBool("performs_io") {
		t.Fatalf("internal package import prefix did not resolve: %+v", f)
	}
}

func TestDartReviewConstructedAndSuperReceivers(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{"lib/types.dart": `
import 'package:http/http.dart';
class Base { const Base(); load() {httpClient.get('/load');} }
class Repo<T> extends Base { const Repo(); Repo.named(); void run() {super.load();} }
void run() {Repo().load(); Repo<int>().load(); Repo.named().load(); new Repo().load(); const Repo().load();}
void unknown(dynamic Repo) {Repo().load();}
class Factory { static dynamic make() => Base(); void load() {httpClient.get('/wrong');} }
void notConstructed() {Factory.make().load();}
`})
	for _, name := range []string{"lib.run", "lib.Repo.run"} {
		f := findFact(all, facts.KindSymbol, name)
		if !hasRelation(f, facts.RelCalls, "lib.Base.load") || !f.PropBool("performs_io") {
			t.Errorf("known constructed/super receiver unresolved: %+v", f)
		}
	}
	for _, name := range []string{"lib.unknown", "lib.notConstructed"} {
		f := findFact(all, facts.KindSymbol, name)
		if hasRelation(f, facts.RelCalls, "lib.Base.load") || hasRelation(f, facts.RelCalls, "lib.Factory.load") || f.PropBool("performs_io") {
			t.Errorf("unknown result type guessed: %+v", f)
		}
	}
}

func TestDartReviewConstructorAndInstanceNamespaces(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{"lib/user.dart": `
class Base { load() {} }
class User extends Base {
 User.foo(); User.load();
 foo() {}
 void run(User other) {foo(); other.foo(); other.load();}
}
void create() {User.foo();User.load();}
`})
	f := findFact(all, facts.KindSymbol, "lib.User.run")
	for _, target := range []string{"lib.User.foo", "lib.Base.load"} {
		if !hasRelation(f, facts.RelCalls, target) {
			t.Errorf("instance namespace lost %s: %+v", target, f)
		}
	}
	for _, target := range []string{"lib.User.User.foo", "lib.User.User.load"} {
		if hasRelation(f, facts.RelCalls, target) {
			t.Errorf("instance call became construction: %+v", f)
		}
	}
	f = findFact(all, facts.KindSymbol, "lib.create")
	for _, target := range []string{"lib.User.User.foo", "lib.User.User.load"} {
		if !hasRelation(f, facts.RelCalls, target) {
			t.Errorf("named construction lost %s: %+v", target, f)
		}
	}
}

func TestDartReviewImportedBaseAndInheritedGetter(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{
		"lib/base.dart":   `import 'package:http/http.dart'; class Base {load(){httpClient.get('/load');} Object get value {httpClient.get('/value');return 1;}}`,
		"other/base.dart": `class Base {load() {}}`,
		"lib/use.dart":    `import 'base.dart'; class User extends Base {set value(Object next) {} run(){load();}} Object read(User user)=>user.value;`,
	})
	f := findFact(all, facts.KindSymbol, "lib.User.run")
	if !hasRelation(f, facts.RelCalls, "lib.Base.load") || !f.PropBool("performs_io") {
		t.Fatalf("imported base was not selected: %+v", f)
	}
	f = findFact(all, facts.KindSymbol, "lib.read")
	if !hasRelation(f, facts.RelCalls, "lib.Base.value") || !f.PropBool("performs_io") {
		t.Fatalf("setter hid inherited getter: %+v", f)
	}
}

func TestDartReviewGetterDoesNotBorrowSetterIO(t *testing.T) {
	for _, setterFirst := range []bool{false, true} {
		getter := `Object get value => 1;`
		setter := `set value(Object next) {httpClient.post('/write');}`
		members := getter + setter
		if setterFirst {
			members = setter + getter
		}
		all := dartReviewFacts(t, map[string]string{"lib/accessor.dart": `import 'package:http/http.dart'; class A {` + members + `} void read(A _db,List<int> rows) {for(final row in rows) {consume(_db.value);}}`})
		f := findFact(all, facts.KindSymbol, "lib.read")
		if !hasRelation(f, facts.RelCalls, "lib.A.value") || f.PropBool("performs_io") {
			t.Fatalf("getter borrowed setter effects (setter first=%v): %+v", setterFirst, f)
		}
		store := facts.NewStore()
		store.Add(all...)
		for _, finding := range perf.Analyze(store) {
			if finding.Symbol == "lib.read" && finding.Kind == "call-in-loop" {
				t.Fatalf("pure getter has an I/O warning: %+v", finding)
			}
		}
	}
}
