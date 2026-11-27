package cppextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A call in a constructor's member initializer list runs as part of the
// constructor, so it is the constructor's call.

func TestCppCtorInitializerListCalls(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/a.cpp": `
int helper(int x) { return x; }
int Upper(int x) { return x; }
struct S {
	int m; int n;
	S() : m(helper(1)), n(Upper(2)) {}
};
`,
	})
	ctor := mustFact(t, ff, "src.S::S")
	for _, want := range []string{"src.helper", "src.Upper"} {
		if !hasRelation(ctor, facts.RelCalls, want) {
			t.Errorf("S::S should call %s, got %+v", want, ctor.Relations)
		}
	}
}

func TestCppCtorInitializerListOutOfLine(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/b.cpp": `
namespace db {
static int ParseGroup(int x) { return x; }
class Bench {
public:
	Bench(int v);
	int group;
};
Bench::Bench(int v) : group(ParseGroup(v)) {}
}
`,
	})
	ctor := mustFact(t, ff, "src.db::Bench::Bench")
	if !hasRelation(ctor, facts.RelCalls, "src.db::ParseGroup@b_cpp") {
		t.Errorf("Bench::Bench should call ParseGroup, got %+v", ctor.Relations)
	}
}

// A parameter forwarded to a member is not a function value, even when a function
// of that name exists.
func TestCppCtorInitializerListParameterIsNotAFunction(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/c.cpp": `
#include <utility>
int flags() { return 0; }
int pass(int x) { return x; }
int on_ready(int x) { return x; }
struct T {
	int f; int g; int (*cb)(int);
	T(int flags, int g) : f(pass(flags)), g(std::move(g)), cb(on_ready) {}
};
`,
	})
	ctor := mustFact(t, ff, "src.T::T")
	if !hasRelation(ctor, facts.RelCalls, "src.pass") {
		t.Errorf("T::T should call pass, got %+v", ctor.Relations)
	}
	if hasRelation(ctor, facts.RelCalls, "src.flags") {
		t.Errorf("the parameter flags is not the function flags, got %+v", ctor.Relations)
	}
}
