package cppextractor

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// --- helpers ---

// extractProject writes the given files (relPath -> content) into a temp dir and
// runs the extractor over them.
func extractProject(t *testing.T, files map[string]string) []facts.Fact {
	t.Helper()
	dir := t.TempDir()
	var rel []string
	for p, content := range files {
		abs := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		rel = append(rel, p)
	}
	sort.Strings(rel) // deterministic ordering for dedup-related assertions
	out, err := New().Extract(context.Background(), dir, rel)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	return out
}

func findFact(ff []facts.Fact, name string) (facts.Fact, bool) {
	for _, f := range ff {
		if f.Name == name {
			return f, true
		}
	}
	return facts.Fact{}, false
}

func countByName(ff []facts.Fact, name string) int {
	n := 0
	for _, f := range ff {
		if f.Name == name {
			n++
		}
	}
	return n
}

func factsOfSymbolKind(ff []facts.Fact, sk string) []facts.Fact {
	var out []facts.Fact
	for _, f := range ff {
		if f.Kind == facts.KindSymbol {
			if got, _ := f.Props["symbol_kind"].(string); got == sk {
				out = append(out, f)
			}
		}
	}
	return out
}

func hasRelation(f facts.Fact, kind, target string) bool {
	for _, r := range f.Relations {
		if r.Kind == kind && r.Target == target {
			return true
		}
	}
	return false
}

func mustFact(t *testing.T, ff []facts.Fact, name string) facts.Fact {
	t.Helper()
	f, ok := findFact(ff, name)
	if !ok {
		t.Fatalf("expected fact %q, not found. names: %v", name, factNames(ff))
	}
	return f
}

func factNames(ff []facts.Fact) []string {
	var out []string
	for _, f := range ff {
		out = append(out, f.Kind+":"+f.Name)
	}
	sort.Strings(out)
	return out
}

// --- Detect ---

func TestDetect(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  bool
	}{
		{"cpp source", map[string]string{"main.cpp": "int main(){}"}, true},
		{"hpp header only", map[string]string{"foo.hpp": "class Foo{};"}, true},
		{"cmake plus header", map[string]string{"CMakeLists.txt": "project(x)", "foo.h": "int x;"}, true},
		{"c source", map[string]string{"main.c": "int main(){}"}, true},                          // pure C is now handled
		{"c only with makefile", map[string]string{"Makefile": "all:", "foo.c": "int x;"}, true}, // .c is decisive
		{"plain header only", map[string]string{"foo.h": "int x;"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			for p, content := range c.files {
				if err := os.WriteFile(filepath.Join(dir, p), []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got, err := New().Detect(dir)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("Detect = %v, want %v", got, c.want)
			}
		})
	}
}

// --- Symbols & inheritance ---

func TestFreeFunction(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/a.cpp": "int compute(int x) { return x + 1; }",
	})
	f := mustFact(t, ff, "src.compute")
	if sk, _ := f.Props["symbol_kind"].(string); sk != facts.SymbolFunc {
		t.Errorf("symbol_kind = %v, want function", f.Props["symbol_kind"])
	}
	if !hasRelation(f, facts.RelDeclares, "src") {
		t.Errorf("missing declares relation to dir, got %+v", f.Relations)
	}
}

func TestClassInheritance(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/m.cpp": `
class Base {};
class Derived : public Base {};
`,
	})
	mustFact(t, ff, "src.Base")
	d := mustFact(t, ff, "src.Derived")
	if sk, _ := d.Props["symbol_kind"].(string); sk != facts.SymbolClass {
		t.Errorf("Derived symbol_kind = %v, want class", d.Props["symbol_kind"])
	}
	if !hasRelation(d, facts.RelImplements, "src.Base") {
		t.Errorf("Derived should implement src.Base (canonicalized), got %+v", d.Relations)
	}
}

func TestStructUnionEnum(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/t.cpp": `
struct Point { int x; int y; };
union Value { int i; float f; };
enum Color { Red, Green };
enum class Status { Ok, Err };
`,
	})
	if sk, _ := mustFact(t, ff, "src.Point").Props["symbol_kind"].(string); sk != facts.SymbolStruct {
		t.Errorf("Point should be struct, got %v", sk)
	}
	u := mustFact(t, ff, "src.Value")
	if u.Props["union"] != true {
		t.Errorf("Value should have union=true, got %+v", u.Props)
	}
	if sk, _ := mustFact(t, ff, "src.Color").Props["symbol_kind"].(string); sk != facts.SymbolEnum {
		t.Errorf("Color should be enum, got %v", sk)
	}
	s := mustFact(t, ff, "src.Status")
	if s.Props["scoped"] != true {
		t.Errorf("Status (enum class) should have scoped=true, got %+v", s.Props)
	}
}

func TestInlineMethod(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/c.cpp": `
class C {
public:
  void m() {}
};
`,
	})
	f := mustFact(t, ff, "src.C::m")
	if sk, _ := f.Props["symbol_kind"].(string); sk != facts.SymbolMethod {
		t.Errorf("C::m symbol_kind = %v, want method", f.Props["symbol_kind"])
	}
	if f.Props["receiver"] != "C" {
		t.Errorf("C::m receiver = %v, want C", f.Props["receiver"])
	}
}

// --- Header/source split (the key C++ challenge) ---

func TestOutOfLineMethodMergesAcrossFiles(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"sub/c.h": `
class C {
public:
  void m();
  void helper();
};
`,
		"sub/c.cpp": `
void C::m() { helper(); }
void C::helper() {}
`,
	})

	// Exactly one class fact and one fact per method (header decl merged with def).
	if n := countByName(ff, "sub.C"); n != 1 {
		t.Errorf("expected exactly 1 sub.C class fact, got %d", n)
	}
	if n := countByName(ff, "sub.C::m"); n != 1 {
		t.Errorf("expected exactly 1 sub.C::m fact, got %d", n)
	}

	m := mustFact(t, ff, "sub.C::m")
	// Definition (with body) wins for File.
	if !strings.HasSuffix(m.File, "c.cpp") {
		t.Errorf("sub.C::m File = %q, want the .cpp definition", m.File)
	}
	// The body's call to a sibling method is preserved and resolved.
	if !hasRelation(m, facts.RelCalls, "sub.C::helper") {
		t.Errorf("sub.C::m should call sub.C::helper, got %+v", m.Relations)
	}
	if m.Props["receiver"] != "C" {
		t.Errorf("sub.C::m receiver = %v, want C", m.Props["receiver"])
	}
}

// --- Namespaces ---

func TestNamespaceNesting(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/n.cpp": `
namespace a { namespace b { void f() {} } }
namespace c::d { void g() {} }
namespace { void anon() {} }
`,
	})
	mustFact(t, ff, "src.a::b::f")
	mustFact(t, ff, "src.c::d::g")
	mustFact(t, ff, "src.anon") // anonymous namespace contributes no scope component
}

func TestNamespacedFunctionMergesAcrossFiles(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"x/e.h": `
namespace E {
  int getOrder(int t);
}
`,
		"x/e.cpp": `
namespace E {}
int E::getOrder(int t) { return t; }
`,
	})
	if n := countByName(ff, "x.E::getOrder"); n != 1 {
		t.Errorf("expected exactly 1 x.E::getOrder fact, got %d: %v", n, factNames(ff))
	}
	f := mustFact(t, ff, "x.E::getOrder")
	if !strings.HasSuffix(f.File, "e.cpp") {
		t.Errorf("x.E::getOrder File = %q, want the .cpp definition", f.File)
	}
}

// --- Call graph ---

func TestCallGraph(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/g.cpp": `
class Msg { public: static void Error(const char *s); };
class Worker {
public:
  void run();
  void step();
};
void Worker::run() {
  this->step();
  Msg::Error("boom");
  Worker w = Worker();
}
`,
	})
	run := mustFact(t, ff, "src.Worker::run")
	if !hasRelation(run, facts.RelCalls, "src.Worker::step") {
		t.Errorf("run should call src.Worker::step, got %+v", run.Relations)
	}
	if !hasRelation(run, facts.RelCalls, "src.Msg::Error") {
		t.Errorf("run should call src.Msg::Error (canonicalized), got %+v", run.Relations)
	}
	if !hasRelation(run, facts.RelInstantiates, "src.Worker") {
		t.Errorf("run should instantiate src.Worker, got %+v", run.Relations)
	}
}

func TestExplicitParameterAndLocalReceiverCalls(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/worker.cpp": `
class Worker { public: void step(); };
void Worker::step() {}
void run(Worker &parameter) {
  Worker local;
  Worker *pointer = &local;
  parameter.step();
  local.step();
  pointer->step();
}`,
	})
	run := mustFact(t, ff, "src.run")
	if !hasRelation(run, facts.RelCalls, "src.Worker::step") {
		t.Fatalf("explicit receiver calls were not resolved: %+v", run.Relations)
	}
}

func TestReceiverTypingRefusesAutoAndTemplateWrappers(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/worker.cpp": `
class Worker { public: void step(); };
void Worker::step() {}
void run(Worker source) {
  auto inferred = source;
  Box<Worker> wrapped;
  inferred.step();
  wrapped.step();
}`,
	})
	run := mustFact(t, ff, "src.run")
	if hasRelation(run, facts.RelCalls, "src.Worker::step") {
		t.Fatalf("auto or template wrapper was treated as Worker: %+v", run.Relations)
	}
}

func TestReceiverTypingDropsMissingMethods(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/worker.cpp": `
class Worker {};
void run(Worker worker) { worker.missing(); }`,
	})
	run := mustFact(t, ff, "src.run")
	for _, rel := range run.Relations {
		if rel.Kind == facts.RelCalls || rel.Kind == relReceiverCallCandidate {
			t.Fatalf("unconfirmed receiver call leaked into the graph: %+v", run.Relations)
		}
	}
}

// duplicatedLexer mirrors two vendored copies of one C header: the same struct,
// with the same function-pointer members, declared in two directories.
func duplicatedLexer() map[string]string {
	const header = `typedef struct Lexer Lexer;
struct Lexer {
  void (*advance)(Lexer *);
};`
	const scanner = `#include "tree/lexer.h"
void scan(Lexer *lexer) { lexer->advance(lexer); }`
	return map[string]string{
		"a/tree/lexer.h": header,
		"a/scan.c":       scanner,
		"b/tree/lexer.h": header,
		"b/scan.c":       scanner,
	}
}

func TestReceiverCallBindsToTheCopyTheCallerIncludes(t *testing.T) {
	ff := extractProject(t, duplicatedLexer())
	for _, dir := range []string{"a", "b"} {
		other := "b"
		if dir == "b" {
			other = "a"
		}
		scan := mustFact(t, ff, dir+".scan")
		if !hasRelation(scan, facts.RelCalls, dir+"/tree.Lexer::advance") {
			t.Errorf("%s/scan.c should call its own copy of Lexer::advance, got %+v", dir, scan.Relations)
		}
		if hasRelation(scan, facts.RelCalls, other+"/tree.Lexer::advance") {
			t.Errorf("%s/scan.c was bound to the copy under %s/, which it never includes: %+v", dir, other, scan.Relations)
		}
	}
}

func TestReceiverCallStaysUnboundWhenNoCopyIsVisible(t *testing.T) {
	files := duplicatedLexer()
	files["c/scan.c"] = `typedef struct Lexer Lexer;
void scan(Lexer *lexer) { lexer->advance(lexer); }`
	ff := extractProject(t, files)
	scan := mustFact(t, ff, "c.scan")
	for _, rel := range scan.Relations {
		if rel.Kind == facts.RelCalls || rel.Kind == relReceiverCallCandidate {
			t.Fatalf("a receiver call with two candidate types and no include path was bound: %+v", scan.Relations)
		}
	}
}

func TestReceiverCallPrefersTheCallersOwnDirectory(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"app/worker.h":    `class Worker { public: void step(); };`,
		"vendor/worker.h": `class Worker { public: void step(); };`,
		"vendor/extra.h":  `void extra();`,
		// A C++ source keeps vendor/'s bare .h files on the C++ grammar.
		"vendor/extra.cpp": `void extra() {}`,
		"app/run.cpp": `#include "worker.h"
#include "../vendor/extra.h"
void run(Worker *worker) { worker->step(); }`,
	})
	run := mustFact(t, ff, "app.run")
	if !hasRelation(run, facts.RelCalls, "app.Worker::step") {
		t.Fatalf("a type declared in the caller's own directory should win over an included copy: %+v", run.Relations)
	}
}

func TestReceiverCallBindsToTheOnlyCopyDeclaringTheMethod(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"lib/worker.h":     `class Worker { public: void step(); };`,
		"lib/worker.cpp":   `void Worker::step() {}`,
		"other/worker.h":   `class Worker { public: int id; };`,
		"other/worker.cpp": `int unrelated() { return 0; }`,
		"app/run.cpp":      `void run(Worker *worker) { worker->step(); }`,
	})
	run := mustFact(t, ff, "app.run")
	if !hasRelation(run, facts.RelCalls, "lib.Worker::step") {
		t.Fatalf("Worker::step is declared once in the repository and should resolve: %+v", run.Relations)
	}
}

// duplicatedBase declares one class name in two directories, each with a consumer
// that includes only its own copy.
func duplicatedBase(consumer string) map[string]string {
	const header = `class Base { public: Base(); static void init(); };`
	return map[string]string{
		"a/base.h":  header,
		"a/use.cpp": "#include \"base.h\"\n" + consumer,
		"b/base.h":  header,
		"b/use.cpp": "#include \"base.h\"\n" + consumer,
	}
}

func TestBaseClassBindsToTheCopyTheCallerIncludes(t *testing.T) {
	ff := extractProject(t, duplicatedBase(`class Derived : public Base {};`))
	derived := mustFact(t, ff, "a.Derived")
	if !hasRelation(derived, facts.RelImplements, "a.Base") || hasRelation(derived, facts.RelImplements, "b.Base") {
		t.Fatalf("a.Derived should implement a.Base only, got %+v", derived.Relations)
	}
}

func TestConstructorBindsToTheCopyTheCallerIncludes(t *testing.T) {
	ff := extractProject(t, duplicatedBase(`void make() { Base made = Base(); }`))
	make := mustFact(t, ff, "a.make")
	if !hasRelation(make, facts.RelInstantiates, "a.Base") || hasRelation(make, facts.RelInstantiates, "b.Base") {
		t.Fatalf("a.make should instantiate a.Base only, got %+v", make.Relations)
	}
}

func TestScopedCallBindsToTheCopyTheCallerIncludes(t *testing.T) {
	ff := extractProject(t, duplicatedBase(`void start() { Base::init(); }`))
	start := mustFact(t, ff, "a.start")
	if !hasRelation(start, facts.RelCalls, "a.Base::init") || hasRelation(start, facts.RelCalls, "b.Base::init") {
		t.Fatalf("a.start should call a.Base::init only, got %+v", start.Relations)
	}
}

func TestDuplicatedTypeWithNoVisibleCopyIsNotGuessed(t *testing.T) {
	files := duplicatedBase(`void start() { Base::init(); }`)
	files["c/use.cpp"] = `class Derived : public Base {};
void make() { Base made = Base(); }
void start() { Base::init(); }`
	ff := extractProject(t, files)
	for _, name := range []string{"c.Derived", "c.make", "c.start"} {
		for _, rel := range mustFact(t, ff, name).Relations {
			if strings.HasPrefix(rel.Target, "a.Base") || strings.HasPrefix(rel.Target, "b.Base") {
				t.Errorf("%s was bound to one of two copies it cannot see: %+v", name, rel)
			}
		}
	}
}

func TestNamespacedMemberResolvesInsideTheChosenDirectory(t *testing.T) {
	const header = `namespace ns { class Server { public: static void instance(); }; }`
	const use = `#include "server.h"
using namespace ns;
void start() { Server::instance(); }
void poll(Server *server) { server->instance(); }`
	ff := extractProject(t, map[string]string{
		"a/server.h": header, "a/use.cpp": use,
		"b/server.h": header, "b/use.cpp": use,
	})
	for _, name := range []string{"a.start", "a.poll"} {
		caller := mustFact(t, ff, name)
		if !hasRelation(caller, facts.RelCalls, "a.ns::Server::instance") {
			t.Errorf("%s should call a.ns::Server::instance, got %+v", name, caller.Relations)
		}
	}
}

func TestBaseClassInsideNamespaceResolvesByItsSimpleName(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/shape.cpp": `namespace geo { class Surface {}; }
using namespace geo;
class Plane : public Surface {};
void make() { Surface made = Surface(); }`,
	})
	if plane := mustFact(t, ff, "src.Plane"); !hasRelation(plane, facts.RelImplements, "src.geo::Surface") {
		t.Errorf("src.Plane should implement src.geo::Surface, got %+v", plane.Relations)
	}
	if made := mustFact(t, ff, "src.make"); !hasRelation(made, facts.RelInstantiates, "src.geo::Surface") {
		t.Errorf("src.make should instantiate src.geo::Surface, got %+v", made.Relations)
	}
}

func TestReferenceParameterAndLocalReceiverCalls(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/worker.cpp": `
class Worker { public: void step(); void stop(); void rest() const; };
void Worker::step() {}
void Worker::stop() {}
void Worker::rest() const {}
void byParameter(Worker &worker) { worker.step(); }
void byConstParameter(const Worker &worker) { worker.rest(); }
void byLocal(Worker *source) {
  Worker &alias = *source;
  alias.stop();
}`,
	})
	for caller, method := range map[string]string{"byParameter": "step", "byConstParameter": "rest", "byLocal": "stop"} {
		if f := mustFact(t, ff, "src."+caller); !hasRelation(f, facts.RelCalls, "src.Worker::"+method) {
			t.Errorf("%s: a call through a reference was not resolved: %+v", caller, f.Relations)
		}
	}
}

func TestFileScopeMacroReferenceStaysInItsOwnDirectory(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"lib/setup.h": `int setup(void);`,
		"lib/setup.c": `int setup(void) { return 0; }`,
		"drv/mod.c": `#include "../lib/setup.h"
int local(void) { return 0; }
module_init(setup);
module_exit(local);`,
	})
	drv := mustFact(t, ff, "drv")
	if !hasRelation(drv, facts.RelCalls, "drv.local") {
		t.Errorf("a registration macro naming a function in its own directory should resolve: %+v", drv.Relations)
	}
	if hasRelation(drv, facts.RelCalls, "lib.setup") {
		t.Errorf("a bare file-scope identifier was bound to a function in another directory: %+v", drv.Relations)
	}
}

func TestBareCallResolvesToUniqueFunctionInAnotherDirectory(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"lib/check.h":   `bool check(int);`,
		"lib/check.cpp": `bool check(int n) { return n > 0; }`,
		"app/run.cpp": `#include "../lib/check.h"
void run() { check(1); }`,
	})
	run := mustFact(t, ff, "app.run")
	if !hasRelation(run, facts.RelCalls, "lib.check") {
		t.Fatalf("unique cross-directory free function was not resolved: %+v", run.Relations)
	}
}

func TestBareCallDoesNotResolveToInvisibleUniqueFunction(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"lib/check.cpp": `bool check(int n) { return n > 0; }`,
		"app/run.cpp":   `void run() { check(1); }`,
	})
	run := mustFact(t, ff, "app.run")
	if !hasRelation(run, facts.RelCalls, "app.check") {
		t.Fatalf("call without an include path to the declaration should remain unresolved: %+v", run.Relations)
	}
	if hasRelation(run, facts.RelCalls, "lib.check") {
		t.Fatalf("invisible unique function was guessed: %+v", run.Relations)
	}
}

func TestBareCallDoesNotGuessBetweenDuplicateFunctionNames(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"one/check.cpp": `bool check(int n) { return n > 0; }`,
		"two/check.cpp": `bool check(int n) { return n < 0; }`,
		"app/run.cpp":   `void run() { check(1); }`,
	})
	run := mustFact(t, ff, "app.run")
	if !hasRelation(run, facts.RelCalls, "app.check") {
		t.Fatalf("ambiguous bare call should retain its unresolved local candidate: %+v", run.Relations)
	}
	if hasRelation(run, facts.RelCalls, "one.check") || hasRelation(run, facts.RelCalls, "two.check") {
		t.Fatalf("ambiguous bare call was guessed: %+v", run.Relations)
	}
}

func TestQualifiedCallResolvesByUniqueQualifiedSuffix(t *testing.T) {
	ff := []facts.Fact{
		{Kind: facts.KindSymbol, Name: "lib.DB::Util::check", File: "lib/check.cpp", Props: map[string]any{"symbol_kind": facts.SymbolMethod}},
		{Kind: facts.KindSymbol, Name: "app.run", File: "app/run.cpp", Props: map[string]any{"symbol_kind": facts.SymbolFunc}, Relations: []facts.Relation{{Kind: facts.RelCalls, Target: "Util::check"}}},
	}
	visibility := &includeVisibility{edges: map[string]map[string]bool{"app": {"lib": true}}, cache: map[string]map[string]bool{}}
	types := newTypeIndex()
	types.add("Util", "lib", "lib.DB::Util")
	stats := canonicalizeTargets(ff, types, indexFunctionNames(ff), visibility)
	run := mustFact(t, ff, "app.run")
	if !hasRelation(run, facts.RelCalls, "lib.DB::Util::check") {
		t.Fatalf("unique qualified cross-directory function was not resolved: %+v", run.Relations)
	}
	if stats.qualifiedSuffix != 1 || stats.unresolved != 0 {
		t.Fatalf("resolution stats = %+v", stats)
	}
}

// --- Templates ---

func TestTemplates(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/tpl.cpp": `
template <class T> class Box { T value; };
class IntBox : public Box<int> {};
`,
	})
	box := mustFact(t, ff, "src.Box")
	if box.Props["templated"] != true {
		t.Errorf("Box should be templated, got %+v", box.Props)
	}
	ib := mustFact(t, ff, "src.IntBox")
	// Template args stripped; base resolves to src.Box.
	if !hasRelation(ib, facts.RelImplements, "src.Box") {
		t.Errorf("IntBox should implement src.Box (args stripped), got %+v", ib.Relations)
	}
}

// --- Includes ---

// TestCppFunctionPointerInitializer mirrors the C ops-table case under the C++
// grammar (shared node kinds): a struct with function-pointer members initialized
// with function names yields RelCalls edges from the variable.
func TestCppFunctionPointerInitializer(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/ops.cpp": `
static int reader() { return 0; }
static int writer() { return 0; }
static const struct ops_t ops = {
	.read = reader,
	.write = writer,
};
`,
	})
	ops := mustFact(t, ff, "src.ops")
	if !hasRelation(ops, facts.RelCalls, "src.reader") || !hasRelation(ops, facts.RelCalls, "src.writer") {
		t.Errorf("ops should reference reader and writer, got %+v", ops.Relations)
	}
}

// TestCppCallbackArgReference mirrors the callback-arg rescue under the C++ grammar.
func TestCppCallbackArgReference(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/reg.cpp": `
static int onEvent() { return 0; }
static int setup() { return subscribe(onEvent); }
`,
	})
	setup := mustFact(t, ff, "src.setup")
	if !hasRelation(setup, facts.RelCalls, "src.onEvent") {
		t.Errorf("setup should reference onEvent passed as a callback arg, got %+v", setup.Relations)
	}
}

func TestIncludes(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"lib/other.h": "class Other {};",
		"lib/use.cpp": `
#include <vector>
#include "other.h"
void use() {}
`,
	})
	// System include produces no dependency fact.
	for _, f := range ff {
		if f.Kind == facts.KindDependency {
			if inc, _ := f.Props["include"].(string); strings.Contains(inc, "vector") {
				t.Errorf("system include <vector> should not yield a dependency fact")
			}
		}
	}
	// Quoted include resolves to the declaring module dir.
	var found bool
	for _, f := range ff {
		if f.Kind == facts.KindDependency {
			if inc, _ := f.Props["include"].(string); inc == "other.h" {
				found = true
				if !hasRelation(f, facts.RelImports, "lib") {
					t.Errorf("other.h dependency should import module 'lib', got %+v", f.Relations)
				}
				if f.Props["source"] != "internal" {
					t.Errorf("other.h should be internal, got %v", f.Props["source"])
				}
			}
		}
	}
	if !found {
		t.Errorf("expected a dependency fact for #include \"other.h\"")
	}
}

func TestProjectAngleIncludeIsInternalButSystemHeaderIsDropped(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"include/project/thing.h": `class Thing {};`,
		"src/use.cpp": `#include <project/thing.h>
#include <vector>
Thing makeThing() { return Thing(); }`,
	})
	var project, system bool
	for _, f := range ff {
		if f.Kind != facts.KindDependency {
			continue
		}
		switch f.PropString("include") {
		case "project/thing.h":
			project = f.PropString("source") == "internal" && f.PropString("include_style") == "angle"
		case "vector":
			system = true
		}
	}
	if !project {
		t.Error("repository-owned angle include was not retained as internal")
	}
	if system {
		t.Error("unresolved system angle include should not become a dependency")
	}
}

func TestHeaderPathIndexDoesNotCrossVendoredLibrariesByBasename(t *testing.T) {
	index := buildHeaderPathIndex([]string{
		"third_party/jemalloc/include/jemalloc/internal/mutex.h",
		"third_party/re2/util/mutex.h",
		"third_party/jemalloc/include/jemalloc/internal/assert.h",
	})
	if got := index.resolve("third_party/jemalloc/src/arena.c", "jemalloc/internal/mutex.h", "quoted"); got != "third_party/jemalloc/include/jemalloc/internal" {
		t.Fatalf("path-qualified jemalloc header resolved to %q", got)
	}
	if got := index.resolve("third_party/re2/re2/re2.cc", "assert.h", "angle"); got != "" {
		t.Fatalf("bare system angle header resolved into vendored code: %q", got)
	}
	if got := index.resolve("src/use.cpp", "mutex.h", "quoted"); got != "" {
		t.Fatalf("ambiguous bare header basename was guessed: %q", got)
	}
}

// --- Forward declarations & operators ---

func TestForwardDeclarationEmitsNoSymbol(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/f.cpp": `
class Fwd;
class Real { Fwd *p; };
`,
	})
	if _, ok := findFact(ff, "src.Fwd"); ok {
		t.Errorf("forward declaration src.Fwd should not emit a symbol")
	}
	mustFact(t, ff, "src.Real")
}

func TestOperatorOverload(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/o.cpp": `
class V {
public:
  bool operator==(const V &o) const;
};
`,
	})
	// Should not crash; the operator method is named and carries the receiver.
	ops := factsOfSymbolKind(ff, facts.SymbolMethod)
	var found bool
	for _, f := range ops {
		if strings.Contains(f.Name, "operator==") && f.Props["receiver"] == "V" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected V::operator== method fact, got %v", factNames(ff))
	}
}

func TestFieldVsMethod(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/fm.cpp": `
class C {
  int x;
  const int y = 3;
  void m();
};
`,
	})
	if sk, _ := mustFact(t, ff, "src.C::x").Props["symbol_kind"].(string); sk != facts.SymbolVariable {
		t.Errorf("C::x should be variable, got %v", sk)
	}
	if sk, _ := mustFact(t, ff, "src.C::y").Props["symbol_kind"].(string); sk != facts.SymbolConstant {
		t.Errorf("C::y (const) should be constant, got %v", sk)
	}
	if sk, _ := mustFact(t, ff, "src.C::m").Props["symbol_kind"].(string); sk != facts.SymbolMethod {
		t.Errorf("C::m should be method, got %v", sk)
	}
}

// --- Preprocessor guards ---

func TestPreprocGuardedClass(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/p.cpp": `
#ifdef FEATURE_X
class Guarded { int z; };
#endif

#if defined(HAVE_FOO)
namespace ns { void wrapped() {} }
#endif
`,
	})
	mustFact(t, ff, "src.Guarded")     // proves #ifdef recursion
	mustFact(t, ff, "src.ns::wrapped") // proves #if + namespace recursion
}

// --- Module facts ---

func TestModuleFacts(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"core/a.cpp": "void a(){}",
		"util/b.cpp": "void b(){}",
	})
	for _, dir := range []string{"core", "util"} {
		f, ok := findFact(ff, dir)
		if !ok || f.Kind != facts.KindModule {
			t.Errorf("expected module fact for %q", dir)
		}
	}
}
