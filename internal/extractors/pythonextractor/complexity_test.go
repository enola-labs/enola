package pythonextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/extractors/ioclosure"
	"github.com/enola-labs/enola/internal/facts"
)

// --- helpers (local to this file; byName/astExtract live in sibling test files) ---

func cxIntProp(t *testing.T, f facts.Fact, key string) int {
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

func cxStrSlice(f facts.Fact, key string) []string {
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

func cxContains(haystack []string, want string) bool {
	for _, s := range haystack {
		if s == want {
			return true
		}
	}
	return false
}

// --- tests ---

func TestPyComplexity_NestedLoops(t *testing.T) {
	src := `
def process(items):
    for x in items:
        for y in helper(x):
            consume(y)

def helper(x):
    return []

def consume(y):
    pass
`
	idx := byName(astExtract(t, "svc.py", src, false))
	f, ok := idx["svc.process"]
	if !ok {
		t.Fatalf("missing svc.process; keys: %v", keys(idx))
	}
	if got := cxIntProp(t, f, "loop_depth"); got != 2 {
		t.Errorf("loop_depth = %d, want 2", got)
	}
	if got := cxIntProp(t, f, "loop_count"); got != 2 {
		t.Errorf("loop_count = %d, want 2", got)
	}
	// 1 (base) + two for loops = 3
	if got := cxIntProp(t, f, "cyclomatic"); got != 3 {
		t.Errorf("cyclomatic = %d, want 3", got)
	}
	if cil := cxStrSlice(f, "calls_in_loop"); !cxContains(cil, "svc.consume") {
		t.Errorf("calls_in_loop = %v, want to contain svc.consume", cil)
	}
}

func TestPyComplexity_CallsInLoopInVsOutside(t *testing.T) {
	src := `
def mixed(items):
    setup()
    for x in items:
        in_loop(x)

def setup():
    pass

def in_loop(x):
    pass
`
	idx := byName(astExtract(t, "svc.py", src, false))
	f := idx["svc.mixed"]
	if !hasRel(f, facts.RelCalls, "svc.setup") || !hasRel(f, facts.RelCalls, "svc.in_loop") {
		t.Errorf("expected call edges to svc.setup and svc.in_loop; relations=%v", f.Relations)
	}
	cil := cxStrSlice(f, "calls_in_loop")
	if !cxContains(cil, "svc.in_loop") {
		t.Errorf("calls_in_loop = %v, want to contain svc.in_loop", cil)
	}
	if cxContains(cil, "svc.setup") {
		t.Errorf("calls_in_loop = %v, must NOT contain svc.setup (called outside loop)", cil)
	}
}

func TestPyComplexity_RecursiveSelf(t *testing.T) {
	src := `
def fib(n):
    if n < 2:
        return n
    return fib(n - 1) + fib(n - 2)
`
	idx := byName(astExtract(t, "svc.py", src, false))
	f := idx["svc.fib"]
	v, ok := f.Props["recursive_self"].(bool)
	if !ok || !v {
		t.Errorf("recursive_self = %v (ok=%v), want true", f.Props["recursive_self"], ok)
	}
	// 1 (base) + if (1) = 2; no loops.
	if got := cxIntProp(t, f, "cyclomatic"); got != 2 {
		t.Errorf("cyclomatic = %d, want 2", got)
	}
	if _, present := f.Props["loop_depth"]; present {
		t.Errorf("loop_depth should be omitted for a loop-free function, got %v", f.Props["loop_depth"])
	}
}

func TestPyComplexity_ComprehensionIsLoop(t *testing.T) {
	src := `
def collect(items):
    return [transform(x) for x in items]

def transform(x):
    return x
`
	idx := byName(astExtract(t, "svc.py", src, false))
	f := idx["svc.collect"]
	if got := cxIntProp(t, f, "loop_depth"); got != 1 {
		t.Errorf("loop_depth = %d, want 1 (comprehension carries an implicit loop)", got)
	}
	if got := cxIntProp(t, f, "loop_count"); got != 1 {
		t.Errorf("loop_count = %d, want 1", got)
	}
	if cil := cxStrSlice(f, "calls_in_loop"); !cxContains(cil, "svc.transform") {
		t.Errorf("calls_in_loop = %v, want to contain svc.transform", cil)
	}
}

func TestPyComplexity_ComprehensionFirstIterableEvaluatedOnce(t *testing.T) {
	// In `[enrich(x) for x in fetch_all()]`, fetch_all() runs once (the first
	// for-clause iterable), while enrich(x) runs per item. Only enrich should be
	// counted as in-loop — otherwise a materialised query reads as a false N+1.
	src := `
def load():
    return [enrich(x) for x in fetch_all()]

def enrich(x):
    return x

def fetch_all():
    return []
`
	idx := byName(astExtract(t, "svc.py", src, false))
	f := idx["svc.load"]
	// Both are call edges.
	if !hasRel(f, facts.RelCalls, "svc.enrich") || !hasRel(f, facts.RelCalls, "svc.fetch_all") {
		t.Errorf("expected call edges to svc.enrich and svc.fetch_all; relations=%v", f.Relations)
	}
	cil := cxStrSlice(f, "calls_in_loop")
	if !cxContains(cil, "svc.enrich") {
		t.Errorf("calls_in_loop = %v, want to contain svc.enrich (per-iteration element)", cil)
	}
	if cxContains(cil, "svc.fetch_all") {
		t.Errorf("calls_in_loop = %v, must NOT contain svc.fetch_all (first iterable runs once)", cil)
	}
	if got := cxIntProp(t, f, "loop_depth"); got != 1 {
		t.Errorf("loop_depth = %d, want 1", got)
	}
}

func TestPyComplexity_BooleanOperatorCyclomatic(t *testing.T) {
	src := `
def check(a, b):
    if a and b:
        return True
    return False
`
	idx := byName(astExtract(t, "svc.py", src, false))
	f := idx["svc.check"]
	// 1 (base) + if (1) + and (1) = 3
	if got := cxIntProp(t, f, "cyclomatic"); got != 3 {
		t.Errorf("cyclomatic = %d, want 3", got)
	}
	if _, present := f.Props["calls_in_loop"]; present {
		t.Errorf("calls_in_loop should be omitted, got %v", f.Props["calls_in_loop"])
	}
}

func TestPyComplexity_ScalingLoopDepth_BoundedInnerDiscounted(t *testing.T) {
	src := `
def process(items):
    for x in items:
        for y in (1, 2, 3):
            consume(x, y)

def consume(x, y):
    pass
`
	idx := byName(astExtract(t, "svc.py", src, false))
	f := idx["svc.process"]
	if got := cxIntProp(t, f, "loop_depth"); got != 2 {
		t.Errorf("loop_depth = %d, want 2", got)
	}
	// Inner loop iterates a literal tuple → bounded → only the outer loop scales.
	if got := cxIntProp(t, f, "scaling_loop_depth"); got != 1 {
		t.Errorf("scaling_loop_depth = %d, want 1", got)
	}
}

func TestPyComplexity_ScalingLoopDepth_FullyBounded(t *testing.T) {
	src := `
def build():
    for x in [1, 2]:
        for y in range(3):
            emit(x, y)

def emit(x, y):
    pass
`
	f := byName(astExtract(t, "svc.py", src, false))["svc.build"]
	if got := cxIntProp(t, f, "loop_depth"); got != 2 {
		t.Errorf("loop_depth = %d, want 2", got)
	}
	if _, present := f.Props["scaling_loop_depth"]; !present {
		t.Fatalf("scaling_loop_depth must be emitted (as 0) when loops exist")
	}
	if got := cxIntProp(t, f, "scaling_loop_depth"); got != 0 {
		t.Errorf("scaling_loop_depth = %d, want 0 (all loops bounded)", got)
	}
}

func TestPyComplexity_ScalingLoopDepth_WhileTrueBounded(t *testing.T) {
	src := `
def poll():
    while True:
        step()

def step():
    pass
`
	f := byName(astExtract(t, "svc.py", src, false))["svc.poll"]
	if got := cxIntProp(t, f, "scaling_loop_depth"); got != 0 {
		t.Errorf("while True scaling_loop_depth = %d, want 0", got)
	}
}

func TestPyComplexity_IODirect(t *testing.T) {
	src := `
def load(session, q):
    session.execute(q)

def fetch(url):
    requests.get(url)

def pure(a, b):
    return a + b
`
	idx := byName(astExtract(t, "svc.py", src, false))
	if v, _ := idx["svc.load"].Props["io_direct"].(bool); !v {
		t.Errorf("session.execute must set io_direct")
	}
	if v, _ := idx["svc.fetch"].Props["io_direct"].(bool); !v {
		t.Errorf("requests.get must set io_direct")
	}
	if _, present := idx["svc.pure"].Props["io_direct"]; present {
		t.Errorf("pure function must not set io_direct")
	}
}

func TestPyComputePerformsIO_Transitive(t *testing.T) {
	// outer → wrapper → (requests.get). outer performs I/O transitively.
	fs := []facts.Fact{
		{Kind: facts.KindSymbol, Name: "m.outer", Relations: []facts.Relation{{Kind: facts.RelCalls, Target: "m.wrapper"}}},
		{Kind: facts.KindSymbol, Name: "m.wrapper", Props: map[string]any{"io_direct": true}},
		{Kind: facts.KindSymbol, Name: "m.pure"},
	}
	ioclosure.Propagate(fs)
	if v, _ := fs[0].Props["performs_io"].(bool); !v {
		t.Errorf("outer must be flagged performs_io transitively")
	}
	if v, _ := fs[1].Props["performs_io"].(bool); !v {
		t.Errorf("wrapper must be flagged performs_io")
	}
	if _, present := fs[2].Props["performs_io"]; present {
		t.Errorf("pure must not be flagged performs_io")
	}
}

func TestPyComplexity_CallsInScalingLoop_BoundedExcluded(t *testing.T) {
	src := `
def process(items):
    for c in (A, B):
        setup(c)
    for x in items:
        consume(x)

def setup(c):
    pass

def consume(x):
    pass
`
	f := byName(astExtract(t, "svc.py", src, false))["svc.process"]
	inLoop := cxStrSlice(f, "calls_in_loop")
	if !cxContains(inLoop, "svc.setup") || !cxContains(inLoop, "svc.consume") {
		t.Errorf("calls_in_loop = %v, want both setup and consume", inLoop)
	}
	scaling := cxStrSlice(f, "calls_in_scaling_loop")
	if !cxContains(scaling, "svc.consume") {
		t.Errorf("calls_in_scaling_loop = %v, want consume (unbounded loop)", scaling)
	}
	if cxContains(scaling, "svc.setup") {
		t.Errorf("calls_in_scaling_loop = %v, must NOT contain setup (bounded literal-tuple loop)", scaling)
	}
}

// --- v99: calls_in_scaling_loop counts REPEATED loops, not just scaling ones --------
//
// `while True:` adds no factor of n (it exits by break/return), but its body still runs
// many times — a retry loop or a parent-chain walk doing one query per level. Its calls
// must remain N+1 candidates even though its depth is discounted.
func TestPyComplexity_CallsInScalingLoop_InfiniteLoopCallsRetained(t *testing.T) {
	src := `
def get_path(node_id):
    while True:
        get_by_id(node_id)

def get_by_id(node_id):
    pass
`
	f := byName(astExtract(t, "svc.py", src, false))["svc.get_path"]
	if got := cxIntProp(t, f, "scaling_loop_depth"); got != 0 {
		t.Errorf("scaling_loop_depth = %d, want 0 (an infinite loop adds no factor of n)", got)
	}
	scaling := cxStrSlice(f, "calls_in_scaling_loop")
	if !cxContains(scaling, "svc.get_by_id") {
		t.Errorf("calls_in_scaling_loop = %v, want get_by_id retained: an infinite loop "+
			"repeats, so a per-iteration query inside it is still an N+1 candidate", scaling)
	}
}

// The key must be present (and empty) whenever calls_in_loop is, or perf's
// scalingLoopCalls() falls back to the unfiltered calls_in_loop.
func TestPyComplexity_CallsInScalingLoop_PresentButEmptyWhenAllBounded(t *testing.T) {
	src := `
def seed():
    for c in (A, B):
        setup(c)

def setup(c):
    pass
`
	f := byName(astExtract(t, "svc.py", src, false))["svc.seed"]
	if !cxContains(cxStrSlice(f, "calls_in_loop"), "svc.setup") {
		t.Fatalf("calls_in_loop = %v, want setup", f.Props["calls_in_loop"])
	}
	if _, present := f.Props["calls_in_scaling_loop"]; !present {
		t.Fatalf("calls_in_scaling_loop must be present even when empty")
	}
	if got := cxStrSlice(f, "calls_in_scaling_loop"); len(got) != 0 {
		t.Fatalf("calls_in_scaling_loop = %v, want empty", got)
	}
}

func TestPyComplexity_CallsInScalingLoop_AbsentWithoutLoopCalls(t *testing.T) {
	src := `
def count(items):
    n = 0
    for _ in items:
        n += 1
    return n
`
	f := byName(astExtract(t, "svc.py", src, false))["svc.count"]
	if _, present := f.Props["calls_in_scaling_loop"]; present {
		t.Fatalf("calls_in_scaling_loop must be absent when calls_in_loop is")
	}
}
