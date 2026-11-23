package config

import (
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/pathglob"
)

// TestCppTestGlobsScopeToTestTrees pins the C and C++ half of the test-ignore
// contract. No tool reserves a directory, so the globs name the trees the corpus
// keeps its tests under: test/ and tests/ (CMake unit tests, the kernel's KUnit
// tests) and testing/ (kernel selftests, an engine's test harness). They are
// ignored and recovered as test references, in both the defaults and the bundled
// mcp-arch.yaml. Production files that only carry the token in a file name, and
// benchmark trees, stay indexed.
func TestCppTestGlobsScopeToTestTrees(t *testing.T) {
	bundled, err := Load(filepath.Join("..", "..", "mcp-arch.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	def := Default()
	for _, tc := range []struct {
		path string
		test bool
	}{
		{"test/api/test_api.cpp", true},
		{"test/include/test_helpers.hpp", true},
		{"tools/cpp/tests/main.cc", true},
		{"lib/tests/printf_kunit.c", true},
		{"drivers/gpu/drm/tests/drm_mm_test.c", true},
		{"tools/testing/selftests/bpf/test_progs.c", true},
		{"tools/testing/selftests/kselftest.h", true},
		{"engine/src/flutter/testing/test_args.cxx", true},
		{"src/pal/tests/palsuite.h", true},
		{"src/main/database.cpp", false},
		{"src/include/duckdb/main/test_helper.hpp", false},
		{"lib/test_bitmap.c", false},
		{"kernel/testing.c", false},
		{"benchmark/micro/bench.cpp", false},
		{"src/testdata.c", false},
	} {
		for name, cfg := range map[string]*Config{"defaults": def, "mcp-arch.yaml": bundled} {
			want := "indexed"
			if tc.test {
				want = "test refs"
			}
			if got := walkFate(cfg, tc.path); got != want {
				t.Errorf("%s under %s: %s, want %s", tc.path, name, got, want)
			}
		}
		if tc.test {
			if !pathglob.MatchAny(tc.path, def.TestGlobs) {
				t.Errorf("%s: ignored but not in TestGlobs, so its references are dropped", tc.path)
			}
			if !facts.IsTestPath(tc.path) {
				t.Errorf("%s: matched by the test globs but facts.IsTestPath calls it production", tc.path)
			}
		}
	}
}
