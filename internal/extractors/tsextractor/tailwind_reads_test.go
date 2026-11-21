package tsextractor

import (
	"context"
	"testing"

	"github.com/enola-labs/enola/pkg/plugin"
)

// The theme reader follows an import into a folder the walk ignores. The file it
// read is recorded, so the cache key covers its content.
func TestTailwindThemeReadsAreRecorded(t *testing.T) {
	dir := setupTSProject(t, map[string]string{
		"tailwind.config.ts": "import { theme } from \"./vendor/theme\";\nexport default { theme: { extend: theme.extend } };\n",
		"vendor/theme.ts":    "export const theme = { extend: { colors: { paper: \"#fff\" } } };\n",
	}, false)
	ctx, probes := plugin.WithDiskProbes(context.Background())
	if _, err := New().Extract(ctx, dir, []string{"tailwind.config.ts"}); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, p := range probes.Reads() {
		got[p] = true
	}
	if !got["vendor/theme.ts"] {
		t.Errorf("the off-walk theme file was read but not recorded; reads %v", probes.Reads())
	}
}
