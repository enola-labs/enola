package pythonextractor

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func extractPyRepo(t *testing.T, files map[string]string) []facts.Fact {
	t.Helper()
	dir := t.TempDir()
	var rel []string
	for name, src := range files {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		rel = append(rel, name)
	}
	slices.Sort(rel)
	ff, err := New().Extract(context.Background(), dir, rel)
	if err != nil {
		t.Fatal(err)
	}
	return ff
}

func pySym(t *testing.T, ff []facts.Fact, suffix string) facts.Fact {
	t.Helper()
	for _, f := range ff {
		if f.Kind == facts.KindSymbol && strings.HasSuffix(f.Name, suffix) {
			return f
		}
	}
	var names []string
	for _, f := range ff {
		if f.Kind == facts.KindSymbol {
			names = append(names, f.Name)
		}
	}
	t.Fatalf("no symbol ending in %s among %v", suffix, names)
	return facts.Fact{}
}

func pyCallsTo(f facts.Fact, suffix string) bool {
	return slices.ContainsFunc(f.Relations, func(r facts.Relation) bool {
		return r.Kind == facts.RelCalls && strings.HasSuffix(r.Target, suffix)
	})
}

func pyIO(f facts.Fact) bool {
	b, _ := f.PropAny("performs_io").(bool)
	return b
}

const pyHookFixture = `class S3Hook:
    def get_conn(self):
        return None

    def load_file(self, path, key):
        client = self.get_conn()
        client.upload_file(path, key)

    def key_name(self, path):
        return path.strip("/")
`

// Every way a class says what it holds in self, and the one that says nothing.
func TestPySelfAttributeCallResolvesByItsDeclaredType(t *testing.T) {
	ff := extractPyRepo(t, map[string]string{
		"pkg/__init__.py":       "",
		"pkg/hooks/__init__.py": "",
		"pkg/hooks/s3.py":       pyHookFixture,
		"pkg/ops/__init__.py":   "",
		"pkg/ops/transfer.py": `from functools import cached_property

from pkg.hooks.s3 import S3Hook


class ByConstructor:
    def __init__(self):
        self.hook = S3Hook()

    def run(self, paths):
        for p in paths:
            self.hook.load_file(p, p)


class ByParameter:
    def __init__(self, hook: S3Hook):
        self.hook = hook

    def run(self, p):
        self.hook.load_file(p, p)


class ByAnnotation:
    hook: S3Hook

    def run(self, p):
        self.hook.load_file(p, p)


class ByProperty:
    @cached_property
    def hook(self) -> S3Hook:
        return S3Hook()

    def run(self, p):
        self.hook.load_file(p, p)


class Unknown:
    def __init__(self, hook):
        self.hook = hook

    def run(self, p):
        self.hook.load_file(p, p)


class Reassigned:
    def __init__(self):
        self.hook = S3Hook()

    def swap(self, other: Unknown):
        self.hook = other

    def run(self, p):
        self.hook.load_file(p, p)
`,
	})
	for _, class := range []string{"ByConstructor", "ByParameter", "ByAnnotation", "ByProperty"} {
		f := pySym(t, ff, "transfer."+class+".run")
		if !pyCallsTo(f, "hooks/s3.S3Hook.load_file") {
			t.Errorf("%s.run relations = %v, want a call to S3Hook.load_file", class, f.Relations)
		}
		if !pyIO(f) {
			t.Errorf("%s.run is not flagged: the hook's load_file uploads", class)
		}
	}
	// The in-loop list names the method by its canonical name, like the edge.
	loop := pySym(t, ff, "transfer.ByConstructor.run")
	if inLoop, _ := loop.PropAny("calls_in_loop").([]string); !slices.Contains(inLoop, "pkg/hooks/s3.S3Hook.load_file") {
		t.Errorf("ByConstructor.run calls_in_loop = %v, want the canonical name", inLoop)
	}
	for _, class := range []string{"Unknown", "Reassigned"} {
		if f := pySym(t, ff, "transfer."+class+".run"); pyCallsTo(f, "S3Hook.load_file") {
			t.Errorf("%s.run has an edge to S3Hook.load_file, and nothing declares what self.hook is: %v", class, f.Relations)
		}
	}
}

// The in-loop lists are resolved with the relations, under a source root too:
// the dotted path and the file path differ there, and the analyzer looks a callee
// up by the file's.
func TestPyInLoopCalleesResolveUnderASourceRoot(t *testing.T) {
	ff := extractPyRepo(t, map[string]string{
		"providers/aws/src/acme/__init__.py":       "",
		"providers/aws/src/acme/hooks/__init__.py": "",
		"providers/aws/src/acme/hooks/s3.py":       pyHookFixture,
		"providers/aws/src/acme/ops/__init__.py":   "",
		"providers/aws/src/acme/ops/copy.py": `from acme.hooks.s3 import S3Hook


def copy_all(paths):
    hook = S3Hook()
    for p in paths:
        hook.load_file(p, hook.key_name(p))
`,
	})
	f := pySym(t, ff, "ops/copy.copy_all")
	inLoop, _ := f.PropAny("calls_in_loop").([]string)
	for _, want := range []string{"providers/aws/src/acme/hooks/s3.S3Hook.load_file", "providers/aws/src/acme/hooks/s3.S3Hook.key_name"} {
		if !slices.Contains(inLoop, want) {
			t.Errorf("copy_all calls_in_loop = %v, want %s", inLoop, want)
		}
	}
}
