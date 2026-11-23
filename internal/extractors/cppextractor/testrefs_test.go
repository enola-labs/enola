package cppextractor

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/pkg/plugin"
)

var _ plugin.TestAwareExtractor = (*CppExtractor)(nil)

// extractTestRefs runs the production extraction with test files beside it and
// returns the test_ref facts. It also pins that the production facts are exactly
// Extract's over the production files alone: the tests change nothing there.
func extractTestRefs(t *testing.T, repo string, tests, prod []string) ([]facts.Fact, error) {
	t.Helper()
	all, err := New().ExtractWithTests(context.Background(), repo, prod, tests)
	if err != nil {
		return nil, err
	}
	alone, err := New().Extract(context.Background(), repo, prod)
	if err != nil {
		return nil, err
	}
	var refs, rest []facts.Fact
	for _, f := range all {
		if f.Kind == facts.KindTestRef {
			refs = append(refs, f)
		} else {
			rest = append(rest, f)
		}
	}
	if got, want := factKeys(rest), factKeys(alone); !equalStrings(got, want) {
		t.Errorf("test files changed the production facts:\n got %v\nwant %v", got, want)
	}
	return refs, nil
}

// factKeys renders facts with their relations, sorted, for comparison. Module
// facts come out of a map, so order is not part of the contract.
func factKeys(ff []facts.Fact) []string {
	out := make([]string, 0, len(ff))
	for _, f := range ff {
		k := f.Kind + "|" + f.Name + "|" + f.File
		for _, r := range f.Relations {
			k += "|" + r.Kind + ">" + r.Target
		}
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func writeCppRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for p, content := range files {
		abs := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

var cppTestRefRepo = map[string]string{
	"src/conn.hpp": `namespace db { class Connection { public: Connection(int); int Query(); }; }`,
	"src/conn.cpp": "#include \"conn.hpp\"\n" +
		`namespace db { Connection::Connection(int) {} int Connection::Query() { return 0; } }`,
	"src/other.hpp": `namespace db { class Appender { public: int Query(); }; }`,
	"src/other.cpp": "#include \"other.hpp\"\n" +
		`namespace db { int Appender::Query() { return 1; } }`,
	"src/util.c": `int checksum(int x) { return x; }`,
	"test/api/test_conn.cpp": "#include \"../../src/conn.hpp\"\n" +
		`static int expect(int v) { return v; }
void TestQuery() { db::Connection con(1); expect(con.Query()); }`,
	// The prototype names the production function; its target carries no test
	// directory.
	"lib/tests/util_test.c": `int checksum(int x);
static void run_case(void) { checksum(3); }`,
	"notes.txt": "not code\n",
}

// TestCppExtractTestRefsEmitsOnlyTestRefFacts pins the reference-only pass over
// ignored C and C++ test trees: each test file yields one test_ref fact naming the
// production code it reaches, resolved as the production walk resolves a call
// (a member call on a typed local binds to that type's method, not to every
// method of the name), and nothing else: no symbol, no module, no dependency. A
// call to a helper the test declares is not a reference.
func TestCppExtractTestRefsEmitsOnlyTestRefFacts(t *testing.T) {
	repo := writeCppRepo(t, cppTestRefRepo)
	prod := []string{"src/conn.hpp", "src/conn.cpp", "src/other.hpp", "src/other.cpp", "src/util.c"}
	tests := []string{"test/api/test_conn.cpp", "lib/tests/util_test.c", "notes.txt"}

	got, err := extractTestRefs(t, repo, tests, prod)
	if err != nil {
		t.Fatal(err)
	}
	byFile := map[string]facts.Fact{}
	for _, f := range got {
		if f.Kind != facts.KindTestRef {
			t.Fatalf("emitted a %s fact %q; only test_ref is allowed", f.Kind, f.Name)
		}
		for _, r := range f.Relations {
			if r.Kind != facts.RelCalls {
				t.Errorf("%s: relation kind %s, want calls", f.Name, r.Kind)
			}
		}
		byFile[f.File] = f
	}
	if len(byFile) != 2 {
		t.Fatalf("want test refs for the two test sources, got %v", keysOf(byFile))
	}

	conn := byFile["test/api/test_conn.cpp"]
	for _, want := range []string{"src.db::Connection::Query"} {
		if !hasRelation(conn, facts.RelCalls, want) {
			t.Errorf("test_conn.cpp should reference %s, got %+v", want, conn.Relations)
		}
	}
	for _, r := range conn.Relations {
		switch r.Target {
		case "src.db::Appender::Query":
			t.Errorf("a call on a Connection local bound to Appender::Query: %+v", conn.Relations)
		case "test/api.expect":
			t.Errorf("the test's own helper is not a reference: %+v", conn.Relations)
		}
	}
	if util := byFile["lib/tests/util_test.c"]; !hasRelation(util, facts.RelCalls, "checksum") {
		t.Errorf("util_test.c should keep the call through its own prototype of checksum, got %+v", util.Relations)
	}
}

// TestCppTestRefsMatchTheEdgesTestsHadWhenIndexed pins that ignoring a test tree
// changes where its references live, not what they resolve to: the test_ref
// targets are exactly the usage edges the same file's symbols carried when it was
// extracted as code, less the calls into the file's own declarations.
func TestCppTestRefsMatchTheEdgesTestsHadWhenIndexed(t *testing.T) {
	repo := writeCppRepo(t, cppTestRefRepo)
	all := []string{"lib/tests/util_test.c", "src/conn.cpp", "src/conn.hpp", "src/other.cpp", "src/other.hpp", "src/util.c", "test/api/test_conn.cpp"}
	indexed, err := New().Extract(context.Background(), repo, all)
	if err != nil {
		t.Fatal(err)
	}
	declared := map[string]bool{}
	for _, f := range indexed {
		if f.Kind == facts.KindSymbol && f.File == "test/api/test_conn.cpp" {
			declared[f.Name] = true
		}
	}
	var want []string
	for _, f := range indexed {
		if f.Kind != facts.KindSymbol || f.File != "test/api/test_conn.cpp" {
			continue
		}
		for _, r := range f.Relations {
			if isUsageRel(r.Kind) && !declared[r.Target] {
				want = append(want, r.Target)
			}
		}
	}
	want = sortedUnique(want)

	got, err := extractTestRefs(t, repo,
		[]string{"test/api/test_conn.cpp"}, []string{"src/conn.cpp", "src/conn.hpp", "src/other.cpp", "src/other.hpp", "src/util.c"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want one test_ref, got %d", len(got))
	}
	var have []string
	for _, r := range got[0].Relations {
		have = append(have, r.Target)
	}
	if len(want) == 0 || !equalStrings(have, want) {
		t.Errorf("test_ref targets %v, want the indexed edges %v", have, want)
	}
}

func keysOf(m map[string]facts.Fact) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedUnique(in []string) []string {
	sort.Strings(in)
	out := in[:0]
	for i, s := range in {
		if i == 0 || s != in[i-1] {
			out = append(out, s)
		}
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestCppTestRefsCreditLifecycleAndInheritedMembers pins the two references a
// test's own edges do not spell. Creating an object runs the constructor and
// destructor of its class and of each base, which the orphan detector credits from
// an instantiates edge a test ref does not carry, so the test ref names them. A
// member call on a field of a test fixture, declared on a base class, is resolved
// by the inherited-calls binder, which the pass runs as the snapshot does.
func TestCppTestRefsCreditLifecycleAndInheritedMembers(t *testing.T) {
	repo := writeCppRepo(t, map[string]string{
		"src/shape.hpp": `namespace geo {
class Shape { public: Shape(); ~Shape(); void Ping(); };
class Circle : public Shape { public: Circle(); };
}`,
		"src/shape.cpp": "#include \"shape.hpp\"\n" +
			`namespace geo { Shape::Shape() {} Shape::~Shape() {} void Shape::Ping() {} Circle::Circle() {} }`,
		"test/shape_test.cpp": "#include \"../src/shape.hpp\"\n" +
			`namespace geo {
class Fixture { public: Fixture() { c.Ping(); } Circle c; };
void TestMake() { Circle made = Circle(); }
}`,
	})
	got, err := extractTestRefs(t, repo,
		[]string{"test/shape_test.cpp"}, []string{"src/shape.cpp", "src/shape.hpp"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want one test_ref, got %d", len(got))
	}
	for _, want := range []string{
		"geo::Circle::Circle", "geo::Circle::~Circle", "geo::Shape::Shape", "geo::Shape::~Shape",
		"src.geo::Shape::Ping",
	} {
		if !hasRelation(got[0], facts.RelCalls, want) {
			t.Errorf("shape_test.cpp should reference %s, got %+v", want, got[0].Relations)
		}
	}
}

// TestCppTestRefsCreditAProductionHeaderOfTestHelpers pins helpers a production
// header declares and the test tree defines (KUnit helper libraries): a test's
// call reaches the header's declaration, so it is credited, while a static helper
// of the same shape stays the test's own.
func TestCppTestRefsCreditAProductionHeaderOfTestHelpers(t *testing.T) {
	repo := writeCppRepo(t, map[string]string{
		"include/kunit_helpers.h": `int make_fixture(int n);
int local_twin(int n);`,
		"src/core.c":              `int local_twin(int n) { return n; }`,
		"drivers/tests/helpers.c": `int make_fixture(int n) { return n; }`,
		"drivers/tests/case_test.c": "#include \"../../include/kunit_helpers.h\"\n" +
			`static int local_twin(int n) { return n; }
static void run(void) { make_fixture(1); local_twin(2); }`,
	})
	got, err := extractTestRefs(t, repo,
		[]string{"drivers/tests/case_test.c", "drivers/tests/helpers.c"},
		[]string{"include/kunit_helpers.h", "src/core.c"})
	if err != nil {
		t.Fatal(err)
	}
	var ref facts.Fact
	for _, f := range got {
		if f.File == "drivers/tests/case_test.c" {
			ref = f
		}
	}
	if !hasRelation(ref, facts.RelCalls, "make_fixture") {
		t.Errorf("the call to a helper the production header declares should be credited, got %+v", ref.Relations)
	}
	for _, r := range ref.Relations {
		if r.Target == "local_twin" || r.Target == "drivers/tests.local_twin" || r.Target == "src.local_twin" {
			t.Errorf("a static helper is the test's own, got %+v", ref.Relations)
		}
	}
}
