package pythonextractor

import "testing"

const pyBatchSrc = `
def query(ids):
    return []

def save(row):
    pass

def fetch(token):
    return None

def parent(i):
    return i

def by_chunks(ids):
    for chunk in chunked(ids, 500):
        rows = query(chunk)
        for row in rows:
            save(row)

def stepped(ids, total, size):
    for i in range(0, total, size):
        batch = ids[i : i + size]
        query(batch)

def paged():
    token = None
    while True:
        page = fetch(token)
        for item in page.items:
            save(item)
        if not page.next:
            break
        token = page.next

def chain(i):
    while i:
        i = parent(i)

def per_item(ids):
    for i in ids:
        rows = query([i])
        for row in rows:
            save(row)
`

func pyScalingCalls(t *testing.T, name string) (int, []string) {
	t.Helper()
	f := byName(astExtract(t, "svc.py", pyBatchSrc, false))[name]
	calls, _ := f.Props["calls_in_scaling_loop"].([]string)
	depth, _ := f.Props["scaling_loop_depth"].(int)
	return depth, calls
}

func pyHas(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func TestPyBatchLoopDrainIsNotNestingAndItsQueryIsNotAnNPlusOne(t *testing.T) {
	for _, name := range []string{"svc.by_chunks", "svc.paged"} {
		depth, calls := pyScalingCalls(t, name)
		if depth > 1 {
			t.Errorf("%s: scaling_loop_depth = %d, want at most 1: each row is visited once", name, depth)
		}
		if pyHas(calls, "svc.query") || pyHas(calls, "svc.fetch") {
			t.Errorf("%s: calls_in_scaling_loop = %v: the per-round fetch is the batching", name, calls)
		}
		if !pyHas(calls, "svc.save") {
			t.Errorf("%s: calls_in_scaling_loop = %v, want svc.save: it still runs once per row", name, calls)
		}
	}
	if _, calls := pyScalingCalls(t, "svc.stepped"); pyHas(calls, "svc.query") {
		t.Errorf("stepped range: calls_in_scaling_loop = %v: one query per slice is the batching", calls)
	}
}

func TestPyLoopsThatOnlyResembleBatchingKeepTheirCalls(t *testing.T) {
	if _, calls := pyScalingCalls(t, "svc.chain"); !pyHas(calls, "svc.parent") {
		t.Errorf("chain walk: calls_in_scaling_loop = %v, want svc.parent (a query per level)", calls)
	}
	depth, calls := pyScalingCalls(t, "svc.per_item")
	if !pyHas(calls, "svc.query") {
		t.Errorf("per-item query: calls_in_scaling_loop = %v, want svc.query (a for over data is not a batch loop)", calls)
	}
	if depth != 2 {
		t.Errorf("per-item query: scaling_loop_depth = %d, want 2", depth)
	}
}
