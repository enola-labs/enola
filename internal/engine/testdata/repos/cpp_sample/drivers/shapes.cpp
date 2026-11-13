// The out-of-line half of include/shapes.h. Two things are pinned here.
//
// 1. Merging. A class-body declaration and its out-of-line definition are ONE
//    symbol: the specifier (`virtual`, `override`, `final`) comes from the header,
//    `has_body` and the location from here, and neither half is reported alone.
// 2. A call through a base pointer. `s->draw()` is written on `Shape *` and lands
//    on the base's own declaration, NOT on Circle::draw — dynamic dispatch is
//    deliberately not resolved (docs/extraction/cpp.md). What makes that limit
//    visible rather than silent is that Circle::draw is marked, so the override
//    is findable by its own props.
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

// The base-pointer call: one edge, to Shape::draw's declaration.
void draw_all(Shape **shapes, int n) {
    for (int i = 0; i < n; i++) {
        shapes[i]->draw();
    }
}

} // namespace shapes
