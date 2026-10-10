package pythonextractor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// writePy writes content to repo/rel, creating parent dirs.
func writePy(t *testing.T, repo, rel, content string) {
	t.Helper()
	p := filepath.Join(repo, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func testRefs(t *testing.T, repo string, testFiles, prodFiles []string) []facts.Fact {
	t.Helper()
	ff, err := New().ExtractTestRefs(context.Background(), repo, testFiles, prodFiles)
	if err != nil {
		t.Fatalf("ExtractTestRefs: %v", err)
	}
	return ff
}

func targets(f facts.Fact) map[string]bool {
	out := map[string]bool{}
	for _, r := range f.Relations {
		out[r.Target] = true
	}
	return out
}

// A test that imports and calls a production function must yield one KindTestRef
// whose target is the canonical slash symbol name — the same spelling a production
// caller would produce, so the dead-code detector needs no special case.
func TestExtractTestRefs_ResolvesAbsoluteImport(t *testing.T) {
	repo := t.TempDir()
	writePy(t, repo, "pkg/service.py", "def cognify(x):\n    return x\n")
	writePy(t, repo, "tests/test_service.py", `
from pkg.service import cognify

def test_it():
    assert cognify(1) == 1
`)

	ff := testRefs(t, repo, []string{"tests/test_service.py"}, []string{"pkg/service.py"})

	if len(ff) != 1 {
		t.Fatalf("expected 1 test-ref fact, got %d: %+v", len(ff), ff)
	}
	if ff[0].Kind != facts.KindTestRef {
		t.Errorf("kind = %q, want %q", ff[0].Kind, facts.KindTestRef)
	}
	if ff[0].File != "tests/test_service.py" {
		t.Errorf("file = %q", ff[0].File)
	}
	if !targets(ff[0])["pkg/service.cognify"] {
		t.Errorf("missing resolved target pkg/service.cognify; got %v", targets(ff[0]))
	}
	for _, r := range ff[0].Relations {
		if r.Kind != facts.RelCalls {
			t.Errorf("relation kind = %q, want only %q", r.Kind, facts.RelCalls)
		}
	}
}

func TestExtractTestRefs_PackageDefinitionsAndReexports(t *testing.T) {
	repo := t.TempDir()
	writePy(t, repo, "pkg/__init__.py", "from pkg.service import cognify\ndef package_helper(): pass\n")
	writePy(t, repo, "pkg/service.py", "def cognify(x): return x\n")
	writePy(t, repo, "tests/test_package.py", `
from pkg import package_helper, cognify
def test_package():
    package_helper()
    cognify(1)
`)
	ff := testRefs(t, repo, []string{"tests/test_package.py"}, []string{"pkg/__init__.py", "pkg/service.py"})
	if len(ff) != 1 || ff[0].Kind != facts.KindTestRef {
		t.Fatalf("expected only one test-ref fact: %+v", ff)
	}
	for _, target := range []string{"pkg/__init__.package_helper", "pkg/service.cognify"} {
		if !targets(ff[0])[target] {
			t.Errorf("missing package reference %s: %v", target, targets(ff[0]))
		}
	}
}

// The contract: reference facts ONLY. This is what lets the pass read files the
// ignore globs exclude without putting test code back into the production graph.
func TestExtractTestRefs_EmitsNoSymbolsModulesOrRoutes(t *testing.T) {
	repo := t.TempDir()
	writePy(t, repo, "pkg/service.py", "def cognify(x):\n    return x\n")
	writePy(t, repo, "tests/test_service.py", `
from fastapi import FastAPI
from pkg.service import cognify

class TestHelper:
    def helper(self):
        return cognify(1)

@app.post("/should-not-be-a-route")
def handler():
    return cognify(2)
`)

	for _, f := range testRefs(t, repo, []string{"tests/test_service.py"}, []string{"pkg/service.py"}) {
		if f.Kind != facts.KindTestRef {
			t.Errorf("emitted a %q fact (%q); only test_ref is allowed", f.Kind, f.Name)
		}
	}
}

// The regression this whole line of work exists to prevent: a pytest fixture that
// mounts routers on a throwaway app must not contribute route facts. Reading the
// file again for references must not reintroduce the test-only prefix.
func TestExtractTestRefs_FixtureRouterMountIsNotARoute(t *testing.T) {
	repo := t.TempDir()
	writePy(t, repo, "pkg/routers.py", "def get_cognify_router():\n    return None\n")
	writePy(t, repo, "tests/conftest.py", `
import pytest
from fastapi import FastAPI
from pkg.routers import get_cognify_router

@pytest.fixture
def app():
    app = FastAPI()
    app.include_router(get_cognify_router(), prefix="/cognify")
    return app
`)

	ff := testRefs(t, repo, []string{"tests/conftest.py"}, []string{"pkg/routers.py"})

	for _, f := range ff {
		if f.Kind == facts.KindRoute {
			t.Fatalf("a fixture's include_router produced a route fact: %+v", f)
		}
	}
	// The reference itself must still be captured — that is the point of the pass.
	if len(ff) != 1 || !targets(ff[0])["pkg/routers.get_cognify_router"] {
		t.Errorf("expected a reference to pkg/routers.get_cognify_router, got %+v", ff)
	}
}

// An unresolvable target must be DROPPED, never guessed at. Binding a dotted path
// to the wrong symbol would silently mark real dead code as live, which is worse
// than the false positive this pass removes.
func TestExtractTestRefs_DropsExternalTargets(t *testing.T) {
	repo := t.TempDir()
	writePy(t, repo, "pkg/service.py", "def cognify(x):\n    return x\n")
	writePy(t, repo, "tests/test_ext.py", `
import json
import numpy as np
from sqlalchemy.orm import sessionmaker

def test_it():
    json.dumps({})
    np.array([1])
    sessionmaker()
`)

	// Every target is stdlib or third-party, so the file yields no fact at all
	// rather than an edgeless one.
	if ff := testRefs(t, repo, []string{"tests/test_ext.py"}, []string{"pkg/service.py"}); len(ff) != 0 {
		t.Errorf("expected no facts for an all-external test file, got %+v", ff)
	}
}

// A bare third-party name that nothing could resolve must be dropped, not kept.
// The dead-code detector falls back to short-name matching, so a stray "FastAPI"
// target would mark any production symbol of that name live — turning real dead
// code into a false negative, which is worse than the false positive this pass
// removes.
func TestExtractTestRefs_DropsUnresolvedBareNames(t *testing.T) {
	repo := t.TempDir()
	writePy(t, repo, "pkg/service.py", "def cognify(x):\n    return x\n")
	writePy(t, repo, "tests/test_mixed.py", `
from fastapi import FastAPI
from pkg.service import cognify

def test_it():
    app = FastAPI()
    cognify(1)
`)

	ff := testRefs(t, repo, []string{"tests/test_mixed.py"}, []string{"pkg/service.py"})
	if len(ff) != 1 {
		t.Fatalf("expected 1 fact, got %+v", ff)
	}
	got := targets(ff[0])
	if !got["pkg/service.cognify"] {
		t.Errorf("lost the resolved internal target; got %v", got)
	}
	for tgt := range got {
		if !strings.ContainsRune(tgt, '/') {
			t.Errorf("kept unresolved bare target %q; only canonical slash names may survive", tgt)
		}
	}
}

// Without prodFiles the dotted target has nothing to resolve against and is
// dropped as external — the failure mode the widened interface exists to avoid.
// Pinning it keeps the parameter from being quietly dropped again.
func TestExtractTestRefs_NeedsProductionFileSet(t *testing.T) {
	repo := t.TempDir()
	writePy(t, repo, "pkg/service.py", "def cognify(x):\n    return x\n")
	writePy(t, repo, "tests/test_service.py", `
from pkg.service import cognify

def test_it():
    cognify(1)
`)

	with := testRefs(t, repo, []string{"tests/test_service.py"}, []string{"pkg/service.py"})
	without := testRefs(t, repo, []string{"tests/test_service.py"}, nil)

	if len(with) != 1 || !targets(with[0])["pkg/service.cognify"] {
		t.Fatalf("with prodFiles: expected resolved target, got %+v", with)
	}
	if len(without) != 0 {
		t.Errorf("without prodFiles the target cannot resolve and must be dropped, got %+v", without)
	}
}

// Non-Python test files (a Ruby spec, a Go test) belong to other extractors and
// must be ignored even if the engine's FileOwner scoping ever changes.
func TestExtractTestRefs_IgnoresNonPythonFiles(t *testing.T) {
	repo := t.TempDir()
	writePy(t, repo, "spec/thing_spec.rb", "describe Thing do; end\n")

	if ff := testRefs(t, repo, []string{"spec/thing_spec.rb"}, nil); len(ff) != 0 {
		t.Errorf("expected no facts from a non-Python file, got %+v", ff)
	}
}
