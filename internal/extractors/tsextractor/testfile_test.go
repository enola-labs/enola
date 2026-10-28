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
	}
	for path, want := range cases {
		if got := isTSTestFile(path); got != want {
			t.Errorf("isTSTestFile(%q) = %v, want %v", path, got, want)
		}
	}
}
