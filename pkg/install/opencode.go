package install

import (
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// opencodePlugin is the plugin written under --hooks. Embedded from a real .js file
// rather than held in a Go string so it stays lintable, syntax-checkable and readable
// as what it is; see opencode_plugin.js for what it does and why.
//
//go:embed opencode_plugin.js
var opencodePlugin string

// opencodeTarget configures opencode, which reads none of the files the other targets
// write.
//
// That is the whole reason this target exists. opencode loads instructions from a fixed
// list — `AGENTS.md`, `CLAUDE.md` and `CONTEXT.md` walking up from the working
// directory, `~/.config/opencode/AGENTS.md` and `~/.claude/CLAUDE.md` globally, plus
// whatever `instructions` in its config names — and `.claude/rules/`, `.cursor/rules/`
// and `.github/instructions/` are on none of them. In a repository without an
// AGENTS.md, which is the common case, `enola install` therefore configured opencode
// with nothing at all while reporting success for five other targets.
//
// It differs from those targets in two further ways, both forced by opencode:
//
//   - It registers the MCP server. Everywhere else that is a documented manual step,
//     but here the same file is already being edited for `instructions`, and an
//     instruction naming tools that are not served is worse than no instruction.
//   - Its hooks are a plugin, because opencode has no hook configuration in the shape
//     Claude Code and Codex accept. See opencode_plugin.js.
func opencodeTarget(o Options, remove bool) ([]Result, error) {
	dir := opencodeDir(o)
	if o.Scope == ScopeGlobal && !remove {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			return []Result{{
				Path:   dir,
				Action: ActionSkipped,
				Reason: ".config/opencode not found in your home directory, so opencode does not appear to be installed",
			}}, nil
		}
	}

	var out []Result

	rs, err := opencodeInstructionResults(o, remove)
	if err != nil {
		return nil, err
	}
	out = append(out, rs...)

	cs, err := opencodeConfigResults(o, remove)
	if err != nil {
		return nil, err
	}
	out = append(out, cs...)

	ps, err := opencodePluginResults(o, remove)
	if err != nil {
		return nil, err
	}
	return append(out, ps...), nil
}

// InstallsOpencodePlugin reports whether opencode is part of this run, and so whether
// --hooks would write the plugin. Asked by the CLI so that "instructions only" can say
// what the user is actually declining, which is not the same thing for every agent.
func InstallsOpencodePlugin(o Options) bool {
	for _, t := range o.selectedTargets() {
		if t == "opencode" {
			return true
		}
	}
	return false
}

// opencodeDir is `.opencode/` under the repository, or opencode's own config directory
// for a global install. Note that the global one is `~/.config/opencode`, NOT
// `~/.opencode`: the latter holds opencode's binary and is not read as configuration.
func opencodeDir(o Options) string {
	if o.Scope == ScopeGlobal {
		return filepath.Join(o.HomeDir, ".config", "opencode")
	}
	return filepath.Join(o.RepoDir, ".opencode")
}

// opencodePruneStop bounds uninstall's directory pruning. Locally enola creates
// `.opencode/` if it is not there, so an empty one goes with the files it held.
// Globally that directory is opencode's own and predates this install.
func opencodePruneStop(o Options) string {
	if o.Scope == ScopeGlobal {
		return opencodeDir(o)
	}
	return o.RepoDir
}

// opencodeInstructionFile is the instruction file enola owns outright.
func opencodeInstructionFile(o Options) string {
	return filepath.Join(opencodeDir(o), "enola.md")
}

// opencodeInstructionRef is how that file is named inside opencode's `instructions`
// list. A relative entry is resolved by globbing UP from the working directory to the
// worktree root, so the repo-relative form works from any subdirectory of the project;
// `~/` is expanded, which is what makes the global entry portable between machines.
func opencodeInstructionRef(o Options) string {
	if o.Scope == ScopeGlobal {
		return "~/.config/opencode/enola.md"
	}
	return ".opencode/enola.md"
}

// opencodeInstructionResults writes the instruction file, unless opencode is already
// being served the same text by the repository's AGENTS.md.
//
// The duplicate is the point of the check. opencode reads a repo-root AGENTS.md on its
// own, the `agents` target maintains enola's block inside it, and a second repo-local
// file would put the identical paragraphs into the same context window twice — paid
// for on every request, for nothing. The same rule already governs the codex and pi
// targets locally.
func opencodeInstructionResults(o Options, remove bool) ([]Result, error) {
	path := opencodeInstructionFile(o)
	if remove {
		r, err := removeOwnedFile(path, opencodePruneStop(o), o.DryRun)
		return []Result{r}, err
	}
	if o.Scope == ScopeLocal {
		if reason := agentsMdServesOpencode(o); reason != "" {
			return []Result{{Path: path, Action: ActionSkipped, Reason: reason}}, nil
		}
	}
	r, err := writeOwnedFile(path, opencodeInstructions(o), o.DryRun)
	return []Result{r}, err
}

// agentsMdServesOpencode reports why the repository's AGENTS.md already puts this text
// in front of opencode, or "" if it does not.
//
// Mere existence of the file is NOT the test, which is what this asked at first. A repo
// can have an AGENTS.md of its own that enola has never touched, and `--targets opencode`
// does not run the `agents` target that would add the block to it. The answer was then
// the worst one available: the MCP server registered, and not one word of instruction
// anywhere, reported as a deliberate skip.
//
// So the question is whether the block is there or is going to be, decided from what
// this run was asked to do rather than from what is on disk at the moment the check
// runs. The `agents` target is ordered before this one, so reading the file after it
// had written would answer correctly in a real run and wrongly in a --dry-run, and a
// preview that disagrees with the apply is its own defect.
func agentsMdServesOpencode(o Options) string {
	raw, err := os.ReadFile(filepath.Join(o.RepoDir, "AGENTS.md"))
	if err != nil {
		return ""
	}
	if strings.Contains(string(raw), beginMarker) {
		return "opencode reads this repository's AGENTS.md, which already carries enola's block"
	}
	for _, t := range o.selectedTargets() {
		if t == "agents" {
			return "opencode reads this repository's AGENTS.md, where the `agents` target in this run writes the same text"
		}
	}
	return ""
}

// opencodePluginResults writes the plugin, which opencode auto-discovers from
// `plugin/` under either scope's directory — no config entry required, verified
// against a running opencode rather than taken from its documentation.
func opencodePluginResults(o Options, remove bool) ([]Result, error) {
	path := filepath.Join(opencodeDir(o), "plugin", "enola.js")
	if remove {
		r, err := removeOwnedFile(path, opencodePruneStop(o), o.DryRun)
		return []Result{r}, err
	}
	if !o.Hooks {
		return nil, nil
	}
	r, err := writeOwnedFile(path, opencodePlugin, o.DryRun)
	return []Result{r}, err
}

// opencodeConfigPath picks the config file to edit, and says why when there is none it
// can safely touch.
//
// opencode merges several config locations, so CREATING one next to an existing one is
// how you get two files disagreeing about the same key. An existing config is therefore
// always preferred, and a new one is only ever created at `.opencode/opencode.json` —
// inside the directory this target already owns, rather than as a new file at the root
// of someone's repository.
//
// A `.jsonc` config is left alone deliberately: it is valid for opencode and its
// comments are the user's, and every JSON writer in this package would drop them.
func opencodeConfigPath(o Options) (string, string) {
	dir := opencodeDir(o)
	if o.Scope == ScopeGlobal {
		if _, err := os.Stat(filepath.Join(dir, "opencode.jsonc")); err == nil {
			return "", "opencode.jsonc holds comments this installer cannot preserve; add the entries by hand"
		}
		return filepath.Join(dir, "opencode.json"), ""
	}
	for _, candidate := range []string{
		filepath.Join(o.RepoDir, "opencode.json"),
		filepath.Join(dir, "opencode.json"),
	} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate, ""
		}
	}
	if _, err := os.Stat(filepath.Join(o.RepoDir, "opencode.jsonc")); err == nil {
		return "", "opencode.jsonc holds comments this installer cannot preserve; add the entries by hand"
	}
	return filepath.Join(dir, "opencode.json"), ""
}

// opencodeServerName is the key enola's MCP server gets in opencode's config. Derived
// from the binary's own name so a differently-named binary registers alongside enola's own server
// rather than overwriting it, which is also how the tool ids the plugin matches are
// spelled: `enola_explore`, `enola-ent_explore`.
func opencodeServerName(o Options) string {
	base := filepath.Base(strings.Trim(o.hookCommand(), `"`))
	base = strings.TrimSuffix(base, ".exe")
	if base == "" || base == "." || base == string(filepath.Separator) {
		return "enola"
	}
	return base
}

// opencodeMCPEntry is the server registration, and the exact value uninstall will
// consent to remove.
func opencodeMCPEntry(o Options) map[string]any {
	return map[string]any{
		"type":    "local",
		"command": []any{strings.Trim(o.hookCommand(), `"`)},
		"enabled": true,
	}
}

// opencodeConfigResults maintains enola's two keys in opencode's config: the
// instruction file in `instructions`, and the MCP server in `mcp`.
//
// The MCP entry is owned by VALUE, not by a marker. Markers are how the hook writers
// here identify their own entries, but opencode validates its config strictly and an
// unknown field inside a server entry is a risk taken with someone's ability to start
// their editor. So enola writes only fields opencode defines, and treats an entry that
// does not match what it would have written as somebody else's: not overwritten on
// install, not deleted on uninstall, reported either way. A hand-registered server
// pointing at a development build is the common case, and losing it to an uninstall
// would be the kind of damage this package exists to make impossible.
func opencodeConfigResults(o Options, remove bool) ([]Result, error) {
	path, skip := opencodeConfigPath(o)
	if skip != "" {
		return []Result{{Path: filepath.Join(opencodeDir(o), "opencode.jsonc"), Action: ActionSkipped, Reason: skip}}, nil
	}

	name := opencodeServerName(o)
	ours := opencodeMCPEntry(o)
	existing := opencodeExistingServer(path, name)
	// Present, and not a registration enola recognises as its own: leave it exactly as
	// it is, in both directions.
	foreign := existing != nil && !opencodeOwnsServer(existing, name)
	repointed := ""
	if existing != nil && !foreign && !sameJSON(existing, ours) {
		repointed = opencodeServerCommand(existing)
	}

	// Only when enola is the one creating the file. opencode's own guidance is that
	// every config should declare its schema so an editor catches mistakes while they
	// are typed, but adding it to a config that already exists is an edit nobody asked
	// for in a file nobody asked us to reformat.
	fresh := false
	if _, err := os.Stat(path); os.IsNotExist(err) {
		fresh = true
	}

	r, err := mutateJSON(path, opencodePruneStop(o), func(doc map[string]any) {
		if fresh && !remove {
			doc["$schema"] = "https://opencode.ai/config.json"
		}
		doc["instructions"] = mergeInstruction(doc["instructions"], opencodeInstructionRef(o), remove)
		if lst, ok := doc["instructions"].([]any); ok && len(lst) == 0 {
			delete(doc, "instructions")
		}

		mcp, _ := doc["mcp"].(map[string]any)
		switch {
		case foreign:
			// Nothing. Somebody else's registration.
		case remove:
			delete(mcp, name)
		default:
			if mcp == nil {
				mcp = map[string]any{}
			}
			mcp[name] = ours
		}
		if len(mcp) == 0 {
			delete(doc, "mcp")
		} else {
			doc["mcp"] = mcp
		}

		// A config left holding nothing but the schema line it was created with is a
		// scaffold advertising a tool that is gone — the same residue mutateJSON deletes
		// a `{}` for, wearing one key so that it no longer counts as empty. Dropping it
		// here is what lets mutateJSON remove the file and prune the directory around it.
		if _, only := doc["$schema"]; only && len(doc) == 1 {
			delete(doc, "$schema")
		}
	}, o.DryRun)
	if err != nil {
		return nil, err
	}

	out := []Result{r}
	switch {
	case foreign:
		out = append(out, Result{
			Path:   "(opencode mcp: " + name + ")",
			Action: ActionSkipped,
			Reason: "already registered with settings enola did not write; left exactly as it is",
		})
	case repointed != "" && !remove:
		// Named explicitly, because "updated opencode.json" does not tell anyone which
		// binary their editor is about to start.
		out = append(out, Result{
			Path:   "(opencode mcp: " + name + ")",
			Action: ActionUpdated,
			Reason: "now points at " + strings.Trim(o.hookCommand(), `"`) + ", was " + repointed,
		})
	}
	return out, nil
}

// opencodeOwnsServer reports whether an existing registration is one enola wrote, and
// may therefore update or remove.
//
// Ownership is by SHAPE rather than by exact equality, which is what this asked at
// first. Exact equality made a registration enola had written itself unrecognisable the
// moment the binary moved — after an `enola upgrade`, or between a release build and a
// development one — so a re-install quietly left the old path in place and reported it
// as somebody else's file. The shape is narrow enough to stay safe: exactly the three
// keys enola writes, a local server, and a command that is a single path to a binary
// called enola. A hand-written entry carrying anything else, a `timeout` for instance,
// is still recognised as the user's and never touched.
func opencodeOwnsServer(entry map[string]any, name string) bool {
	if len(entry) != 3 || entry["type"] != "local" || entry["enabled"] != true {
		return false
	}
	cmd := opencodeServerCommand(entry)
	if cmd == "" {
		return false
	}
	base := strings.TrimSuffix(filepath.Base(cmd), ".exe")
	return base == name
}

// opencodeServerCommand returns a local server's binary, or "" if it does not have
// exactly one.
func opencodeServerCommand(entry map[string]any) string {
	argv, _ := entry["command"].([]any)
	if len(argv) != 1 {
		return ""
	}
	s, _ := argv[0].(string)
	return s
}

// opencodeExistingServer reads one MCP server entry out of a config that may not exist
// and may not parse. Every failure reads as "no entry", which is the safe answer: the
// mutation that follows is itself a no-op on an unparseable file.
func opencodeExistingServer(path, name string) map[string]any {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var doc map[string]any
	if json.Unmarshal(raw, &doc) != nil {
		return nil
	}
	mcp, _ := doc["mcp"].(map[string]any)
	entry, _ := mcp[name].(map[string]any)
	return entry
}

// sameJSON compares two decoded documents by their canonical encoding, so a value read
// back from a file (float64 for every number) compares equal to the one enola builds.
func sameJSON(a, b any) bool {
	x, err1 := json.Marshal(a)
	y, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(x) == string(y)
}

// mergeInstruction adds or removes enola's instruction file in opencode's
// `instructions` list, preserving both the order and the contents of every other entry.
func mergeInstruction(existing any, ref string, remove bool) any {
	entries, _ := existing.([]any)
	out := make([]any, 0, len(entries)+1)
	for _, e := range entries {
		if s, ok := e.(string); ok && s == ref {
			continue
		}
		out = append(out, e)
	}
	if !remove {
		out = append(out, ref)
	}
	return out
}
