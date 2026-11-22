package rustextractor

import "testing"

func dieselIODirect(t *testing.T, src string, crates []crateInfo) bool {
	t.Helper()
	ff, _ := extractFileAST([]byte(src), "pkg/lib.rs", crates, map[string]bool{"pkg": true})
	f, ok := findFact(ff, "pkg.f")
	if !ok {
		t.Fatal("pkg.f not extracted")
	}
	b, _ := f.Props["io_direct"].(bool)
	return b
}

// A diesel query method that runs the query (RunQueryDsl: load, first,
// get_result, get_results, load_iter, load_stream) is I/O when the file brings
// diesel into scope and the call hands it a connection.
func TestRustIODirect_DieselQueryMethods(t *testing.T) {
	pkg := []crateInfo{{name: "pkg", dir: "pkg"}}
	// db re-exports diesel (`pub use diesel::*`): its manifest depends on it.
	reexport := []crateInfo{{name: "pkg", dir: "pkg"}, {name: "db", dir: "db", deps: map[string]bool{"diesel": true}}}
	cases := map[string]struct {
		src    string
		crates []crateInfo
	}{
		"first on a filtered table": {`
use diesel::prelude::*;
fn f(conn: &mut PgConnection) {
    let c = crates::table.filter(crates::name.eq(n)).first::<Crate>(conn);
}`, pkg},
		"load through diesel_async": {`
use diesel_async::{AsyncPgConnection, RunQueryDsl};
async fn f(conn: &mut AsyncPgConnection) {
    let v = versions::table.select(versions::id).load(&mut conn).await;
}`, pkg},
		"get_result on a query held in a local": {`
use diesel::prelude::*;
fn f(conn: &mut PgConnection) {
    let q = diesel::insert_into(t::table).values(&row);
    let id: i32 = q.get_result(&mut *conn).unwrap();
}`, pkg},
		"get_results through a field": {`
use diesel::RunQueryDsl;
fn f(&self) {
    let v = q.get_results(&mut self.conn);
}`, pkg},
		"load_stream": {`
use diesel_async::RunQueryDsl;
async fn f(conn: &mut AsyncPgConnection) {
    let s = q.load_stream::<Row>(conn).await;
}`, pkg},
		"diesel re-exported by an in-repo crate": {`
use db::{ExpressionMethods, prelude::*};
fn f(conn: &mut DBConnection) {
    let rows = dsl::table.filter(dsl::id.eq(id)).load::<Row>(&mut *conn);
}`, reexport},
		"RunQueryDsl named through another path": {`
use store::{QueryDsl, RunQueryDsl};
fn f(conn: &mut DBConnection) {
    let row = dsl::table.first::<Row>(conn);
}`, pkg},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if !dieselIODirect(t, c.src, c.crates) {
				t.Error("io_direct = false, want true: a diesel query method runs the query")
			}
		})
	}
}

// The same method names are not I/O on a slice, an atomic or an iterator, nor
// where nothing puts diesel in scope.
func TestRustIODirect_DieselNamesWithoutDieselNotIO(t *testing.T) {
	pkg := []crateInfo{{name: "pkg", dir: "pkg"}}
	other := []crateInfo{{name: "pkg", dir: "pkg"}, {name: "util", dir: "util", deps: map[string]bool{"serde": true}}}
	cases := map[string]struct {
		src    string
		crates []crateInfo
	}{
		"slice first, diesel in scope": {`
use diesel::prelude::*;
fn f(v: &[i32]) {
    let x = v.first();
}`, pkg},
		"atomic load by path, diesel in scope": {`
use diesel::prelude::*;
fn f(c: &AtomicUsize) {
    let x = c.load(Ordering::SeqCst);
}`, pkg},
		"atomic load by imported variant, diesel in scope": {`
use diesel::prelude::*;
use std::sync::atomic::Ordering::SeqCst;
fn f(c: &AtomicUsize) {
    let x = c.load(SeqCst);
}`, pkg},
		"one-argument load without diesel": {`
use std::sync::atomic::AtomicUsize;
fn f(conn: &mut Conn) {
    let x = q.load(conn);
    let y = q.first(conn);
}`, pkg},
		"in-repo crate that does not depend on diesel": {`
use util::prelude::*;
fn f(conn: &mut Conn) {
    let x = q.get_result(conn);
}`, other},
		"literal argument": {`
use diesel::prelude::*;
fn f() {
    let x = cache.load("key");
}`, pkg},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if dieselIODirect(t, c.src, c.crates) {
				t.Error("io_direct = true, want false: no diesel query runs here")
			}
		})
	}
}
