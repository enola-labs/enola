package config

import "testing"

// TestMochaTestDirectoryIsTests pins Mocha's default layout, a test/ directory of
// JavaScript with no .test. or .spec. in the name (QUnit and the Ember plugins of a
// Rails app use the same one). Only JavaScript: across the corpus every .js, .mjs
// and .cjs file under a test/ directory is a test, a helper or a fixture, while
// TypeScript there includes production code (a runtime's own node:test reporters,
// a form widget registered from its parent module). And not pytest's *_test.py on
// its own: outside a test tree that name is also a production module about testing
// a connection.
func TestMochaTestDirectoryIsTests(t *testing.T) {
	testWalkFates(t, map[string]string{
		"test/index.js":       "test refs",
		"test/smoke-test.mjs": "test refs",
		"test/setup.cjs":      "test refs",
		"plugins/poll/test/javascripts/acceptance/poll-results-test.js": "test refs",
		"packages/core/test/render/render_spec.js":                      "test refs",
		"ext/node/polyfills/test/reporters.ts":                          "indexed",
		"src/app/shared/json-schema/test/test.widget.ts":                "indexed",
		"src/testing/harness.js":                                        "indexed",
		"src/test.js":                                                   "indexed",
		"src/airflow/models/connection_test.py":                         "indexed",
	})
}
