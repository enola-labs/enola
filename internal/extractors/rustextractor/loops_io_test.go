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

// TestRustPerformsIO_CrossesAModulePathCall is the layered-app shape: a service
// calls the db layer through its module path, `db::load_users()`, which is how Rust
// calls a function in another module. The call used to be emitted as the bare
// "load_users", which names no fact, so the closure stopped there and the service
// layer never carried performs_io. TestRustComputePerformsIO_Transitive only
// covers a same-file bare call, which always resolved.
func TestRustPerformsIO_CrossesAModulePathCall(t *testing.T) {
	crates := []crateInfo{{name: "app", dir: "."}}
	dirs := map[string]bool{"src": true}
	db, _ := extractFileAST([]byte(`
pub async fn load_users() -> Vec<User> {
    let sql = "SELECT * FROM users";
    sqlx::query(sql).fetch_all(&pool()).await.unwrap()
}
`), "src/db.rs", crates, dirs)
	service, _ := extractFileAST([]byte(`
use crate::db;

pub async fn list_all() -> Vec<User> {
    db::load_users().await
}
`), "src/service.rs", crates, dirs)
	ff := append(db, service...)
	ioclosure.Propagate(ff)

	svc, ok := findFact(ff, "src.list_all")
	if !ok {
		t.Fatal("src.list_all not extracted")
	}
	if !hasRelation(svc, facts.RelCalls, "src.load_users") {
		t.Errorf("db::load_users() must call src.load_users, got %+v", svc.Relations)
	}
	if b, _ := svc.Props["performs_io"].(bool); !b {
		t.Errorf("src.list_all performs_io = %v, want true (through db::load_users)", svc.Props["performs_io"])
	}
	// sqlx is an external crate: its path is not resolved, so the call keeps the
	// bare name it always had.
	load, _ := findFact(ff, "src.load_users")
	if !hasRelation(load, facts.RelCalls, "query") {
		t.Errorf("sqlx::query must stay the bare \"query\", got %+v", load.Relations)
	}
}

// TestAST_ModulePathCallResolution pins which written paths resolve. Only a path
// that starts inside the crate does; an external crate, the standard library, a type
// and a method on a value keep the bare name, because none of them names a function
// this crate declares at a place the path can tell.
func TestAST_ModulePathCallResolution(t *testing.T) {
	crates := []crateInfo{{name: "app", dir: "."}, {name: "app_util", dir: "util"}}
	dirs := map[string]bool{"src": true, "src/jobs": true, "util/src": true}
	for _, tc := range []struct {
		name, body, want string
	}{
		{"crate path", "crate::db::load_users()", "src.load_users"},
		{"imported module", "db::load_users()", "src.load_users"},
		{"self path", "self::helper()", "src/jobs.helper"},
		{"super path", "super::db::load_users()", "src.load_users"},
		{"directory module", "crate::jobs::run()", "src/jobs.run"},
		{"sibling crate", "app_util::slugify(s)", "util/src.slugify"},
		{"external crate", "sqlx::query(sql)", "query"},
		{"standard library", "std::fs::read(path)", "read"},
		{"method on a value", "conn.execute(sql)", "execute"},
		{"type path", "Order::find(id)", "find"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ff, _ := extractFileAST([]byte(`
use crate::db;

pub fn caller() {
    `+tc.body+`;
}
`), "src/jobs/worker.rs", crates, dirs)
			f, ok := findFact(ff, "src/jobs.caller")
			if !ok {
				t.Fatal("src/jobs.caller not extracted")
			}
			if !hasRelation(f, facts.RelCalls, tc.want) {
				t.Errorf("%s: want a calls edge to %q, got %+v", tc.body, tc.want, f.Relations)
			}
		})
	}
}
