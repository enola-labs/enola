package goextractor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// targetsOf returns the RelCalls targets of a single test-ref fact, failing the
// test unless exactly one fact was produced and it has the shape the contract
// demands: kind test_ref, name == file == relFile, language "go", and no
// relation other than "calls".
func targetsOf(t *testing.T, ff []facts.Fact, relFile string) map[string]bool {
	t.Helper()
	if len(ff) != 1 {
		t.Fatalf("want exactly 1 test_ref fact, got %d: %+v", len(ff), ff)
	}
	f := ff[0]
	if f.Kind != facts.KindTestRef {
		t.Fatalf("kind = %q, want %q", f.Kind, facts.KindTestRef)
	}
	if f.Name != relFile || f.File != relFile {
		t.Fatalf("name/file = %q/%q, want %q", f.Name, f.File, relFile)
	}
	if lang, _ := f.Props["language"].(string); lang != "go" {
		t.Fatalf("props[language] = %v, want \"go\"", f.Props["language"])
	}
	out := map[string]bool{}
	for _, r := range f.Relations {
		if r.Kind != facts.RelCalls {
			t.Fatalf("relation kind = %q, want only %q", r.Kind, facts.RelCalls)
		}
		out[r.Target] = true
	}
	return out
}

// TestExtractTestRefs_InPackageCallResolvesToProductionSymbol pins the dominant Go
// idiom: a `_test.go` in the SAME package calls the production function
// unqualified. The emitted target must equal the production symbol's fact name
// ("<pkgDir>.<Name>"), which is what orphans.candidateNames matches against. This
// is the shape behind golf's NewRateLimiter false positive. (v100)
func TestExtractTestRefs_InPackageCallResolvesToProductionSymbol(t *testing.T) {
	dir := setupGoProject(t, map[string]string{
		"pkg/svc/svc.go": `package svc

func NewThing() *Thing { return &Thing{} }

type Thing struct{}

func (t *Thing) Allow() bool { return true }
`,
		"pkg/svc/svc_test.go": `package svc

import "testing"

func TestNewThing(t *testing.T) {
	thing := NewThing()
	if !thing.Allow() {
		t.Fatal("denied")
	}
}
`,
	})

	ff, err := New().ExtractTestRefs(context.Background(), dir, []string{"pkg/svc/svc_test.go"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ff = dirNamed(t, dir, ff)
	got := targetsOf(t, ff, "pkg/svc/svc_test.go")

	if !got["pkg/svc.NewThing"] {
		t.Errorf("missing target pkg/svc.NewThing; got %v", got)
	}
	// `thing := NewThing()` is resolved through collectLocalTypes' constructor
	// convention, so the method call lands on the canonical method fact name
	// rather than dangling on a raw "thing.Allow" join.
	if !got["pkg/svc.Thing.Allow"] {
		t.Errorf("missing target pkg/svc.Thing.Allow; got %v", got)
	}
}

// TestExtractTestRefs_ExternalTestPackageResolvesThroughImport pins the other Go
// test idiom: `package svc_test` importing the package under test. The import
// alias must resolve to the module-relative package dir so the target is the same
// canonical name the in-package idiom produces. (v100)
func TestExtractTestRefs_ExternalTestPackageResolvesThroughImport(t *testing.T) {
	dir := setupGoProject(t, map[string]string{
		"pkg/svc/svc.go": `package svc

func NewThing() int { return 1 }
`,
		"pkg/svc/ext_test.go": `package svc_test

import (
	"testing"

	"testmod/pkg/svc"
)

func TestExternal(t *testing.T) {
	if svc.NewThing() != 1 {
		t.Fatal("bad")
	}
}
`,
	})

	ff, err := New().ExtractTestRefs(context.Background(), dir, []string{"pkg/svc/ext_test.go"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ff = dirNamed(t, dir, ff)
	got := targetsOf(t, ff, "pkg/svc/ext_test.go")

	if !got["pkg/svc.NewThing"] {
		t.Errorf("missing target pkg/svc.NewThing; got %v", got)
	}
}

// TestExtractTestRefs_SkipsBuiltinsAndEmitsNoSymbols guards the two invariants the
// plugin.TestRefExtractor contract states: reference-only facts (never a symbol,
// module or route fact — test code must not become a dead-code candidate), and no
// phantom targets for Go's predeclared identifiers.
//
// `min` is the load-bearing case: since Go 1.21 it is a builtin, but a package may
// still declare its own. goBuiltins drops it, so a package-level `min` exercised
// only by its test stays an orphan — exactly as it does when production code calls
// it. That is parity with the production resolver, not a regression. (v100)
func TestExtractTestRefs_SkipsBuiltinsAndEmitsNoSymbols(t *testing.T) {
	dir := setupGoProject(t, map[string]string{
		"pkg/svc/svc.go": `package svc

func Helper(xs []int) int { return len(xs) }

func min(a, b int) int { return a }
`,
		"pkg/svc/svc_test.go": `package svc

import "testing"

func TestHelper(t *testing.T) {
	xs := make([]int, 0)
	_ = len(xs)
	_ = string(rune(65))
	_ = min(1, 2)
	_ = Helper(xs)
}
`,
	})

	ff, err := New().ExtractTestRefs(context.Background(), dir, []string{"pkg/svc/svc_test.go"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ff = dirNamed(t, dir, ff)
	for _, f := range ff {
		if f.Kind != facts.KindTestRef {
			t.Fatalf("emitted a non-test_ref fact: %+v", f)
		}
	}
	got := targetsOf(t, ff, "pkg/svc/svc_test.go")

	if !got["pkg/svc.Helper"] {
		t.Errorf("missing target pkg/svc.Helper; got %v", got)
	}
	for _, builtin := range []string{"len", "make", "string", "rune", "min", "pkg/svc.min"} {
		if got[builtin] {
			t.Errorf("builtin %q must not become a target; got %v", builtin, got)
		}
	}
}

// TestExtractTestRefs_IgnoresFilesItDoesNotOwn is the guard for the engine's
// hand-off. GoExtractor deliberately does NOT implement plugin.FileOwner (doing so
// would switch on its incremental caching and re-partition every other extractor's
// cache key), so runTestRefExtractors passes it EVERY test file in the repo —
// including Ruby specs. It must filter to *_test.go itself, and must not treat a
// production .go file as a test. (v100)
func TestExtractTestRefs_IgnoresFilesItDoesNotOwn(t *testing.T) {
	dir := setupGoProject(t, map[string]string{
		"pkg/svc/svc.go": "package svc\n\nfunc NewThing() int { return 1 }\n",
	})
	if err := os.MkdirAll(filepath.Join(dir, "spec"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "spec", "thing_spec.rb"), []byte("Thing.new\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ff, err := New().ExtractTestRefs(context.Background(), dir, []string{
		"spec/thing_spec.rb", // Ruby's, not ours
		"pkg/svc/svc.go",     // production source, not a test
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(ff) != 0 {
		t.Fatalf("want no facts from files goextractor does not own, got %+v", ff)
	}
}

// TestExtractTestRefs_ReferenceFreeFileYieldsNoFact mirrors refsFromRuby: a test
// file that references nothing produces no fact at all, rather than an empty one.
func TestExtractTestRefs_ReferenceFreeFileYieldsNoFact(t *testing.T) {
	dir := setupGoProject(t, map[string]string{
		"pkg/svc/empty_test.go": "package svc\n\nvar x = 1\n",
	})

	ff, err := New().ExtractTestRefs(context.Background(), dir, []string{"pkg/svc/empty_test.go"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ff = dirNamed(t, dir, ff)
	if len(ff) != 0 {
		t.Fatalf("want no fact for a reference-free test file, got %+v", ff)
	}
}
