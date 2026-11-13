// Polymorphism specifiers (virtual / override / final / const / static). The C++
// corpus this extractor was validated against (getdp, gmsh, the Linux kernel) is
// pre-C++11 and never writes `override`, so the specifier path had no coverage on
// real code. This header is the fixture that exercises it: an override keeps
// `virtual` even though the keyword is not repeated, `final` is recorded on a member
// and on a type, and a member whose NAME merely contains a specifier keyword
// (`staticCast`) gets none.
//
// Parsed with tree-sitter-cpp. drivers/shapes.cpp holds the out-of-line half, and it
// is deliberately in ANOTHER directory: a symbol name carries its directory, so the
// declaration here and its definition there stay two symbols — the specifier rides on
// this half, `has_body` on that one. A same-directory pair does collapse into one
// symbol, and that case is pinned by
// internal/extractors/cppextractor/qualifiers_test.go.
// docs/extraction/cpp.md says what each prop means.
#pragma once

namespace shapes {

class Shape {
public:
    // Virtual declared here, defined out-of-line in drivers/shapes.cpp.
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

// `final` on a type: nothing may derive from Square. Its member is an ordinary
// override, so a type-level `final` must not imply `virtual` on anything.
class Square final : public Shape {
public:
    void draw() override;
};

} // namespace shapes
