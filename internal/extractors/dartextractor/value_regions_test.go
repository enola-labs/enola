package dartextractor

import (
	"slices"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func anyRelation(all []facts.Fact, kind, target string) bool {
	for i := range all {
		if hasRelation(&all[i], kind, target) {
			return true
		}
	}
	return false
}

func TestDartParameterDefaultReferences(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{"lib/defaults.dart": `
String _savepoint(int d) => 's';
String _release(int d) => 'r';
Object _callback() => 1;
Object _named() => 2;
Object _param() => 3;
class Delegate {
  final Object savepoint;
  final Object release;
  const Delegate({this.savepoint = _savepoint, this.release = _release});
}
class Methods {
  void positional([Object f = _callback]) {}
}
void top({Object cb = const Holder(fn: _named)}) {}
void shadow({Object _param = 1}) {}
`})
	for _, target := range []string{"lib._savepoint", "lib._release", "lib._callback", "lib._named"} {
		if !anyRelation(all, facts.RelNames, target) {
			t.Errorf("parameter default recorded no use of %s", target)
		}
	}
	if anyRelation(all, facts.RelNames, "lib._param") {
		t.Error("a parameter's own name became a reference")
	}
	ctor := findFact(all, facts.KindSymbol, "lib.Delegate.Delegate")
	if ctor == nil {
		t.Fatal("fixture did not emit the constructor")
	}
	if _, ok := ctor.Props["cyclomatic"]; ok {
		t.Errorf("a body-less constructor gained body metrics: %+v", ctor.Props)
	}
}

func TestDartIfNullValueReference(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{"lib/coalesce.dart": `
T _identity<T>(T v) => v;
void _handler(Object r) {}
Object _called() => 1;
Object pick(Object? f) { final w = f ?? _identity; return w; }
void run(Object? h, Object r) { (h ?? _handler)(r); }
Object call(Object? f) => f ?? _called();
`})
	for _, target := range []string{"lib._identity", "lib._handler"} {
		if !anyRelation(all, facts.RelNames, target) {
			t.Errorf("right side of ?? recorded no use of %s", target)
		}
	}
	if anyRelation(all, facts.RelNames, "lib._called") {
		t.Error("an invocation on the right of ?? became a tear-off")
	}
}

func TestDartTopLevelGetterReads(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{"lib/state.dart": `
Object get _store => 1;
external Object get _nav;
Object read() => _store;
void use() { _nav.locks; }
void loop(List<int> rows) { for (final row in rows) { consume(_store); } }
void shadow() { final _store = 3; consume(_store); }
class C {
  Object get _store => 2;
  Object own() => _store;
}
`})
	for name, target := range map[string]string{"lib.read": "lib._store", "lib.use": "lib._nav", "lib.loop": "lib._store", "lib.C.own": "lib.C._store"} {
		if f := findFact(all, facts.KindSymbol, name); !hasRelation(f, facts.RelCalls, target) {
			t.Errorf("%s: getter read of %s dropped: %+v", name, target, f)
		}
	}
	if f := findFact(all, facts.KindSymbol, "lib.loop"); f == nil || !slices.Contains(f.Props["calls_in_loop"].([]string), "lib._store") {
		t.Errorf("getter read in a loop is not per-iteration: %+v", f)
	}
	for _, name := range []string{"lib.shadow", "lib.C.own"} {
		if f := findFact(all, facts.KindSymbol, name); hasRelation(f, facts.RelCalls, "lib._store") {
			t.Errorf("%s: shadowed name read the top-level getter", name)
		}
	}
}
