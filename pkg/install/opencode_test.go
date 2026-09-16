package install

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func opencodeOpts(t *testing.T, hooks bool) Options {
	t.Helper()
	o := opts(t, hooks)
	o.Targets = []string{"opencode"}
	o.HookCommand = "/opt/enola/enola"
	return o
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(read(t, path)), &doc); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	return doc
}

// TestOpencode_WritesOnlyWhatOpencodeReads is the regression this target exists for.
//
// opencode reads a fixed set of instruction files and none of them is `.claude/rules/`,
// `.cursor/rules/` or `.github/instructions/`. So on a repository with no AGENTS.md —
// the common case — `enola install` used to report success for five targets and
// configure opencode with precisely nothing.
func TestOpencode_WritesOnlyWhatOpencodeReads(t *testing.T) {
	o := opencodeOpts(t, true)
	rs, err := Install(o)
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		filepath.Join(".opencode", "enola.md"),
		filepath.Join(".opencode", "opencode.json"),
		filepath.Join(".opencode", "plugin", "enola.js"),
	} {
		if _, ok := actionFor(rs, want); !ok {
			t.Errorf("%s was not written; opencode would read nothing", want)
		}
	}

	// The instruction file is inert unless opencode is told to load it: it is not on
	// the list opencode reads by itself, so an unregistered file is the silent failure
	// this package exists to prevent.
	doc := readJSON(t, filepath.Join(o.RepoDir, ".opencode", "opencode.json"))
	list, _ := doc["instructions"].([]any)
	found := false
	for _, e := range list {
		if s, _ := e.(string); s == ".opencode/enola.md" {
			found = true
		}
	}
	if !found {
		t.Errorf("the instruction file was written but never registered: instructions=%v", list)
	}
}

// TestOpencode_PluginIsOptIn — the plugin refuses tool calls, so it is executable
// behaviour and follows the same rule as every hook here: never without --hooks.
func TestOpencode_PluginIsOptIn(t *testing.T) {
	o := opencodeOpts(t, false)
	if _, err := Install(o); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(o.RepoDir, ".opencode", "plugin", "enola.js")); !os.IsNotExist(err) {
		t.Errorf("plugin written without --hooks (err=%v); it must be opt-in", err)
	}
	// The instruction file must not describe it either.
	if body := read(t, filepath.Join(o.RepoDir, ".opencode", "enola.md")); strings.Contains(body, "plugin is installed") {
		t.Error("the instructions describe a plugin that was not installed")
	}
}

// TestOpencode_LeavesAForeignMCPRegistrationAlone.
//
// Registering an MCP server is not like writing a rule file: the entry may already be
// there, pointing at a development build, and it is the user's. Enola identifies its own
// by value rather than by a marker field, because opencode validates its config strictly
// and an unknown key inside a server entry is a risk taken with someone's ability to
// start their editor.
func TestOpencode_LeavesAForeignMCPRegistrationAlone(t *testing.T) {
	o := opencodeOpts(t, false)
	cfg := filepath.Join(o.RepoDir, "opencode.json")
	mine := `{"mcp":{"enola":{"type":"local","command":["/home/me/src/enola/enola"],"enabled":true,"timeout":240000}}}`
	if err := os.WriteFile(cfg, []byte(mine), 0o644); err != nil {
		t.Fatal(err)
	}

	rs, err := Install(o)
	if err != nil {
		t.Fatal(err)
	}
	if a, _ := actionFor(rs, "(opencode mcp: enola)"); a != ActionSkipped {
		t.Errorf("a foreign registration was not reported as skipped: %v", rs)
	}
	entry := readJSON(t, cfg)["mcp"].(map[string]any)["enola"].(map[string]any)
	if entry["command"].([]any)[0] != "/home/me/src/enola/enola" {
		t.Errorf("the user's own registration was overwritten: %v", entry)
	}
	if entry["timeout"] == nil {
		t.Errorf("settings enola does not write were dropped: %v", entry)
	}

	// And uninstall must not delete it either, which is the half that would actually
	// cost someone something.
	if _, err := Uninstall(o); err != nil {
		t.Fatal(err)
	}
	after := readJSON(t, cfg)
	if _, ok := after["mcp"].(map[string]any)["enola"]; !ok {
		t.Errorf("uninstall deleted a registration enola never wrote: %v", after)
	}
}

// TestOpencode_PrefersAnExistingConfigOverCreatingASecond — opencode merges several
// config locations, so creating one next to an existing one is how two files end up
// disagreeing about the same key.
func TestOpencode_PrefersAnExistingConfigOverCreatingASecond(t *testing.T) {
	o := opencodeOpts(t, false)
	cfg := filepath.Join(o.RepoDir, "opencode.json")
	if err := os.WriteFile(cfg, []byte(`{"model":"local/qwen"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(o); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(o.RepoDir, ".opencode", "opencode.json")); !os.IsNotExist(err) {
		t.Error("a second config was created beside the one already there")
	}
	if doc := readJSON(t, cfg); doc["model"] != "local/qwen" {
		t.Errorf("the existing config lost settings: %v", doc)
	}
}

// TestOpencode_WillNotRewriteAJsoncConfig — a `.jsonc` config is valid for opencode and
// its comments are the user's. Every JSON writer here would silently drop them, so the
// only honest outcome is to say so and write nothing.
func TestOpencode_WillNotRewriteAJsoncConfig(t *testing.T) {
	o := opencodeOpts(t, false)
	jsonc := filepath.Join(o.RepoDir, "opencode.jsonc")
	body := "{\n  // my models\n  \"model\": \"local/qwen\"\n}\n"
	if err := os.WriteFile(jsonc, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	rs, err := Install(o)
	if err != nil {
		t.Fatal(err)
	}
	if a, ok := actionFor(rs, "opencode.jsonc"); !ok || a != ActionSkipped {
		t.Errorf("a jsonc config was not reported as skipped: %v", rs)
	}
	if read(t, jsonc) != body {
		t.Error("the jsonc config was rewritten and its comments are gone")
	}
}

// TestOpencode_DoesNotDuplicateWhatAgentsMdAlreadySays — opencode reads a repo-root
// AGENTS.md on its own, and the `agents` target maintains enola's block inside it. A
// second repo-local file would put the same paragraphs into the same context window
// twice, paid for on every request.
func TestOpencode_DoesNotDuplicateWhatAgentsMdAlreadySays(t *testing.T) {
	o := opencodeOpts(t, false)
	o.Targets = nil // every target, so the `agents` target writes the block in this run
	if err := os.WriteFile(filepath.Join(o.RepoDir, "AGENTS.md"), []byte("# house rules\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rs, err := Install(o)
	if err != nil {
		t.Fatal(err)
	}
	if a, _ := actionFor(rs, filepath.Join(".opencode", "enola.md")); a != ActionSkipped {
		t.Errorf("the instruction file was duplicated alongside AGENTS.md: %v", rs)
	}
	// The MCP registration is NOT part of that duplication and must still happen:
	// AGENTS.md carries the instructions, it cannot serve the tools they name.
	doc := readJSON(t, filepath.Join(o.RepoDir, ".opencode", "opencode.json"))
	if _, ok := doc["mcp"].(map[string]any)["enola"]; !ok {
		t.Errorf("the server was not registered: %v", doc)
	}
}

// TestOpencode_GlobalWaitsForOpencodeToExist — the config directory is the evidence
// opencode is installed. Creating `~/.config/opencode` for someone who does not use it
// is littering in a home directory to no purpose.
func TestOpencode_GlobalWaitsForOpencodeToExist(t *testing.T) {
	o := opencodeOpts(t, true)
	o.Scope = ScopeGlobal

	rs, err := Install(o)
	if err != nil {
		t.Fatal(err)
	}
	if a, _ := actionFor(rs, filepath.Join(".config", "opencode")); a != ActionSkipped {
		t.Errorf("wrote into a home directory with no opencode in it: %v", rs)
	}

	dir := filepath.Join(o.HomeDir, ".config", "opencode")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(o); err != nil {
		t.Fatal(err)
	}
	// `~/` rather than an absolute path: the entry has to survive being read on a
	// machine whose home directory is somewhere else.
	doc := readJSON(t, filepath.Join(dir, "opencode.json"))
	list, _ := doc["instructions"].([]any)
	if len(list) != 1 || list[0] != "~/.config/opencode/enola.md" {
		t.Errorf("global instructions entry is not portable: %v", list)
	}
	if _, err := os.Stat(filepath.Join(dir, "plugin", "enola.js")); err != nil {
		t.Errorf("global plugin not written: %v", err)
	}
}

// TestOpencode_GlobalUninstallLeavesOpencodesOwnDirectory — `~/.config/opencode` is
// opencode's and predates this install; `plugin/` inside it may not be.
func TestOpencode_GlobalUninstallLeavesOpencodesOwnDirectory(t *testing.T) {
	o := opencodeOpts(t, true)
	o.Scope = ScopeGlobal
	dir := filepath.Join(o.HomeDir, ".config", "opencode")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(o); err != nil {
		t.Fatal(err)
	}
	if _, err := Uninstall(o); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("uninstall removed opencode's own config directory: %v", err)
	}
	for _, gone := range []string{"enola.md", "opencode.json", filepath.Join("plugin", "enola.js")} {
		if _, err := os.Stat(filepath.Join(dir, gone)); !os.IsNotExist(err) {
			t.Errorf("%s survived uninstall (err=%v)", gone, err)
		}
	}
}

// TestOpencode_ServerNameMatchesThePluginsPrefix.
//
// opencode names an MCP tool `<server>_<tool>`, and the plugin refuses a search by
// telling the model to call `<prefix>explore` instead. If the installer registers the
// server under one name and the plugin guesses another, the refusal names a tool that
// does not exist — a worse outcome than never having gated the search at all.
func TestOpencode_ServerNameMatchesThePluginsPrefix(t *testing.T) {
	for cmd, want := range map[string]string{
		"/opt/enola/enola":      "enola",
		`"/opt/my tools/enola"`: "enola",
		"/opt/enola/acme-arch":  "acme-arch",
		"/opt/enola/enola.exe":  "enola",
		"enola":                 "enola",
	} {
		o := opencodeOpts(t, false)
		o.HookCommand = cmd
		if got := opencodeServerName(o); got != want {
			t.Errorf("%s registered as %q, want %q", cmd, got, want)
		}
		// The plugin recognises exactly the names the installer can produce.
		if !strings.Contains(opencodePlugin, `binary === "enola" || binary.startsWith("enola-")`) {
			t.Fatal("the plugin no longer detects enola servers by binary name")
		}
	}
}

// TestOpencode_ReinstallIsIdempotent — a second run must report unchanged rather than
// accumulating a duplicate instructions entry.
func TestOpencode_ReinstallIsIdempotent(t *testing.T) {
	o := opencodeOpts(t, true)
	if _, err := Install(o); err != nil {
		t.Fatal(err)
	}
	rs, err := Install(o)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rs {
		if r.Action != ActionUnchanged {
			t.Errorf("re-running install changed %s (%s)", r.Path, r.Action)
		}
	}
	doc := readJSON(t, filepath.Join(o.RepoDir, ".opencode", "opencode.json"))
	if list, _ := doc["instructions"].([]any); len(list) != 1 {
		t.Errorf("instructions accumulated duplicates: %v", list)
	}
}

// TestOpencode_AgentsMdWithoutEnolasBlockIsNotCoverage.
//
// The regression this replaced was silent and total: on a repository with an AGENTS.md
// of its own, `--targets opencode` skipped the instruction file because the file
// existed, while the `agents` target that would have filled it was not part of the run.
// The result registered the MCP server and delivered no instructions at all, and called
// it a deliberate skip.
func TestOpencode_AgentsMdWithoutEnolasBlockIsNotCoverage(t *testing.T) {
	o := opencodeOpts(t, false)
	if err := os.WriteFile(filepath.Join(o.RepoDir, "AGENTS.md"), []byte("# house rules\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rs, err := Install(o)
	if err != nil {
		t.Fatal(err)
	}
	if a, _ := actionFor(rs, filepath.Join(".opencode", "enola.md")); a != ActionCreated {
		t.Fatalf("opencode was left with no instructions at all: %v", rs)
	}

	// And once the block IS in AGENTS.md, the same run skips: the file is only there to
	// cover what AGENTS.md does not.
	if err := os.WriteFile(filepath.Join(o.RepoDir, "AGENTS.md"),
		[]byte("# house rules\n\n"+block("anything")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Uninstall(o); err != nil {
		t.Fatal(err)
	}
	rs, err = Install(o)
	if err != nil {
		t.Fatal(err)
	}
	if a, _ := actionFor(rs, filepath.Join(".opencode", "enola.md")); a != ActionSkipped {
		t.Errorf("the instruction file was duplicated alongside a block that already says it: %v", rs)
	}
}

// TestOpencode_PreviewMatchesTheApply — the `agents` target runs before this one, so a
// coverage check that read AGENTS.md off disk would see the block in a real run and not
// in a --dry-run, and report two different plans for the same command.
func TestOpencode_PreviewMatchesTheApply(t *testing.T) {
	o := opencodeOpts(t, true)
	o.Targets = nil
	if err := os.WriteFile(filepath.Join(o.RepoDir, "AGENTS.md"), []byte("# house rules\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	preview := o
	preview.DryRun = true
	planned, err := Install(preview)
	if err != nil {
		t.Fatal(err)
	}
	applied, err := Install(o)
	if err != nil {
		t.Fatal(err)
	}
	for i := range planned {
		if planned[i].Path != applied[i].Path || planned[i].Action != applied[i].Action {
			t.Errorf("preview said %v, apply did %v", planned[i], applied[i])
		}
	}
}

// TestOpencode_RepointsItsOwnRegistrationWhenTheBinaryMoves — an `enola upgrade`, or a
// switch between a release build and a development one, changes the path without
// changing whose registration it is. Identifying it by exact value left the old path in
// place and reported the entry as somebody else's.
func TestOpencode_RepointsItsOwnRegistrationWhenTheBinaryMoves(t *testing.T) {
	o := opencodeOpts(t, false)
	o.HookCommand = "/old/bin/enola"
	if _, err := Install(o); err != nil {
		t.Fatal(err)
	}

	moved := o
	moved.HookCommand = "/new/bin/enola"
	rs, err := Install(moved)
	if err != nil {
		t.Fatal(err)
	}
	if a, _ := actionFor(rs, "(opencode mcp: enola)"); a != ActionUpdated {
		t.Errorf("the move was not reported to the user: %v", rs)
	}
	cfg := filepath.Join(o.RepoDir, ".opencode", "opencode.json")
	entry := readJSON(t, cfg)["mcp"].(map[string]any)["enola"].(map[string]any)
	if got := entry["command"].([]any)[0]; got != "/new/bin/enola" {
		t.Errorf("opencode would still start the old binary: %v", got)
	}

	// And uninstall still recognises it, so the move does not strand the entry either.
	if _, err := Uninstall(moved); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cfg); !os.IsNotExist(err) {
		t.Errorf("registration survived uninstall after the binary moved (err=%v)", err)
	}
}

// TestOpencode_ShapeOwnershipStaysNarrow — the looser ownership test must not start
// claiming entries the user wrote. Anything carrying a setting enola does not write is
// theirs.
func TestOpencode_ShapeOwnershipStaysNarrow(t *testing.T) {
	for name, entry := range map[string]map[string]any{
		"an extra setting": {"type": "local", "command": []any{"/x/enola"}, "enabled": true, "timeout": 240000.0},
		"a remote server":  {"type": "remote", "command": []any{"/x/enola"}, "enabled": true},
		"disabled":         {"type": "local", "command": []any{"/x/enola"}, "enabled": false},
		"arguments":        {"type": "local", "command": []any{"/x/enola", "cfg.yaml"}, "enabled": true},
		"a different tool": {"type": "local", "command": []any{"/x/something"}, "enabled": true},
	} {
		if opencodeOwnsServer(entry, "enola") {
			t.Errorf("%s: enola claimed a registration it did not write: %v", name, entry)
		}
	}
	own := map[string]any{"type": "local", "command": []any{"/x/enola"}, "enabled": true}
	if !opencodeOwnsServer(own, "enola") {
		t.Errorf("enola failed to recognise its own registration: %v", own)
	}
}
