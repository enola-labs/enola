package dartextractor

import (
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// TestDartFileScopeConstructions pins a construction in a file-scope initializer:
// the file records the class it builds, as a class-body field initializer does, and
// an invocation of a top-level function stays a call.
func TestDartFileScopeConstructions(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{"lib/a.dart": `
class Router { Router(); }
class Box<T> { Box(T v); }
class Opt { const Opt(); }
Object make() => 1;
Object Build() => 1;
final router = Router();
final box = Box<int>(1);
const opt = Opt();
final made = make();
final built = Build();
`})
	inst := relationSet(all, facts.RelInstantiates)
	for _, want := range []string{
		"file_ref lib/a.dart -> lib.Router",
		"file_ref lib/a.dart -> lib.Box",
		"file_ref lib/a.dart -> lib.Opt",
	} {
		if !inst[want] {
			t.Errorf("missing instantiates %q; got %v", want, inst)
		}
	}
	for edge := range inst {
		if strings.HasSuffix(edge, "make") || strings.HasSuffix(edge, "Build") {
			t.Errorf("function invocation read as a construction: %s", edge)
		}
	}
}

// TestDartNamedConstructorConstructions pins `Foo.named()` as a construction of Foo
// wherever it is written: a function body, a class-body field initializer, a
// file-scope initializer and an annotation argument. It binds only when Foo declares
// a constructor of that name: a static method, an import prefix and a value receiver
// are not constructions.
func TestDartNamedConstructorConstructions(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{
		"lib/a.dart": `
import 'b.dart' as p;
class Controller {
  Controller.withDefaults();
  Controller._();
  static Controller create() => Controller._();
}
class Marker { const Marker.fast(); }
class Ann { final Object v; const Ann(this.v); }
class Built { Built.named(); }
class Lone { Lone.only(); }
class Single {
  static final instance = Single._();
  Single._();
}
class Holder {
  late final _c = Controller.withDefaults();
  final s = Controller.create();
  final q = p.Other.named();
}
final lone = Lone.only();
@Ann(Marker.fast())
class Annotated {}
void main() {
  Built.named();
  final c = Controller.withDefaults();
  c.toString();
}
`,
		"lib/b.dart": `class Other { Other.named(); }`,
	})
	inst := relationSet(all, facts.RelInstantiates)
	for _, want := range []string{
		"file_ref lib/a.dart -> lib.Controller",
		"file_ref lib/a.dart -> lib.Lone",
		"file_ref lib/a.dart -> lib.Single",
		"symbol lib.Annotated -> lib.Marker",
		"symbol lib.main -> lib.Built",
		"symbol lib.main -> lib.Controller",
		"symbol lib.Controller.create -> lib.Controller",
		"file_ref lib/a.dart -> lib.Other",
	} {
		if !inst[want] {
			t.Errorf("missing instantiates %q; got %v", want, inst)
		}
	}
	calls := relationSet(all, facts.RelCalls)
	if !calls["file_ref lib/a.dart -> lib.Controller.Controller.withDefaults"] {
		t.Errorf("a field initializer's named constructor is not a call of it; got %v", calls)
	}
	for _, f := range all {
		for _, r := range f.Relations {
			if r.Kind == relDartCtorRef {
				t.Errorf("unresolved constructor reference leaked: %s -> %s", f.Name, r.Target)
			}
			if r.Kind == facts.RelInstantiates && (r.Target == "p" || strings.HasSuffix(r.Target, ".create")) {
				t.Errorf("not a construction: %s -> %s", f.Name, r.Target)
			}
		}
	}
}

// TestDartNamedConstructorReceiverScope keeps the rule to receivers that are the
// class itself: `super.named()`, `this.named()` and a lowercase value receiver are
// not constructions of the class.
func TestDartNamedConstructorReceiverScope(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{"lib/a.dart": `
class Base { Base.named(); }
class Child extends Base {
  Child() : super.named();
  Child.other() : this();
}
class Value { Value.named(); }
void run(Object named) {}
`})
	for _, f := range all {
		for _, r := range f.Relations {
			if r.Kind == facts.RelInstantiates && (r.Target == "lib.Base" || r.Target == "lib.Child") {
				t.Errorf("a super or this redirection read as a construction: %s -> %s", f.Name, r.Target)
			}
		}
	}
}

// TestDartRedirectingFactory pins `factory A() = _A;`: the factory is a constructor
// symbol of A, and it builds its target, named or import-prefixed.
func TestDartRedirectingFactory(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{
		"lib/a.dart": `
import 'b.dart' as p;
abstract class A {
  factory A() = _A;
  const factory A.named(int x) = _A.other;
  factory A.remote() = p.Remote<int>;
}
class _A implements A {
  _A();
  const _A.other(int x);
}
void main() { A(); }
`,
		"lib/b.dart": `class Remote<T> {}`,
	})
	for _, name := range []string{"lib.A.A", "lib.A.A.named", "lib.A.A.remote"} {
		f := findFact(all, facts.KindSymbol, name)
		if f == nil || !f.PropBool("constructor") {
			t.Errorf("redirecting factory %s has no constructor symbol: %+v", name, f)
		}
	}
	inst := relationSet(all, facts.RelInstantiates)
	for _, want := range []string{
		"symbol lib.A.A -> lib._A",
		"symbol lib.A.A.named -> lib._A",
		"symbol lib.A.A.remote -> lib.Remote",
	} {
		if !inst[want] {
			t.Errorf("missing instantiates %q; got %v", want, inst)
		}
	}
	if calls := relationSet(all, facts.RelCalls); !calls["symbol lib.A.A.named -> lib._A._A.other"] {
		t.Errorf("named redirect target is not a call of its constructor; got %v", calls)
	}
}
