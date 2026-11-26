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
		"tests/e2e/helpers.ts":                                          "indexed",
		"src/testing/harness.js":                                        "indexed",
		"src/test.js":                                                   "indexed",
		"src/airflow/models/connection_test.py":                         "indexed",
	})
}
