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

// The lines a caller hashes itself are the lines BodyHash hashes, so a run of them found
// inside a body compares equal to a version taken from that body.
func TestNormalizedLinesHashLikeBodyHash(t *testing.T) {
	src := []byte("a\n\n  b  \n\tc\n\n")
	for _, span := range [][2]int{{1, 4}, {2, 3}, {2, 2}, {1, 1}, {4, 9}} {
		lines, ok := NormalizedLines(src, span[0], span[1])
		if !ok {
			t.Fatalf("span %v: not inside src", span)
		}
		if got, want := HashLines(lines), BodyHash(src, span[0], span[1]); got != want {
			t.Errorf("span %v: HashLines %q, BodyHash %q", span, got, want)
		}
	}
	if _, ok := NormalizedLines(src, 9, 12); ok {
		t.Error("a span past the end is not inside src")
	}
}

func TestDeclHash(t *testing.T) {
	hash := func(src string) string { return DeclHash([]byte(src), 1, 100) }
	for _, tc := range []struct {
		name string
		a, b string
	}{
		{"typescript accessors", "get value(): number {\n return 1;\n}", "set value(n: number) {\n this.v = n;\n}"},
		{"python property", "def val(self):\n    return 1", "def val(self, n):\n    self.v = n"},
		{"java overloads behind one annotation", "@Override\nint f(int a) {\n}", "@Override\nint f(String a) {\n}"},
		{"csharp attributes", "[HttpGet]\nint f(int a) {\n}", "[HttpGet]\nint f(string a) {\n}"},
		{"rust attributes", "#[inline]\nfn f(a: i32) {\n}", "#[inline]\nfn f(a: i64) {\n}"},
		{"signature over several lines", "int f(\n int a) {\n}", "int f(\n String a) {\n}"},
	} {
		if hash(tc.a) == hash(tc.b) {
			t.Errorf("%s: the two declarations share a hash", tc.name)
		}
	}
	for _, tc := range []struct {
		name string
		a, b string
	}{
		{"body rewritten", "int f(int a) {\n return 1;\n}", "int f(int a) {\n return 2;\n return 3;\n}"},
		{"reindented", "int f(int a) {\n}", "\t\tint f(int a) {\n\t}"},
		{"annotation changed", "@A\nint f(int a) {\n}", "@B\nint f(int a) {\n}"},
	} {
		if hash(tc.a) != hash(tc.b) {
			t.Errorf("%s: the hash moved", tc.name)
		}
	}
	if got := DeclHash([]byte("x\n"), 5, 6); got != "" {
		t.Errorf("a span outside the source hashed to %q", got)
	}
	if hash("@OnlyAnAnnotation") == "" {
		t.Error("a span of nothing but annotations has no hash")
	}
}
