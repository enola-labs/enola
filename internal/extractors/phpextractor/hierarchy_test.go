package phpextractor

import "testing"

func phpScaling(t *testing.T, body string) (loop, scaling int) {
	t.Helper()
	f, ok := symbolsByName(extractFileAST([]byte("<?php\nfunction r($xs, $ys) {\n"+body+"\n}\n"), "x.php"))["r"]
	if !ok {
		t.Fatalf("missing r")
	}
	loop, _ = f.Props["loop_depth"].(int)
	scaling, _ = f.Props["scaling_loop_depth"].(int)
	return loop, scaling
}

func TestPHPHierarchicalLoopAddsNoScalingDepth(t *testing.T) {
	cases := map[string]string{
		"method of the element":   `foreach ($xs as $d) { foreach ($d->models() as $m) { use_it($m); } }`,
		"property of the element": `foreach ($xs as $d) { foreach ($d->models as $m) { use_it($m); } }`,
		"derived local":           `foreach ($xs as $d) { $ms = $d->models ?? []; foreach ($ms as $m) { use_it($m); } }`,
		"the value itself":        `foreach ($xs as $type => $rows) { foreach ($rows as $row) { use_it($row); } }`,
		"keyed lookup":            `foreach ($xs as $key) { foreach ($ys[$key] as $row) { use_it($row); } }`,
		"view of the element":     `foreach ($xs as $d) { foreach (array_keys($d->attrs) as $k) { use_it($k); } }`,
		"three levels":            `foreach ($xs as $t) { foreach ($t->rows as $r) { foreach ($r->files as $f) { use_it($f); } } }`,
	}
	for name, body := range cases {
		loop, scaling := phpScaling(t, body)
		if loop < 2 {
			t.Errorf("%s: loop_depth = %d, want the lexical nesting kept", name, loop)
		}
		if scaling != 1 {
			t.Errorf("%s: scaling_loop_depth = %d, want 1", name, scaling)
		}
	}
}

func TestPHPIndependentLoopsStillMultiply(t *testing.T) {
	cases := map[string]string{
		"all pairs":               `foreach ($xs as $a) { foreach ($xs as $b) { cmp($a, $b); } }`,
		"two collections":         `foreach ($xs as $a) { foreach ($ys as $b) { cmp($a, $b); } }`,
		"slice from the index":    `foreach ($xs as $i => $a) { foreach (array_slice($xs, $i + 1) as $b) { cmp($a, $b); } }`,
		"function of the element": `foreach ($xs as $a) { foreach (candidates_for($a) as $b) { cmp($a, $b); } }`,
		"method with arguments":   `foreach ($xs as $t) { foreach ($t->flatRelatives(false) as $r) { use_it($r); } }`,
		"neighbour by index":      `foreach ($xs as $i => $a) { foreach ($ys[$i + 1] as $b) { cmp($a, $b); } }`,
	}
	for name, body := range cases {
		if _, scaling := phpScaling(t, body); scaling < 2 {
			t.Errorf("%s: scaling_loop_depth = %d, want >= 2", name, scaling)
		}
	}
}

func TestPHPHierarchicalLoopStillRepeats(t *testing.T) {
	f := symbolsByName(extractFileAST([]byte(`<?php
function r($xs) { foreach ($xs as $d) { foreach ($d->models as $m) { save_it($m); } } }
`), "x.php"))["r"]
	calls, _ := f.Props["calls_in_scaling_loop"].([]string)
	found := false
	for _, c := range calls {
		if c == "save_it" {
			found = true
		}
	}
	if !found {
		t.Errorf("calls_in_scaling_loop = %v, want to contain save_it", calls)
	}
}
