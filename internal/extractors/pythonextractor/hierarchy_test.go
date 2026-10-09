package pythonextractor

import "testing"

func pyScaling(t *testing.T, body string) (loop, scaling int) {
	t.Helper()
	f := byName(astExtract(t, "svc.py", "def r(xs, ys, by_id):\n"+body+"\n", false))["svc.r"]
	return cxIntProp(t, f, "loop_depth"), cxIntProp(t, f, "scaling_loop_depth")
}

func TestPyHierarchicalLoopAddsNoScalingDepth(t *testing.T) {
	cases := map[string]string{
		"attribute of the element": "    for dag in xs:\n        for task in dag.tasks:\n            use(task)",
		"method of the element":    "    for dag in xs:\n        for k, v in dag.params.items():\n            use(k, v)",
		"derived local":            "    for dag in xs:\n        tasks = dag.tasks or []\n        for task in tasks:\n            use(task)",
		"the element itself":       "    for row in xs:\n        for cell in row:\n            use(cell)",
		"keyed lookup":             "    for key in xs:\n        for row in by_id[key]:\n            use(row)",
		"keyed get":                "    for x in xs:\n        for row in by_id.get(x.id, []):\n            use(row)",
		"view of the element":      "    for dag in xs:\n        for i, task in enumerate(dag.tasks):\n            use(i, task)",
		"comprehension":            "    for dag in xs:\n        out = [t.id for t in dag.tasks]\n        use(out)",
		"three levels":             "    for a in xs:\n        for b in a.kids:\n            for c in b.kids:\n                use(c)",
	}
	for name, body := range cases {
		loop, scaling := pyScaling(t, body)
		if loop < 2 {
			t.Errorf("%s: loop_depth = %d, want the lexical nesting kept", name, loop)
		}
		if scaling != 1 {
			t.Errorf("%s: scaling_loop_depth = %d, want 1", name, scaling)
		}
	}
}

func TestPyIndependentLoopsStillMultiply(t *testing.T) {
	cases := map[string]string{
		"all pairs":               "    for a in xs:\n        for b in xs:\n            cmp(a, b)",
		"two collections":         "    for a in xs:\n        for b in ys:\n            cmp(a, b)",
		"slice from the index":    "    for i, a in enumerate(xs):\n        for b in xs[i + 1:]:\n            cmp(a, b)",
		"triangular range":        "    for i in range(len(xs)):\n        for j in range(i):\n            cmp(xs[i], xs[j])",
		"function of the element": "    for a in xs:\n        for b in candidates_for(a):\n            cmp(a, b)",
		"method with arguments":   "    for t in xs:\n        for rel in t.get_flat_relatives(upstream=False):\n            use(rel)",
		"comprehension all pairs": "    for a in xs:\n        out = [cmp(a, b) for b in ys]\n        use(out)",
	}
	for name, body := range cases {
		if _, scaling := pyScaling(t, body); scaling < 2 {
			t.Errorf("%s: scaling_loop_depth = %d, want >= 2", name, scaling)
		}
	}
}

func TestPyHierarchicalLoopStillRepeats(t *testing.T) {
	f := byName(astExtract(t, "svc.py", `
def save(t):
    pass

def r(xs):
    for dag in xs:
        for task in dag.tasks:
            save(task)
`, false))["svc.r"]
	calls, _ := f.Props["calls_in_scaling_loop"].([]string)
	found := false
	for _, c := range calls {
		if c == "svc.save" {
			found = true
		}
	}
	if !found {
		t.Errorf("calls_in_scaling_loop = %v, want to contain svc.save", calls)
	}
}
