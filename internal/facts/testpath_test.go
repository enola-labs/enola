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
