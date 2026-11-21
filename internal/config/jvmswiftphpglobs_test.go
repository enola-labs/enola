package config

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/pathglob"
)

// TestKotlinSwiftPHPTestGlobsScopeToTestTrees pins the Kotlin, Swift and PHP half of
// the test-ignore contract: their test files are ignored for normal indexing AND
// matched by TestGlobs (the two lists must agree), every such path is one
// facts.IsTestPath also calls a test, and production files that merely END in
// Test/Tests stay indexed. The globs are scoped to the tool-enforced test tree
// (Gradle source sets, SPM and Xcode test targets, PHPUnit's tests/), never a bare
// name suffix: `**/*Test.kt` deleted a production model named ABTest.kt.
func TestKotlinSwiftPHPTestGlobsScopeToTestTrees(t *testing.T) {
	cfg := Default()
	for _, tc := range []struct {
		path string
		test bool
	}{
		// Kotlin: Gradle unit tests, Android instrumented tests, KMP target test sets.
		{"src/test/kotlin/demo/PricingTest.kt", true},
		{"app/src/test/java/demo/FooTest.kt", true},
		{"app/src/androidTest/java/demo/ui/TestUtil.kt", true},
		{"shared/src/commonTest/kotlin/demo/Fakes.kt", true},
		{"shared/src/jvmTest/kotlin/demo/JvmSpec.kt", true},
		{"api/src/main/java/demo/model/ABTest.kt", false},
		{"app/src/main/java/demo/test/Seed.kt", false},
		// Swift: SPM Tests/, Xcode <Target>Tests/ and <Target>UITests/.
		{"Tests/DemoTests/PricingTests.swift", true},
		{"Tests/Helpers.swift", true},
		{"MyAppTests/LoginViewModelTests.swift", true},
		{"App/MyAppUITests/LaunchTests.swift", true},
		{"Sources/Demo/OnboardingABTest/OnboardingABTest.swift", false},
		{"Sources/Demo/Settings/Settings.ExperimentTests.swift", false},
		{"Sources/Demo/PricingTests.swift", false},
		// PHP: PHPUnit, Pest and Codeception keep tests under tests/; Laminas under test/.
		{"tests/PricingTest.php", true},
		{"tests/TestCase.php", true},
		{"tests/Acceptance/HomeCest.php", true},
		{"apps/files/tests/Controller/ApiControllerTest.php", true},
		{"test/Router/RouteTest.php", true},
		{"src/Testing/ApiTestCase.php", false},
		{"src/Http/AbTest.php", false},
	} {
		ignored := pathglob.MatchAny(tc.path, cfg.Ignore)
		globbed := pathglob.MatchAny(tc.path, cfg.TestGlobs)
		if ignored != tc.test || globbed != tc.test {
			t.Errorf("%s: ignored=%v testGlob=%v, want both %v", tc.path, ignored, globbed, tc.test)
		}
		if tc.test && !facts.IsTestPath(tc.path) {
			t.Errorf("%s: matched by the test globs but facts.IsTestPath calls it production", tc.path)
		}
	}
	for _, bare := range []string{"**/*Test.kt", "**/*Tests.swift", "**/*Test.swift", "**/*Test.php", "tests/**"} {
		if contains(cfg.Ignore, bare) || contains(cfg.TestGlobs, bare) {
			t.Errorf("Default() has %q: a bare suffix swallows production files, and a bare directory ignore prunes before the test globs run", bare)
		}
	}
}
