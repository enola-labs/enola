package command

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/enola-labs/enola/internal/config"
	"github.com/enola-labs/enola/internal/hookstate"
	"github.com/enola-labs/enola/internal/workspace"
	"github.com/enola-labs/enola/pkg/install"
)

// runInstall is `enola install` / `enola uninstall`: write enola's instructions, and
// optionally its hooks, into the config files the agents on this machine actually read.
func (r *Runner) Install(args []string, remove bool) {
	name := "install"
	if remove {
		name = "uninstall"
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		global  = fs.Bool("global", false, "configure this user (all projects) instead of this repository")
		hooks   = fs.Bool("hooks", false, "also install the session-start and stop hooks (install only)")
		noHooks = fs.Bool("no-hooks", false, "remove the hooks an earlier install wrote (install only)")
		dryRun  = fs.Bool("dry-run", false, "show what would change, write nothing")
		targets = fs.String("targets", "", "comma-separated subset of: "+strings.Join(install.TargetNames, ", "))
		yes     = fs.Bool("yes", false, "skip the confirmation prompt")
		check   = fs.Bool("check", false, "say whether what an earlier install wrote is current with this binary; writes nothing, exits 1 when it is not (install only)")
	)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s %s [flags] [repo_path]\n\n", r.name(), name)
		if remove {
			fmt.Fprintf(os.Stderr, "Remove everything `%s install` wrote, leaving the rest of each file untouched.\n\n", r.name())
		} else {
			fmt.Fprint(os.Stderr,
				"Write enola's instructions into the files your coding agents read, and its\n"+
					"skills into .claude/skills (Claude Code) and .agents/skills (Codex, Cursor,\n"+
					"Copilot, Pi, opencode).\n\n"+
					"By default this writes INSTRUCTIONS AND SKILLS ONLY — inert context telling the\n"+
					"agent the tools exist and how to use them. Pass --hooks to also run the loop\n"+
					"automatically: a baseline pinned at session start, and the architectural delta\n"+
					"reported at the end if the change introduced a regression. Hooks run commands,\n"+
					"so they are opt-in.\n\n"+
					"Re-run it after `enola upgrade`, with no flags: hooks an earlier install wrote\n"+
					"are kept and updated without --hooks. --no-hooks removes them.\n\n")
		}
		fmt.Fprint(os.Stderr, "Flags:\n")
		fs.PrintDefaults()
		r.writeHelpFooter(fs.Output(), args, name)
	}
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	repoDir, err := os.Getwd()
	if err != nil {
		r.installFatal("cannot determine the working directory: %v", err)
	}
	if rest := fs.Args(); len(rest) > 0 {
		if !isDirectory(rest[0]) {
			r.installFatal("%q is not a directory", rest[0])
		}
		repoDir = rest[0]
	}
	home, err := os.UserHomeDir()
	if err != nil && *global {
		r.installFatal("cannot determine the home directory: %v", err)
	}

	if *check && !remove {
		os.Exit(r.installCheck(repoDir, home))
	}

	opts := install.Options{
		Scope:       install.ScopeLocal,
		RepoDir:     repoDir,
		HomeDir:     home,
		Hooks:       *hooks && !remove,
		DryRun:      *dryRun,
		HookCommand: hookBinary(),
	}
	if *global {
		opts.Scope = install.ScopeGlobal
	}
	// A re-run keeps what the first run set up, so `enola install` alone is the whole
	// instruction after an upgrade. Without this a plain re-run over a --hooks install
	// rewrote the instructions without the hooks note and switched Pi's hooks off, while
	// Claude Code's entries stayed behind, never updated again.
	keptHooks := false
	if !remove {
		switch {
		case *hooks && *noHooks:
			r.installFatal("--hooks and --no-hooks contradict each other")
		case *noHooks:
			opts.DropHooks = true
		case !*hooks && install.HooksInstalled(opts):
			opts.Hooks, keptHooks = true, true
		}
	}
	// A baseline and its grading snapshot the directory they run in as one repository,
	// so in a folder of repositories those hooks could only ever skip, and installing
	// them would configure a mechanism that does nothing. The hooks that steer the agent
	// to enola need no repository, and a folder of repositories is where an agent most
	// needs them: they are installed, and only they.
	folder := opts.Hooks && opts.Scope == install.ScopeLocal && workspace.IsFolderOfRepos(repoDir)
	if folder {
		opts.FolderOfRepos = true
		for _, line := range wrap(fmt.Sprintf("%s is a folder of repositories, not a repository. Sessions "+
			"started here are graded per repository, in Claude Code only: a repository's baseline is "+
			"pinned before the session's first edit in it, and the change graded when the turn ends. "+
			"Codex and Pi get no hooks here; run `install --hooks` inside a repository for those.", repoDir), 80) {
			fmt.Println(line)
		}
		fmt.Println()
	}
	if *targets != "" {
		for _, t := range strings.Split(*targets, ",") {
			if t = strings.TrimSpace(t); t != "" {
				opts.Targets = append(opts.Targets, t)
			}
		}
	}

	// Preview first, always. These are the user's files; showing what will be touched
	// before touching it is what makes the operation reviewable rather than a leap of
	// faith — and it is the difference between a tool that gets trusted and one that
	// gets uninstalled.
	preview := opts
	preview.DryRun = true
	planned, err := act(preview, remove)
	if err != nil {
		r.installFatal("%v", err)
	}

	fmt.Printf("%s\n", strings.ToUpper(name[:1])+name[1:])
	if opts.Scope == install.ScopeLocal {
		// Say where the target came from, not just what it is. Run from a checkout of
		// enola itself, "Repository: /path/to/enola" reads as if the tool were
		// configuring its own installation directory rather than the current one.
		origin := "current directory"
		if len(fs.Args()) > 0 {
			origin = "from the path you gave"
		}
		fmt.Printf("Target:     %s   (%s)\n", repoDir, origin)
		if origin == "current directory" {
			fmt.Println("            Pass a path to configure a different repository.")
		}
	} else {
		fmt.Printf("Target:     %s   (this user, all projects)\n", home)
	}
	if !remove {
		if opts.Hooks {
			// Described by the installer itself, so this can never announce a hook that
			// is not actually configured.
			if opts.FolderOfRepos {
				fmt.Println("Hooks:      yes, Claude Code's (a folder of repositories, see above)")
			} else {
				fmt.Println("Hooks:      yes")
			}
			if keptHooks {
				fmt.Println("            Kept: an earlier install wrote them. --no-hooks removes them.")
			}
			for _, d := range install.HookSummary(opts) {
				for i, line := range wrap(d, 66) {
					prefix := "            · "
					if i > 0 {
						prefix = "              "
					}
					fmt.Println(prefix + line)
				}
			}
			if install.InstallsSessionHooks(opts) {
				fmt.Println("            Never blocks, and never interrupts on failure.")
			}
		} else if opts.DropHooks {
			fmt.Println("Hooks:      removed (--no-hooks); instructions and skills only.")
		} else if install.InstallsOpencodePlugin(opts) && !install.InstallsSessionHooks(opts) {
			// opencode's --hooks is a plugin, not the grading loop. Offering "the loop"
			// here would name a mechanism this run cannot install however it is re-run.
			fmt.Println("Hooks:      no — instructions and skills only, which an agent is free to ignore.")
			fmt.Println("            Re-run with --hooks for opencode's plugin, which points a")
			fmt.Println("            session's first searches at the index instead.")
		} else {
			fmt.Println("Hooks:      no — instructions and skills only. Re-run with --hooks to automate the loop.")
		}
	}
	fmt.Println()
	printPlan(planned)

	if *dryRun {
		fmt.Println("\nDry run — nothing was written.")
		return
	}
	if !changesAnything(planned) {
		fmt.Println("\nNothing to do.")
		return
	}
	if !*yes && !confirm() {
		fmt.Println("Aborted; nothing was written.")
		os.Exit(1)
	}

	applied, err := act(opts, remove)
	if err != nil {
		r.installFatal("%v", err)
	}
	fmt.Println()
	// The preview above and the result below are the same list in the overwhelmingly
	// common case, and printing it twice reads as a rendering bug rather than as a
	// report: with --yes there is not even a confirmation prompt between the two
	// copies. So the second one collapses to a single line UNLESS it diverges, which
	// is the only case where re-reading it tells you something — a file that changed
	// under the preview, or a write that could not be made.
	if n := countChanged(applied); n > 0 && samePlan(planned, applied) {
		fmt.Printf("%s — %s as previewed.\n", appliedVerb(remove), plural(n, "file"))
	} else {
		fmt.Println("Applied (differs from the preview):")
		printPlan(applied)
	}

	// Stamp the heartbeat so `enola doctor` can later say "installed on X, never fired
	// since" — the one report that distinguishes hooks that are wired up from hooks that
	// merely look wired up. Local scope only: a global install has no repository whose
	// output directory could hold the record.
	if opts.Scope == install.ScopeLocal {
		if outDir := hookOutputDir(repoDir); outDir != "" {
			switch {
			case remove, opts.DropHooks:
				clearHookHeartbeat(repoDir, outDir)
			case install.InstallsSessionHooks(opts):
				// Only the session hooks, not every --hooks run. The heartbeat is what
				// `doctor` compares against to say "configured on this date, never fired
				// since", and opencode's plugin never invokes `enola hook` at all — so
				// recording an opencode-only install here would manufacture exactly the
				// broken-hook report this file exists to make trustworthy.
				hookstate.RecordInstalled(outDir, opts.HookCommand)
			}
		}
	}

	if !remove && opts.Hooks {
		fmt.Println("\nRestart your agent session for the hooks to take effect.")
		// `doctor` reads the heartbeat the session hooks write, and only they write it.
		// Offering it after an opencode-only install would send the user to a report
		// that can only ever say the hooks have never fired.
		if install.InstallsSessionHooks(opts) {
			fmt.Printf("Then `%s doctor` will tell you whether they are actually firing.\n", r.name())
		}
		if touchedCodexHooks(applied) {
			fmt.Println("Codex also requires you to approve a new hook once before it runs — inside Codex, run `/hooks`.")
		}
	}
	if !remove && touchedOpencode(applied) {
		// Said whether or not hooks were installed: opencode reads its config once at
		// startup and never reloads it, so an instruction file and an MCP server
		// registered mid-session are both invisible until it is restarted.
		fmt.Println("\nopencode loads its configuration once at startup — quit and restart it for these to take effect.")
	}
	if !remove && install.InstallsPiExtension(opts) && touchedPi(applied) {
		// Without trust Pi skips `.pi/extensions/` silently and still loads AGENTS.md, so
		// the session looks configured and has no enola tools.
		fmt.Println("\nPi loads a project's extensions only once the project is trusted: start Pi here and")
		fmt.Println("accept the trust prompt, run `/trust` inside it, or start it with `pi --approve`.")
	}
}

// touchedPi reports whether this run wrote or updated Pi's extension.
func touchedPi(rs []install.Result) bool {
	for _, r := range rs {
		if !strings.HasSuffix(filepath.ToSlash(r.Path), "/.pi/"+install.PiExtensionFile) {
			continue
		}
		if r.Action == install.ActionCreated || r.Action == install.ActionUpdated {
			return true
		}
	}
	return false
}

// touchedOpencode reports whether this run changed anything opencode reads.
func touchedOpencode(rs []install.Result) bool {
	for _, r := range rs {
		if r.Action != install.ActionCreated && r.Action != install.ActionUpdated {
			continue
		}
		p := filepath.ToSlash(r.Path)
		if strings.Contains(p, "/.opencode/") || strings.Contains(p, "/.config/opencode/") {
			return true
		}
	}
	return false
}

// touchedCodexHooks reports whether this run wrote or updated Codex's hooks.json.
func touchedCodexHooks(rs []install.Result) bool {
	for _, r := range rs {
		if !strings.HasSuffix(filepath.ToSlash(r.Path), "/.codex/hooks.json") {
			continue
		}
		if r.Action == install.ActionCreated || r.Action == install.ActionUpdated {
			return true
		}
	}
	return false
}

// clearHookHeartbeat drops the record `install --hooks` stamped, and the output directory
// with it when that record was the only thing in it.
//
// A stale "installed on X, never fired since" after the hooks were deliberately removed
// would be a false alarm, so the record goes with them. The directory has to go too: in a
// repository that has never been snapshotted, `install --hooks` is what created it, and
// uninstall that leaves an empty `.enola/` behind is not the byte-for-byte reversal the
// README promises.
//
// os.Remove is the guard as well as the mechanism — an output directory still holding a
// baseline or a snapshot refuses to go, and those are the user's work, not the installer's
// leftovers. The repoDir comparison covers an output directory configured as the
// repository itself, which is not ours to remove under any circumstances.
func clearHookHeartbeat(repoDir, outDir string) {
	hookstate.Clear(outDir)
	if filepath.Clean(outDir) != filepath.Clean(repoDir) {
		_ = os.Remove(outDir)
	}
}

// hookOutputDir resolves a repository's enola output directory without building an
// engine, so `install` neither pays for plugin registration nor emits the engine's
// config line in the middle of its own report.
//
// It mirrors what the hooks themselves do: a repo-local mcp-arch.yaml when there is
// one, built-in defaults otherwise. A config that cannot be read falls back to the
// defaults rather than failing the install — the heartbeat is a diagnostic, and a
// diagnostic must never be the reason an install breaks.
func hookOutputDir(repoDir string) string {
	cfg := config.Default()
	if p := filepath.Join(repoDir, "mcp-arch.yaml"); fileExists(p) {
		if loaded, err := config.Load(p); err == nil {
			cfg = loaded
		}
	}
	if err := cfg.Normalize(); err != nil {
		return ""
	}
	return cfg.OutputDirFor(repoDir)
}

func act(o install.Options, remove bool) ([]install.Result, error) {
	if remove {
		return install.Uninstall(o)
	}
	return install.Install(o)
}

func printPlan(rs []install.Result) {
	for _, r := range rs {
		line := fmt.Sprintf("  %-9s %s", r.Action, r.Path)
		if r.Reason != "" {
			line += "\n            " + r.Reason
		}
		fmt.Println(line)
	}
}

// samePlan reports whether applying produced exactly the plan that was previewed —
// same entries, same order, same reasons. Compared in full rather than by count: a
// `created` that came back `unchanged`, or a reason that changed, is precisely the
// divergence worth showing the whole list for.
func samePlan(planned, applied []install.Result) bool {
	if len(planned) != len(applied) {
		return false
	}
	for i := range planned {
		if planned[i] != applied[i] {
			return false
		}
	}
	return true
}

// countChanged counts the entries that actually touched a file, which is what the
// one-line summary reports. `skipped` and `unchanged` entries are in the plan for
// the reasons they carry, and counting them would overstate what was done.
func countChanged(rs []install.Result) int {
	n := 0
	for _, r := range rs {
		switch r.Action {
		case install.ActionCreated, install.ActionUpdated, install.ActionRemoved:
			n++
		}
	}
	return n
}

func appliedVerb(remove bool) string {
	if remove {
		return "Removed"
	}
	return "Written"
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// changesAnything reports whether any file would actually be touched, so an idempotent
// re-run says "nothing to do" instead of asking for confirmation it does not need.
func changesAnything(rs []install.Result) bool {
	for _, r := range rs {
		switch r.Action {
		case install.ActionCreated, install.ActionUpdated, install.ActionRemoved:
			return true
		}
	}
	return false
}

func confirm() bool {
	fmt.Print("\nApply these changes? [y/N] ")
	var answer string
	if _, err := fmt.Scanln(&answer); err != nil {
		return false
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes"
}

// hookBinary is the command the installed hook will invoke: the absolute path of the
// binary running this install, falling back to a bare "enola" only if that cannot be
// resolved.
//
// A bare "enola" is resolved through PATH at hook time, by whatever process the agent
// runs hooks in — which is not necessarily the binary the user just installed with. A
// stale enola earlier on PATH does not fail cleanly: an older build does not know the
// `hook` subcommand, treats its arguments as a config path, and starts an MCP server on
// stdio, so every agent turn hangs until the hook times out. Pinning the path makes the
// installed hook mean the binary the user actually chose.
func hookBinary() string {
	exe, err := os.Executable()
	if err != nil {
		return "enola"
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	// A path with spaces has to survive being re-split by whatever runs the command.
	if strings.ContainsAny(exe, " \t") {
		return `"` + exe + `"`
	}
	return exe
}

// wrap breaks text into lines of at most width characters, on word boundaries. The hook
// descriptions come from the installer rather than being written for this layout, so
// they have to be laid out here instead of being hand-fitted at their source.
func wrap(text string, width int) []string {
	words := strings.Fields(text)
	if len(words) == 0 {
		return nil
	}
	lines := []string{words[0]}
	for _, w := range words[1:] {
		last := len(lines) - 1
		if len(lines[last])+1+len(w) <= width {
			lines[last] += " " + w
			continue
		}
		lines = append(lines, w)
	}
	return lines
}

func (r *Runner) installFatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, r.name()+" install: "+format+"\n", args...)
	os.Exit(2)
}

// installDrift is what a plain `enola install` run from here would change in one scope,
// with the options that run would use: this binary in the hooks, and a folder of
// repositories treated as one. installed is false when nothing of enola's is there.
func installDrift(scope install.Scope, repoDir, home string) (changed []install.Result, installed bool, err error) {
	o := install.Options{Scope: scope, RepoDir: repoDir, HomeDir: home, HookCommand: hookBinary()}
	if scope == install.ScopeGlobal {
		if home == "" {
			return nil, false, nil
		}
	} else {
		o.FolderOfRepos = workspace.IsFolderOfRepos(repoDir)
	}
	return install.Drift(o)
}

// installCheck is `install --check`: whether this repository's and this user's install
// are what a plain `enola install` from this binary would leave, which `enola upgrade`
// asks the new binary right after replacing the old one. It returns the exit code: 1
// when something is behind, so a script can act on it.
func (r *Runner) installCheck(repoDir, home string) int {
	if abs, err := filepath.Abs(repoDir); err == nil {
		repoDir = abs
	}
	local := installStatusFor(install.ScopeLocal, repoDir, home)
	global := installStatusFor(install.ScopeGlobal, repoDir, home)
	if !local.Installed && !global.Installed && local.Error == "" && global.Error == "" {
		fmt.Printf("Nothing of enola's is installed for your agents in %s or your home directory.\n", repoDir)
		fmt.Printf("In each repository you work in, `%s install --hooks` wires your agents to enola.\n", r.name())
		return 0
	}
	r.printInstall(repoDir, home, local, global)
	fmt.Printf("Each other repository you ran `%s install` in is checked the same way by\n", r.name())
	fmt.Printf("`%s doctor` or `%s install --check` there.\n", r.name(), r.name())
	if len(local.Changes) > 0 || len(global.Changes) > 0 || local.Error != "" || global.Error != "" {
		return 1
	}
	return 0
}

// installDriftNote tells an agent session that what `enola install` wrote here is behind
// the installed binary, or returns "" when it is not. The session-start hook is the one
// thing that runs in every repository after an upgrade, including one made through pip
// or the install script, where `enola upgrade` never ran to say so.
//
// Worded like updatecheck's agent notice: the fact, and the decision left to the user.
// An agent told to run `enola install` would run it, and rewriting the user's agent
// configuration mid-task is not its call.
func installDriftNote(repoDir, home string) string {
	var where, cmds []string
	n := 0
	for _, s := range []struct {
		scope install.Scope
		name  string
		cmd   string
	}{
		{install.ScopeLocal, "in this repository", "`enola install`"},
		{install.ScopeGlobal, "for this user", "`enola install --global`"},
	} {
		rs, _, err := installDrift(s.scope, repoDir, home)
		if err != nil || len(rs) == 0 {
			continue
		}
		n += len(rs)
		where, cmds = append(where, s.name), append(cmds, s.cmd)
	}
	if n == 0 {
		return ""
	}
	verb, update := "differ", "update"
	if n == 1 {
		verb = "differs"
	}
	if len(cmds) == 1 {
		update = "updates"
	}
	return fmt.Sprintf("Separately: %s that `enola install` wrote %s %s from what the installed enola "+
		"writes, most likely left by an older version, so enola's instructions, hooks or skills are out of "+
		"date. %s %s them. Mention this to the user, who decides whether to run it. Do not run it yourself.",
		plural(n, "file"), strings.Join(where, " and "), verb, strings.Join(cmds, " and "), update)
}
