package pythonextractor

import "testing"

// A for statement evaluates its iterable once, before the first iteration.
func TestPyForIterableIsNotPerIteration(t *testing.T) {
	f := byName(astExtract(t, "svc.py", `
def load():
    return []

def use(x):
    pass

def r():
    for spec in sorted(load(), key=str):
        use(spec)
`, false))["svc.r"]
	calls, _ := f.Props["calls_in_loop"].([]string)
	has := func(name string) bool {
		for _, c := range calls {
			if c == name {
				return true
			}
		}
		return false
	}
	if has("svc.load") {
		t.Errorf("calls_in_loop = %v: load() is in the for header, evaluated once", calls)
	}
	if !has("svc.use") {
		t.Errorf("calls_in_loop = %v, want to contain svc.use", calls)
	}
}
