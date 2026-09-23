package goextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// --- helpers ---

func intProp(t *testing.T, f facts.Fact, key string) int {
	t.Helper()
	v, ok := f.Props[key]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case int:
		return n
	case float64: // after a JSONL round-trip ints decode as float64
		return int(n)
	}
	t.Fatalf("prop %q is not numeric: %T", key, v)
	return 0
}

func strSliceProp(f facts.Fact, key string) []string {
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

func containsStr(haystack []string, want string) bool {
	for _, s := range haystack {
		if s == want {
			return true
		}
	}
	return false
}

// --- tests ---

func TestExtract_LoopMetrics_NestedLoops(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/proc.go": `package pkg

func Process(items []int) {
	for _, x := range items {
		for y := 0; y < x; y++ {
			helper()
		}
	}
}

func helper() {}
`,
	})

	f, ok := findFact(ff, "pkg.Process")
	if !ok {
		t.Fatalf("missing pkg.Process; got %v", ff)
	}
	if got := intProp(t, f, "loop_depth"); got != 2 {
		t.Errorf("loop_depth = %d, want 2", got)
	}
	if got := intProp(t, f, "loop_count"); got != 2 {
		t.Errorf("loop_count = %d, want 2", got)
	}
	// 1 (base) + range (1) + for (1) = 3
	if got := intProp(t, f, "cyclomatic"); got != 3 {
		t.Errorf("cyclomatic = %d, want 3", got)
	}
	if cil := strSliceProp(f, "calls_in_loop"); !containsStr(cil, "pkg.helper") {
		t.Errorf("calls_in_loop = %v, want to contain pkg.helper", cil)
	}
}

func TestExtract_CallsInLoop_InVsOutside(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/mixed.go": `package pkg

func Mixed(items []int) {
	setup()
	for _, x := range items {
		inLoop(x)
	}
}

func setup()       {}
func inLoop(x int) {}
`,
	})

	f, ok := findFact(ff, "pkg.Mixed")
	if !ok {
		t.Fatalf("missing pkg.Mixed; got %v", ff)
	}
	// Both calls remain as call edges.
	if !hasRelation(f, facts.RelCalls, "pkg.setup") || !hasRelation(f, facts.RelCalls, "pkg.inLoop") {
		t.Errorf("expected call edges to both pkg.setup and pkg.inLoop; relations=%v", f.Relations)
	}
	cil := strSliceProp(f, "calls_in_loop")
	if !containsStr(cil, "pkg.inLoop") {
		t.Errorf("calls_in_loop = %v, want to contain pkg.inLoop", cil)
	}
	if containsStr(cil, "pkg.setup") {
		t.Errorf("calls_in_loop = %v, must NOT contain pkg.setup (called outside loop)", cil)
	}
}

func TestExtract_RecursiveSelf(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/fib.go": `package pkg

func Fib(n int) int {
	if n < 2 {
		return n
	}
	return Fib(n-1) + Fib(n-2)
}
`,
	})

	f, ok := findFact(ff, "pkg.Fib")
	if !ok {
		t.Fatalf("missing pkg.Fib; got %v", ff)
	}
	v, ok := f.Props["recursive_self"].(bool)
	if !ok || !v {
		t.Errorf("recursive_self = %v (ok=%v), want true", f.Props["recursive_self"], ok)
	}
	// if (1) contributes to cyclomatic; no loops.
	if got := intProp(t, f, "cyclomatic"); got != 2 {
		t.Errorf("cyclomatic = %d, want 2", got)
	}
	if _, present := f.Props["loop_depth"]; present {
		t.Errorf("loop_depth should be omitted for a loop-free function, got %v", f.Props["loop_depth"])
	}
}

func TestExtract_NoLoops_BaselineCyclomatic(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/simple.go": `package pkg

func Simple(a, b bool) bool {
	if a && b {
		return true
	}
	return false
}
`,
	})

	f, ok := findFact(ff, "pkg.Simple")
	if !ok {
		t.Fatalf("missing pkg.Simple; got %v", ff)
	}
	// 1 (base) + if (1) + && (1) = 3
	if got := intProp(t, f, "cyclomatic"); got != 3 {
		t.Errorf("cyclomatic = %d, want 3", got)
	}
	if _, present := f.Props["calls_in_loop"]; present {
		t.Errorf("calls_in_loop should be omitted, got %v", f.Props["calls_in_loop"])
	}
}

func TestExtract_ScalingLoopDepth_BoundedDiscounted(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/proc.go": `package pkg

func Process(items []int) {
	for _, x := range items {
		for _, m := range []string{"a", "b"} {
			use(x, m)
		}
	}
}

func Poll() {
	for {
		tick()
	}
}

func use(x int, m string) {}
func tick()               {}
`,
	})

	f, _ := findFact(ff, "pkg.Process")
	if got := intProp(t, f, "loop_depth"); got != 2 {
		t.Errorf("loop_depth = %d, want 2", got)
	}
	// Inner range is over a composite literal → bounded → only the outer loop scales.
	if got := intProp(t, f, "scaling_loop_depth"); got != 1 {
		t.Errorf("scaling_loop_depth = %d, want 1", got)
	}

	p, _ := findFact(ff, "pkg.Poll")
	if got := intProp(t, p, "scaling_loop_depth"); got != 0 {
		t.Errorf("infinite for{} scaling_loop_depth = %d, want 0", got)
	}
}

func TestExtract_CallsInScalingLoop_BoundedExcluded(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/proc.go": `package pkg

func Process(items []int) {
	for _, c := range []string{"a", "b"} {
		setup(c)
	}
	for _, x := range items {
		consume(x)
	}
}

func setup(c string) {}
func consume(x int)   {}
`,
	})
	f, _ := findFact(ff, "pkg.Process")
	scaling := strSliceProp(f, "calls_in_scaling_loop")
	if !containsStr(scaling, "pkg.consume") {
		t.Errorf("calls_in_scaling_loop = %v, want consume (range over slice arg)", scaling)
	}
	if containsStr(scaling, "pkg.setup") {
		t.Errorf("calls_in_scaling_loop = %v, must NOT contain setup (range over composite literal)", scaling)
	}
}

// --- v99: calls_in_scaling_loop counts REPEATED loops, not just scaling ones --------
//
// A bare `for {}` adds no factor of n (it is exited by break/return), but its body still
// runs many times — a parent-chain walk doing one query per level. Its calls must stay
// N+1 candidates even though its depth is discounted. Reproduced on fairwayhub/golf:
// OrganizationRepository.GetOrganizationPath (`for { org = GetByID(parentID) }`) and
// GroupRepository.makeUniqueSlug (`for { QueryRowContext(...) }`) are both real
// high/medium-severity N+1 findings that ride on this.
func TestExtract_CallsInScalingLoop_InfiniteLoopCallsRetained(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/proc.go": `package pkg

func GetPath(id int) {
	for {
		getByID(id)
	}
}

func getByID(id int) {}
`,
	})
	f, _ := findFact(ff, "pkg.GetPath")
	if got := intProp(t, f, "scaling_loop_depth"); got != 0 {
		t.Errorf("scaling_loop_depth = %d, want 0 (an infinite loop adds no factor of n)", got)
	}
	scaling := strSliceProp(f, "calls_in_scaling_loop")
	if !containsStr(scaling, "pkg.getByID") {
		t.Errorf("calls_in_scaling_loop = %v, want getByID retained: an infinite loop "+
			"repeats, so a per-iteration query inside it is still an N+1 candidate", scaling)
	}
}

// calls_in_scaling_loop must be PRESENT (and empty) whenever calls_in_loop is, or
// perf.scalingLoopCalls() reads its absence as "extractor never computed the subset"
// and falls back to the unfiltered calls_in_loop — defeating the discount in exactly
// the case it exists for.
func TestExtract_CallsInScalingLoop_PresentButEmptyWhenAllBounded(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/proc.go": `package pkg

func Seed() {
	for _, c := range []string{"a", "b"} {
		setup(c)
	}
}

func setup(c string) {}
`,
	})
	f, _ := findFact(ff, "pkg.Seed")
	if !containsStr(strSliceProp(f, "calls_in_loop"), "pkg.setup") {
		t.Fatalf("calls_in_loop = %v, want setup", f.Props["calls_in_loop"])
	}
	v, present := f.Props["calls_in_scaling_loop"]
	if !present {
		t.Fatalf("calls_in_scaling_loop must be present even when empty")
	}
	if got := strSliceProp(f, "calls_in_scaling_loop"); len(got) != 0 {
		t.Fatalf("calls_in_scaling_loop = %v (%T), want empty", got, v)
	}
}

// ...and absent when there are no in-loop calls at all.
func TestExtract_CallsInScalingLoop_AbsentWithoutLoopCalls(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/proc.go": `package pkg

func Count(items []int) int {
	n := 0
	for range items {
		n++
	}
	return n
}
`,
	})
	f, _ := findFact(ff, "pkg.Count")
	if _, present := f.Props["calls_in_scaling_loop"]; present {
		t.Fatalf("calls_in_scaling_loop must be absent when calls_in_loop is")
	}
}

// --- bounded-loop rules -----------------------------------------------------

// A loop whose trip count is written into it adds no factor of n, however large the
// constant: `for d := 1; d <= 10; d++` runs ten times on an empty graph and ten times
// on the Linux kernel.
func TestExtract_ScalingDepth_ConstBoundedLoop(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/x.go": `package pkg

func ConstOuter(rows [][]int) int {
	n := 0
	for d := 0; d < 10; d++ {
		for _, r := range rows {
			n += len(r)
		}
	}
	return n
}

func VariableOuter(rows [][]int, limit int) int {
	n := 0
	for d := 0; d < limit; d++ {
		for _, r := range rows {
			n += len(r)
		}
	}
	return n
}
`,
	})
	if got := scalingLoopDepthOf(ff, "pkg.ConstOuter"); got != 1 {
		t.Errorf("const-bounded outer: scaling_loop_depth = %d, want 1", got)
	}
	// The control: a bound that is a variable is not a bound at all.
	if got := scalingLoopDepthOf(ff, "pkg.VariableOuter"); got != 2 {
		t.Errorf("variable outer: scaling_loop_depth = %d, want 2", got)
	}
}

// A loop that advances an index its enclosing loop is already advancing is part of
// one scan, not a scan per element.
func TestExtract_ScalingDepth_SharedCursorLoop(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/x.go": `package pkg

func Scan(b []byte) int {
	n := 0
	for i := 0; i < len(b); i++ {
		if b[i] != '"' {
			continue
		}
		i++
		for i < len(b) && b[i] != '"' {
			n++
			i++
		}
	}
	return n
}

func PerElement(b []byte, other []byte) int {
	n := 0
	for i := 0; i < len(b); i++ {
		for j := 0; j < len(other); j++ {
			n++
		}
	}
	return n
}
`,
	})
	if got := scalingLoopDepthOf(ff, "pkg.Scan"); got != 1 {
		t.Errorf("shared cursor: scaling_loop_depth = %d, want 1", got)
	}
	// The control: an inner loop with its own index over its own collection is the
	// quadratic shape the rule must leave alone.
	if got := scalingLoopDepthOf(ff, "pkg.PerElement"); got != 2 {
		t.Errorf("independent inner index: scaling_loop_depth = %d, want 2", got)
	}
}

// A collection rebuilt from a literal on every iteration holds a fixed number of
// entries; one initialised outside the loop that fills it is an accumulator and grows
// with whatever the loop walks. The two look alike and must not be treated alike.
func TestExtract_ScalingDepth_LocalLiteralCollection(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/x.go": `package pkg

import "strings"

func Forms(files []string, repo string) int {
	n := 0
	for _, f := range files {
		forms := []string{f}
		if t := strings.TrimPrefix(f, repo); t != f {
			forms = append(forms, t)
		}
		for _, form := range forms {
			n += len(form)
		}
	}
	return n
}

func Accumulate(files []string) int {
	seen := []string{}
	for _, f := range files {
		seen = append(seen, f)
	}
	n := 0
	for _, f := range files {
		for _, s := range seen {
			n += len(s)
		}
	}
	return n
}

func Spread(files []string, more []string) int {
	batch := []string{"first"}
	batch = append(batch, more...)
	n := 0
	for _, f := range files {
		for _, b := range batch {
			n += len(b) + len(f)
		}
	}
	return n
}
`,
	})
	if got := scalingLoopDepthOf(ff, "pkg.Forms"); got != 1 {
		t.Errorf("literal rebuilt per iteration: scaling_loop_depth = %d, want 1", got)
	}
	// Controls. An accumulator grows with the input even though it starts as a
	// literal, and a spread append adds however many the spread collection holds.
	if got := scalingLoopDepthOf(ff, "pkg.Accumulate"); got != 2 {
		t.Errorf("accumulator: scaling_loop_depth = %d, want 2", got)
	}
	if got := scalingLoopDepthOf(ff, "pkg.Spread"); got != 2 {
		t.Errorf("spread append: scaling_loop_depth = %d, want 2", got)
	}
}

// A for statement may carry an init and a post and no condition at all. The loop
// analysis inspects the condition, and ast.Inspect panics on a nil node rather than
// ignoring it, so this shape crashed extraction of any file containing one.
func TestExtract_ScalingDepth_ForWithoutCondition(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/x.go": `package pkg

func Walk(items []int) int {
	n := 0
	for i := 0; ; i++ {
		if i >= len(items) {
			break
		}
		n += items[i]
	}
	return n
}
`,
	})
	if got := scalingLoopDepthOf(ff, "pkg.Walk"); got < 0 {
		t.Fatalf("pkg.Walk was not extracted")
	}
}

// --- cross-call hierarchy signals -------------------------------------------

func strSliceOf(t *testing.T, ff []facts.Fact, name, key string) []string {
	t.Helper()
	for _, f := range ff {
		if f.Kind == facts.KindSymbol && f.Name == name {
			return strSliceProp(f, key)
		}
	}
	t.Fatalf("no symbol %q", name)
	return nil
}

// A call is only a continuation of the caller's walk when it is HANDED what the
// caller is walking. Recording that is half the cross-call hierarchical rule; the
// callee looping over its parameter is the other half, and neither means anything
// alone.
func TestExtract_CallsOnLoopElement(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/x.go": `package pkg

func Direct(items []string, other string) {
	for _, it := range items {
		takes(it)
		takesOther(other)
	}
}

func Derived(items []string) {
	for _, it := range items {
		norm := clean(it)
		takes(norm)
	}
}

func Receiver(items []*Box) {
	for _, b := range items {
		b.Open()
	}
}

type Box struct{}

func (b *Box) Open() {}

func takes(s string)      {}
func takesOther(s string) {}
func clean(s string) string { return s }
`,
	})

	got := strSliceOf(t, ff, "pkg.Direct", "calls_on_loop_element")
	if !containsStr(got, "pkg.takes") {
		t.Errorf("Direct: calls_on_loop_element = %v, want to contain pkg.takes", got)
	}
	// The control: a call inside the same loop that receives something else is not
	// continuing the walk, and charging it as one would discount a real factor.
	if containsStr(got, "pkg.takesOther") {
		t.Errorf("Direct: a call handed an unrelated value must not count: %v", got)
	}

	// One indirection later is still the element: `norm` is what the loop is walking.
	if got := strSliceOf(t, ff, "pkg.Derived", "calls_on_loop_element"); !containsStr(got, "pkg.takes") {
		t.Errorf("Derived: calls_on_loop_element = %v, want to contain pkg.takes", got)
	}
	// A method called ON the element counts the same as one passed it. The target is
	// recorded as this extractor resolves it — unqualified, because the receiver's
	// type is not known here — so the analyzer only acts on it when the same
	// unqualified name reaches it as a function. That is a limit of call resolution,
	// not of this signal.
	if got := strSliceOf(t, ff, "pkg.Receiver", "calls_on_loop_element"); !containsStr(got, "b.Open") {
		t.Errorf("Receiver: calls_on_loop_element = %v, want to contain b.Open", got)
	}
}

// loops_over_param is what makes a callee's loop a continuation rather than a
// traversal of its own, whether it is written as a range or as an indexed for.
func TestExtract_LoopsOverParam(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/x.go": `package pkg

var registry = []string{"a", "b"}

func Ranged(items []string) int {
	n := 0
	for _, it := range items {
		n += len(it)
	}
	return n
}

func Indexed(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		n += int(s[i])
	}
	return n
}

func OverGlobal(count int) int {
	n := 0
	for _, r := range registry {
		n += len(r) + count
	}
	return n
}

type Holder struct{ items []string }

func (h *Holder) Walk() int {
	n := 0
	for _, it := range h.items {
		n += len(it)
	}
	return n
}
`,
	})
	loopsOverParam := func(name string) bool {
		for _, f := range ff {
			if f.Kind == facts.KindSymbol && f.Name == name {
				v, _ := f.Props["loops_over_param"].(bool)
				return v
			}
		}
		t.Fatalf("no symbol %q", name)
		return false
	}
	for _, name := range []string{"pkg.Ranged", "pkg.Indexed", "pkg.Holder.Walk"} {
		if !loopsOverParam(name) {
			t.Errorf("%s: loops_over_param = false, want true", name)
		}
	}
	// The control: a loop over package state is not walking anything a caller
	// handed in, so calling it from a loop really does multiply.
	if loopsOverParam("pkg.OverGlobal") {
		t.Errorf("pkg.OverGlobal: loops_over_param = true, want false")
	}
}
