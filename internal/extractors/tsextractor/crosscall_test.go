package tsextractor

import (
	"reflect"
	"testing"
)

// The two facts the analyzer joins: the caller hands its loop's element to a
// callee, and the callee loops over the parameter it arrives in.
func TestTsCrossCallFacts(t *testing.T) {
	ff := extractAll(t, map[string]string{"src/x.ts": `
export function render(frame) {
  for (const el of frame.elements) { draw(el) }
}
export function hasQuery(item, words) {
  return words.some(w => item.title.includes(w))
}
export function viaLocal(group) {
  const members = group.members ?? [];
  return members.map(m => m.id)
}
export function r(frames, words) {
  for (const frame of frames) {
    render(frame)
    hasQuery(frame, words)
    viaLocal(frame.group)
    unrelated(words)
  }
}
export function draw(el) {}
export function unrelated(ws) { for (const w of ws) { draw(w) } }
`}, false)

	want := map[string][]int{"src.render": {0}, "src.hasQuery": {1}, "src.viaLocal": {0}, "src.unrelated": {0}}
	for name, idx := range want {
		f, ok := findFact(ff, name)
		if !ok {
			t.Fatalf("missing %s", name)
		}
		got, _ := f.Props["loops_over_param_index"].([]int)
		if f.Props["loops_over_param"] != true || !reflect.DeepEqual(got, idx) {
			t.Errorf("%s: loops_over_param=%v index=%v, want true %v", name, f.Props["loops_over_param"], got, idx)
		}
	}

	r, _ := findFact(ff, "src.r")
	calls := tsStrSlice(r, "calls_on_loop_element")
	args, _ := r.Props["calls_on_loop_element_arg"].([]int)
	if !reflect.DeepEqual(calls, []string{"src.render", "src.hasQuery", "src.viaLocal"}) || !reflect.DeepEqual(args, []int{0, 0, 0}) {
		t.Errorf("calls_on_loop_element = %v at %v; want render, hasQuery, viaLocal each at 0 (unrelated(words) passes no element)", calls, args)
	}
}

// A method called on the element, looping over its own state, is the same walk.
func TestTsCrossCallThroughTheReceiver(t *testing.T) {
	ff := extractAll(t, map[string]string{"src/x.ts": `
export class Group {
  members: number[] = [];
  total() { return this.members.reduce((a, b) => a + b, 0) }
}
export class Report {
  groups: Group[] = [];
  sum() {
    let s = 0;
    for (const g of this.groups) { s += g.total() }
    return s;
  }
}
`}, false)
	total, ok := findFact(ff, "src.Group.total")
	if !ok {
		t.Fatalf("missing src.Group.total")
	}
	if idx, _ := total.Props["loops_over_param_index"].([]int); !reflect.DeepEqual(idx, []int{-1}) {
		t.Errorf("Group.total loops over this.members: loops_over_param_index = %v, want [-1]", idx)
	}
}
