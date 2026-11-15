// Reference declarators — a member or function that returns a reference is a symbol
// like any other. tree-sitter-cpp tags the wrapped node as field "declarator" on
// pointer_declarator and does NOT tag it on reference_declarator, where the `&` and the
// wrapped node are both bare children. A declarator walk written against the field
// therefore follows `int* ptr()` and stops dead at `const int& slot()`: the member
// declaration, its out-of-line definition, its callers and its override all disappear
// with nothing to show but a short symbol list.
//
// Pins the reference-returning member, its out-of-line definition merging into that same
// symbol, a reference-returning free function, a reference data member, and the
// pointer-returning control that always worked — the asymmetry that makes this a
// field-tag gap rather than a reference problem.
int g_counter;

class Registry {
public:
    int& m_ref;                                 // reference data member
    virtual const int& slot(int i) const;       // reference-returning member
    int* ptr(int i) const;                      // pointer return (control)
};

class LoudRegistry : public Registry {
public:
    const int& slot(int i) const override;      // reference return + override
};

const int& Registry::slot(int i) const { return g_counter; }
const int& LoudRegistry::slot(int i) const { return g_counter; }

// Reference-returning free function: declaration and definition, one symbol.
int& counter();
int& counter() { return g_counter; }

void tune() {
    counter() = 3;
}
