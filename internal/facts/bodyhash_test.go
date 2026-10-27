package facts

import "testing"

// A body's version changes when its code does and not when it is re-indented, padded
// with blank lines or moved.
func TestBodyHash(t *testing.T) {
	src := []byte("import x\n\nfunction f() {\n\treturn 1;\n}\n")
	base := BodyHash(src, 3, 5)
	if base == "" {
		t.Fatal("no hash for a span inside the file")
	}
	for name, tc := range map[string]struct {
		src        string
		start, end int
		same       bool
	}{
		"moved down":   {"import x\n\n\n\nfunction f() {\n\treturn 1;\n}\n", 5, 7, true},
		"re-indented":  {"if (a) {\n    function f() {\n        return 1;\n    }\n}\n", 2, 4, true},
		"blank inside": {"function f() {\n\n\treturn 1;\n\n}", 1, 5, true},
		"edited":       {"function f() {\n\treturn 2;\n}\n", 1, 3, false},
		"line added":   {"function f() {\n\tlog();\n\treturn 1;\n}\n", 1, 4, false},
	} {
		if got := BodyHash([]byte(tc.src), tc.start, tc.end) == base; got != tc.same {
			t.Errorf("%s: same version = %v, want %v", name, got, tc.same)
		}
	}
	for name, span := range map[string][2]int{"past the end": {9, 12}, "no span": {3, 0}, "no line": {0, 2}} {
		if h := BodyHash(src, span[0], span[1]); h != "" {
			t.Errorf("%s: hash %q, want none", name, h)
		}
	}
}
