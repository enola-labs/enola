package goextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func TestClassifyImport(t *testing.T) {
	cases := []struct {
		path, module, want string
	}{
		// A module named without a dot owns its packages: they are not stdlib.
		{"golf", "golf", "internal"},
		{"golf/internal/app", "golf", "internal"},
		{"layersgate/notify", "layersgate", "internal"},
		// A prefix that is not a path segment is not the module.
		{"golfclub/x", "golf", "stdlib"},
		{"fmt", "golf", "stdlib"},
		{"net/http", "golf", "stdlib"},
		{"github.com/x/y", "golf", "external"},
		{"example.com/app/pkg/c", "example.com/app", "internal"},
		{"example.com/app", "example.com/app", "internal"},
		{"example.com/application", "example.com/app", "external"},
		// No go.mod: nothing is internal.
		{"fmt", "", "stdlib"},
		{"github.com/x/y", "", "external"},
	}
	for _, c := range cases {
		if got := classifyImport(c.path, c.module); got != c.want {
			t.Errorf("classifyImport(%q, %q) = %q, want %q", c.path, c.module, got, c.want)
		}
	}
}

// The shared fixture's module is "testmod", which has no dot: the case that
// labelled every package of such a module stdlib.
func TestExtract_DotlessModuleImportsAreInternal(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"a/a.go": "package a\n\nimport (\n\t\"fmt\"\n\t\"testmod/b\"\n)\n\nfunc A() { fmt.Println(b.B()) }\n",
		"b/b.go": "package b\n\nfunc B() string { return \"\" }\n",
	})
	want := map[string]string{"a -> testmod/b": "internal", "a -> fmt": "stdlib"}
	for name, source := range want {
		f, ok := findFact(ff, name)
		if !ok || f.Kind != facts.KindDependency {
			t.Fatalf("no dependency fact %q", name)
		}
		if got := f.PropString("source"); got != source {
			t.Errorf("%s: source = %q, want %q", name, got, source)
		}
	}
}
