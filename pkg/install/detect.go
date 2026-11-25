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

// InstalledTargets returns the targets that have something enola wrote in o's scope, in
// TargetNames order. Each is asked through its own uninstall, as a dry run, so this
// agrees with what `enola uninstall` would remove. The shared .agents/skills directory
// is left out: it belongs to no one target, and counting it would make every target
// that reads it look installed.
func InstalledTargets(o Options) ([]string, error) {
	o.DryRun = true
	var out []string
	for _, name := range TargetNames {
		rs, err := runTarget(name, o, true)
		if err != nil {
			return nil, err
		}
		for _, r := range rs {
			if r.Action == ActionRemoved {
				out = append(out, name)
				break
			}
		}
	}
	return out, nil
}

// Drift is what a plain `enola install` would change in o's scope, as a dry run: the
// files that differ from what this binary writes, are missing, or were retired. It
// covers only the targets already installed there, with the hooks when an earlier
// install wrote them, so a narrowed install is not reported as behind for the targets
// it left out. Nil, with installed false, when nothing of enola's is in this scope.
//
// o.HookCommand should be the running binary: a hook or extension that invokes another
// path is drift too, since `enola install` would rewrite it.
func Drift(o Options) (changed []Result, installed bool, err error) {
	targets, err := InstalledTargets(o)
	if err != nil || len(targets) == 0 {
		return nil, false, err
	}
	o.Targets, o.DryRun = targets, true
	o.Hooks = o.Hooks || HooksInstalled(o)
	rs, err := run(o, false)
	if err != nil {
		return nil, true, err
	}
	for _, r := range rs {
		switch r.Action {
		case ActionCreated, ActionUpdated, ActionRemoved:
			changed = append(changed, r)
		}
	}
	return changed, true, nil
}
