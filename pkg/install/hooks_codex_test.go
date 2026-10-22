package install

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// codexHooksAfter runs an install (or uninstall) and returns the parsed hooks map from
// .codex/hooks.json, or nil if the file was not written.
func codexHooksAfter(t *testing.T, o Options, remove bool) map[string]any {
	t.Helper()
	var err error
	if remove {
		_, err = Uninstall(o)
	} else {
		_, err = Install(o)
	}
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(codexDir(o), "hooks.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("reading hooks.json: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("hooks.json is not valid JSON: %v\n%s", err, raw)
	}
	hooks, _ := doc["hooks"].(map[string]any)
	return hooks
}

// Without --hooks, Codex gets no hooks.json at all — same rule as every other target.
func TestInstall_CodexHooksNotWrittenWithoutFlag(t *testing.T) {
	o := opts(t, false)
	if hooks := codexHooksAfter(t, o, false); hooks != nil {
		t.Errorf("hooks.json written without --hooks: %#v", hooks)
	}
}

// Locally, hooks.json is its own file and is written unconditionally, like Cursor's
// rule file — .codex/ existing beforehand is not required.
func TestInstall_CodexHooksWrittenLocally(t *testing.T) {
	o := opts(t, true)
	hooks := codexHooksAfter(t, o, false)

	for _, h := range installedHooks {
		list, ok := hooks[h.Event].([]any)
		if h.ClaudeOnly {
			// A hook about how Claude Code lists MCP tools has no place in Codex's file.
			if ok {
				t.Errorf("%s: a Claude-only hook was written for Codex:\n%#v", h.Event, hooks)
			}
			continue
		}
		if !ok || len(list) == 0 {
			t.Errorf("%s: not written:\n%#v", h.Event, hooks)
			continue
		}
		entries, _ := enolaEntries(t, hooks, h.Event)
		if len(entries) != 1 {
			t.Errorf("%s: got %d enola entries, want 1", h.Event, len(entries))
			continue
		}
		entry := entries[0]
		if async, ok := entry["async"].(bool); !ok || async {
			t.Errorf("%s: async = %#v, want required value false", h.Event, entry["async"])
		}
		if entry["timeoutSec"] != float64(hookTimeoutSeconds) {
			t.Errorf("%s: timeoutSec = %#v, want %d", h.Event, entry["timeoutSec"], hookTimeoutSeconds)
		}
		if _, legacy := entry["timeout"]; legacy {
			t.Errorf("%s: contains Claude-only timeout field: %#v", h.Event, entry)
		}
	}
}

// Globally, hooks.json follows the same rule as ~/.codex/AGENTS.md: only written when
// ~/.codex already exists, so enola never creates it for a non-Codex user.
func TestInstall_CodexHooksGlobalOnlyWhenInstalled(t *testing.T) {
	o := opts(t, true)
	o.Scope = ScopeGlobal

	if hooks := codexHooksAfter(t, o, false); hooks != nil {
		t.Errorf("hooks.json written under a home directory with no ~/.codex: %#v", hooks)
	}

	if err := os.MkdirAll(filepath.Join(o.HomeDir, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	hooks := codexHooksAfter(t, o, false)
	if hooks[installedHooks[0].Event] == nil {
		t.Errorf("hooks.json not written under an existing ~/.codex: %#v", hooks)
	}
}

// Uninstall must remove only enola's entries, leaving a hand-written hook in the same
// file untouched.
func TestInstall_CodexHooksUninstallPreservesUserEntries(t *testing.T) {
	o := opts(t, true)
	path := filepath.Join(codexDir(o), "hooks.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	userHooks := `{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "notify-send done"}]}]}}`
	if err := os.WriteFile(path, []byte(userHooks), 0o644); err != nil {
		t.Fatal(err)
	}

	codexHooksAfter(t, o, false)
	hooks := codexHooksAfter(t, o, true)

	entries, _ := enolaEntries(t, hooks, "Stop")
	if len(entries) != 0 {
		t.Errorf("uninstall left enola entries behind: %#v", entries)
	}
	list, _ := hooks["Stop"].([]any)
	if len(list) == 0 {
		t.Error("uninstall removed the user's own Stop hook")
	}
}
