package goextractor

import "testing"

const batchLoopSrc = `package pkg

type Rows struct{}

func (r *Rows) Next() bool { return false }
func query(ids []int) *Rows { return nil }
func list(page int) ([]int, int) { return nil, 0 }
func save(int)   {}
func parent(int) int { return 0 }
func loadAll() []int { return nil }

// One IN query per chunk of ids, drained inside.
func chunked(ids []int, size int) {
	left := len(ids)
	for left > 0 {
		rows := query(ids[:size])
		for rows.Next() {
			save(1)
		}
		left -= size
		ids = ids[size:]
	}
}

// One request per page, drained inside.
func paged() {
	page := 1
	for {
		items, next := list(page)
		for _, it := range items {
			save(it)
		}
		if next == 0 {
			break
		}
		page = next
	}
}

// A chain walk: one query per level, and nothing drained.
func chain(id int) {
	for {
		id = parent(id)
		if id == 0 {
			break
		}
	}
}

// A cursor walked a row at a time, with a query for each row's children: the
// outer loop takes one ELEMENT per round, so the query is an N+1.
func perRow(rows *Rows) {
	for rows.Next() {
		children := query([]int{1})
		for children.Next() {
			save(1)
		}
	}
}

// A query per element whose rows are then walked: an N+1, not batching.
func perItem(ids []int) {
	for _, id := range ids {
		rows := query([]int{id})
		for rows.Next() {
			save(id)
		}
	}
}
`

func TestBatchLoopDrainIsNotNestingAndItsQueryIsNotAnNPlusOne(t *testing.T) {
	ff := extractAll(t, map[string]string{"pkg/x.go": batchLoopSrc})

	for _, name := range []string{"pkg.chunked", "pkg.paged"} {
		f, ok := findFact(ff, name)
		if !ok {
			t.Fatalf("missing %s", name)
		}
		if got := intProp(t, f, "loop_depth"); got != 2 {
			t.Errorf("%s: loop_depth = %d, want the lexical 2", name, got)
		}
		if got := intProp(t, f, "scaling_loop_depth"); got > 1 {
			t.Errorf("%s: scaling_loop_depth = %d, want at most 1: each row is visited once", name, got)
		}
		scaling := strSliceProp(f, "calls_in_scaling_loop")
		if containsStr(scaling, "pkg.query") || containsStr(scaling, "pkg.list") {
			t.Errorf("%s: calls_in_scaling_loop = %v: the per-round fetch is the batching", name, scaling)
		}
		if !containsStr(scaling, "pkg.save") {
			t.Errorf("%s: calls_in_scaling_loop = %v, want pkg.save: it still runs once per row", name, scaling)
		}
	}
}

func TestLoopsThatOnlyResembleBatchingKeepTheirCalls(t *testing.T) {
	ff := extractAll(t, map[string]string{"pkg/x.go": batchLoopSrc})

	chain, _ := findFact(ff, "pkg.chain")
	if scaling := strSliceProp(chain, "calls_in_scaling_loop"); !containsStr(scaling, "pkg.parent") {
		t.Errorf("chain walk: calls_in_scaling_loop = %v, want pkg.parent (a query per level)", scaling)
	}

	per, _ := findFact(ff, "pkg.perItem")
	if scaling := strSliceProp(per, "calls_in_scaling_loop"); !containsStr(scaling, "pkg.query") {
		t.Errorf("per-item query: calls_in_scaling_loop = %v, want pkg.query (a range over data is not a batch loop)", scaling)
	}
	if got := intProp(t, per, "scaling_loop_depth"); got != 2 {
		t.Errorf("per-item query: scaling_loop_depth = %d, want 2", got)
	}
}

// The suspicion recorded in the plan: a row cursor whose body fetches and drains
// has the shape of a batch loop.
func TestRowCursorWithAQueryPerRowIsNotABatchLoop(t *testing.T) {
	ff := extractAll(t, map[string]string{"pkg/x.go": batchLoopSrc})
	per, ok := findFact(ff, "pkg.perRow")
	if !ok {
		t.Fatal("missing pkg.perRow")
	}
	if scaling := strSliceProp(per, "calls_in_scaling_loop"); !containsStr(scaling, "pkg.query") {
		t.Errorf("row cursor: calls_in_scaling_loop = %v, want pkg.query: it runs once per row", scaling)
	}
	// What the row's query returned belongs to the row: draining it is no second
	// factor of n.
	if got := intProp(t, per, "scaling_loop_depth"); got > 1 {
		t.Errorf("row cursor: scaling_loop_depth = %d, want at most 1", got)
	}
}
