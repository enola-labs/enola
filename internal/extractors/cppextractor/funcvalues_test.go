package cppextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A function named in value position is a use of it, whatever the shape of the
// value or of the code that holds it.

func TestCppFunctionValueInDirectInitialization(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/a.cpp": `
struct Holder { Holder(const char *n, int (*f)(int)) {} };
static int DeclCbUpper(int x) { return x; }
static int DeclCbLower(int x) { return x; }
void setup() {
	Holder h1("a", DeclCbUpper);
	Holder h2("b", &DeclCbLower);
}
`,
	})
	setup := mustFact(t, ff, "src.setup")
	for _, want := range []string{"src.DeclCbUpper", "src.DeclCbLower"} {
		if !hasRelation(setup, facts.RelCalls, want) {
			t.Errorf("setup should reference %s, got %+v", want, setup.Relations)
		}
	}
}

func TestCppQualifiedFunctionValue(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/a.cpp": `
struct Driver { int (*conn_new)(int); int (*conn_free)(int); };
namespace ns { int conn_new(int x) { return x; } int conn_free(int x) { return x; } int on_ready(int x) { return x; } }
void attach(int (*f)(int)) {}
void init(Driver *d) {
	d->conn_new = ns::conn_new;
	d->conn_free = &ns::conn_free;
	attach(ns::on_ready);
}
`,
	})
	init := mustFact(t, ff, "src.init")
	for _, want := range []string{"src.ns::conn_new", "src.ns::conn_free", "src.ns::on_ready"} {
		if !hasRelation(init, facts.RelCalls, want) {
			t.Errorf("init should reference %s, got %+v", want, init.Relations)
		}
	}
}

func TestCppTemplateFunctionValue(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/a.cpp": `
template <class T> int InitAnalyze(int x) { return x; }
template <class T> int Propagate(int x) { return x; }
void attach(int (*f)(int)) {}
int (*pick())(int) { return Propagate<int>; }
void wire() { attach(InitAnalyze<long>); }
`,
	})
	if pick := mustFact(t, ff, "src.pick"); !hasRelation(pick, facts.RelCalls, "src.Propagate") {
		t.Errorf("pick should reference Propagate, got %+v", pick.Relations)
	}
	if wire := mustFact(t, ff, "src.wire"); !hasRelation(wire, facts.RelCalls, "src.InitAnalyze") {
		t.Errorf("wire should reference InitAnalyze, got %+v", wire.Relations)
	}
}

func TestCppInitializerTableInsideFunction(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/a.cpp": `
struct Rule { bool (*match)(int); bool (*combine)(int); };
static bool match_a(int) { return true; }
static bool combine_a(int) { return true; }
void init() {
	static const Rule rules[] = {{match_a, combine_a}};
}
`,
	})
	init := mustFact(t, ff, "src.init")
	for _, want := range []string{"src.match_a", "src.combine_a"} {
		if !hasRelation(init, facts.RelCalls, want) {
			t.Errorf("init should reference %s, got %+v", want, init.Relations)
		}
	}
}

// A qualified value that names a data member or an enumerator is not a function,
// even when a function elsewhere has that name.
func TestCppQualifiedValueThatIsNotAFunction(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/a.cpp": `
struct Cfg { static int limit; };
int limit() { return 0; }
namespace mode { enum Kind { fast, slow }; }
int fast() { return 1; }
void use(int *out) {
	*out = Cfg::limit;
	int m = mode::fast;
}
`,
	})
	use := mustFact(t, ff, "src.use")
	for _, r := range use.Relations {
		if r.Kind == facts.RelCalls {
			t.Errorf("no function is named here, got %+v", r)
		}
	}
}

// A parameter or local passed to a local declaration, or copied into one, is a
// variable, even when a function elsewhere has that name.
func TestCppDeclarationValueThatIsALocal(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/a.cpp": `
struct Holder { Holder(const char *n, int v) {} };
int tag() { return 0; }
int other() { return 0; }
int it() { return 0; }
void use(int tag) {
	int other = 1;
	Holder h("a", tag);
	Holder g{"b", other};
	int it = tag + other;
	int copy = it;
}
`,
	})
	use := mustFact(t, ff, "src.use")
	for _, r := range use.Relations {
		if r.Kind == facts.RelCalls {
			t.Errorf("only variables are named here, got %+v", r)
		}
	}
}

// `T result(Kind::A, type, Fn<X>)` with only type-looking arguments parses as a
// function declaration; its "parameter types" are the arguments.
func TestCppFunctionValueInAVexingParse(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/rle.cpp": `
namespace db {
enum class Kind { RLE };
struct Compression { Compression(Kind k, int t, int (*a)(int), int (*b)(int)) {} };
template <class T> int InitAnalyze(int x) { return x; }
template <class T> int Analyze(int x) { return x; }
template <class T> Compression GetFunction(int data_type) {
	Compression result(Kind::RLE, data_type, InitAnalyze<T>, Analyze<T>);
	return result;
}
}
`,
	})
	get := mustFact(t, ff, "src.db::GetFunction")
	for _, want := range []string{"src.db::InitAnalyze", "src.db::Analyze"} {
		if !hasRelation(get, facts.RelCalls, want) {
			t.Errorf("GetFunction should reference %s, got %+v", want, get.Relations)
		}
	}
}
