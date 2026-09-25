package install

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func opts(t *testing.T, hooks bool) Options {
	t.Helper()
	return Options{Scope: ScopeLocal, RepoDir: t.TempDir(), HomeDir: t.TempDir(), Hooks: hooks}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(b)
}

func actionFor(rs []Result, suffix string) (Action, bool) {
	for _, r := range rs {
		if strings.HasSuffix(r.Path, suffix) {
			return r.Action, true
		}
	}
	return "", false
}

// TestInstall_WritesOnlyFilesEnolaOwnsByDefault — without --hooks nothing executable is
// configured. The instructions are inert context; the hooks run commands, and that
// difference has to be a decision the user makes rather than one they discover.
func TestInstall_WritesOnlyFilesEnolaOwnsByDefault(t *testing.T) {
	o := opts(t, false)
	if _, err := Install(o); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(o.RepoDir, ".claude", "rules", "enola.md")); err != nil {
		t.Errorf("claude rule not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(o.RepoDir, ".cursor", "rules", "enola.mdc")); err != nil {
		t.Errorf("cursor rule not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(o.RepoDir, ".claude", "settings.json")); !os.IsNotExist(err) {
		t.Errorf("settings.json was written without --hooks (err=%v); hooks must be opt-in", err)
	}
}

// TestInstall_NeverCreatesAgentsMd — a tool that drops a new AGENTS.md into someone's
// repository uninvited is a tool they remove. Claude Code does not read AGENTS.md, so
// skipping it costs nothing.
func TestInstall_NeverCreatesAgentsMd(t *testing.T) {
	o := opts(t, false)
	rs, err := Install(o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(o.RepoDir, "AGENTS.md")); !os.IsNotExist(err) {
		t.Errorf("AGENTS.md was created uninvited (err=%v)", err)
	}
	if a, ok := actionFor(rs, "AGENTS.md"); !ok || a != ActionSkipped {
		t.Errorf("AGENTS.md action = %q, want %q and an explanation", a, ActionSkipped)
	}
}

// TestInstall_PreservesHandWrittenContentInSharedFiles is the graphify failure mode:
// updating an owned section must not disturb a single byte the user wrote around it.
func TestInstall_PreservesHandWrittenContentInSharedFiles(t *testing.T) {
	o := opts(t, false)
	agents := filepath.Join(o.RepoDir, "AGENTS.md")
	original := "# Team rules\n\nRun the linter.\n\n## Notes\n\nDo not delete me.\n"
	if err := os.WriteFile(agents, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Install(o); err != nil {
		t.Fatal(err)
	}
	got := read(t, agents)
	for _, want := range []string{"# Team rules", "Run the linter.", "## Notes", "Do not delete me."} {
		if !strings.Contains(got, want) {
			t.Errorf("install destroyed hand-written content %q:\n%s", want, got)
		}
	}

	// A second install must update in place, not append a duplicate block.
	if _, err := Install(o); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(read(t, agents), beginMarker); n != 1 {
		t.Errorf("found %d enola blocks after two installs, want 1", n)
	}

	// Uninstall must restore the file exactly.
	if _, err := Uninstall(o); err != nil {
		t.Fatal(err)
	}
	if got := read(t, agents); got != original {
		t.Errorf("uninstall did not restore the file byte-for-byte:\n got %q\nwant %q", got, original)
	}
}

// TestInstall_RefusesUnbalancedMarkers — a file whose markers have been hand-edited is
// one we cannot safely interpret, and the right response to not understanding a user's
// file is to stop rather than guess where our section ends.
func TestInstall_RefusesUnbalancedMarkers(t *testing.T) {
	o := opts(t, false)
	agents := filepath.Join(o.RepoDir, "AGENTS.md")
	broken := "# Rules\n\n" + beginMarker + "\nhalf a block, no end marker\n\n## Something the user wrote\n"
	if err := os.WriteFile(agents, []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}

	rs, err := Install(o)
	if err != nil {
		t.Fatal(err)
	}
	if a, _ := actionFor(rs, "AGENTS.md"); a != ActionSkipped {
		t.Errorf("action = %q, want %q for unbalanced markers", a, ActionSkipped)
	}
	if got := read(t, agents); got != broken {
		t.Errorf("a file with unbalanced markers was modified:\n%s", got)
	}
}

// TestInstall_HooksMergeWithoutDisturbingTheUsersConfig — settings.json belongs to the
// user and very likely already holds hooks and permissions that must survive untouched.
func TestInstall_HooksMergeWithoutDisturbingTheUsersConfig(t *testing.T) {
	o := opts(t, true)
	settings := filepath.Join(o.RepoDir, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o755); err != nil {
		t.Fatal(err)
	}
	existing := `{
  "permissions": {"allow": ["Bash(npm test)"]},
  "hooks": {
    "Stop": [{"type": "command", "command": "my-own-notify.sh"}],
    "PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "audit.sh"}]}]
  }
}`
	if err := os.WriteFile(settings, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Install(o); err != nil {
		t.Fatal(err)
	}
	got := read(t, settings)
	for _, want := range []string{
		"my-own-notify.sh", "audit.sh", "Bash(npm test)",
		"enola hook stop", "enola hook session-start",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("after install, %q is missing:\n%s", want, got)
		}
	}
	// The user's own flat Stop entry is preserved as written. It does not fire — no
	// flat entry does — but it is their file, and rewriting somebody's config into a
	// shape they did not choose is not a repair we get to make silently.
	var parsed map[string]any
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatal(err)
	}
	hookMap, _ := parsed["hooks"].(map[string]any)
	stop, _ := hookMap["Stop"].([]any)
	foundUsersEntry := false
	for _, e := range stop {
		m, ok := e.(map[string]any)
		if ok && m["command"] == "my-own-notify.sh" {
			foundUsersEntry = true
		}
	}
	if !foundUsersEntry {
		t.Errorf("the user's own flat Stop entry was not preserved verbatim:\n%s", got)
	}

	// Uninstall removes exactly enola's entries and nothing else.
	if _, err := Uninstall(o); err != nil {
		t.Fatal(err)
	}
	after := read(t, settings)
	for _, want := range []string{"my-own-notify.sh", "audit.sh", "Bash(npm test)"} {
		if !strings.Contains(after, want) {
			t.Errorf("uninstall removed the user's own config %q:\n%s", want, after)
		}
	}
	if strings.Contains(after, "enola hook") {
		t.Errorf("uninstall left enola hooks behind:\n%s", after)
	}
	// SessionStart existed only for enola, so the key should be gone entirely.
	var doc map[string]any
	if err := json.Unmarshal([]byte(after), &doc); err != nil {
		t.Fatal(err)
	}
	hooks, _ := doc["hooks"].(map[string]any)
	if _, present := hooks["SessionStart"]; present {
		t.Errorf("an empty SessionStart key was left behind:\n%s", after)
	}
}

// TestHookSummary_DescribesExactlyWhatIsInstalled guards against the tool announcing a
// mechanism it does not configure.
//
// It already happened once: SessionStart was pulled from the installer, and the CLI went
// on telling users "a baseline is pinned at session start". A config writer that
// misdescribes itself is worse than one that says nothing, because the user then
// troubleshoots behaviour that was never installed.
func TestHookSummary_DescribesExactlyWhatIsInstalled(t *testing.T) {
	o := opts(t, true)

	// Scoped to the targets that actually carry the session hooks: with every target
	// selected the summary also describes opencode's plugin, which is a different
	// mechanism and is asserted separately below.
	sessionOnly := o
	sessionOnly.Targets = []string{"claude"}
	if len(HookSummary(sessionOnly)) != len(installedHooks) {
		t.Fatalf("HookSummary has %d entries for %d installed hooks", len(HookSummary(sessionOnly)), len(installedHooks))
	}

	// The same rule one target further along: opencode gets a plugin and no session
	// hooks, so it must be described as neither more nor less than that.
	opencodeOnly := o
	opencodeOnly.Targets = []string{"opencode"}
	if got := HookSummary(opencodeOnly); len(got) != 1 {
		t.Fatalf("opencode alone should be described by exactly its plugin, got %d entries: %v", len(got), got)
	}
	if InstallsSessionHooks(opencodeOnly) {
		t.Error("opencode is reported as installing session hooks, which it cannot")
	}
	if got := HookSummary(o); len(got) != len(installedHooks)+1 {
		t.Errorf("a full install should describe every hook and the plugin, got %d entries", len(got))
	}

	if _, err := Install(o); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(read(t, filepath.Join(o.RepoDir, ".claude", "settings.json"))), &doc); err != nil {
		t.Fatal(err)
	}
	hooks, _ := doc["hooks"].(map[string]any)

	// Every hook described is present in the written config...
	for _, h := range installedHooks {
		if _, ok := hooks[h.Event]; !ok {
			t.Errorf("%s is described but was not written to settings.json", h.Event)
		}
		if h.Description == "" {
			t.Errorf("%s is installed with no description for the user", h.Event)
		}
	}
	// ...and nothing was written that is not described.
	for event := range hooks {
		described := false
		for _, h := range installedHooks {
			if h.Event == event {
				described = true
			}
		}
		if !described {
			t.Errorf("%s was written to settings.json but is not described to the user", event)
		}
	}
}

// TestInstall_CopilotOwnsItsFileAndAppliesUnconditionally — `.github/instructions/*.md`
// is chosen over `.github/copilot-instructions.md` for the same reason `.claude/rules/`
// was: it is a file enola owns outright, so it can never damage one the user maintains.
//
// The frontmatter is not decoration. `applyTo` is required for a file in that directory,
// and without it the file governs nothing — it exists, it looks installed, and it never
// applies.
func TestInstall_CopilotOwnsItsFileAndAppliesUnconditionally(t *testing.T) {
	o := opts(t, false)
	if _, err := Install(o); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(o.RepoDir, ".github", "instructions", "enola.instructions.md")
	got := read(t, path)

	if !strings.HasPrefix(got, "---\napplyTo: \"**\"\n---\n") {
		t.Errorf("copilot instructions must open with applyTo:\"**\" frontmatter, got:\n%s", got[:min(len(got), 120)])
	}
	// It must not also write the shared copilot-instructions.md, which users hand-maintain.
	if _, err := os.Stat(filepath.Join(o.RepoDir, ".github", "copilot-instructions.md")); !os.IsNotExist(err) {
		t.Errorf("enola must not write the shared copilot-instructions.md (err=%v)", err)
	}
}

// TestInstall_CodexAndPiAreNotDuplicatedLocally — both read the repo-root AGENTS.md the
// `agents` target already writes. Writing a second repo-local instruction file for them
// would put the same instruction into the same context window twice, for no gain. Pi
// still gets its extension, which is a tool bridge, not a second copy of the text.
func TestInstall_CodexAndPiAreNotDuplicatedLocally(t *testing.T) {
	o := opts(t, false)
	if err := os.WriteFile(filepath.Join(o.RepoDir, "AGENTS.md"), []byte("# Team\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rs, err := Install(o)
	if err != nil {
		t.Fatal(err)
	}
	if a, ok := actionFor(rs, "(codex)"); !ok || a != ActionSkipped {
		t.Errorf("(codex) local action = %q, want %q with an explanation", a, ActionSkipped)
	}
	if a, ok := actionFor(rs, "enola.js"); !ok || a != ActionCreated {
		t.Errorf("pi extension local action = %q, want %q", a, ActionCreated)
	}
	// Exactly one repo-local block, in AGENTS.md.
	if n := strings.Count(read(t, filepath.Join(o.RepoDir, "AGENTS.md")), beginMarker); n != 1 {
		t.Errorf("AGENTS.md has %d enola blocks, want 1", n)
	}
}

// TestInstall_GlobalCodexAndPiOnlyWhenInstalled — the tool's config directory is the
// evidence it exists. Creating ~/.codex for someone who does not use Codex would be
// littering in a home directory to no purpose.
func TestInstall_GlobalCodexAndPiOnlyWhenInstalled(t *testing.T) {
	o := opts(t, false)
	o.Scope = ScopeGlobal
	// Codex present, Pi absent.
	if err := os.MkdirAll(filepath.Join(o.HomeDir, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}

	rs, err := Install(o)
	if err != nil {
		t.Fatal(err)
	}

	codex := filepath.Join(o.HomeDir, ".codex", "AGENTS.md")
	if _, err := os.Stat(codex); err != nil {
		t.Errorf("codex is installed, so its user-level AGENTS.md should be written: %v", err)
	}
	if got := read(t, codex); !strings.Contains(got, beginMarker) {
		t.Errorf("codex AGENTS.md missing enola's block:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(o.HomeDir, ".pi")); !os.IsNotExist(err) {
		t.Errorf("~/.pi was created for a user who does not have Pi (err=%v)", err)
	}
	if a, _ := actionFor(rs, "AGENTS.md"); a == "" {
		t.Error("expected a result row for the codex user-level file")
	}

	// Uninstall must strip the block and remove a file that then holds nothing else.
	if _, err := Uninstall(o); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(codex); !os.IsNotExist(err) {
		t.Errorf("uninstall left behind a file enola created (err=%v)", err)
	}
}

// TestInstall_UnparseableJSONIsBackedUpNotClobbered — a file we cannot read is one we
// cannot safely rewrite, and it may be the user's only copy.
func TestInstall_UnparseableJSONIsBackedUpNotClobbered(t *testing.T) {
	o := opts(t, true)
	settings := filepath.Join(o.RepoDir, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o755); err != nil {
		t.Fatal(err)
	}
	garbage := "{ this is not json at all"
	if err := os.WriteFile(settings, []byte(garbage), 0o644); err != nil {
		t.Fatal(err)
	}

	rs, err := Install(o)
	if err != nil {
		t.Fatal(err)
	}
	if a, _ := actionFor(rs, "settings.json"); a != ActionSkipped {
		t.Errorf("action = %q, want %q for unparseable JSON", a, ActionSkipped)
	}
	if got := read(t, settings); got != garbage {
		t.Errorf("unparseable settings.json was overwritten:\n%s", got)
	}
	if _, err := os.Stat(settings + ".enola-backup"); err != nil {
		t.Errorf("no backup was taken of the unparseable file: %v", err)
	}
}

// TestInstall_IsIdempotent — re-running must report unchanged rather than churn files.
func TestInstall_IsIdempotent(t *testing.T) {
	o := opts(t, true)
	if _, err := Install(o); err != nil {
		t.Fatal(err)
	}
	rs, err := Install(o)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rs {
		if r.Action != ActionUnchanged && r.Action != ActionSkipped {
			t.Errorf("second install reported %s for %s, want unchanged", r.Action, r.Path)
		}
	}
}

// TestInstall_DryRunWritesNothing — the preview must be a preview.
func TestInstall_DryRunWritesNothing(t *testing.T) {
	o := opts(t, true)
	o.DryRun = true
	rs, err := Install(o)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) == 0 {
		t.Fatal("dry run reported no planned changes")
	}
	entries, err := os.ReadDir(o.RepoDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("dry run wrote %d entries into the repo", len(entries))
	}
}

// TestUninstall_LeavesNoResidue — an installer that leaves empty files and directories
// behind advertises a tool that is no longer there.
//
// The hooks axis is the point. This test used to run without --hooks, so it never reached
// the two JSON configs, and uninstall shipped leaving `.claude/settings.json` and
// `.codex/hooks.json` behind as `{}` scaffolds — with their directories — while the test
// named exactly the property they broke.
func TestUninstall_LeavesNoResidue(t *testing.T) {
	for _, hooks := range []bool{false, true} {
		t.Run(fmt.Sprintf("hooks=%v", hooks), func(t *testing.T) {
			o := opts(t, hooks)
			if _, err := Install(o); err != nil {
				t.Fatal(err)
			}
			if _, err := Uninstall(o); err != nil {
				t.Fatal(err)
			}
			for _, path := range residue(t, o.RepoDir) {
				t.Errorf("uninstall left %q behind", path)
			}
		})
	}
}

// TestUninstall_GlobalLeavesNoResidueBeyondTheToolsOwnRoots — the same property in the
// home directory, where the bound on reversal is different: `~/.codex` and `~/.pi` are
// evidence those tools are installed, enola never creates them, and it gates its own
// global install on finding them. It may empty them; it may not remove them.
func TestUninstall_GlobalLeavesNoResidueBeyondTheToolsOwnRoots(t *testing.T) {
	o := opts(t, true)
	o.Scope = ScopeGlobal
	for _, d := range []string{".codex", filepath.Join(".pi", "agent")} {
		if err := os.MkdirAll(filepath.Join(o.HomeDir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := Install(o); err != nil {
		t.Fatal(err)
	}
	if _, err := Uninstall(o); err != nil {
		t.Fatal(err)
	}

	for _, path := range residue(t, o.HomeDir) {
		if path == ".codex" || path == ".pi" {
			continue
		}
		t.Errorf("uninstall left %q behind in the home directory", path)
	}
	for _, root := range []string{".codex", ".pi"} {
		if _, err := os.Stat(filepath.Join(o.HomeDir, root)); err != nil {
			t.Errorf("uninstall removed %s, a directory enola never created: %v", root, err)
		}
	}
}

// residue lists every path left under root, relative and slash-separated.
func residue(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	if err := filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
		if err != nil || path == root {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestUninstall_LeavesAnUntouchedEmptyConfigAlone — the removal of an emptied config is
// bounded by having emptied it. A settings.json a user happens to have left at `{}` is
// not enola's residue, and deleting it would be reversing something it never did.
func TestUninstall_LeavesAnUntouchedEmptyConfigAlone(t *testing.T) {
	o := opts(t, false)
	settings := filepath.Join(o.RepoDir, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settings, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	rs, err := Uninstall(o)
	if err != nil {
		t.Fatal(err)
	}
	if a, _ := actionFor(rs, "settings.json"); a != ActionUnchanged {
		t.Errorf("action = %q, want %q for a config enola never wrote to", a, ActionUnchanged)
	}
	if got := read(t, settings); got != "{}\n" {
		t.Errorf("a hand-written empty settings.json was rewritten: %q", got)
	}
}

// TestUninstall_KeepsAConfigStillHoldingTheUsersOwnSettings — the file is removed because
// it is empty, never because enola is leaving.
func TestUninstall_KeepsAConfigStillHoldingTheUsersOwnSettings(t *testing.T) {
	o := opts(t, true)
	settings := filepath.Join(o.RepoDir, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settings, []byte(`{"permissions": {"allow": ["Bash(npm test)"]}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Install(o); err != nil {
		t.Fatal(err)
	}
	if _, err := Uninstall(o); err != nil {
		t.Fatal(err)
	}
	got := read(t, settings)
	if !strings.Contains(got, "Bash(npm test)") {
		t.Errorf("uninstall removed a settings.json holding the user's own config:\n%s", got)
	}
}

// TestUninstall_DryRunRemovesNothing — the preview must be a preview on the way out too,
// and it must still report the removals it would make, or the confirmation prompt would
// describe an uninstall that does less than it does.
func TestUninstall_DryRunRemovesNothing(t *testing.T) {
	o := opts(t, true)
	if _, err := Install(o); err != nil {
		t.Fatal(err)
	}
	before := residue(t, o.RepoDir)

	o.DryRun = true
	rs, err := Uninstall(o)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"settings.json", filepath.Join(".codex", "hooks.json")} {
		if a, ok := actionFor(rs, name); !ok || a != ActionRemoved {
			t.Errorf("%s action = %q, want %q", name, a, ActionRemoved)
		}
	}
	if after := residue(t, o.RepoDir); !slices.Equal(before, after) {
		t.Errorf("dry-run uninstall changed the tree:\nbefore %v\nafter  %v", before, after)
	}
}

// TestInstall_GlobalScopeSkipsPerRepoTargets — Cursor has no user-level rules directory
// and AGENTS.md is a per-repository convention. Inventing a path for either would be the
// silent failure this design exists to avoid: a write that lands where nothing reads it.
func TestInstall_GlobalScopeSkipsPerRepoTargets(t *testing.T) {
	o := opts(t, false)
	o.Scope = ScopeGlobal

	rs, err := Install(o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(o.HomeDir, ".claude", "rules", "enola.md")); err != nil {
		t.Errorf("global claude rule not written: %v", err)
	}
	for _, target := range []string{"(cursor)", "(agents)"} {
		a, ok := actionFor(rs, target)
		if !ok || a != ActionSkipped {
			t.Errorf("%s action = %q, want %q with a reason", target, a, ActionSkipped)
		}
	}
	entries, _ := os.ReadDir(o.RepoDir)
	if len(entries) != 0 {
		t.Errorf("global install wrote %d entries into the repository", len(entries))
	}
}

// TestInstall_HooksNoteOnlyWhenHooksInstalled — the instructions must not describe a
// mechanism that is not there, or the agent is told about behaviour that never happens.
func TestInstall_HooksNoteOnlyWhenHooksInstalled(t *testing.T) {
	without := opts(t, false)
	if _, err := Install(without); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(without.RepoDir, ".claude", "rules", "enola.md")); strings.Contains(got, "hook is installed") {
		t.Error("the rule describes hooks that were not installed")
	}

	with := opts(t, true)
	if _, err := Install(with); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(with.RepoDir, ".claude", "rules", "enola.md")); !strings.Contains(got, "hook is installed") {
		t.Error("the rule does not mention the hooks that were installed")
	}
}
