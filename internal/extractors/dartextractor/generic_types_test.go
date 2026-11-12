package dartextractor

import (
	"slices"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func TestDartGenericReceiverTypes(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{
		"lib/repo.dart": `import 'package:http/http.dart';
 class Repo {void load() {httpClient.get('/');}}
 class Other {void load() {}}
 class Box<T> {final T value; T? maybe; T get item => value; T pick() => value; Future<T> fetch() async => value; Box(this.value);}
 class Pair<A, B> {A first; B second; Pair(this.first, this.second);}
 class Mapper<T> {R apply<R>(R x) => x; T get one => throw 0;}
 class Sub extends Box<Repo> {Sub(super.value);}
 `,
		"lib/run.dart": `import 'repo.dart';
 void field(Box<Repo> b) {for(final r in [1]){b.value.load();}}
 void getter(Box<Repo> b) {b.item.load();}
 void method(Box<Repo> b) {b.pick().load();}
 void nullable(Box<Repo>? b) {b?.maybe?.load();}
 Future awaited(Box<Repo> b) async {final r = await b.fetch(); r.load();}
 void second(Pair<Other, Repo> p) {p.second.load(); p.first.load();}
 void raw(Box b) {b.value.load();}
 void dynamicArg(Box<dynamic> b) {b.value.load();}
 void ownParam(Mapper<Repo> m) {m.apply(Other()).load();}
 void inherited(Sub s) {s.value.load();}
 `})
	for _, name := range []string{"lib.field", "lib.getter", "lib.method", "lib.awaited", "lib.second", "lib.nullable"} {
		f := findFact(all, facts.KindSymbol, name)
		if !hasReviewCall(f, "lib.Repo.load") {
			t.Errorf("%s did not resolve through type arguments: %+v", name, f)
		}
	}
	f := findFact(all, facts.KindSymbol, "lib.second")
	if !hasReviewCall(f, "lib.Other.load") {
		t.Errorf("first type argument lost: %+v", f)
	}
	for _, name := range []string{"lib.raw", "lib.dynamicArg", "lib.ownParam", "lib.inherited"} {
		if f := findFact(all, facts.KindSymbol, name); hasReviewCall(f, "lib.Repo.load") || hasReviewCall(f, "lib.Other.load") {
			t.Errorf("%s guessed a target: %+v", name, f)
		}
	}
	if f := findFact(all, facts.KindSymbol, "lib.field"); !f.PropBool("performs_io") {
		t.Errorf("I/O did not propagate: %+v", f)
	}
	calls, _ := findFact(all, facts.KindSymbol, "lib.field").Props["calls_in_loop"].([]string)
	if !slices.Contains(calls, "lib.Repo.load") {
		t.Errorf("loop target lost: %v", calls)
	}
}
