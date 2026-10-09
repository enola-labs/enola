package tsextractor

import "testing"

// A loop over the children of the element an enclosing loop is on visits every
// child once: it raises loop_depth and leaves scaling_loop_depth alone.
func TestTsHierarchicalLoopAddsNoScalingDepth(t *testing.T) {
	cases := map[string]string{
		"member of the element": `export function r(jobs) {
  for (const job of jobs) {
    for (const need of job.needs) { use(need) }
  }
}`,
		"derived local": `export function r(jobs) {
  for (const job of jobs) {
    const needs = job.needs || [];
    for (const need of needs) { use(need) }
  }
}`,
		"destructured element": `export function r(groups) {
  for (const {values} of groups) {
    for (const v of values) { use(v) }
  }
}`,
		"iterator callbacks": `export function r(frames) {
  return frames.map(frame => frame.fields.filter(f => keep(f)))
}`,
		"reduce over the element's children": `export function r(groups) {
  return groups.reduce((acc, group) => acc + group.members.filter(m => keep(m)).length, 0)
}`,
		"keyed lookup": `export function r(ids, byId) {
  for (const id of ids) {
    for (const row of byId.get(id) ?? []) { use(row) }
  }
}`,
		"selected by index": `export function r(rows) {
  for (let i = 0; i < rows.length; i++) {
    for (let j = 0; j < rows[i].length; j++) { use(rows[i][j]) }
  }
}`,
		"object entries of the element": `export function r(items) {
  for (const item of items) {
    for (const [k, v] of Object.entries(item.attrs)) { use(k, v) }
  }
}`,
		"three levels": `export function r(namespaces) {
  for (const ns of namespaces) {
    for (const group of ns.groups) {
      for (const rule of group.rules) { use(rule) }
    }
  }
}`,
	}
	for name, src := range cases {
		f := tsExtractFunc(t, src, "src.r")
		if got := tsIntProp(t, f, "loop_depth"); got < 2 {
			t.Errorf("%s: loop_depth = %d, want the lexical nesting kept", name, got)
		}
		if got := tsIntProp(t, f, "scaling_loop_depth"); got != 1 {
			t.Errorf("%s: scaling_loop_depth = %d, want 1", name, got)
		}
	}
}

// Mentioning the outer variable is not reaching the collection through it. Each of
// these multiplies and has to keep counting.
func TestTsIndependentLoopsStillMultiply(t *testing.T) {
	cases := map[string]string{
		"all pairs": `export function r(xs) {
  for (const a of xs) {
    for (const b of xs) { cmp(a, b) }
  }
}`,
		"all pairs from the outer index": `export function r(xs) {
  for (let i = 0; i < xs.length; i++) {
    for (const b of xs.slice(i + 1)) { cmp(xs[i], b) }
  }
}`,
		"triangular": `export function r(xs) {
  for (let i = 0; i < xs.length; i++) {
    for (let j = i + 1; j < xs.length; j++) { cmp(xs[i], xs[j]) }
  }
}`,
		"unrelated collection filtered by the element": `export function r(orders, items) {
  for (const o of orders) {
    for (const it of items.filter(x => x.order === o.id)) { use(it) }
  }
}`,
		"function of the element": `export function r(xs) {
  for (const a of xs) {
    for (const b of candidatesFor(a)) { use(b) }
  }
}`,
		"method of the element that takes arguments": `export function r(tasks) {
  for (const t of tasks) {
    for (const rel of t.getFlatRelatives(false)) { use(rel) }
  }
}`,
		"reduce over the accumulator": `export function r(xs) {
  return xs.reduce((acc, x) => acc.filter(a => a !== x), xs)
}`,
		"neighbour by index": `export function r(rows) {
  for (let i = 0; i < rows.length; i++) {
    for (const c of rows[i + 1]) { use(c) }
  }
}`,
	}
	for name, src := range cases {
		f := tsExtractFunc(t, src, "src.r")
		if got := tsIntProp(t, f, "scaling_loop_depth"); got < 2 {
			t.Errorf("%s: scaling_loop_depth = %d, want >= 2", name, got)
		}
	}
}

// Under a loop with a constant trip count there is no factor of n to cancel, and
// a closure defined in a loop does not run inside it.
func TestTsHierarchyNeedsAnAmortizingOuterLoop(t *testing.T) {
	f := tsExtractFunc(t, `export function r(cfg) {
  for (const key of ["a", "b"]) {
    for (const v of cfg[key]) { use(v) }
  }
}`, "src.r")
	if got := tsIntProp(t, f, "scaling_loop_depth"); got != 1 {
		t.Errorf("constant outer: scaling_loop_depth = %d, want 1 (the inner loop is the one that scales)", got)
	}
}

// A call inside a hierarchical loop still runs once per child: it stays an N+1
// candidate.
func TestTsHierarchicalLoopStillRepeats(t *testing.T) {
	f := tsExtractFunc(t, `export function r(jobs) {
  for (const job of jobs) {
    for (const need of job.needs) { save(need) }
  }
}`, "src.r")
	if cil := tsStrSlice(f, "calls_in_scaling_loop"); !tsContains(cil, "src.save") {
		t.Errorf("calls_in_scaling_loop = %v, want to contain src.save", cil)
	}
}
