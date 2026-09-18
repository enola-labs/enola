package swiftextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func swIntProp(t *testing.T, f facts.Fact, key string) int {
	t.Helper()
	v, ok := f.Props[key]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	}
	t.Fatalf("prop %q is not numeric: %T", key, v)
	return 0
}

func swStrSlice(f facts.Fact, key string) []string {
	v, ok := f.Props[key]
	if !ok {
		return nil
	}
	switch s := v.(type) {
	case []string:
		return s
	case []any:
		out := make([]string, 0, len(s))
		for _, a := range s {
			if str, ok := a.(string); ok {
				out = append(out, str)
			}
		}
		return out
	}
	return nil
}

func swContains(haystack []string, want string) bool {
	for _, s := range haystack {
		if s == want {
			return true
		}
	}
	return false
}

func TestSwComplexity_ForLoop(t *testing.T) {
	ff := extractAST(t, "func run() {\n  for x in items { use(x) }\n}", false)
	f, ok := findFact(ff, "pkg.run")
	if !ok {
		t.Fatalf("missing pkg.run; got %v", ff)
	}
	if got := swIntProp(t, f, "loop_depth"); got != 1 {
		t.Errorf("loop_depth = %d, want 1", got)
	}
	if got := swIntProp(t, f, "loop_count"); got != 1 {
		t.Errorf("loop_count = %d, want 1", got)
	}
	if cil := swStrSlice(f, "calls_in_loop"); !swContains(cil, "pkg.use") {
		t.Errorf("calls_in_loop = %v, want to contain pkg.use", cil)
	}
}

func TestSwComplexity_ForEachClosureIsLoop(t *testing.T) {
	ff := extractAST(t, "func run() {\n  items.forEach { g($0) }\n}", false)
	f, _ := findFact(ff, "pkg.run")
	if got := swIntProp(t, f, "loop_depth"); got != 1 {
		t.Errorf("loop_depth = %d, want 1 (forEach closure is a loop)", got)
	}
	if cil := swStrSlice(f, "calls_in_loop"); !swContains(cil, "pkg.g") {
		t.Errorf("calls_in_loop = %v, want to contain pkg.g", cil)
	}
}

func TestSwComplexity_NestedClosureIterators(t *testing.T) {
	ff := extractAST(t, "func run() {\n  outer.forEach { o in\n    o.items.map { inner($0) }\n  }\n}", false)
	f, _ := findFact(ff, "pkg.run")
	if got := swIntProp(t, f, "loop_depth"); got != 2 {
		t.Errorf("loop_depth = %d, want 2 (nested forEach/map)", got)
	}
	if cil := swStrSlice(f, "calls_in_loop"); !swContains(cil, "pkg.inner") {
		t.Errorf("calls_in_loop = %v, want to contain pkg.inner", cil)
	}
}

func TestSwComplexity_IteratorReceiverEvaluatedOnce(t *testing.T) {
	// service.load() is the iterator receiver — evaluated once, not per element.
	ff := extractAST(t, "func run() {\n  service.load().forEach { use($0) }\n}", false)
	f, _ := findFact(ff, "pkg.run")
	cil := swStrSlice(f, "calls_in_loop")
	if !swContains(cil, "pkg.use") {
		t.Errorf("calls_in_loop = %v, want to contain pkg.use", cil)
	}
	if swContains(cil, "service.load") {
		t.Errorf("calls_in_loop = %v, must NOT contain service.load (receiver runs once)", cil)
	}
}

func TestSwComplexity_NonIteratorClosureNotLoop(t *testing.T) {
	ff := extractAST(t, "func run() {\n  Task { persist() }\n}", false)
	f, _ := findFact(ff, "pkg.run")
	if _, present := f.Props["loop_depth"]; present {
		t.Errorf("Task { } closure must not be a loop; loop_depth=%v", f.Props["loop_depth"])
	}
	if cil := swStrSlice(f, "calls_in_loop"); swContains(cil, "pkg.persist") {
		t.Errorf("calls_in_loop = %v, must NOT contain pkg.persist (closure runs once)", cil)
	}
}

func TestSwComplexity_InLoopMethodCallCaptured(t *testing.T) {
	// A method call on a lowercase receiver inside a loop is captured (metrics-only)
	// so the analyzer's keyword heuristic can flag per-iteration I/O.
	ff := extractAST(t, "func run() {\n  items.forEach { item in\n    context.fetch(item)\n  }\n}", false)
	f, _ := findFact(ff, "pkg.run")
	if cil := swStrSlice(f, "calls_in_loop"); !swContains(cil, "context.fetch") {
		t.Errorf("calls_in_loop = %v, want to contain context.fetch", cil)
	}
}

func TestSwComplexity_NestedDeferredClosureNotInLoop(t *testing.T) {
	// A closure defined inside an iterator closure is deferred (runs when invoked,
	// not per element). `handle` must NOT be in calls_in_loop, while `use` (called
	// directly per element) is.
	ff := extractAST(t, "func run() {\n  items.forEach { x in\n    use(x)\n    onTap { handle(x) }\n  }\n}", false)
	f, _ := findFact(ff, "pkg.run")
	cil := swStrSlice(f, "calls_in_loop")
	if !swContains(cil, "pkg.use") {
		t.Errorf("calls_in_loop = %v, want to contain pkg.use (per element)", cil)
	}
	if swContains(cil, "pkg.handle") {
		t.Errorf("calls_in_loop = %v, must NOT contain pkg.handle (deferred closure)", cil)
	}
}

func TestSwComplexity_RecursiveSelf(t *testing.T) {
	ff := extractAST(t, "func fib(_ n: Int) -> Int {\n  if n < 2 { return n }\n  return fib(n - 1) + fib(n - 2)\n}", false)
	f, _ := findFact(ff, "pkg.fib")
	v, ok := f.Props["recursive_self"].(bool)
	if !ok || !v {
		t.Errorf("recursive_self = %v (ok=%v), want true", f.Props["recursive_self"], ok)
	}
	if _, present := f.Props["loop_depth"]; present {
		t.Errorf("loop_depth should be omitted for a loop-free function, got %v", f.Props["loop_depth"])
	}
}

func TestSwComplexity_BoundedForRange(t *testing.T) {
	for _, src := range []string{
		"func run() {\n  for i in 0..<10 { use(i) }\n}",
		"func run() {\n  for i in 1...5 { use(i) }\n}",
	} {
		ff := extractAST(t, src, false)
		f, _ := findFact(ff, "pkg.run")
		if _, present := f.Props["loop_depth"]; present {
			t.Errorf("bounded range %q: loop_depth should be omitted, got %v", src, f.Props["loop_depth"])
		}
		if got := swIntProp(t, f, "loop_count"); got != 1 {
			t.Errorf("bounded range %q: loop_count = %d, want 1 (still a loop for cyclomatic)", src, got)
		}
	}
}

func TestSwComplexity_UnboundedRangeEndpointStillScales(t *testing.T) {
	// A variable endpoint (0..<items.count) scales with n — must keep loop_depth.
	ff := extractAST(t, "func run() {\n  for i in 0..<items.count { use(i) }\n}", false)
	f, _ := findFact(ff, "pkg.run")
	if got := swIntProp(t, f, "loop_depth"); got != 1 {
		t.Errorf("loop_depth = %d, want 1 (variable endpoint scales)", got)
	}
}

func TestSwComplexity_StrideBounded(t *testing.T) {
	ff := extractAST(t, "func run() {\n  for i in stride(from: 0, to: 10, by: 2) { use(i) }\n}", false)
	f, _ := findFact(ff, "pkg.run")
	if _, present := f.Props["loop_depth"]; present {
		t.Errorf("literal-bound stride: loop_depth should be omitted, got %v", f.Props["loop_depth"])
	}
}

func TestSwComplexity_StrideVariableBoundScales(t *testing.T) {
	ff := extractAST(t, "func run() {\n  for i in stride(from: 0, to: n, by: 2) { use(i) }\n}", false)
	f, _ := findFact(ff, "pkg.run")
	if got := swIntProp(t, f, "loop_depth"); got != 1 {
		t.Errorf("loop_depth = %d, want 1 (variable stride bound scales)", got)
	}
}

func TestSwComplexity_LiteralArrayForEachBounded(t *testing.T) {
	ff := extractAST(t, "func run() {\n  [a, b, c].forEach { use($0) }\n}", false)
	f, _ := findFact(ff, "pkg.run")
	if _, present := f.Props["loop_depth"]; present {
		t.Errorf("literal-array forEach: loop_depth should be omitted, got %v", f.Props["loop_depth"])
	}
}

func TestSwComplexity_ScreamingSnakeConstForEachBounded(t *testing.T) {
	ff := extractAST(t, "func run() {\n  STOP_CHARS.forEach { use($0) }\n}", false)
	f, _ := findFact(ff, "pkg.run")
	if _, present := f.Props["loop_depth"]; present {
		t.Errorf("ALL-CAPS constant forEach: loop_depth should be omitted, got %v", f.Props["loop_depth"])
	}
}

func TestSwComplexity_BoundedOuterScalingInnerIsLinear(t *testing.T) {
	// A bounded outer loop over a scaling inner loop is O(n), not O(n²): the inner
	// loop must be measured against the real input, not multiplied by a constant.
	ff := extractAST(t, "func run() {\n  for i in 0..<10 {\n    for x in items { use(x) }\n  }\n}", false)
	f, _ := findFact(ff, "pkg.run")
	if got := swIntProp(t, f, "loop_depth"); got != 1 {
		t.Errorf("loop_depth = %d, want 1 (bounded outer × scaling inner is O(n))", got)
	}
}

func TestSwComplexity_ComputedPropertyGetter(t *testing.T) {
	ff := extractAST(t, "final class C {\n  var totals: Int {\n    for x in items { context.fetch(x) }\n    return 0\n  }\n}", false)
	f, ok := findFact(ff, "pkg.C.totals")
	if !ok {
		t.Fatalf("missing pkg.C.totals; got %v", ff)
	}
	if got := swIntProp(t, f, "loop_depth"); got != 1 {
		t.Errorf("computed getter loop_depth = %d, want 1", got)
	}
	if cil := swStrSlice(f, "calls_in_loop"); !swContains(cil, "context.fetch") {
		t.Errorf("computed getter calls_in_loop = %v, want to contain context.fetch", cil)
	}
}

func TestSwComplexity_DidSetObserver(t *testing.T) {
	ff := extractAST(t, "final class C {\n  var n = 0 {\n    didSet { for x in items { context.fetch(x) } }\n  }\n}", false)
	f, ok := findFact(ff, "pkg.C.n")
	if !ok {
		t.Fatalf("missing pkg.C.n; got %v", ff)
	}
	if cil := swStrSlice(f, "calls_in_loop"); !swContains(cil, "context.fetch") {
		t.Errorf("didSet calls_in_loop = %v, want to contain context.fetch", cil)
	}
}

func TestSwComplexity_StoredPropertyNoMetrics(t *testing.T) {
	// A plain stored property with an initializer must NOT get a cyclomatic prop
	// (avoids noise on every field).
	ff := extractAST(t, "final class C {\n  var count = compute()\n}", false)
	f, _ := findFact(ff, "pkg.C.count")
	if _, present := f.Props["cyclomatic"]; present {
		t.Errorf("stored property should have no cyclomatic prop, got %v", f.Props["cyclomatic"])
	}
}

func TestSwComplexity_SubscriptOnSameNameLocalNotRecursion(t *testing.T) {
	// `parameters["x"] = 1` inside `func parameters()` is a subscript on a local,
	// not a self-call — must NOT be flagged recursive_self (nor emit a self call).
	src := "func parameters() -> [String: Any] {\n" +
		"  var parameters: [String: Any] = [:]\n" +
		"  parameters[\"x\"] = 1\n" +
		"  return parameters\n" +
		"}"
	ff := extractAST(t, src, false)
	f, ok := findFact(ff, "pkg.parameters")
	if !ok {
		t.Fatalf("missing pkg.parameters; got %v", ff)
	}
	if v, _ := f.Props["recursive_self"].(bool); v {
		t.Errorf("subscript on same-name local must not set recursive_self")
	}
	if hasRelation(f, facts.RelCalls, "pkg.parameters") {
		t.Errorf("subscript on same-name local must not emit a self call edge")
	}
}

func TestSwComplexity_SubscriptKeyCallStillCaptured(t *testing.T) {
	// A call inside a subscript key (`cache[keyFor(id)]`) is still captured.
	ff := extractAST(t, "func run() {\n  _ = cache[keyFor(id)]\n}", false)
	f, _ := findFact(ff, "pkg.run")
	if !hasRelation(f, facts.RelCalls, "pkg.keyFor") {
		t.Errorf("call in subscript key should still emit a RelCalls edge; got %+v", f.Relations)
	}
}

func TestSwComplexity_OverloadDelegationNotRecursion(t *testing.T) {
	// `decode(key:)` calling stdlib `decode(_:forKey:)` shares the bare name but has
	// different argument labels — it is NOT self-recursion.
	src := "struct C {\n" +
		"  func decode(key: String) -> Int { return decode(key, forKey: key) }\n" +
		"}"
	ff := extractAST(t, src, false)
	f, _ := findFact(ff, "pkg.C.decode")
	if v, _ := f.Props["recursive_self"].(bool); v {
		t.Errorf("overload delegation (different labels) must not set recursive_self")
	}
}

func TestSwComplexity_SuperCallNotRecursion(t *testing.T) {
	// An override calling super.method() must not be flagged recursive.
	src := "class C {\n" +
		"  override func setSelected(_ selected: Bool, animated: Bool) {\n" +
		"    super.setSelected(selected, animated: animated)\n" +
		"  }\n" +
		"}"
	ff := extractAST(t, src, false)
	f, _ := findFact(ff, "pkg.C.setSelected")
	if v, _ := f.Props["recursive_self"].(bool); v {
		t.Errorf("super.method() override must not set recursive_self")
	}
}

func TestSwComplexity_GenuineRecursionMatchingLabels(t *testing.T) {
	// A self-call whose labels match the parameters is genuine recursion.
	src := "struct C {\n" +
		"  func walk(from node: Node) -> Node {\n" +
		"    if let p = node.parent { return walk(from: p) }\n" +
		"    return node\n" +
		"  }\n" +
		"}"
	ff := extractAST(t, src, false)
	f, _ := findFact(ff, "pkg.C.walk")
	if v, _ := f.Props["recursive_self"].(bool); !v {
		t.Errorf("matching-label self-call should set recursive_self")
	}
}

func TestSwComplexity_WhileLoop(t *testing.T) {
	ff := extractAST(t, "func run() {\n  while ready() { step() }\n}", false)
	f, _ := findFact(ff, "pkg.run")
	if got := swIntProp(t, f, "loop_depth"); got != 1 {
		t.Errorf("loop_depth = %d, want 1", got)
	}
	if cil := swStrSlice(f, "calls_in_loop"); !swContains(cil, "pkg.step") {
		t.Errorf("calls_in_loop = %v, want to contain pkg.step", cil)
	}
}
