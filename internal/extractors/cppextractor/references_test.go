package cppextractor

// A member that returns a reference, or a data member bound to one, is a symbol like
// any other. It used to not be a symbol at all: reference_declarator wraps its inner
// node without the "declarator" field tag that pointer_declarator carries, so the
// descent stopped at the `&` and the declaration, its definition, its callers and its
// `override` all disappeared with nothing to show but a short symbol list.

import "testing"

func TestCppReferenceDeclaratorsAreExtracted(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"r/mesh.hpp": `
namespace r {

class Mesh {
  float m_v[4];
public:
  virtual const float& vert(int i) const;   // reference return
  float* ptr(int i) const;                  // pointer return (control)
  int& m_ref;                               // reference data member
};

class LoudMesh : public Mesh {
public:
  const float& vert(int i) const override;  // reference return + override
};

const float& Mesh::vert(int i) const { return m_v[i]; }

const float& LoudMesh::vert(int i) const { return m_v[i]; }

} // namespace r
`,
	})

	base := mustFact(t, ff, "r.r::Mesh::vert")
	if hb, _ := base.Props["has_body"].(bool); !hb {
		t.Error("Mesh.vert: has_body = false, want the out-of-line definition merged in")
	}
	if r, _ := base.Props["receiver"].(string); r != "Mesh" {
		t.Errorf("Mesh.vert: receiver = %q, want Mesh", r)
	}

	// The declaration and the out-of-line definition both exist now, and they are one
	// symbol: location and body from the definition, receiver from the class.
	derived := mustFact(t, ff, "r.r::LoudMesh::vert")
	if hb, _ := derived.Props["has_body"].(bool); !hb {
		t.Error("LoudMesh.vert: has_body = false, want the out-of-line definition merged in")
	}
	if r, _ := derived.Props["receiver"].(string); r != "LoudMesh" {
		t.Errorf("LoudMesh.vert: receiver = %q, want LoudMesh", r)
	}
	if c, _ := derived.Props["const"].(bool); !c {
		t.Error("LoudMesh.vert: const = false, want true — the trailing const is on the declarator")
	}
	// The two fixes on this branch compose: a member that is missed by the declarator
	// walk cannot carry a specifier either, so the corpus sites that produced no
	// `override` prop were producing no symbol. With the descent fixed, the specifier
	// reading reaches them. The engine fixture shows the same shape on
	// drivers.LoudRegistry::slot: `override` and `has_body` in one fact.
	if o, _ := derived.Props["override"].(bool); !o {
		t.Errorf("LoudMesh.vert: override = %v, want true", derived.Props["override"])
	}
	if v, _ := derived.Props["virtual"].(bool); !v {
		t.Error("LoudMesh.vert: virtual = false, want true — an override is virtual")
	}
	if _, ok := findFact(ff, "r.r::LoudMesh::vert"); !ok {
		t.Error("LoudMesh.vert: the declaration and its definition must collapse to one fact")
	}

	// The pointer-returning control keeps working: pointer_declarator was always
	// field-tagged, which is exactly why this bug looked like a reference problem
	// rather than a descent problem.
	if _, ok := findFact(ff, "r.r::Mesh::ptr"); !ok {
		t.Error("Mesh.ptr: missing — the pointer-returning control must still be extracted")
	}

	ref := mustFact(t, ff, "r.r::Mesh::m_ref")
	if sk, _ := ref.Props["symbol_kind"].(string); sk != "variable" {
		t.Errorf("Mesh.m_ref: symbol_kind = %q, want variable", sk)
	}
}
