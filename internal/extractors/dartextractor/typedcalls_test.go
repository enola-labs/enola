package dartextractor

import (
	"reflect"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A call on a local whose type its declaration names reaches the member that type
// declares, else the one its nearest supertype declares, even where the bare name is
// declared by several types. A call through a subtype keeps the subtype it went
// through; a type the project does not declare is not guessed at.
func TestTypedLocalCallsBindThroughTheSupertypes(t *testing.T) {
	src := `
abstract class Base {
  void run() { step(); }
  void step();
}
class Concrete extends Base {
  void step() {}
}
class Other {
  void run() {}
  void step() {}
}
void main() {
  final c = Concrete();
  Other o = makeOther();
  var s = StringBuffer();
  c.run();
  o.step();
  s.write();
}
`
	all := resolveCallTargets(walkSource(t, "lib/a.dart", src))
	main := findFact(all, facts.KindSymbol, "lib.main")
	if main == nil {
		t.Fatal("no lib.main")
	}
	for _, want := range []string{"lib.Base.run", "lib.Other.step"} {
		if !hasRelation(main, facts.RelCalls, want) {
			t.Errorf("main does not call %s: %v", want, main.Relations)
		}
	}
	if got := main.PropStrings(facts.PropInheritedCalls); !reflect.DeepEqual(got, []string{"lib.Concrete.run"}) {
		t.Errorf("%s = %v, want [lib.Concrete.run]", facts.PropInheritedCalls, got)
	}
}
