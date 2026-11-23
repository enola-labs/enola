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
	if !hasRelation(ctor, facts.RelCalls, "src.db::ParseGroup") {
		t.Errorf("Bench::Bench should call ParseGroup, got %+v", ctor.Relations)
	}
}
