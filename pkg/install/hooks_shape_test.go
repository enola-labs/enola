package install

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// A hook registered in the wrong shape is a config that parses cleanly, reports
// success, and never runs. `Stop` was written as a flat list of entries for exactly
// that reason — a confident comment said the two events differed, a unit test
// asserted the shape it already believed in, and nothing ever checked whether the
// agent triggered it. It did not.
//
// So these tests assert mechanics — grouping as an invariant across ALL events,
// idempotency, migration, clean uninstall — and deliberately do not try to prove
// the shape is the RIGHT one. A unit test cannot: it can only compare the output
// against the same belief that produced it. That job belongs to the end-to-end test
// in hooks_e2e_test.go, which ends a real session and looks for the verdict.

// settingsAfter runs an install (or uninstall) and returns the parsed hooks map. A
// settings.json that is gone means no hooks, which is a legitimate outcome of an
// uninstall: a file left holding nothing is removed rather than emptied.
func settingsAfter(t *testing.T, o Options, remove bool) map[string]any {
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
	raw, err := os.ReadFile(filepath.Join(o.RepoDir, ".claude", "settings.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("reading settings.json: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("settings.json is not valid JSON: %v\n%s", err, raw)
	}
	hooks, _ := doc["hooks"].(map[string]any)
	return hooks
}

// enolaEntries returns every entry enola owns under an event, and the number of
// groups it had to look inside — so a test can tell one group holding two entries
// from two groups holding one each.
func enolaEntries(t *testing.T, hooks map[string]any, event string) (entries []map[string]any, groups int) {
	t.Helper()
	list, _ := hooks[event].([]any)
	for _, g := range list {
		group, ok := g.(map[string]any)
		if !ok {
			continue
		}
		groups++
		inner, _ := group["hooks"].([]any)
		for _, e := range inner {
			if entry, ok := e.(map[string]any); ok && isEnolaEntry(entry) {
				entries = append(entries, entry)
			}
		}
	}
	return entries, groups
}

// TestInstall_EveryEventIsMatcherGrouped is the invariant, stated once for all
// events rather than per-event.
//
// The defect was not a typo — it was a per-event belief about shape, held in a
// comment and in a test, that turned out to be wrong for one of the two. Asserting
// "grouped, always" is a claim a future event cannot quietly opt out of.
func TestInstall_EveryEventIsMatcherGrouped(t *testing.T) {
	hooks := settingsAfter(t, opts(t, true), false)

	for _, h := range installedHooks {
		list, ok := hooks[h.Event].([]any)
		if !ok || len(list) == 0 {
			t.Errorf("%s: not written at all:\n%#v", h.Event, hooks)
			continue
		}
		for i, g := range list {
			group, ok := g.(map[string]any)
			if !ok {
				t.Errorf("%s[%d] is not an object: %#v", h.Event, i, g)
				continue
			}
			inner, ok := group["hooks"].([]any)
			if !ok || len(inner) == 0 {
				t.Errorf("%s[%d] has no nested hooks array — this is the shape that "+
					"parses and never fires: %#v", h.Event, i, group)
				continue
			}
			for j, e := range inner {
				entry, ok := e.(map[string]any)
				if !ok || entry["type"] != "command" || entry["command"] == "" {
					t.Errorf("%s[%d].hooks[%d] is not a command entry: %#v", h.Event, i, j, e)
				}
			}
		}
	}
}

// The matcher is what tells the two events apart, and only that. An event with no
// matcher omits the key rather than writing an empty one, because a group keyed on
// "" is not the same document as a group with no key.
func TestInstall_MatcherPresentOnlyWhereTheSpecHasOne(t *testing.T) {
	o := opts(t, true)
	o.FolderOfRepos = true // every hook enola has, PreToolUse's two included
	hooks := settingsAfter(t, o, false)

	want := map[string]map[string]bool{}
	for _, h := range installedHooks {
		if want[h.Event] == nil {
			want[h.Event] = map[string]bool{}
		}
		want[h.Event][h.Matcher] = true
	}
	for event, matchers := range want {
		list, _ := hooks[event].([]any)
		if len(list) != len(matchers) {
			t.Errorf("%s has %d groups, want %d", event, len(list), len(matchers))
		}
		for _, g := range list {
			group, _ := g.(map[string]any)
			m, present := group["matcher"]
			s, _ := m.(string)
			switch {
			case matchers[""] && len(matchers) == 1 && present:
				t.Errorf("%s has no matcher in its spec but the group carries %#v", event, m)
			case !matchers[s]:
				t.Errorf("%s carries a group with matcher %#v no spec has", event, m)
			}
		}
	}
}

// A second install must update the group already there, not append another one.
// This is where an empty matcher is easy to get wrong: comparing group["matcher"]
// to "" never matches a group that omits the key (nil != ""), so every run would
// add a duplicate and every session would run the hook one more time.
func TestInstall_HooksDoNotAccumulateOnReinstall(t *testing.T) {
	o := opts(t, true)
	settingsAfter(t, o, false)
	hooks := settingsAfter(t, o, false)

	for _, h := range installedHooks {
		entries, groups := enolaEntries(t, hooks, h.Event)
		if len(entries) != 1 {
			t.Errorf("%s: %d enola entries after two installs, want 1", h.Event, len(entries))
		}
		if groups != 1 {
			t.Errorf("%s: %d groups after two installs, want 1", h.Event, groups)
		}
	}
}

// legacyFlatSettings is what `enola install --hooks` wrote before this was fixed:
// SessionStart grouped, Stop flat. Every existing installation looks like this.
const legacyFlatSettings = `{
  "hooks": {
    "SessionStart": [{"matcher": "startup|resume", "hooks": [
      {"type": "command", "command": "enola hook session-start", "source": "enola"}]}],
    "Stop": [{"type": "command", "command": "enola hook stop", "source": "enola"}]
  }
}`

func writeLegacySettings(t *testing.T, o Options) string {
	t.Helper()
	path := filepath.Join(o.RepoDir, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(legacyFlatSettings), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// Re-installing over the broken shape has to REPLACE it. Fed to the group merger
// unguarded, a flat entry is a map without a `hooks` key: it would be treated as a
// group, have a hooks array grafted onto a command entry, and produce a hybrid that
// parses and does nothing — the original defect, preserved through its own fix.
func TestInstall_MigratesTheLegacyFlatStopEntry(t *testing.T) {
	o := opts(t, true)
	writeLegacySettings(t, o)

	hooks := settingsAfter(t, o, false)
	entries, groups := enolaEntries(t, hooks, "Stop")
	if len(entries) != 1 || groups != 1 {
		t.Fatalf("Stop: %d enola entries in %d groups after upgrading, want 1 in 1:\n%#v",
			len(entries), groups, hooks["Stop"])
	}
	list, _ := hooks["Stop"].([]any)
	for _, g := range list {
		group, _ := g.(map[string]any)
		if group["type"] != nil || group["command"] != nil {
			t.Errorf("a flat entry survived as a group, or was merged into one: %#v", group)
		}
	}
}

// An upgrade is not the only path off the old shape: someone may simply uninstall.
// If uninstall only knew the new shape, the dead entry would stay in their
// settings.json forever, invoking a binary they removed.
func TestUninstall_RemovesTheLegacyFlatStopEntry(t *testing.T) {
	o := opts(t, true)
	path := writeLegacySettings(t, o)

	hooks := settingsAfter(t, o, true)
	if len(hooks) != 0 {
		t.Errorf("uninstall left hooks behind:\n%#v", hooks)
	}
	// These settings held nothing but enola's hooks, so the file goes with them rather
	// than surviving as an empty `{}` scaffold.
	if raw, err := os.ReadFile(path); err == nil {
		t.Errorf("uninstall left a settings.json holding only enola's hooks behind:\n%s", raw)
	}
}

// A flat entry that is not enola's is left exactly where it is. It does not fire,
// but diagnosing that is the user's business; deleting configuration we did not
// write, on a theory about what the harness does with it, is not a repair.
func TestInstall_LeavesForeignFlatEntriesAlone(t *testing.T) {
	o := opts(t, true)
	path := filepath.Join(o.RepoDir, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	foreign := `{"hooks": {"Stop": [{"type": "command", "command": "theirs.sh"}]}}`
	if err := os.WriteFile(path, []byte(foreign), 0o644); err != nil {
		t.Fatal(err)
	}

	hooks := settingsAfter(t, o, false)
	list, _ := hooks["Stop"].([]any)
	found := false
	for _, g := range list {
		if group, ok := g.(map[string]any); ok && group["command"] == "theirs.sh" {
			found = true
		}
	}
	if !found {
		t.Errorf("a foreign flat Stop entry was rewritten or dropped:\n%#v", list)
	}
}
