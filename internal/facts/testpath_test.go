package facts

import "testing"

func TestIsTestPath(t *testing.T) {
	tests := []struct {
		name string
		path string
		want bool
	}{
		// Directory segments — the primary signal. Test trees are tool-enforced
		// (Xcode targets, Gradle source sets, pytest/rspec convention).
		{"swift xcode test target", "Tests/Testability/Sources/Assert.swift", true},
		{"swift xcode mock target", "Mocks/APIStub.swift", true},
		{"gradle unit test source set", "app/src/test/java/de/example/app/FooTest.kt", true},
		{"gradle instrumented source set", "app/src/androidTest/java/de/example/app/EspressoMocks.kt", true},
		{"kmp per-target test tree", "shared/src/commonTest/kotlin/Foo.kt", true},
		{"jest colocated", "src/components/__tests__/Button.tsx", true},
		{"rspec tree", "spec/models/user_spec.rb", true},
		{"go testdata", "internal/engine/testdata/repos/x/main.go", true},
		{"test-support package", "internal/testutil/helpers.go", true},
		{"fixtures tree", "src/dashboard/fixtures/mockNativeFilters.ts", true},
		{"camel-case integration tests", "integrationTests/node/index.mjs", true},
		{"hyphenated type tests", "drizzle-orm/type-tests/mysql/tables.ts", true},
		{"hyphenated integration tests", "integration-tests/tests/pg.ts", true},
		{"python shared test support", "tests_common/pytest_plugins.py", true},
		{"cypress default folder", "modules/web/cypress/support/commands.ts", true},
		{"playwright suite folder", "playwright/support/helpers/links.ts", true},
		{"angular cli e2e folder", "client/e2e/src/po/login.po.ts", true},
		{"nx e2e project", "apps/gauzy-e2e/src/app.cy.ts", true},
		{"cypress component spec beside its component", "src/components/Button.cy.tsx", true},
		{"cypress config at the root", "cypress.config.ts", true},
		{"playwright config in a package", "apps/web/playwright.config.ts", true},
		{"a production module named like a config", "src/config/cypress.ts", false},
		{"playwright project", "projects/demo-playwright/utils/goto.ts", true},
		{"suffix alone is not a project", "src/-e2e/x.ts", false},
		{"e2e inside a word", "src/pre2e/model.ts", false},
		{"cypress inside a word", "src/cypresslike/view.ts", false},

		// Filename conventions — ONLY the tool-enforced ones. A compiler or runner
		// keys off each of these, so production code cannot adopt one by accident.
		{"go test file", "internal/engine/cache_test.go", true},
		{"go test file colocated", "pkg/a/a_ext_test.go", true},
		{"pytest conftest", "superset/conftest.py", true},
		{"jest dotted suffix", "src/utils/format.test.ts", true},
		{"vitest dotted spec", "src/utils/format.spec.tsx", true},
		// Genuinely outside any test segment on python/superset — the one case the
		// dotted rule uniquely earns.
		{"cypress e2e outside a test tree", "superset-frontend/cypress-base/cypress/e2e/explore/chart.test.js", true},

		// Colocated conventions that are safe on their own.
		{"rspec suffix", "app/models/user_spec.rb", true},
		{"pytest suffix colocated", "pkg/foo_test.py", true},

		// Real Ruby/Swift/Kotlin tests are claimed by their DIRECTORY, not by their
		// filename — their basename rules were dropped as pure false-positive
		// generators. These still classify, via the segment rules.
		{"rspec in spec tree", "spec/models/user_spec.rb", true},
		{"minitest in test tree", "test/models/user_test.rb", true},
		{"pytest prefix in tests tree", "tests/unit/test_date_parser.py", true},
		{"swift test in test target", "Tests/CoreKitTests/CoreKitTests.swift", true},
		{"kotlin test in source set", "app/src/test/java/de/example/FooTest.kt", true},

		// --- Production code that MUST NOT be classified as test ---
		//
		// Every one of these is a real corpus file that a filename-suffix rule
		// wrongly claimed. This is the fixed/28 defect class: a name that merely
		// LOOKS like a test. Suppressing production code from the analysis is
		// silent, so the predicate errs toward keeping it.
		{"swift production A/B feature", "Sources/ExampleFoundation/Components/FeatureRolloutABTest/FeatureRolloutABTest.swift", false},
		{"swift production feature ending in Test", "Sources/ExampleFoundation/Components/RecommendationTest/RecommendationTest.swift", false},
		{"swift production tracking ext", "Sources/ExampleFoundation/Components/Tracking/Tracking+ExperimentABTest.swift", false},
		{"swift production ending in Tests", "Sources/ExampleFoundation/Components/Settings/Settings.ExperimentTests.swift", false},
		{"kotlin production model ending in Test", "api/src/main/java/de/example/app/api/model/ABTest.kt", false},
		{"ruby production job named test_job", "app/jobs/test_job.rb", false},
		{"ruby production job named test_fail_job", "app/jobs/test_fail_job.rb", false},
		// The fixed/28 file itself: it genuinely ends in `_test.rb`, which is why a
		// bare suffix rule can never be safe for Ruby.
		{"ruby production ab-test job (fixed/28)", "app/jobs/notification/finish_experiment_ab_test.rb", false},
		// Production CLI/commands on python/superset that "test a DB connection".
		{"python production cli test_db", "superset/cli/test_db.py", false},
		{"python production test_connection command", "superset/commands/database/test_connection.py", false},

		// The v60 hardening cases from ModuleRoleForPath: a genuine single-token
		// name must never misfire.
		{"latest is not a test dir", "app/services/latest/fetcher.rb", false},
		{"contest is not a test dir", "app/models/contest/entry.rb", false},
		{"abtest is not a test dir", "app/features/abtest/toggle.rb", false},
		{"protest is not a test dir", "src/protest/handler.go", false},

		// Ordinary production paths.
		{"plain source", "Sources/ExampleCore/APIService.swift", false},
		{"plain go", "internal/facts/graph.go", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsTestPath(tt.path); got != tt.want {
				t.Errorf("IsTestPath(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

// A Gradle test source set is named `<target>Test` directly under src/, for every
// Kotlin Multiplatform target and the Android plugin's host and device tests. A
// production directory that only ends in Test, or a Test directory elsewhere, is not
// one.
func TestIsTestSourceSet(t *testing.T) {
	for path, want := range map[string]bool{
		"okhttp/src/androidHostTest/kotlin/okhttp3/A.kt": true,
		"lib/src/androidDeviceTest/kotlin/A.kt":          true,
		"lib/src/jsTest/kotlin/A.kt":                     true,
		"lib/src/linuxX64Test/kotlin/A.kt":               true,
		"lib/src/commonMain/kotlin/A.kt":                 false,
		"lib/src/main/kotlin/com/acme/abTest/A.kt":       false,
		"lib/src/main/kotlin/com/acme/loadTest/A.kt":     false,
		"lib/src/Test/A.kt":                              false,
		"lib/src/my-Test/A.kt":                           false,
		"loadTest/src/main/kotlin/A.kt":                  false,
	} {
		if got := IsTestPath(path); got != want {
			t.Errorf("IsTestPath(%q) = %v, want %v", path, got, want)
		}
	}
	if got := ModuleRoleForPath("okhttp/src/androidHostTest/kotlin/okhttp3"); got != ModuleRoleTest {
		t.Errorf("ModuleRoleForPath(androidHostTest) = %q, want %q", got, ModuleRoleTest)
	}
}

// A module named for test support, by a test word and a support word, is test code;
// a test word alone, or a support word alone, is not enough for IsTestPath.
func TestIsTestSupportModule(t *testing.T) {
	for path, want := range map[string]bool{
		"okhttp-testing-support/src/main/kotlin/A.kt": true,
		"packages/core-test-utils/src/a.ts":           true,
		"dubbo-test/dubbo-test-common/src/A.java":     true,
		"crates/crates_io_test_utils/src/lib.rs":      true,
		"features/ab-testing/src/A.kt":                false,
		"libs/shared-utils/src/a.ts":                  false,
		"tools/latest-common/a.go":                    false,
	} {
		if got := IsTestPath(path); got != want {
			t.Errorf("IsTestPath(%q) = %v, want %v", path, got, want)
		}
	}
	if got := ModuleRoleForPath("okhttp-testing-support/src/main/kotlin/okhttp3"); got != ModuleRoleTest {
		t.Errorf("ModuleRoleForPath(okhttp-testing-support) = %q, want %q", got, ModuleRoleTest)
	}
}

// IsTestPath runs the test-support check on every segment of every path, so the
// check itself allocates nothing.
func TestIsTestSupportModuleAllocatesNothing(t *testing.T) {
	segs := []string{"okhttp-testing-support", "drivers", "net_device_utils", "Core-Test-Utils"}
	if n := testing.AllocsPerRun(100, func() {
		for _, s := range segs {
			IsTestSupportModule(s)
		}
	}); n != 0 {
		t.Errorf("IsTestSupportModule allocates %v times per run, want 0", n)
	}
	if !IsTestSupportModule("Core-Test-Utils") || IsTestSupportModule("net_device_utils") {
		t.Error("case-insensitive match lost")
	}
}
