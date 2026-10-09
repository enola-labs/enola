package pythonextractor

import (
	"reflect"
	"testing"
)

// The two facts the analyzer joins: the caller hands its loop's element to a
// callee, and the callee loops over the parameter it arrives in.
func TestPyCrossCallFacts(t *testing.T) {
	idx := byName(astExtract(t, "svc.py", `
def validate(dag):
    for task in dag.tasks:
        check(task)

def has_query(item, words):
    return any(w in item.title for w in words)

def via_local(group):
    members = group.members or []
    return [m.id for m in members]

def check(task):
    pass

class Report:
    def total(self):
        return sum(g for g in self.groups)

    def per(self, dags, words):
        for dag in dags:
            validate(dag)
            has_query(dag, words)
            via_local(dag.group)
            self.note(words, dag)

    def note(self, words, dag):
        for t in dag.tasks:
            check(t)
`, false))

	want := map[string][]int{
		"svc.validate":     {0},
		"svc.has_query":    {1},
		"svc.via_local":    {0},
		"svc.Report.total": {-1},
		"svc.Report.note":  {1}, // self does not take a position
	}
	for name, pos := range want {
		f := idx[name]
		got, _ := f.Props["loops_over_param_index"].([]int)
		if f.Props["loops_over_param"] != true || !reflect.DeepEqual(got, pos) {
			t.Errorf("%s: loops_over_param=%v index=%v, want true %v", name, f.Props["loops_over_param"], got, pos)
		}
	}

	per := idx["svc.Report.per"]
	calls, _ := per.Props["calls_on_loop_element"].([]string)
	args, _ := per.Props["calls_on_loop_element_arg"].([]int)
	wantCalls := []string{"svc.validate", "svc.has_query", "svc.via_local", "svc.Report.note"}
	if !reflect.DeepEqual(calls, wantCalls) || !reflect.DeepEqual(args, []int{0, 0, 0, 1}) {
		t.Errorf("calls_on_loop_element = %v at %v; want %v at [0 0 0 1]", calls, args, wantCalls)
	}
}
