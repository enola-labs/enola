package tsextractor

import "testing"

func TestIsTSTestFile(t *testing.T) {
	cases := map[string]bool{
		"src/app.test.ts":                         true,
		"src/app.spec.js":                         true,
		"tests/unit/models/user-test.ts":          true,
		"src/ab-test.ts":                          false,
		"src/__tests__/helpers.ts":                true,
		"src/utils/__tests__/fixtures/mod.js":     true,
		"integration-tests/__tests__/admin/a.mjs": true,
		"src/__tests__.ts":                        false,
		"src/tests__/a.ts":                        false,
		"src/app.ts":                              false,
		"test/index.js":                           true,
		"plugins/poll/test/javascripts/a-test.js": true,
		"test/smoke.mjs":                          true,
		"ext/node/polyfills/test/reporters.ts":    true,
		"test/fixtures/users.fixture.ts":          true,
		"src/vs/base/test/common/utils.tsx":       true,
		"test/helpers/render.jsx":                 true,
		"src/test.ts":                             false,
		"src/testing/harness.ts":                  false,
		"tests/e2e/helpers.ts":                    true,
		"tests/unit/test_util.ts":                 true,
		"packages/tests/src/api/videos.ts":        true,
		"apps/web/src/app/core/tests/index.ts":    true,
		"tests/fixtures/app.mjs":                  true,
		"src/tests.ts":                            false,
		"src/e2e-tests/helpers.ts":                false,
		"src/test.js":                             false,
		"src/testing/harness.js":                  false,
		"test/fixtures/basic/app.vue":             true,
		"tests/specs/fmt/badly_formatted.svelte":  true,
		"src/test.vue":                            false,
		"src/components/Test.svelte":              false,
	}
	for path, want := range cases {
		if got := isTSTestFile(path); got != want {
			t.Errorf("isTSTestFile(%q) = %v, want %v", path, got, want)
		}
	}
}
