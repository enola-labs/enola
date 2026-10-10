package rustextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/extractors/ioclosure"
	"github.com/enola-labs/enola/internal/facts"
)

// propInt returns an int-valued prop, or -1 when absent (so an absent prop is
// distinguishable from a legitimate 0).
func propInt(f facts.Fact, key string) int {
	if v, ok := f.Props[key].(int); ok {
		return v
	}
	return -1
}

func propStrings(f facts.Fact, key string) ([]string, bool) {
	v, ok := f.Props[key].([]string)
	return v, ok
}

func sliceContains(s []string, want string) bool {
	for _, x := range s {
		if x == want {
			return true
		}
	}
	return false
}

func TestRustComplexity_ScalingLoopDepth_TripleNestedFor(t *testing.T) {
	ff := extractAST(t, `
fn hot(a: &[i32], b: &[i32], c: &[i32]) {
    for x in a {
        for y in b {
            for z in c {
                drop((x, y, z));
            }
        }
    }
}
`)
	f, _ := findFact(ff, "pkg.hot")
	if got := propInt(f, "loop_depth"); got != 3 {
		t.Errorf("loop_depth = %d, want 3", got)
	}
	if got := propInt(f, "scaling_loop_depth"); got != 3 {
		t.Errorf("scaling_loop_depth = %d, want 3 (all three iterate variables)", got)
	}
	if got := propInt(f, "loop_count"); got != 3 {
		t.Errorf("loop_count = %d, want 3", got)
	}
}

func TestRustComplexity_ScalingLoopDepth_ConstantRangeDiscounted(t *testing.T) {
	ff := extractAST(t, `
fn warm(items: &[i32]) {
    for i in 0..10 {
        for x in items {
            drop((i, x));
        }
    }
}
`)
	f, _ := findFact(ff, "pkg.warm")
	if got := propInt(f, "loop_depth"); got != 2 {
		t.Errorf("loop_depth = %d, want 2 (both loops counted)", got)
	}
	if got := propInt(f, "scaling_loop_depth"); got != 1 {
		t.Errorf("scaling_loop_depth = %d, want 1 (constant 0..10 discounted)", got)
	}
}

func TestRustComplexity_ScalingLoopDepth_InfiniteLoopDiscounted(t *testing.T) {
	ff := extractAST(t, `
fn daemon(items: &[i32]) {
    loop {
        for x in items {
            drop(x);
        }
    }
}
`)
	f, _ := findFact(ff, "pkg.daemon")
	if got := propInt(f, "loop_depth"); got != 2 {
		t.Errorf("loop_depth = %d, want 2", got)
	}
	if got := propInt(f, "scaling_loop_depth"); got != 1 {
		t.Errorf("scaling_loop_depth = %d, want 1 (infinite loop {} discounted)", got)
	}
}

func TestRustComplexity_CallsInScalingLoop_ScalingRetained(t *testing.T) {
	ff := extractAST(t, `
fn each(items: &[i32]) {
    for x in items {
        helper(x);
    }
}
fn helper(_x: &i32) {}
`)
	f, _ := findFact(ff, "pkg.each")
	scaling, ok := propStrings(f, "calls_in_scaling_loop")
	if !ok {
		t.Fatalf("calls_in_scaling_loop absent; props=%+v", f.Props)
	}
	if !sliceContains(scaling, "pkg.helper") {
		t.Errorf("calls_in_scaling_loop = %v, want to contain pkg.helper", scaling)
	}
}

func TestRustComplexity_CallsInScalingLoop_ConstantExcluded(t *testing.T) {
	ff := extractAST(t, `
fn each() {
    for i in 0..10 {
        helper(i);
    }
}
fn helper(_i: i32) {}
`)
	f, _ := findFact(ff, "pkg.each")
	// A loop exists, so the (three-valued) prop is present but empty: the call
	// sits in a constant-trip loop, so it is not a scaling N+1 candidate.
	scaling, ok := propStrings(f, "calls_in_scaling_loop")
	if !ok {
		t.Fatalf("calls_in_scaling_loop absent; want present-but-empty; props=%+v", f.Props)
	}
	if len(scaling) != 0 {
		t.Errorf("calls_in_scaling_loop = %v, want empty (constant 0..10 loop)", scaling)
	}
	loop, ok := propStrings(f, "calls_in_loop")
	if !ok || !sliceContains(loop, "pkg.helper") {
		t.Errorf("calls_in_loop = %v (ok=%v), want to contain pkg.helper", loop, ok)
	}
}

func TestRustComplexity_IODirect(t *testing.T) {
	ff := extractAST(t, `
fn query(conn: &Conn, sql: &str) {
    conn.execute(sql);
}
`)
	f, _ := findFact(ff, "pkg.query")
	if b, _ := f.Props["io_direct"].(bool); !b {
		t.Errorf("io_direct = %v, want true (.execute is a DB round-trip)", f.Props["io_direct"])
	}
}

func TestRustComplexity_IODirect_ScopedFilePrimitive(t *testing.T) {
	ff := extractAST(t, `
fn load(path: &str) {
    let _f = File::open(path);
}
`)
	f, _ := findFact(ff, "pkg.load")
	if b, _ := f.Props["io_direct"].(bool); !b {
		t.Errorf("io_direct = %v, want true (File::open)", f.Props["io_direct"])
	}
}

func TestRustComplexity_IODirect_AmbiguousVerbExcluded(t *testing.T) {
	ff := extractAST(t, `
fn compute(map: &Cache, k: &str) {
    let _v = map.get(k);
}
`)
	f, _ := findFact(ff, "pkg.compute")
	if b, _ := f.Props["io_direct"].(bool); b {
		t.Errorf("io_direct = true, want false (.get is an in-memory accessor, deliberately excluded)")
	}
}

func TestRustComplexity_RecursiveSelf(t *testing.T) {
	ff := extractAST(t, `
fn fact(n: u64) -> u64 {
    if n <= 1 {
        1
    } else {
        n * fact(n - 1)
    }
}
`)
	f, _ := findFact(ff, "pkg.fact")
	if b, _ := f.Props["recursive_self"].(bool); !b {
		t.Errorf("recursive_self = %v, want true", f.Props["recursive_self"])
	}
}

func TestRustComputePerformsIO_Transitive(t *testing.T) {
	ff := extractAST(t, `
fn handler() {
    load_row();
}
fn load_row() {
    let conn = get_conn();
    conn.execute("SELECT 1");
}
`)
	// io_direct is per-function (set by the walker); performs_io is the transitive
	// fixpoint computed at the package level, so run it here over the file's facts.
	ioclosure.Propagate(ff)

	lr, _ := findFact(ff, "pkg.load_row")
	if b, _ := lr.Props["io_direct"].(bool); !b {
		t.Fatalf("pkg.load_row io_direct = %v, want true (precondition)", lr.Props["io_direct"])
	}
	h, _ := findFact(ff, "pkg.handler")
	if b, _ := h.Props["performs_io"].(bool); !b {
		t.Errorf("pkg.handler performs_io = %v, want true (transitive through load_row)", h.Props["performs_io"])
	}
}
