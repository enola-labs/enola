// The out-of-line half of include/shapes.hpp. Two things are pinned here.
//
// 1. The per-directory symbol name. A definition here is named
//    "drivers.shapes::Circle::area" while the declaration in include/ is
//    "include.shapes::Circle::area", so the two halves stay two facts: the specifier
//    (`virtual`, `override`, `final`) rides on the declaring half and `has_body` on
//    this one. find_orphans can therefore still list an override's body even though
//    its declaration is marked. The same-directory pair that DOES collapse into one
//    symbol is pinned by internal/extractors/cppextractor/qualifiers_test.go.
// 2. A call through a base pointer. `shape->draw()` is written on `Shape *` and lands
//    on the base's own declaration, NOT on Circle::draw — dynamic dispatch is
//    deliberately not resolved (docs/extraction/cpp.md). What makes that limit
//    visible rather than silent is that Circle::draw is marked, so the override is
//    findable by its own props.
//
// Parsed with tree-sitter-cpp.
#include "shapes.hpp"

namespace shapes {

double Shape::area() { return 0.0; }

double Circle::area() { return 3.14159; }

void Circle::draw() {}

int Circle::staticCast(int raw) { return raw; }

void Shape::describe() const {}

Shape *Shape::make(const char *kind) { return new Circle(); }

// The base-pointer call: one edge, to Shape::draw's declaration. Circle::draw gets
// no inbound edge from here, because dispatch is not followed.
void draw_one(Shape *shape) {
    shape->draw();
}

// The other half of that limit, pinned so it stays honest: a subscripted `Shape **`
// receiver is not tracked, so this call produces no edge at all.
void draw_all(Shape **shapes, int n) {
    for (int i = 0; i < n; i++) {
        shapes[i]->draw();
    }
}

} // namespace shapes
