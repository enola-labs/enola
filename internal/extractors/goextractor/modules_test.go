package goextractor

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// writeRepo writes files under a fresh directory and returns it with the
// repository-relative paths, sorted, as the engine would hand them over.
func writeRepo(t *testing.T, files map[string]string) (string, []string) {
	t.Helper()
	dir := t.TempDir()
	var rel []string
	for p, content := range files {
		abs := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		rel = append(rel, p)
	}
	sort.Strings(rel)
	return dir, rel
}

func TestReadModules_EveryGoModInTheFileList(t *testing.T) {
	dir, files := writeRepo(t, map[string]string{
		"go.mod":          "module example.com/app\n\nrequire (\n\texample.com/app/sdk v1.0.0 // indirect\n\tgithub.com/x/y v0.1.0\n)\n\nrequire example.com/app/tools v0.0.0\n",
		"tools/go.mod":    "module example.com/app/tools\n",
		"svc/b/go.mod":    "module \"example.com/b\"\n\ngo 1.22\n",
		"svc/b/x.go":      "package b\n",
		"testdata/go.mod": "module fixture\n",
	})
	// The ignore globs drop testdata/ before the extractor sees the list.
	var listed []string
	for _, f := range files {
		if f != "testdata/go.mod" {
			listed = append(listed, f)
		}
	}
	got := readModules(dir, listed)
	want := []goModule{
		{path: "example.com/app/tools", dir: "tools"},
		{path: "example.com/app", dir: "."},
		{path: "example.com/b", dir: "svc/b"},
	}
	if len(got.list) != len(want) {
		t.Fatalf("got %+v, want %+v", got.list, want)
	}
	for i := range want {
		if got.list[i] != want[i] {
			t.Errorf("module %d: got %+v, want %+v", i, got.list[i], want[i])
		}
	}
	wantReq := []string{"example.com/app/sdk", "example.com/app/tools", "github.com/x/y"}
	if len(got.required) != len(wantReq) {
		t.Fatalf("required: got %v, want %v", got.required, wantReq)
	}
	for _, r := range wantReq {
		if !got.required[r] {
			t.Errorf("required: %q missing from %v", r, got.required)
		}
	}
	if got.rootPath() != "example.com/app" {
		t.Errorf("rootPath = %q", got.rootPath())
	}
}

func TestReadModules_RootGoModOutsideTheList(t *testing.T) {
	dir, _ := writeRepo(t, map[string]string{"go.mod": "module example.com/app\n"})
	if got := readModules(dir, nil).rootPath(); got != "example.com/app" {
		t.Errorf("a root go.mod is read as it always was: got %q", got)
	}
}

func TestGoModules_Resolve(t *testing.T) {
	mods := goModules{
		list: []goModule{
			{path: "example.com/app/tools", dir: "tools"},
			{path: "example.com/app", dir: "."},
			{path: "example.com/b", dir: "svc/b"},
		},
		// sdk is a separate module under the root's path; tools is required and
		// also in the repository, the shape of a local replace.
		required: map[string]bool{"example.com/app/sdk": true, "example.com/app/tools": true},
	}
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"example.com/app", ".", true},
		{"example.com/app/internal/x", "internal/x", true},
		// The nested module claims its own path, not the root's "tools/..." by accident.
		{"example.com/app/tools", "tools", true},
		{"example.com/app/tools/gen", "tools/gen", true},
		{"example.com/b", "svc/b", true},
		{"example.com/b/client", "svc/b/client", true},
		{"example.com/application", "", false},
		// Go gives an import to the longest matching module: a required one here.
		{"example.com/app/sdk", "", false},
		{"example.com/app/sdk/client", "", false},
		{"example.com/app/sdkx", "sdkx", true},
		{"fmt", "", false},
	}
	for _, c := range cases {
		got, ok := mods.resolve(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("resolve(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

// A monorepo with no root go.mod: one module per service, importing each other.
func TestExtract_NestedModulesResolveAndAreInternal(t *testing.T) {
	dir, files := writeRepo(t, map[string]string{
		"go.work":                "go 1.22\n\nuse (\n\t./svc/a\n\t./svc/b\n)\n",
		"svc/a/go.mod":           "module example.com/a\n",
		"svc/a/pkg/x/x.go":       "package x\n\nfunc X() int { return 1 }\n",
		"svc/a/cmd/main.go":      "package main\n\nimport (\n\t\"example.com/a/pkg/x\"\n\t\"example.com/b/client\"\n)\n\nfunc main() { _ = x.X(); client.Call() }\n",
		"svc/b/go.mod":           "module example.com/b\n",
		"svc/b/client/client.go": "package client\n\nfunc Call() {}\n",
	})
	ff, err := New().Extract(context.Background(), dir, files)
	if err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{
		"svc/a/cmd -> example.com/a/pkg/x":  "svc/a/pkg/x",
		"svc/a/cmd -> example.com/b/client": "svc/b/client",
	} {
		f, ok := findFact(ff, name)
		if !ok {
			t.Fatalf("no dependency fact %q", name)
		}
		if f.PropString("source") != "internal" || !hasRelation(f, facts.RelImports, target) {
			t.Errorf("%s: source %q, relations %+v; want internal, imports %s", name, f.PropString("source"), f.Relations, target)
		}
	}
	main, ok := findFact(ff, "svc/a/cmd.main")
	if !ok {
		t.Fatal("no svc/a/cmd.main")
	}
	for _, callee := range []string{"svc/a/pkg/x.X", "svc/b/client.Call"} {
		if !hasRelation(main, facts.RelCalls, callee) {
			t.Errorf("main does not call %s: %+v", callee, main.Relations)
		}
	}
	// No module at the root, so no root package carries a module path.
	for _, f := range ff {
		if f.Kind == facts.KindModule && f.PropString("modulePath") != "" {
			t.Errorf("%s carries modulePath %q", f.Name, f.PropString("modulePath"))
		}
	}
}
