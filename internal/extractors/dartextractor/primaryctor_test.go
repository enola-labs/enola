package dartextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// Dart 3.13 primary and concise constructors. The grammar before 6b86540 had neither, so
// a class declared this way was one ERROR node and lost every member after it.
func TestPrimaryConstructor(t *testing.T) {
	src := `
class Point(final int x, var int y, {required final Repo repo, int scale = 1}) extends Shape;

class const Pair.of(final String left, this.right, final String _hidden) {
  String right;
  new named(int v) : right = '';
  factory build() => Pair.of('', '', '');
  void show() {}
}

class After {
  void run() {}
}
`
	ff := walkSource(t, "lib/shapes.dart", src)
	for _, want := range []string{
		"lib.Point", "lib.Point.Point", "lib.Point.x", "lib.Point.y", "lib.Point.repo",
		"lib.Pair", "lib.Pair.Pair.of", "lib.Pair.left", "lib.Pair.Pair.named",
		"lib.Pair.Pair.build", "lib.Pair.show", "lib.After", "lib.After.run",
	} {
		if findFact(ff, facts.KindSymbol, want) == nil {
			var got []string
			for _, f := range factsOfKind(ff, facts.KindSymbol) {
				got = append(got, f.Name)
			}
			t.Errorf("missing %s; got %v", want, got)
		}
	}
	// A plain parameter declares no field, and a private field is not a symbol.
	for _, absent := range []string{"lib.Point.scale", "lib.Pair._hidden"} {
		if findFact(ff, facts.KindSymbol, absent) != nil {
			t.Errorf("%s should not be a symbol", absent)
		}
	}
	ctor := findFact(ff, facts.KindSymbol, "lib.Point.Point")
	if ctor == nil || ctor.Props["primary_constructor"] != true {
		t.Fatalf("lib.Point.Point is not marked as a primary constructor: %+v", ctor)
	}
	if !hasRelation(ctor, facts.RelInjects, "Repo") {
		t.Errorf("the primary constructor does not inject Repo: %+v", ctor.Relations)
	}
	if p := findFact(ff, facts.KindSymbol, "lib.Point"); !hasRelation(p, facts.RelDeclares, "lib.Point.x") {
		t.Errorf("lib.Point does not declare its primary-constructor field x")
	}
}
