package tsextractor

import "testing"

// A framework alias or a baseUrl path names a repository file when exactly one known
// file ends in it; a package name never does, even when a file shares its last word.
func TestFileIndex_ResolvesInternalSpecifiersOnly(t *testing.T) {
	ix := newTSFileIndex(map[string]bool{
		"web/src/lib/modals/timezone-utils.ts": true,
		"apps/web/app/_utils.ts":               true,
		"src/components/Button/index.tsx":      true,
		"src/lib/react.ts":                     true,
		"a/shared/format.ts":                   true,
		"b/shared/format.ts":                   true,
		"web/src/lib/utils.ts":                 true,
		"server/src/utils.ts":                  true,
	})
	for spec, want := range map[string]string{
		"$lib/modals/timezone-utils": "web/src/lib/modals/timezone-utils.ts",
		"app/_utils":                 "apps/web/app/_utils.ts",
		"~/components/Button":        "src/components/Button/index.tsx",
		"react":                      "",                     // a bare package name
		"@acme/sdk/lib/react":        "",                     // a scoped package subpath
		"shared/format":              "",                     // two files end in it, equally near
		"$lib/utils":                 "web/src/lib/utils.ts", // the one nearer the importer
		"lodash/merge":               "",                     // no file ends in it
	} {
		got, ok := ix.resolve(spec, "web/src/lib/modals/Modal.svelte")
		if got != want || ok != (want != "") {
			t.Errorf("resolve(%q) = %q, %v; want %q", spec, got, ok, want)
		}
	}
}
