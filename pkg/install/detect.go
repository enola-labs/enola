package install

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// HooksInstalled reports whether an earlier install in o's scope wrote enola's hooks:
// an entry enola owns in Claude Code's settings.json or Codex's hooks.json, Pi's
// extension with its hooks switched on, or opencode's plugin.
//
// `enola install` reads it so a re-run without --hooks keeps what the first run set up.
// Without it, the plain re-run an upgrade asks for rewrote the instructions without the
// hooks note and switched Pi's hooks off, while the Claude Code entries stayed behind
// and were never updated again.
func HooksInstalled(o Options) bool {
	if jsonHasEnolaHook(filepath.Join(claudeDir(o), "settings.json")) ||
		jsonHasEnolaHook(filepath.Join(codexDir(o), "hooks.json")) {
		return true
	}
	root, _ := piRoot(o)
	if data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(PiExtensionFile))); err == nil &&
		strings.Contains(string(data), PiHooksMarker) {
		return true
	}
	if _, err := os.Stat(filepath.Join(opencodeDir(o), "plugin", "enola.js")); err == nil {
		return true
	}
	return false
}

// jsonHasEnolaHook reports whether the hooks object of the JSON file at path holds an
// entry enola owns. It walks whatever is under "hooks" rather than one shape, so it
// keeps working for both harnesses and for a shape an older enola wrote.
func jsonHasEnolaHook(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var doc struct {
		Hooks any `json:"hooks"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return false
	}
	var walk func(any) bool
	walk = func(n any) bool {
		switch t := n.(type) {
		case map[string]any:
			if isEnolaEntry(t) {
				return true
			}
			for _, c := range t {
				if walk(c) {
					return true
				}
			}
		case []any:
			for _, c := range t {
				if walk(c) {
					return true
				}
			}
		}
		return false
	}
	return walk(doc.Hooks)
}
