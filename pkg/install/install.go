package install

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Scope decides whose configuration is touched: this repository, or this user.
type Scope string

const (
	// ScopeLocal writes into the repository, so the team shares the setup through
	// source control. The default, because enola is a property of the codebase.
	ScopeLocal Scope = "local"
	// ScopeGlobal writes into the user's home directory, applying to every project.
	ScopeGlobal Scope = "global"
)

// Options configures a run. RepoDir and HomeDir are injected rather than resolved
// internally so tests never touch the real home directory.
type Options struct {
	Scope   Scope
	RepoDir string
	HomeDir string
	// Hooks installs the session-start and stop hooks in addition to the instructions.
	// Off by default: instructions are inert context, whereas hooks run commands, and
	// that difference should be a decision the user makes rather than one they discover.
	Hooks bool
	// HookCommand is the binary the hook config invokes. Defaults to "enola".
	HookCommand string
	// DryRun reports what would change without writing anything.
	DryRun bool
	// Targets restricts the run to named targets; empty means every applicable one.
	Targets []string
}

func (o Options) hookCommand() string {
	if o.HookCommand == "" {
		return "enola"
	}
	return o.HookCommand
}

// TargetNames are the agents enola can configure.
//
// `agents` is the repo-root AGENTS.md, which Codex, Copilot and Pi all read — so the
// codex/copilot/pi targets below add only what AGENTS.md does NOT already cover: their
// user-level files, and Copilot's own owned-file format. Writing a second repo-local file
// for a tool that already reads AGENTS.md would duplicate the instruction into the same
// context window twice.
var TargetNames = []string{"claude", "cursor", "agents", "codex", "copilot", "pi", "opencode"}

// agentsMdReaders are the tools served by the repo-root AGENTS.md, named in output so a
// user can tell what coverage they actually got.
const agentsMdReaders = "Codex, Copilot, Pi, opencode and other AGENTS.md-aware agents"

// Install writes the instructions, and the hooks when asked, returning one Result per
// file so the caller can show exactly what happened.
func Install(o Options) ([]Result, error) { return run(o, false) }

// Uninstall removes everything Install wrote, leaving the rest of each file untouched.
func Uninstall(o Options) ([]Result, error) { return run(o, true) }

func run(o Options, remove bool) ([]Result, error) {
	if o.Scope == "" {
		o.Scope = ScopeLocal
	}
	if o.Scope != ScopeLocal && o.Scope != ScopeGlobal {
		return nil, fmt.Errorf("unknown scope %q (want %q or %q)", o.Scope, ScopeLocal, ScopeGlobal)
	}
	if o.Scope == ScopeLocal && o.RepoDir == "" {
		return nil, fmt.Errorf("local scope needs a repository directory")
	}
	if o.Scope == ScopeGlobal && o.HomeDir == "" {
		return nil, fmt.Errorf("global scope needs a home directory")
	}

	var out []Result
	for _, name := range o.selectedTargets() {
		rs, err := runTarget(name, o, remove)
		if err != nil {
			return nil, err
		}
		out = append(out, rs...)
	}
	return out, nil
}

func (o Options) selectedTargets() []string {
	if len(o.Targets) == 0 {
		return TargetNames
	}
	want := map[string]bool{}
	for _, t := range o.Targets {
		want[t] = true
	}
	var out []string
	for _, n := range TargetNames {
		if want[n] {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

func runTarget(name string, o Options, remove bool) ([]Result, error) {
	switch name {
	case "claude":
		return claudeTarget(o, remove)
	case "cursor":
		return cursorTarget(o, remove)
	case "agents":
		return agentsTarget(o, remove)
	case "codex":
		return codexTarget(o, remove)
	case "pi":
		return globalAgentsTarget(o, remove, "pi", filepath.Join(".pi", "agent", "AGENTS.md"))
	case "copilot":
		return copilotTarget(o, remove)
	case "opencode":
		return opencodeTarget(o, remove)
	default:
		return nil, fmt.Errorf("unknown target %q", name)
	}
}

// codexDir is .codex/ under the repo (local) or the home directory (global).
func codexDir(o Options) string {
	if o.Scope == ScopeGlobal {
		return filepath.Join(o.HomeDir, ".codex")
	}
	return filepath.Join(o.RepoDir, ".codex")
}

// codexPruneStop keeps uninstall from removing `~/.codex` itself. Globally that directory
// is Codex's, and enola only writes into it because it was already there; locally enola
// creates `.codex/` in the repository, so an empty one goes with the files it held.
func codexPruneStop(o Options) string {
	if o.Scope == ScopeGlobal {
		return codexDir(o)
	}
	return o.RepoDir
}

// codexTarget combines Codex's AGENTS.md block with its own hooks.json. Global scope
// only writes hooks.json when ~/.codex already exists, same rule as globalAgentsTarget.
func codexTarget(o Options, remove bool) ([]Result, error) {
	out, err := globalAgentsTarget(o, remove, "codex", filepath.Join(".codex", "AGENTS.md"))
	if err != nil {
		return nil, err
	}

	path := filepath.Join(codexDir(o), "hooks.json")
	if o.Scope == ScopeGlobal {
		if _, err := os.Stat(codexDir(o)); os.IsNotExist(err) {
			return append(out, Result{
				Path:   path,
				Action: ActionSkipped,
				Reason: ".codex not found in your home directory, so codex does not appear to be installed",
			}), nil
		}
	}
	if !remove && !o.Hooks {
		return out, nil
	}
	hr, err := writeCodexHooks(path, codexPruneStop(o), o, remove)
	if err != nil {
		return nil, err
	}
	return append(out, hr...), nil
}

// globalAgentsTarget maintains enola's block in a tool's USER-level AGENTS.md — Codex
// reads `~/.codex/AGENTS.md`, Pi reads `~/.pi/agent/AGENTS.md`.
//
// Locally both read the repo-root AGENTS.md the `agents` target already writes, so this
// adds only what that cannot: guidance in projects where nobody has run `enola install`.
//
// It writes only when the tool's config directory already exists. That directory is the
// evidence the tool is actually installed; creating `~/.codex/` for someone who does not
// use Codex would be littering in a home directory to no purpose.
func globalAgentsTarget(o Options, remove bool, tool, rel string) ([]Result, error) {
	if o.Scope != ScopeGlobal {
		return []Result{{
			Path:   "(" + tool + ")",
			Action: ActionSkipped,
			Reason: "locally, " + tool + " reads the repository's AGENTS.md, which the `agents` target already writes",
		}}, nil
	}
	path := filepath.Join(o.HomeDir, rel)
	// filepath.Dir of ".pi/agent/AGENTS.md" is ".pi/agent"; require the tool's ROOT dir,
	// which is the part that says the tool exists. It doubles as the pruning bound on the
	// way out: `~/.pi/agent/` is enola's to remove, `~/.pi/` is not.
	root := filepath.Join(o.HomeDir, strings.SplitN(rel, string(filepath.Separator), 2)[0])
	if remove {
		r, err := removeSection(path, root, o.DryRun)
		return []Result{r}, err
	}
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return []Result{{
			Path:   path,
			Action: ActionSkipped,
			Reason: filepath.Base(root) + " not found in your home directory, so " + tool + " does not appear to be installed",
		}}, nil
	}
	r, err := upsertSection(path, block(body(o)), o.DryRun)
	return []Result{r}, err
}

// copilotTarget writes `.github/instructions/enola.instructions.md`, a file enola owns
// outright.
//
// Copilot reads three things: `.github/copilot-instructions.md`, the repo-root AGENTS.md,
// and `.github/instructions/*.instructions.md`. The last is chosen for the same reason
// `.claude/rules/` was — each file stands alone, so enola never edits a document the user
// hand-maintains, and cannot damage one. `applyTo: "**"` is what makes it unconditional;
// the frontmatter is required, and without it the file governs nothing.
func copilotTarget(o Options, remove bool) ([]Result, error) {
	if o.Scope == ScopeGlobal {
		return []Result{{
			Path:   "(copilot)",
			Action: ActionSkipped,
			Reason: "Copilot's user-level instructions live in IDE/account settings, not a file enola can write",
		}}, nil
	}
	path := filepath.Join(o.RepoDir, ".github", "instructions", "enola.instructions.md")
	if remove {
		r, err := removeOwnedFile(path, o.RepoDir, o.DryRun)
		return []Result{r}, err
	}
	r, err := writeOwnedFile(path, copilotInstructions(o), o.DryRun)
	return []Result{r}, err
}

// claudeDir is .claude/ under the repo (local) or the home directory (global).
func claudeDir(o Options) string {
	if o.Scope == ScopeGlobal {
		return filepath.Join(o.HomeDir, ".claude")
	}
	return filepath.Join(o.RepoDir, ".claude")
}

// claudeTarget writes a rule file enola owns outright, plus optionally the hooks.
//
// A rule file rather than a section inside CLAUDE.md: rules without `paths` frontmatter
// load at launch with the same priority as a project CLAUDE.md, and each rule is its own
// file — so enola never edits a document the user hand-maintains, and cannot damage one.
func claudeTarget(o Options, remove bool) ([]Result, error) {
	var out []Result
	rule := filepath.Join(claudeDir(o), "rules", "enola.md")

	if remove {
		r, err := removeOwnedFile(rule, pruneStop(o), o.DryRun)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
		hr, err := writeHooks(o, true)
		if err != nil {
			return nil, err
		}
		return append(out, hr...), nil
	}

	r, err := writeOwnedFile(rule, claudeRule(o), o.DryRun)
	if err != nil {
		return nil, err
	}
	out = append(out, r)

	if o.Hooks {
		hr, err := writeHooks(o, false)
		if err != nil {
			return nil, err
		}
		out = append(out, hr...)
	}
	return out, nil
}

// cursorTarget writes .cursor/rules/enola.mdc, another file enola owns outright.
// Cursor has no user-level equivalent, so global scope skips it rather than inventing a
// path — writing somewhere the tool does not read is a silent failure, and the whole
// point of checking each target's docs was to avoid exactly that.
func cursorTarget(o Options, remove bool) ([]Result, error) {
	if o.Scope == ScopeGlobal {
		return []Result{{
			Path:   "(cursor)",
			Action: ActionSkipped,
			Reason: "Cursor has no user-level rules directory; install locally instead",
		}}, nil
	}
	path := filepath.Join(o.RepoDir, ".cursor", "rules", "enola.mdc")
	if remove {
		r, err := removeOwnedFile(path, o.RepoDir, o.DryRun)
		return []Result{r}, err
	}
	r, err := writeOwnedFile(path, cursorRule(o), o.DryRun)
	return []Result{r}, err
}

// agentsTarget maintains a sentinel-delimited block in AGENTS.md — the one shared file
// enola touches, and the only place the section machinery is needed.
//
// It is never CREATED unless it already exists: a tool that drops a new AGENTS.md into
// someone's repository uninvited is a tool they remove. Claude Code does not read
// AGENTS.md at all, so skipping it costs Claude users nothing.
func agentsTarget(o Options, remove bool) ([]Result, error) {
	if o.Scope == ScopeGlobal {
		return []Result{{
			Path:   "(agents)",
			Action: ActionSkipped,
			Reason: "AGENTS.md is a per-repository convention; see the codex and pi targets for their user-level files",
		}}, nil
	}
	path := filepath.Join(o.RepoDir, "AGENTS.md")
	if remove {
		r, err := removeSection(path, o.RepoDir, o.DryRun)
		return []Result{r}, err
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return []Result{{
			Path:   path,
			Action: ActionSkipped,
			Reason: "does not exist; enola will not create it. Creating it would configure " +
				agentsMdReaders + " at once — worth doing deliberately, so create it yourself and re-run",
		}}, nil
	}
	r, err := upsertSection(path, block(body(o)), o.DryRun)
	return []Result{r}, err
}

// pruneStop is the directory an uninstall's pruning must not climb past: the repository
// for a local install, the home directory for a global one. Targets whose install is
// gated on a directory it did not create pass that directory instead — see codexRoot.
func pruneStop(o Options) string {
	if o.Scope == ScopeGlobal {
		return o.HomeDir
	}
	return o.RepoDir
}
