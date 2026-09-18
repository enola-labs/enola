package dartextractor

import (
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// funcProps returns the props of the first top-level function in src.
func funcProps(t *testing.T, src string) map[string]any {
	t.Helper()
	for _, f := range walkSource(t, "lib/a.dart", src) {
		if f.Kind == facts.KindSymbol && f.PropString("symbol_kind") == facts.SymbolFunc {
			return f.Props
		}
	}
	t.Fatalf("no function symbol extracted from:\n%s", src)
	return nil
}

// TestCyclomaticCountsDartOperators pins the operator counting, which was wrong in both
// directions before it was measured.
//
// Dart does not model `&&` as a generic binary expression: it has its own node kinds,
// and the OPERATOR is a node of its own. Matching a generic `binary_expression` counted
// nothing at all, so every logical operator in the corpus was invisible. Counting
// occurrences in the enclosing expression's TEXT would have been wrong the other way —
// `a && b && c` nests, so the outer node's operators would be recounted at each level.
// Counting operator nodes is exact.
func TestCyclomaticCountsDartOperators(t *testing.T) {
	for _, tc := range []struct {
		name, src string
		want      int
	}{
		{"straight line", "int f() { return 1; }", 1},
		{"one and", "bool f(a, b) { return a && b; }", 2},
		{"chained ands do not double count", "bool f(a, b, c, d) { return a && b && c && d; }", 4},
		{"mixed and/or", "bool f(a, b, c) { return a && b || c; }", 3},
		{"null coalescing short-circuits", "int f(a, b) { return a ?? b; }", 2},
		{"ternary", "int f(a, b, c) { return a ? b : c; }", 2},
		{"if", "int f(a) { if (a) { return 1; } return 0; }", 2},
		{"switch arms", "String f(x) { switch (x) { case 1: return 'a'; case 2: return 'b'; } return ''; }", 3},
		{"catch", "void f() { try { g(); } catch (e) { h(); } }", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := funcProps(t, tc.src)["cyclomatic"].(int)
			if got != tc.want {
				t.Errorf("cyclomatic = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestConstantTripLoopDiscount pins the discount that keeps an honest O(n) from being
// reported as O(n²).
//
// A C-style `for` and a `for-in` are the SAME node kind in this grammar and are told
// apart only by their for_loop_parts — the C-style form carries a relational_expression
// condition, the for-in form carries none. Matching on node kinds that do not exist
// (which is what the first attempt did) makes the discount silently never apply, so
// every literal-bounded loop inflates the scaling depth the performance analyzer reads.
func TestConstantTripLoopDiscount(t *testing.T) {
	for _, tc := range []struct {
		name, src        string
		wantLoopDepth    int
		wantScalingDepth int
	}{
		{
			name:             "literal bound does not scale",
			src:              "void f() { for (var i = 0; i < 10; i++) { g(i); } }",
			wantLoopDepth:    1,
			wantScalingDepth: 0,
		},
		{
			name:             "for-in scales with its collection",
			src:              "void f(items) { for (final x in items) { g(x); } }",
			wantLoopDepth:    1,
			wantScalingDepth: 1,
		},
		{
			name:             "a length bound scales even in C-style form",
			src:              "void f(xs) { for (var i = 0; i < xs.length; i++) { g(i); } }",
			wantLoopDepth:    1,
			wantScalingDepth: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			props := funcProps(t, tc.src)
			gotLoop, _ := props["loop_depth"].(int)
			gotScaling, _ := props["scaling_loop_depth"].(int)
			if gotLoop != tc.wantLoopDepth {
				t.Errorf("loop_depth = %d, want %d", gotLoop, tc.wantLoopDepth)
			}
			if gotScaling != tc.wantScalingDepth {
				t.Errorf("scaling_loop_depth = %d, want %d", gotScaling, tc.wantScalingDepth)
			}
		})
	}
}

// TestCallsResolveOnlyToCallables pins that a call edge never binds to a constant.
//
// Bare call targets resolve by unique short name, and Dart's short names collide across
// kinds. immich declares the enum constant `LogLevel.severe` and separately calls
// `log.severe(...)` on a logger; with every symbol kind in the index the constant was
// the unique `severe`, so 117 call sites bound to it and the god-class explainer
// reported a data constant as a high-fan-in symbol with 117 dependents.
func TestCallsResolveOnlyToCallables(t *testing.T) {
	src := `
enum LogLevel { info, severe }

class Logger {
  void warn(String m) {}
}

void doWork(Logger log) {
  log.severe('boom');
  log.warn('careful');
}
`
	all := resolveCallTargets(walkSource(t, "lib/a.dart", src))

	var work *facts.Fact
	for i := range all {
		if all[i].Name == "lib.doWork" {
			work = &all[i]
		}
	}
	if work == nil {
		t.Fatal("missing doWork")
	}
	for _, r := range work.Relations {
		if r.Kind != facts.RelCalls {
			continue
		}
		if r.Target == "lib.LogLevel.severe" {
			t.Error("a call resolved to an ENUM CONSTANT; it must stay bare instead")
		}
	}
	// The callable one still resolves, so the guard costs no real edge.
	found := false
	for _, r := range work.Relations {
		if r.Kind == facts.RelCalls && r.Target == "lib.Logger.warn" {
			found = true
		}
	}
	if !found {
		t.Error("a call to a real method should still resolve to it")
	}
}

// TestClosureBodiesContributeCalls pins a missed-edge class that inflated the
// dead-code report.
//
// A closure body's invocation is a direct child SEQUENCE of the closure node
// (`() => getTiles(ctx)` is [identifier getTiles][selector (ctx)]), so a walk that
// descends into the closure's children without also scanning the closure node itself
// loses the call entirely. Arrow closures are pervasive in Flutter — `onPressed: () =>
// save()`, `builder: (c) => Widget()`, `.map((e) => f(e))` — and every call inside one
// was invisible, so functions that are plainly used were reported as dead.
func TestClosureBodiesContributeCalls(t *testing.T) {
	src := `
List<int> getTiles(x) => [1];
void other() {}

void build() {
  final a = useMemoized(() => getTiles(ctx));
  final b = [1, 2].map((e) => other());
}
`
	var build *facts.Fact
	all := walkSource(t, "lib/a.dart", src)
	for i := range all {
		if all[i].Name == "lib.build" {
			build = &all[i]
		}
	}
	if build == nil {
		t.Fatal("missing build")
	}
	targets := map[string]bool{}
	for _, r := range build.Relations {
		if r.Kind == facts.RelCalls {
			targets[r.Target] = true
		}
	}
	for _, want := range []string{"getTiles", "other"} {
		if !targets[want] {
			t.Errorf("a call inside a closure body should be recorded: missing %q (got %v)",
				want, keysOf(targets))
		}
	}
}

// TestRecursionRequiresSelfReceiver pins that recursion means reaching THIS symbol.
//
// Matching on the short name alone dominated the performance report: `dispose()`
// calling `controller.dispose()`, `fromJson` calling a nested `fromJson`, `stop()`
// calling `player.stop()` are all ordinary delegation to a different object. On one
// mid-size Flutter app that produced 64 false recursion findings — 63 of the 75
// performance findings the analyzer emitted.
func TestRecursionRequiresSelfReceiver(t *testing.T) {
	for _, tc := range []struct {
		name, src string
		want      bool
	}{
		{
			name: "bare self call is recursion",
			src:  "int walk(n) { if (n > 0) { return walk(n - 1); } return 0; }",
			want: true,
		},
		{
			name: "this-qualified self call is recursion",
			src:  "int walk(n) { if (n > 0) { return this.walk(n - 1); } return 0; }",
			want: true,
		},
		{
			name: "delegating to another object's same-named method is not",
			src:  "void dispose() { controller.dispose(); }",
			want: false,
		},
		{
			name: "super is the ancestor's implementation, not self",
			src:  "void dispose() { super.dispose(); }",
			want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := funcProps(t, tc.src)["recursive_self"].(bool)
			if got != tc.want {
				t.Errorf("recursive_self = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestDependencyFactsCarryImporter pins the shared `<importer> -> <imported>` naming.
//
// It is a contract, not a style: the package-metrics explainer recovers the
// importing side by splitting on " -> ". Naming a dependency by its target alone made
// every Dart edge unrecoverable there, so Ce came out 0 for every package and average
// instability 0.00 — the metrics were computed over an empty edge set and nothing
// failed.
func TestDependencyFactsCarryImporter(t *testing.T) {
	all := walkSource(t, "lib/data/api.dart", "import '../models/user.dart';\nclass A {}\n")
	deps := factsOfKind(all, facts.KindDependency)
	if len(deps) == 0 {
		t.Fatal("no dependency facts")
	}
	for _, d := range deps {
		importer, imported, ok := strings.Cut(d.Name, " -> ")
		if !ok {
			t.Errorf("dependency %q is not named \"<importer> -> <imported>\"", d.Name)
			continue
		}
		if importer != "lib/data" {
			t.Errorf("importer = %q, want lib/data", importer)
		}
		if imported != "lib/models" {
			t.Errorf("imported = %q, want lib/models", imported)
		}
	}
}

// TestSymbolsDeclareTheirModule pins the attribution package-metrics reads.
//
// The explainer takes a symbol's FIRST `declares` target as its package. A Dart class
// also declares its own members, so without a leading module edge the explainer read a
// MEMBER name as a package and minted one phantom package per class — 1,746 of them on
// drift against 199 real modules.
func TestSymbolsDeclareTheirModule(t *testing.T) {
	all := attributeSymbolsToModules(walkSource(t, "lib/data/api.dart",
		"class Repo {\n  void save() {}\n}\n"))
	repo := findFact(all, facts.KindSymbol, "lib/data.Repo")
	if repo == nil {
		t.Fatal("missing Repo")
	}
	if len(repo.Relations) == 0 || repo.Relations[0].Kind != facts.RelDeclares ||
		repo.Relations[0].Target != "lib/data" {
		t.Errorf("first relation should be declares->lib/data, got %+v", repo.Relations)
	}
	// The member edge survives; it is what makes the type's surface walkable.
	if !hasRelation(repo, facts.RelDeclares, "lib/data.Repo.save") {
		t.Error("the class should still declare its member")
	}
}
