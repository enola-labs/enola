// Polymorphism specifiers (virtual / override / final / const / static). The C++
// corpus this extractor was validated against (getdp, gmsh, the Linux kernel) is
// pre-C++11 and never writes `override`, so the specifier path had no coverage on
// real code. This header is the fixture that exercises it: an override keeps
// `virtual` even though the keyword is not repeated, `final` is recorded, and a
// member whose NAME merely contains a specifier keyword (`staticCast`) gets none.
//
// Parsed with tree-sitter-cpp. See drivers/shapes.cpp for the out-of-line half of
// the merge, and docs/extraction/cpp.md for what each prop means.
#pragma once

namespace shapes {

class Shape {
public:
    // Virtual declared here, defined out-of-line in drivers/shapes.cpp: the merged
    // symbol carries `virtual` from this declaration and `has_body` from there.
    virtual double area();

    // A pure virtual: one symbol, no body.
    virtual void draw() = 0;

    // Trailing const on the declarator, next to `virtual`.
    void describe() const;

    // A static factory: `static` yes, `virtual` no.
    static Shape *make(const char *kind);
};

class Circle : public Shape {
public:
    // `override` never repeats `virtual`, yet the member is virtual — the language
    // guarantees it. Defined out-of-line below.
    double area() override;

    // `final` on a member: also virtual, and nothing may override it again.
    void draw() final;

    // A name containing a keyword is not a specifier. Before the specifiers were
    // read from the AST, this member was tagged `static`.
    int staticCast(int raw);
};

} // namespace shapes
