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
int add_virtual_intf(void *priv) { return 0; }
struct net_device *virtual_addr;
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

// `final` spelled on a TYPE is the other use of that word: it marks the class as
// underivable, rides on the class fact alone, and must not leak onto the members or
// imply `virtual` anywhere.
func TestCppClassFinalSpecifier(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"fig/shape.hpp": `
class Shape {
public:
  virtual void draw();
};

class Square final : public Shape {
public:
  void draw() override;
};

struct Sealed final : public Shape {
public:
  void draw() override;
};

struct Open : public Shape {
public:
  void draw() override;
};
`,
	})

	for _, name := range []string{"fig.Square", "fig.Sealed"} {
		f := mustFact(t, ff, name)
		if v, _ := f.Props["final"].(bool); !v {
			t.Errorf("%s: final = %v, want true — the type is declared final", name, f.Props["final"])
		}
		if v, ok := f.Props["virtual"].(bool); ok && v {
			t.Errorf("%s: virtual = true, want absent — a final class is not a virtual member", name)
		}
		if o, ok := f.Props["override"].(bool); ok && o {
			t.Errorf("%s: override = true, want absent — a type never overrides", name)
		}
	}

	open := mustFact(t, ff, "fig.Open")
	if v, ok := open.Props["final"].(bool); ok && v {
		t.Error("Open: final = true, want absent — it derives, it is not final")
	}

	// The member inside a final class keeps its own specifiers and nothing else: it
	// overrides, and it is not itself final.
	member := mustFact(t, ff, "fig.Square::draw")
	if o, _ := member.Props["override"].(bool); !o {
		t.Errorf("Square::draw: override = %v, want true", member.Props["override"])
	}
	if v, ok := member.Props["final"].(bool); ok && v {
		t.Error("Square::draw: final = true, want absent — the class is final, the member is not")
	}
}

// Stacked macro attributes break the grammar and bury the specifiers: tree-sitter
// absorbs the first attribute macro as the return type and error recovery
// re-tokenizes `static`/`virtual` as plain identifier tokens inside an ERROR node.
// A specifier scan over named children alone silently loses the flag on exactly
// these lines (gtest's paired GTEST_ATTRIBUTE_* macros hit this seven times).
// The recovery reads the keywords back off the ERROR span — token-exact, so
// `staticCast` as an identifier stays unfalsified even inside a broken parse.
func TestCppStackedMacroAttributeSpecifiers(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"gated/gate.cpp": `
#define NOINL __attribute__((noinline))
#define NOSAN __attribute__((no_sanitize("address")))

NOINL
NOSAN
static bool stack_grows_down() { return true; }

NOINL
NOSAN
virtual bool poly_gate() { return true; }

NOINL
static bool one_attr_static() { return true; }

bool plain() { return true; }
int staticCast(int raw);
`,
		"gated/gate.c": `
#define NOINL __attribute__((noinline))
#define NOSAN __attribute__((no_sanitize("address")))

NOINL
NOSAN
static int c_gate(int v) { return v; }
`,
	})

	grown := mustFact(t, ff, "gated.stack_grows_down")
	if v, _ := grown.Props["static"].(bool); !v {
		t.Error("stack_grows_down: static = false, want true — specifier buried in an ERROR node must be recovered")
	}
	poly := mustFact(t, ff, "gated.poly_gate")
	if v, _ := poly.Props["virtual"].(bool); !v {
		t.Error("poly_gate: virtual = false, want true — same recovery, virtual spelling")
	}
	if o, ok := poly.Props["override"].(bool); ok && o {
		t.Error("poly_gate: override = true, want absent — recovered virtual declares, it does not override")
	}
	one := mustFact(t, ff, "gated.one_attr_static")
	if v, _ := one.Props["static"].(bool); !v {
		t.Error("one_attr_static: static = false, want true — single-attribute form must keep working")
	}

	cGate := mustFact(t, ff, "gated.c_gate")
	if v, _ := cGate.Props["static"].(bool); !v {
		t.Error("c_gate: static = false, want true — C grammar, same recovery")
	}
	if v, ok := cGate.Props["exported"].(bool); ok && v {
		t.Error("c_gate: exported = true, want false — recovered C static keeps internal linkage")
	}
}
