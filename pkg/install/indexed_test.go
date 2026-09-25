package install

import (
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/config"
	"github.com/enola-labs/enola/internal/pathglob"
)

// sharedFiles are the files install edits a part of. They belong to the user and stay
// indexed; everything else a local install creates is enola's own.
var sharedFiles = map[string]bool{
	"AGENTS.md":               true,
	".claude/settings.json":   true,
	".codex/hooks.json":       true,
	".opencode/opencode.json": true,
}

// TestInstall_OwnedFilesAreNeverIndexed — a file enola writes into a repository must not
// show up in that repository's architecture. It did: Pi's extension, as a .ts file,
// made a Go repository detect as TypeScript. Checked against a real install of every
// target, so a new owned file that config.InstalledFiles does not list fails here,
// and against a config with its own ignore list, which replaces the defaults.
func TestInstall_OwnedFilesAreNeverIndexed(t *testing.T) {
	o := opts(t, true)
	rs, err := Install(o)
	if err != nil {
		t.Fatal(err)
	}

	custom := &config.Config{Ignore: []string{"vendor/**"}}
	if err := custom.Normalize(); err != nil {
		t.Fatal(err)
	}
	defaults := config.Default()
	if err := defaults.Normalize(); err != nil {
		t.Fatal(err)
	}

	owned := 0
	for _, r := range rs {
		if r.Action != ActionCreated {
			continue
		}
		rel, err := filepath.Rel(o.RepoDir, r.Path)
		if err != nil {
			t.Fatal(err)
		}
		rel = filepath.ToSlash(rel)
		if sharedFiles[rel] {
			continue
		}
		owned++
		for name, cfg := range map[string]*config.Config{"default": defaults, "custom ignore list": custom} {
			if !pathglob.MatchAny(rel, cfg.Ignore) {
				t.Errorf("%s is indexed under the %s config; add it to config.InstalledFiles", rel, name)
			}
			if nested := "services/api/" + rel; !pathglob.MatchAny(nested, cfg.Ignore) {
				t.Errorf("%s is indexed under the %s config", nested, name)
			}
		}
	}
	// Only enola's own files: the user's extension beside it, and a file of the same
	// name anywhere else, are the repository's and stay in the graph.
	for _, rel := range []string{".pi/extensions/mine.js", "src/enola.js", "docs/enola.md", ".claude/rules/team.md"} {
		if pathglob.MatchAny(rel, defaults.Ignore) {
			t.Errorf("%s is ignored, but enola did not write it", rel)
		}
	}
	if owned == 0 {
		t.Fatalf("the install created no owned files, so this test checked nothing: %v", rs)
	}
}
