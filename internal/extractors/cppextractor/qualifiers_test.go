package cppextractor

import "testing"

// C++ polymorphism specifiers are read from the AST, not from the declaration's
// source text.
//
// Two failure classes are pinned here.
//
//  1. The specifier is missing. A C++11 `override` never repeats the base's
//     `virtual`, so nothing marked it at all: the override looked like a plain
//     method, `find_orphans` reported it as unreferenced dead code, and the
//     class's polymorphic surface read as flat. `final` was invisible too.
//  2. The specifier is invented. A substring test over the signature text reads
//     `static` out of `staticCast` and `virtual` out of `callVirtual`; in C the
//     false `static` additionally flipped exported to false, which moves the
//     symbol into the "safe to delete" half of the orphan report.
func TestCppVirtualOverrideFinalSpecifiers(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"actors/shape.h": `
namespace actors {

class Shape {
public:
  virtual double area();
  virtual void draw() = 0;
  virtual ~Shape();
  void describe() const;
  static Shape *make(const char *kind);
};

class Circle : public Shape {
public:
  double area() override;
  void draw() final;
  int staticCast(int raw);
};

} // namespace actors
`,
		"actors/shape.cpp": `
namespace actors {
double Circle::area() { return 3.14; }
void Circle::draw() {}
int Circle::staticCast(int raw) { return raw; }
}
`,
	})

	shapeArea := mustFact(t, ff, "actors.actors::Shape::area")
	if v, _ := shapeArea.Props["virtual"].(bool); !v {
		t.Errorf("Shape.area: virtual = %v, want true", shapeArea.Props["virtual"])
	}
	if o, ok := shapeArea.Props["override"].(bool); ok && o {
		t.Error("Shape.area: override = true, want absent — it declares, it does not override")
	}

	circleArea := mustFact(t, ff, "actors.actors::Circle::area")
	if o, _ := circleArea.Props["override"].(bool); !o {
		t.Errorf("Circle.area: override = %v, want true", circleArea.Props["override"])
	}
	if v, _ := circleArea.Props["virtual"].(bool); !v {
		t.Error("Circle.area: virtual = false, want true — an override is virtual whether or not the keyword is repeated")
	}
	// The out-of-line definition carries no specifier of its own; the merged
	// symbol has to keep the one the class body declared.
	if hb, _ := circleArea.Props["has_body"].(bool); !hb {
		t.Error("Circle.area: has_body = false, want the definition merged into the declaring symbol")
	}

	circleDraw := mustFact(t, ff, "actors.actors::Circle::draw")
	if f, _ := circleDraw.Props["final"].(bool); !f {
		t.Errorf("Circle.draw: final = %v, want true", circleDraw.Props["final"])
	}
	// A member may only be marked `final` if it overrides a virtual base member, so
	// a final override is an override: without this it stays in the dead-code report.
	if o, _ := circleDraw.Props["override"].(bool); !o {
		t.Errorf("Circle.draw: override = %v, want true (final implies it overrides)", circleDraw.Props["override"])
	}

	// A member whose NAME contains the keyword gets no specifier.
	trap := mustFact(t, ff, "actors.actors::Circle::staticCast")
	for _, prop := range []string{"static", "virtual"} {
		if v, ok := trap.Props[prop].(bool); ok && v {
			t.Errorf("Circle.staticCast: %s = true, want absent — the keyword is inside the identifier", prop)
		}
	}

	// A free function whose name contains "virtual" likewise.
	free := mustFact(t, ff, "actors.actors::Shape::make")
	if v, ok := free.Props["virtual"].(bool); ok && v {
		t.Error("Shape.make: virtual = true, want absent — static factory, not virtual")
	}
	if s, _ := free.Props["static"].(bool); !s {
		t.Errorf("Shape.make: static = %v, want true", free.Props["static"])
	}
}

// Trailing const still rides on the declarator, next to a virtual specifier.
func TestCppConstQualifierSurvivesTheASTRewrite(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"geo/shape.hpp": `
class Shape {
public:
  virtual void describe() const;
  void plain();
};
`,
	})
	withConst := mustFact(t, ff, "geo.Shape::describe")
	if c, _ := withConst.Props["const"].(bool); !c {
		t.Errorf("Shape.describe: const = %v, want true", withConst.Props["const"])
	}
	if v, _ := withConst.Props["virtual"].(bool); !v {
		t.Errorf("Shape.describe: virtual = %v, want true", withConst.Props["virtual"])
	}
	without := mustFact(t, ff, "geo.Shape::plain")
	if c, ok := without.Props["const"].(bool); ok && c {
		t.Error("Shape.plain: const = true, want absent")
	}
}

// C has no polymorphism, but it shares this code path — and `static` there is the
// one specifier with a second consequence, so both halves are pinned.
func TestCStaticStillMeansInternalLinkage(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"drv/board.c": `
static int omap_reserve(void) { return 0; }
int staticLike(int n) { return n; }
`,
	})
	internal := mustFact(t, ff, "drv.omap_reserve")
	if s, _ := internal.Props["static"].(bool); !s {
		t.Errorf("omap_reserve: static = %v, want true", internal.Props["static"])
	}
	if e, _ := internal.Props["exported"].(bool); e {
		t.Error("omap_reserve: exported = true, want false — a C static has internal linkage")
	}
	external := mustFact(t, ff, "drv.staticLike")
	if s, ok := external.Props["static"].(bool); ok && s {
		t.Error("staticLike: static = true, want absent — the keyword is inside the identifier")
	}
	if e, _ := external.Props["exported"].(bool); !e {
		t.Errorf("staticLike: exported = %v, want true", external.Props["exported"])
	}
}
