package config

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/pathglob"
)

// walkFate is what the engine's walk does with one file under a config: index it as
// source, collect it for reference-only extraction (an ignored file matching a test
// glob), or drop it. A directory matching an ignore glob is skipped whole, so a file
// below it is dropped before the test globs are ever asked.
func walkFate(cfg *Config, rel string) string {
	segs := strings.Split(rel, "/")
	for i := 1; i < len(segs); i++ {
		if pathglob.MatchAny(strings.Join(segs[:i], "/"), cfg.Ignore) {
			return "dropped (directory " + strings.Join(segs[:i], "/") + "/ skipped)"
		}
	}
	if !pathglob.MatchAny(rel, cfg.Ignore) {
		return "indexed"
	}
	if pathglob.MatchAny(rel, cfg.TestGlobs) {
		return "test refs"
	}
	return "dropped"
}

// TestBundledConfigTreatsTestFilesLikeDefaults keeps the shipped mcp-arch.yaml from
// deciding a test file's fate differently from config.Default().
//
// A config's lists replace the defaults, so the bundled file's ignore list is the
// whole story for every repository it governs. It used to ignore Kotlin, Swift and
// PHP tests by bare name suffix while the defaults indexed them, so the same function
// read as isolated dead code under one and test-only under the other; the suffixes
// also deleted production files that merely end in the token (ABTest.kt). Its
// top-level tests/** skipped the directory before the test globs ran, dropping
// Python, TypeScript and C# tests there. Both lists now carry the same scoped globs.
func TestBundledConfigTreatsTestFilesLikeDefaults(t *testing.T) {
	bundled, err := Load(filepath.Join("..", "..", "mcp-arch.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	def := Default()
	for _, rel := range []string{
		// Kotlin / Android
		"src/test/kotlin/demo/PricingTest.kt",
		"app/src/androidTest/java/demo/ui/ScreenTest.kt",
		"app/src/androidTest/java/demo/ui/TestUtil.kt",
		"api/src/main/java/demo/model/ABTest.kt",
		// Swift
		"Tests/DemoTests/PricingTests.swift",
		"Tests/DemoTests/PricingTest.swift",
		"Sources/Demo/OnboardingABTest/OnboardingABTest.swift",
		// PHP
		"tests/PricingTest.php",
		"tests/TestCase.php",
		"tests/Acceptance/HomeCest.php",
		"src/Testing/ApiTestCase.php",
		// Other languages' tests under a top-level tests/ directory
		"tests/test_pricing.py",
		"tests/unit/pricing-test.ts",
		"tests/Demo.Tests/PricingTests.cs",
		// Controls the bundled file shares with the defaults
		"pkg/pricing/pricing_test.go",
		"src/pricing.test.ts",
		"spec/models/pricing_spec.rb",
	} {
		if got, want := walkFate(bundled, rel), walkFate(def, rel); got != want {
			t.Errorf("%s: mcp-arch.yaml %s, defaults %s", rel, got, want)
		}
	}
	// And the decision itself: these tests are ignored and recovered by the test
	// globs, and their production look-alikes stay indexed.
	for rel, want := range map[string]string{
		"src/test/kotlin/demo/PricingTest.kt":                  "test refs",
		"app/src/androidTest/java/demo/ui/TestUtil.kt":         "test refs",
		"Tests/DemoTests/PricingTests.swift":                   "test refs",
		"tests/PricingTest.php":                                "test refs",
		"api/src/main/java/demo/model/ABTest.kt":               "indexed",
		"Sources/Demo/OnboardingABTest/OnboardingABTest.swift": "indexed",
		"src/Testing/ApiTestCase.php":                          "indexed",
	} {
		if got := walkFate(def, rel); got != want {
			t.Errorf("%s: defaults %s, want %s", rel, got, want)
		}
	}
}
