package cppextractor

import (
	"slices"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A bare capitalized callee in C++ is a constructor only when it names a type the
// caller can see. When it names a function instead, it is a call.

func TestCppCapitalizedFreeFunctionCall(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/a.cpp": `
namespace duckdb {
static int lower_helper(int x) { return x; }
static int UpperHelper(int x) { return x; }
void Entry() { lower_helper(1); UpperHelper(2); }
}
`,
	})
	entry := mustFact(t, ff, "src.duckdb::Entry")
	if !hasRelation(entry, facts.RelCalls, "src.duckdb::UpperHelper") {
		t.Errorf("Entry should call UpperHelper, got %+v", entry.Relations)
	}
	if !hasRelation(entry, facts.RelCalls, "src.duckdb::lower_helper") {
		t.Errorf("Entry should call lower_helper, got %+v", entry.Relations)
	}
	for _, r := range entry.Relations {
		if r.Kind == facts.RelInstantiates || r.Kind == relCapitalizedCallCandidate {
			t.Errorf("no instantiation or provisional edge expected, got %+v", r)
		}
	}
}

func TestCppCapitalizedTemplateCallThroughHeader(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"lib/helpers.hpp": `
namespace db {
template <class T> int GetFirst(int x) { return x; }
}
`,
		"app/use.cpp": `
#include "../lib/helpers.hpp"
namespace db {
int Use(int y) { return GetFirst<int>(y); }
}
`,
	})
	use := mustFact(t, ff, "app.db::Use")
	if !hasRelation(use, facts.RelCalls, "lib.db::GetFirst") {
		t.Errorf("Use should call the header template GetFirst, got %+v", use.Relations)
	}
}

func TestCppCapitalizedSiblingMethodCall(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/alloc.cpp": `
class Pool {
public:
  bool Initialize() const;
  bool InitializePool() const;
};
bool Pool::Initialize() const { return InitializePool(); }
bool Pool::InitializePool() const { return true; }
`,
	})
	init := mustFact(t, ff, "src.Pool::Initialize")
	if !hasRelation(init, facts.RelCalls, "src.Pool::InitializePool") {
		t.Errorf("Initialize should call its sibling InitializePool, got %+v", init.Relations)
	}
}

// The constructor cases that must not move: a visible type stays an instantiation
// even when a function of that name exists elsewhere, the class's own name inside
// its methods is the class, and a capitalized name that is neither (a macro or an
// external function) still adds no edge.
func TestCppCapitalizedCallKeepsConstructors(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"geo/vec.hpp": `
struct Vec { Vec(int); };
`,
		"other/vec.cpp": `
int Vec(int x) { return x; }
`,
		"geo/use.cpp": `
#include "vec.hpp"
class Box {
public:
  Box(int);
  Box Grow() const;
};
Box Box::Grow() const { return Box(2); }
void Make() { Vec v = Vec(1); Malloc(3); }
`,
	})
	mk := mustFact(t, ff, "geo.Make")
	if !hasRelation(mk, facts.RelInstantiates, "geo.Vec") {
		t.Errorf("Make should instantiate the visible type Vec, got %+v", mk.Relations)
	}
	for _, r := range mk.Relations {
		if r.Kind == facts.RelCalls && (r.Target == "other.Vec" || r.Target == "geo.Malloc") {
			t.Errorf("unexpected call edge %+v", r)
		}
	}
	grow := mustFact(t, ff, "geo.Box::Grow")
	if !hasRelation(grow, facts.RelInstantiates, "geo.Box") {
		t.Errorf("Grow should instantiate its own class Box, got %+v", grow.Relations)
	}
	if hasRelation(grow, facts.RelCalls, "geo.Box::Box") {
		t.Errorf("Box(2) inside Box is the class, not a call to its constructor, got %+v", grow.Relations)
	}
}

// A class template's own name is the class too, and a capitalized name that is a
// typedef or a template parameter is not a function: no call edge to it.
func TestCppCapitalizedCallTemplateClassAndTypedef(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"geo/chain.hpp": `
typedef int Headers;
template <class C> class Chain {
public:
  Chain(int);
  Chain<C> Project() const;
  int Count() const;
};
template <class C> Chain<C>::Chain(int) {}
template <class C> Chain<C> Chain<C>::Project() const { return Chain<C>(1); }
template <class C> int Chain<C>::Count() const { return Headers(2); }
`,
	})
	project := mustFact(t, ff, "geo.Chain<C>::Project")
	for _, r := range project.Relations {
		if r.Kind == facts.RelCalls && r.Target == "geo.Chain<C>::Chain" {
			t.Errorf("Chain(1) inside Chain<C> is the class, not a call, got %+v", project.Relations)
		}
	}
	count := mustFact(t, ff, "geo.Chain<C>::Count")
	for _, r := range count.Relations {
		if r.Kind == facts.RelCalls && r.Target == "geo.Headers" {
			t.Errorf("Headers(2) names a typedef, not a function, got %+v", count.Relations)
		}
	}
}

// A constructor found by its short name is a type the type index could not see
// (declared out of line only): it stays without an edge, as before.
func TestCppCapitalizedCallSkipsConstructorFallback(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/w.cpp": `
class Writer { public: int Start(); };
Writer::Guard::Guard(Writer &w) {}
int Writer::Start() { Guard(*this); return 0; }
`,
	})
	start := mustFact(t, ff, "src.Writer::Start")
	if hasRelation(start, facts.RelCalls, "src.Writer::Guard::Guard") {
		t.Errorf("Guard(*this) constructs a Guard, not a call, got %+v", start.Relations)
	}
}

// A capitalized implicit-this call to a method no file-local declaration names
// is left to the inherited-calls binder, as a lowercase one is.
func TestCppCapitalizedCallNotesMemberCall(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/d.cpp": `
class Derived : public Base {
public:
  void Run();
};
void Derived::Run() { Flush(); }
`,
	})
	run := mustFact(t, ff, "src.Derived::Run")
	if !slices.Contains(run.PropStrings(facts.PropMemberCalls), "Flush") {
		t.Errorf("Run should note Flush as a member call, got %+v", run.Props)
	}
}
