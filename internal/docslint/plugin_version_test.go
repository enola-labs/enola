package docslint

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TestPluginVersionIsLatestRelease ties the Claude Code plugin's version to the newest
// release in CHANGELOG.md. Claude Code keys plugin updates on that string: while it stays
// put, installed copies are never replaced, however many commits land. It sat at 0.4.21
// for three releases because nothing in the release path touches these files.
//
// The release, not main, is the target, because the plugin's skills name the flags of
// the binary users have, and that binary is the released one.
func TestPluginVersionIsLatestRelease(t *testing.T) {
	changelog, err := os.ReadFile(filepath.Join(repoRoot, "CHANGELOG.md"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^## v(\d+\.\d+\.\d+)\b`).FindSubmatch(changelog)
	if m == nil {
		t.Fatal("CHANGELOG.md: no release heading of the form \"## vX.Y.Z\"")
	}
	latest := string(m[1])

	for _, rel := range []string{
		"plugins/enola/.claude-plugin/plugin.json",
		".claude-plugin/marketplace.json",
	} {
		raw, err := os.ReadFile(filepath.Join(repoRoot, rel))
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Version string `json:"version"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		if doc.Version != latest {
			t.Errorf("%s: version %q, but the newest release in CHANGELOG.md is %q. "+
				"Installed plugins update only when this string changes, so bump it "+
				"in the release commit.", rel, doc.Version, latest)
		}
	}
}
