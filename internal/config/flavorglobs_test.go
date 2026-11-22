package config

import (
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// TestGradleFlavorTestSetsAreTests pins Android's per-variant test source sets:
// src/test<Variant>/ for unit tests and src/androidTest<Variant>/ for instrumented
// ones (src/testDemo, src/androidTestProd). The variant name is capitalised by
// Gradle, which is what keeps a production directory such as src/testable/ out.
// The defaults and the bundled mcp-arch.yaml decide each path the same way, and
// facts.IsTestPath agrees with both.
func TestGradleFlavorTestSetsAreTests(t *testing.T) {
	testWalkFates(t, map[string]string{
		"app/src/testDemo/kotlin/demo/ui/AppStateTest.kt":      "test refs",
		"app/src/testDemoDebug/kotlin/demo/ui/AppStateTest.kt": "test refs",
		"app/src/androidTestProd/java/demo/ui/ScreenTest.kt":   "test refs",
		"app/src/testable/kotlin/demo/Probe.kt":                "indexed",
		"app/src/main/java/demo/testDemo/Seed.kt":              "indexed",
		"app/src/demo/kotlin/demo/ui/DemoScreen.kt":            "indexed",
		"app/src/main/java/demo/androidTestHelpers/Harness.kt": "indexed",
		"app/src/testDemo/screenshots/AppState_compact.png":    "indexed",
	})
}

// testWalkFates checks each path's fate under config.Default() and under the
// bundled mcp-arch.yaml, and that every test path is one facts.IsTestPath agrees on.
func testWalkFates(t *testing.T, want map[string]string) {
	t.Helper()
	bundled, err := Load(filepath.Join("..", "..", "mcp-arch.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	def := Default()
	for rel, w := range want {
		if got := walkFate(def, rel); got != w {
			t.Errorf("%s: defaults %s, want %s", rel, got, w)
		}
		if got := walkFate(bundled, rel); got != w {
			t.Errorf("%s: mcp-arch.yaml %s, want %s", rel, got, w)
		}
		if w == "test refs" && !facts.IsTestPath(rel) {
			t.Errorf("%s: matched by the test globs but facts.IsTestPath calls it production", rel)
		}
	}
}
