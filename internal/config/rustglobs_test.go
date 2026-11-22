package config

import (
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/pathglob"
)

// TestRustTestGlobsScopeToCargoTestTrees pins the Rust half of the test-ignore
// contract. Cargo builds every file under a crate's tests/ and benches/ as a test or
// bench target, so those trees are ignored and recovered as test references, in
// both the defaults and the bundled mcp-arch.yaml. A tests/ module directory under
// src/ matches too: it is a `#[cfg(test)] mod tests;` module or a `[[test]]` target
// path, never code the library ships. Production files that merely carry the token
// in a file or directory name stay indexed.
func TestRustTestGlobsScopeToCargoTestTrees(t *testing.T) {
	bundled, err := Load(filepath.Join("..", "..", "mcp-arch.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	def := Default()
	for _, tc := range []struct {
		path string
		test bool
	}{
		{"tests/integration.rs", true},
		{"tests/common/mod.rs", true},
		{"crates/engine/tests/it/main.rs", true},
		{"benches/throughput.rs", true},
		{"crates/engine/benches/spin.rs", true},
		{"crates/engine/src/runtime/tests/queue.rs", true},
		{"src/tests/mod.rs", true},
		{"src/lib.rs", false},
		{"src/testing.rs", false},
		{"src/test_util/mod.rs", false},
		{"src/bench.rs", false},
		{"crates/testkit/src/lib.rs", false},
		{"src/tests.rs", false},
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
