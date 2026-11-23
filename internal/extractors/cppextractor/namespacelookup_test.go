package cppextractor

import (
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// An unqualified call is looked up as C++ does: in the caller's enclosing
// namespaces, innermost first, among the declarations the caller's file may see.
// A function of the same short name declared in another directory, which the file
// cannot include, does not hide it.

func TestCppUnqualifiedCallFindsTheEnclosingNamespace(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"bench/runner.cpp": `
namespace db {
static int ParseGroup(int x) { return x; }
static int helper(int x) { return x; }
int Run(int v) { return ParseGroup(v) + helper(v); }
}
`,
		"tests/sql/other.cpp": `
static int ParseGroup(int x) { return x; }
static int helper(int x) { return x; }
int Register() { return ParseGroup(1) + helper(2); }
`,
	})
	run := mustFact(t, ff, "bench.db::Run")
	for _, want := range []string{"bench.db::ParseGroup", "bench.db::helper"} {
		if !hasRelation(run, facts.RelCalls, want) {
			t.Errorf("Run should call %s, got %+v", want, run.Relations)
		}
	}
	reg := mustFact(t, ff, "tests/sql.Register")
	for _, r := range reg.Relations {
		if strings.HasPrefix(r.Target, "bench.") {
			t.Errorf("a global caller in another directory does not see db::, got %+v", r)
		}
	}
}

func TestCppUnqualifiedCallInOutOfLineConstructorInitializer(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"bench/include/bench.hpp": `
namespace db {
class Benchmark { public: Benchmark(bool, int); };
class Interpreted : public Benchmark { public: Interpreted(int path); };
}
`,
		"bench/interpreted.cpp": "#include \"include/bench.hpp\"\n" + `
namespace db {
static int ParseGroupFromPath(int file) { return file; }
Interpreted::Interpreted(int path) : Benchmark(true, ParseGroupFromPath(path)) {}
}
`,
		"tests/sql/logic.cpp": `
static int ParseGroupFromPath(int file) { return file; }
`,
	})
	ctor := mustFact(t, ff, "bench.db::Interpreted::Interpreted")
	if !hasRelation(ctor, facts.RelCalls, "bench.db::ParseGroupFromPath") {
		t.Errorf("the constructor should call db::ParseGroupFromPath, got %+v", ctor.Relations)
	}
}

func TestCppUnqualifiedCallBindsToTheIncludedDeclaration(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/include/db/util.hpp": `namespace db { int Compute(int); }`,
		"src/util/util.cpp":       "#include \"db/util.hpp\"\n" + `namespace db { int Compute(int x) { return x; } }`,
		"src/exec/exec.cpp":       "#include \"db/util.hpp\"\n" + `namespace db { int Exec(int x) { return Compute(x); } }`,
		"tools/shell/shell.cpp":   `int Compute(int x) { return x; }`,
	})
	exec := mustFact(t, ff, "src/exec.db::Exec")
	if !hasRelation(exec, facts.RelCalls, "src/include/db.db::Compute") {
		t.Errorf("Exec should call the db::Compute its file includes, got %+v", exec.Relations)
	}
	for _, r := range exec.Relations {
		if strings.HasPrefix(r.Target, "tools/") {
			t.Errorf("Exec includes nothing from tools, got %+v", r)
		}
	}
}

func TestCppUnqualifiedCallOutOfSightStaysUnbound(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"a/one.cpp":  `namespace db { int Shared(int x) { return x; } }`,
		"b/two.cpp":  `namespace db { int Shared(int x) { return x; } }`,
		"c/user.cpp": `namespace db { int Use(int x) { return Shared(x); } }`,
	})
	use := mustFact(t, ff, "c.db::Use")
	for _, r := range use.Relations {
		if strings.HasPrefix(r.Target, "a.") || strings.HasPrefix(r.Target, "b.") {
			t.Errorf("c includes neither copy of db::Shared, got %+v", r)
		}
	}
}

func TestCppUnqualifiedCallInMethodPrefersAnInheritedMember(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/include/base.hpp": `namespace db {
class Base { public: int Flush(int); };
class Derived : public Base { public: int Work(int); };
int Flush(int);
}`,
		"src/core/derived.cpp": "#include \"base.hpp\"\n" + `namespace db {
int Flush(int x) { return x; }
int Derived::Work(int x) { return Flush(x); }
}`,
	})
	work := mustFact(t, ff, "src/core.db::Derived::Work")
	for _, r := range work.Relations {
		if r.Kind == facts.RelCalls && (r.Target == "src/core.db::Flush" || r.Target == "src/include.db::Flush") {
			t.Errorf("Flush inside Derived names the inherited Base::Flush, not db::Flush, got %+v", r)
		}
	}
}
