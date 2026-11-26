package config

import "testing"

// TestMochaTestDirectoryIsTests pins Mocha's default layout, a test/ directory of
// JavaScript with no .test. or .spec. in the name (QUnit and the Ember plugins of a
// Rails app use the same one), and the same directory of TypeScript: fixtures,
// mocks and setup files an e2e suite imports, and the test trees a codebase keeps
// beside each layer. A runtime's own node:test polyfill and a form widget named
// `test` live under test/ as well and are test code by the same rule, as
// facts.IsTestPath already classifies them. Not pytest's *_test.py on its own:
// outside a test tree that name is also a production module about testing a
// connection.
func TestMochaTestDirectoryIsTests(t *testing.T) {
	testWalkFates(t, map[string]string{
		"test/index.js":       "test refs",
		"test/smoke-test.mjs": "test refs",
		"test/setup.cjs":      "test refs",
		"plugins/poll/test/javascripts/acceptance/poll-results-test.js": "test refs",
		"packages/core/test/render/render_spec.js":                      "test refs",
		"test/fixtures/repository/users.repository.fixture.ts":          "test refs",
		"src/vs/base/test/common/utils.ts":                              "test refs",
		"packages/core/test/render3/view_fixture.tsx":                   "test refs",
		"test/helpers/render.jsx":                                       "test refs",
		"ext/node/polyfills/test/reporters.ts":                          "test refs",
		"src/app/shared/json-schema/test/test.widget.ts":                "test refs",
		"src/testing/harness.ts":                                        "indexed",
		"src/test.ts":                                                   "indexed",
		"src/contest/entry.ts":                                          "indexed",
		"src/testing/harness.js":                                        "indexed",
		"clients/adapters/nuxt/test/fixtures/basic/app.vue":             "test refs",
		"test/fixtures/Widget.svelte":                                   "test refs",
		"src/test.vue":                                                  "indexed",
		"src/testing/Harness.svelte":                                    "indexed",
		"src/test.js":                                                   "indexed",
		"src/airflow/models/connection_test.py":                         "indexed",
	})
}

// TestTestsDirectoryIsTests pins the same rule for a tests/ directory, the name
// Playwright, Vitest and Deno suites use: page objects, fixtures, setup files and
// Deno's *_test.ts carry no reserved suffix there. A file merely named tests, or a
// directory that only contains the word, stays indexed.
func TestTestsDirectoryIsTests(t *testing.T) {
	testWalkFates(t, map[string]string{
		"tests/e2e/helpers.ts":                       "test refs",
		"tests/unit/test_util.ts":                    "test refs",
		"packages/tests/src/api/videos.ts":           "test refs",
		"apps/gauzy-e2e/tests/pages/login.page.tsx":  "test refs",
		"frontend/discourse/tests/helpers/qunit.js":  "test refs",
		"tests/fixtures/app.mjs":                     "test refs",
		"tests/setup.cjs":                            "test refs",
		"tests/components/Button.jsx":                "test refs",
		"src/tests.ts":                               "indexed",
		"src/e2e-tests/helpers.ts":                   "indexed",
		"src/contests/entry.ts":                      "indexed",
		"src/tests.js":                               "indexed",
		"tests/specs/fmt/component/formatted.svelte": "test refs",
		"tests/components/Button.vue":                "test refs",
		"src/tests.vue":                              "indexed",
		"src/contests/Entry.svelte":                  "indexed",
	})
}
