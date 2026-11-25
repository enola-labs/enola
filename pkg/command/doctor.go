package command

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/enola-labs/enola/internal/config"
	"github.com/enola-labs/enola/internal/engine"
	"github.com/enola-labs/enola/internal/hookstate"
	"github.com/enola-labs/enola/internal/providers"
	"github.com/enola-labs/enola/internal/updatecheck"
	"github.com/enola-labs/enola/internal/workspace"
	"github.com/enola-labs/enola/pkg/bootstrap"
	"github.com/enola-labs/enola/pkg/install"
)

// runDoctor is `enola doctor`: does the loop actually run in this repository?
//
// It exists because of a failure mode a test cannot cover. `enola install --hooks`
// wrote a configuration the agent silently ignored, so the hooks never fired — and
// nothing anywhere said so. The installer reported success, the hook binary worked when
// invoked by hand, and the agent simply never called it.
//
// The shape of that configuration is a contract owned by the agent, which ships on its
// own schedule and can change after enola is released. So the durable check is not a
// test in this repository — it is asking, on the machine where it matters, whether the
// hooks have fired lately. That is all this command does.
//
// Exit codes follow `enola coverage`, not `enola check`: this is a report, so it exits
// 0 whenever it ran, and 2 only for a usage error. A non-zero code from enola means
// "your change did something", and a diagnostic must not borrow that meaning.
func (r *Runner) Doctor(args []string) {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	asJSON := fs.Bool("json", false, "machine-readable output")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr,
			"Usage: "+r.name()+" doctor [flags] [repo_path]\n\n"+
				"Report whether enola's agent hooks are actually firing in this repository.\n\n"+
				"`install --hooks` can write a configuration the agent ignores — it reports\n"+
				"success either way. This asks the only question that settles it: when did the\n"+
				"hooks last run?\n\nIt also lists the files `install` wrote that differ from what this binary\nwould write, here and in your home directory, and enola's skills in each\ndirectory `install` writes them to.\n\nFlags:\n")
		fs.PrintDefaults()
		r.writeHelpFooter(fs.Output(), args, "doctor")
	}
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	repoDir, err := os.Getwd()
	if err != nil {
		r.cmdFatal("doctor", "cannot determine the working directory: %v", err)
	}
	if rest := fs.Args(); len(rest) > 0 {
		if !isDirectory(rest[0]) {
			r.cmdFatal("doctor", "%q is not a directory", rest[0])
		}
		repoDir = rest[0]
	}
	abs, err := filepath.Abs(repoDir)
	if err == nil {
		repoDir = abs
	}

	outDir := hookOutputDir(repoDir)
	state := hookstate.Load(outDir)
	where := hooksConfiguredIn(repoDir)
	installed := where != ""
	baseline := r.baselineUsability(repoDir, outDir)

	memLimit, memSource := bootstrap.MemoryLimit()
	// Zero — "nothing to say" — for a binary this manifest does not describe, so the
	// --json contract carries the same silence the text output does rather than an
	// answer to a question that was not asked. See Runner.selfUpgrades.
	var update updatecheck.Notice
	if r.selfUpgrades() {
		update = updatecheck.For(engine.ExtractorVersion())
	}

	home, _ := os.UserHomeDir()
	local := installStatusFor(install.ScopeLocal, repoDir, home)
	global := installStatusFor(install.ScopeGlobal, repoDir, home)

	if *asJSON {
		out := map[string]any{
			"install":           map[string]installStatus{"local": local, "global": global},
			"repo":              repoDir,
			"hooks_configured":  installed,
			"state":             state,
			"baseline_state":    baseline.State,
			"baseline_unusable": baseline.Reason,
			"memory_limit":      memLimit,
			"memory_limit_from": memSource,
			"update":            update,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
		return
	}

	fmt.Printf(r.name()+" doctor: %s\n\n", repoDir)

	// The runtime settings this process applied to itself. Reported here — and only
	// here, plus the server's startup log — because it belongs in a diagnostic and not
	// at the top of every command's output, which is where it used to be.
	fmt.Println("Runtime")
	fmt.Printf("  %s\n", bootstrap.MemoryLimitLine())
	fmt.Printf("  %s %s (extractors %s)\n", r.name(), r.buildVersion(), engine.ExtractorVersion())
	if eng, err := bootstrap.NewEngineFromConfig(config.Default()); err == nil {
		fmt.Printf("  %s\n", binderLine(eng.BindersByStage()))
	}
	fmt.Println()

	fmt.Println("Providers carried by this binary")
	for _, name := range providers.BuiltInNames() {
		fmt.Printf("  %s: %s\n", name, builtInStatus(name))
	}
	fmt.Println()

	// Reported unconditionally, unlike the one-line notice the other commands print:
	// this is the command someone runs when they want to know whether their setup is
	// healthy, so "you are on the current release" is an answer worth stating rather
	// than leaving to be inferred from silence.
	fmt.Println("Updates")
	switch {
	case !r.selfUpgrades():
		// The manifest this check reads advertises enola releases. A wrapper ships
		// on its own schedule with its own version numbers, so comparing the two
		// would not report an upgrade — it would report the distance between two
		// unrelated release streams. See Runner.selfUpgrades.
		fmt.Printf("  not checked (%s ships through its own release path)\n", r.name())
	case updatecheck.Suppressed():
		fmt.Println("  not checked (disabled, or this is a dev build)")
	case update.Available:
		fmt.Printf("  v%s is available — you have v%s\n", update.Latest, update.Current)
		if update.ExtractorMoved {
			fmt.Println("  Extractors changed since your build, so snapshots taken with it are")
			fmt.Println("  missing facts a current enola would extract.")
		}
		fmt.Printf("      %s upgrade\n", r.name())
	default:
		fmt.Println("  up to date, as far as the last check knows")
	}
	fmt.Println()

	// Asked and answered BEFORE a session ends, which is the difference between this
	// and reading the last outcome below: an unusable baseline makes the hooks decline
	// to grade, and they decline quietly. Knowing now beats finding out afterwards.
	// A folder of repositories is never graded, so there is no baseline to judge.
	folder := workspace.IsFolderOfRepos(repoDir)
	fmt.Println("Baseline")
	if folder {
		fmt.Println("  none — a folder of repositories is not graded; each repository is, by hooks")
		fmt.Println("  installed inside it")
	} else {
		switch baseline.State {
		case baselineComparable:
			fmt.Println("  comparable — the gate can grade against it")
		case baselineAbsent:
			fmt.Println("  none pinned — nothing to grade against yet. Pin one with")
			fmt.Printf("      %s baseline pin\n", r.name())
			fmt.Println("  or let the session-start hook pin it.")
		case baselineUnknown:
			fmt.Printf("  could not be judged: %s\n", baseline.Reason)
		default:
			fmt.Printf("  NOT COMPARABLE: %s\n", baseline.Reason)
			fmt.Println("  Nothing will be graded against it until it is re-pinned:")
			fmt.Printf("      %s baseline pin\n", r.name())
		}
	}
	fmt.Println()

	r.printInstall(repoDir, home, local, global)
	r.printSkills(repoDir)

	fmt.Println("Session hooks")

	if !installed {
		fmt.Println("  not configured for this repository.")
		if dir, _, ok := enclosingFolderHooks(repoDir); ok {
			printWrapped(fmt.Sprintf("Sessions started in %s get that folder's enola hooks, which point "+
				"the agent at enola's tools here too but grade nothing: grading needs a session started "+
				"inside this repository.", displayPath(dir)))
		}
		fmt.Printf("  Run `%s install --hooks` to have the loop run itself: a baseline pinned\n", r.name())
		fmt.Println("  at session start, and the change graded when the session ends.")
		return
	}
	fmt.Printf("  configured in %s\n", where)
	if !state.InstalledAt.IsZero() {
		fmt.Printf("  installed        %s (%s ago)\n",
			state.InstalledAt.Format(time.RFC3339), humanSince(state.InstalledAt))
	}
	if state.HookCommand != "" {
		fmt.Printf("  hook command     %s\n", state.HookCommand)
	}
	fmt.Println()

	if folder {
		// Only the hooks that steer the agent are installed in a folder of repositories.
		st := state.Get(hookstate.EventSessionStart)
		fmt.Printf("  %-15s %s\n", "session-start", "tells the agent how to reach enola's tools")
		if st == nil || st.Count == 0 {
			fmt.Printf("  %-15s NEVER FIRED\n", "")
		} else {
			fmt.Printf("  %-15s last ran %s ago · %d run(s)\n", "", humanSince(st.LastFired), st.Count)
		}
		fmt.Printf("  %-15s %s\n", "pre-tool", "refuses a session's first searches until an enola tool is used")
		fmt.Println()
		msg := ""
		if st != nil && st.Count > 0 {
			msg = "These hooks are working: they point the agent at enola's tools in whichever repository it works in. "
		}
		printWrapped(msg + "Grading a change (a baseline when the session starts, a report when it ends) needs a " +
			"session started inside one repository, with `" + r.name() + " install --hooks` run there. From this " +
			"folder, ask the agent to pin a baseline and diff against it, or run `" + r.name() + " baseline pin` " +
			"and `" + r.name() + " check` in the repository.")
		return
	}

	for _, e := range []struct {
		event hookstate.Event
		label string
		does  string
	}{
		{hookstate.EventSessionStart, "session-start", "pins the baseline your change is graded against"},
		{hookstate.EventStop, "stop", "grades the change when the session ends"},
	} {
		r := state.Get(e.event)
		fmt.Printf("  %-15s %s\n", e.label, e.does)
		if r == nil || r.Count == 0 {
			fmt.Printf("  %-15s NEVER FIRED\n", "")
			continue
		}
		fmt.Printf("  %-15s last ran %s ago · %d run(s) · last outcome: %s\n",
			"", humanSince(r.LastFired), r.Count, r.LastOutcome)
	}

	// The verdict, stated rather than left for the reader to assemble.
	fmt.Println()
	switch {
	case outcomeIs(state, hookstate.EventStop, hookstate.OutcomeNotARepo) ||
		outcomeIs(state, hookstate.EventSessionStart, hookstate.OutcomeNotARepo):
		fmt.Println("  The hooks are firing and doing nothing, on purpose: this is a folder of")
		fmt.Println("  repositories, not a repository, and grading it would snapshot all of them as")
		fmt.Printf("  one. Install the hooks inside each repository instead (`%s install --hooks`\n", r.name())
		fmt.Println("  run there).")
	case !state.Fired(hookstate.EventStop) && !state.Fired(hookstate.EventSessionStart) && folderHooksRan(repoDir) != "":
		// Sessions start in an enclosing folder of repositories, whose own hooks run
		// instead. Reporting that as a broken configuration sent people hunting for a
		// defect that is a choice of where to start the agent.
		fmt.Println("  NEVER RUN FROM A SESSION STARTED HERE.")
		printWrapped(folderHooksRan(repoDir))
		printWrapped("An agent runs the hooks of the folder its session starts in, so these run only for a " +
			"session started inside this repository. Changes made here from that folder are not graded " +
			"automatically: start the agent here, or pin and check yourself (`" + r.name() + " baseline pin`, " +
			"then `" + r.name() + " check`).")
	case !state.Fired(hookstate.EventStop) && !state.Fired(hookstate.EventSessionStart):
		fmt.Println("  NEITHER HOOK HAS EVER RUN.")
		fmt.Println("  The configuration exists but nothing is invoking it. Start a session in this")
		fmt.Println("  repository and re-run; if it still says this, the agent is not reading the")
		fmt.Println("  hooks — a shape enola writes that your agent version ignores would look")
		fmt.Println("  exactly like this. Please report it.")
	case !state.Fired(hookstate.EventStop):
		fmt.Println("  THE STOP HOOK HAS NEVER RUN — the half that reports regressions.")
		fmt.Println("  A baseline is being pinned and nothing is grading against it.")
	case state.Get(hookstate.EventStop).LastOutcome == hookstate.OutcomeDeclined:
		fmt.Println("  The stop hook is running, but last declined to grade: the baseline was not")
		fmt.Println("  comparable to the current snapshot. It stays silent in-session when this")
		fmt.Println("  happens, so it looks identical to a clean run. Re-pin with")
		fmt.Printf("  `%s baseline pin` and check `%s check` for the reason.\n", r.name(), r.name())
	case state.Get(hookstate.EventStop).LastOutcome == hookstate.OutcomeUnavailable:
		fmt.Println("  The stop hook is running but has nothing to grade against — usually no")
		fmt.Printf("  baseline yet. `%s baseline pin` fixes it; the session-start hook will\n", r.name())
		fmt.Println("  also do it on the next session.")
	default:
		fmt.Println("  Both hooks are firing. The loop is closed.")
	}
}

// baselineUsability judges the pinned baseline the way `baseline show` does (see
// judgeBaseline), from metadata alone, so `doctor` stays a report rather than
// becoming a snapshot.
func (r *Runner) baselineUsability(repoDir, outDir string) baselineJudgement {
	baseDir := engine.ResolveBaselineDir(outDir, "pinned")
	if _, err := readSnapshotMeta(baseDir); err != nil {
		// Answered before an engine is built, so a repository with no baseline costs
		// doctor no config resolution.
		return baselineJudgement{State: baselineAbsent, Reason: "no baseline pinned"}
	}
	eng, cfg, err := r.newEngine(bootstrap.Options{ConfigPath: configForRepo(repoDir)})
	if err != nil {
		return baselineJudgement{State: baselineUnknown, Reason: err.Error()}
	}
	cfg.Repo, cfg.Repos = repoDir, nil
	return judgeBaseline(eng, baseDir, []string{repoDir})
}

// hooksConfiguredIn names the file that installs enola's session hooks for this
// repository, or "" when none does: `.claude/settings.json` with an entry enola owns, or
// Pi's extension written with its hooks switched on.
func hooksConfiguredIn(repoDir string) string {
	if hooksConfigured(repoDir) {
		return filepath.Join(".claude", "settings.json")
	}
	rel := filepath.Join(".pi", filepath.FromSlash(install.PiExtensionFile))
	if data, err := os.ReadFile(filepath.Join(repoDir, rel)); err == nil &&
		strings.Contains(string(data), install.PiHooksMarker) {
		return rel
	}
	return ""
}

// hooksConfigured reports whether .claude/settings.json carries an entry enola owns.
//
// Deliberately a shallow scan for the marker rather than a shape check: whether the
// shape is one the agent honours is exactly the question this command cannot answer
// from the file, and pretending otherwise is how the original defect survived. The
// heartbeat answers it; this only establishes that something was written.
func hooksConfigured(repoDir string) bool {
	data, err := os.ReadFile(filepath.Join(repoDir, ".claude", "settings.json"))
	if err != nil {
		return false
	}
	var doc struct {
		Hooks map[string]json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return false
	}
	for _, raw := range doc.Hooks {
		if containsEnolaMarker(raw) {
			return true
		}
	}
	return false
}

// containsEnolaMarker walks arbitrary JSON looking for enola's ownership marker, so it
// keeps working if the surrounding structure changes shape again.
func containsEnolaMarker(raw json.RawMessage) bool {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return false
	}
	var walk func(any) bool
	walk = func(n any) bool {
		switch t := n.(type) {
		case map[string]any:
			if s, ok := t["source"].(string); ok && s == "enola" {
				return true
			}
			for _, child := range t {
				if walk(child) {
					return true
				}
			}
		case []any:
			for _, child := range t {
				if walk(child) {
					return true
				}
			}
		}
		return false
	}
	return walk(v)
}

// humanSince renders an age at the resolution a human cares about here — the question
// is "recently, or never", not "how many milliseconds".
func humanSince(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "less than a minute"
	case d < time.Hour:
		return fmt.Sprintf("%d minute(s)", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d hour(s)", int(d.Hours()))
	default:
		return fmt.Sprintf("%d day(s)", int(d.Hours()/24))
	}
}

// outcomeIs reports whether event last ran with outcome o. Nil-safe: an event that
// never fired has no record.
func outcomeIs(state hookstate.State, e hookstate.Event, o hookstate.Outcome) bool {
	r := state.Get(e)
	return r != nil && r.LastOutcome == o
}

// printSkills reports the skills directories `enola install` writes, against what this
// binary would write there: which of enola's skills are current, which an older or newer
// enola wrote, which are missing, and which name a skill of the user's. A home-directory
// location is listed only when it holds something, since most installs are per
// repository.
func (r *Runner) printSkills(repoDir string) {
	fmt.Println("Skills")
	home, _ := os.UserHomeDir()
	locs, err := install.SkillsReport(repoDir, home)
	if err != nil {
		fmt.Printf("  could not be read: %v\n\n", err)
		return
	}
	found := false
	var todo []string
	for _, l := range locs {
		if l.Global && !l.Installed() && !l.ServedByPlugin {
			continue
		}
		shown := l.Dir
		if l.Global {
			shown = "~" + strings.TrimPrefix(l.Dir, home)
		} else if rel, err := filepath.Rel(repoDir, l.Dir); err == nil {
			shown = rel
		}
		fmt.Printf("  %-18s %s\n", shown, "for "+l.Agents)
		if l.ServedByPlugin {
			fmt.Printf("  %-18s served by the enola Claude Code plugin\n", "")
		}
		byState := map[install.SkillState][]string{}
		for _, s := range l.Skills {
			byState[s.State] = append(byState[s.State], s.Name)
		}
		if !l.Installed() && !l.ServedByPlugin {
			fmt.Printf("  %-18s none installed\n", "")
		} else {
			found = true
		}
		// A skill of the user's under enola's name gets the sentence below, not a row.
		for _, st := range []install.SkillState{install.SkillCurrent, install.SkillOutdated, install.SkillMissing, install.SkillRetired} {
			names := byState[st]
			if len(names) == 0 || (st == install.SkillMissing && !l.Installed()) {
				continue
			}
			fmt.Printf("  %-18s %-9s %s\n", "", st, strings.Join(names, ", "))
			switch st {
			case install.SkillOutdated:
				todo = append(todo, shown+": differs from what this version writes (another version wrote it, or it was edited)")
			case install.SkillMissing:
				todo = append(todo, shown+": a skill this version ships is not there")
			case install.SkillRetired:
				todo = append(todo, shown+": a skill this version no longer installs there")
			}
		}
		if names := byState[install.SkillTheirs]; len(names) > 0 {
			fmt.Printf("  %-18s %s is a skill of yours under enola's name; install leaves it alone\n", "", strings.Join(names, ", "))
		}
	}
	switch {
	case !found:
		fmt.Printf("  No enola skills are installed. `%s install` writes them; docs/SKILLS.md says what each does.\n", r.name())
	case len(todo) > 0:
		fmt.Println()
		for _, t := range todo {
			fmt.Printf("  %s\n", t)
		}
		fmt.Printf("  Run `%s install` to bring them in line with this binary.\n", r.name())
	}
	fmt.Println()
}

// binderLine says how many binders this binary runs on every snapshot, by stage. A
// binder that fails is logged and skipped, and the run goes on; the snapshot's receipt
// lists the binders that ran without error, so the one missing there is the one to
// look for in the log.
func binderLine(preLink, postLink []string) string {
	return fmt.Sprintf("binders: %d run on every snapshot (%d pre-link, %d post-link); a snapshot's receipt lists those that ran without error",
		len(preLink)+len(postLink), len(preLink), len(postLink))
}

// installStatus is how one scope's install compares with what this binary writes.
type installStatus struct {
	Installed bool `json:"installed"`
	// Changes are the files a plain `enola install` would change, as "<action> <path>".
	Changes []string `json:"changes,omitempty"`
	Error   string   `json:"error,omitempty"`
}

func installStatusFor(scope install.Scope, repoDir, home string) installStatus {
	rs, installed, err := installDrift(scope, repoDir, home)
	st := installStatus{Installed: installed}
	if err != nil {
		st.Error = err.Error()
	}
	base := repoDir
	if scope == install.ScopeGlobal {
		base = home
	}
	for _, r := range rs {
		p := r.Path
		if rel, err := filepath.Rel(base, p); err == nil && !strings.HasPrefix(rel, "..") {
			p = rel
			if scope == install.ScopeGlobal {
				p = filepath.Join("~", rel)
			}
		}
		st.Changes = append(st.Changes, string(r.Action)+" "+p)
	}
	return st
}

// printInstall reports whether the files `enola install` wrote are the ones this binary
// would write: the instructions, the hooks and the extensions as well as the skills,
// which an upgrade leaves as the old version wrote them. The home directory is listed
// only when something of enola's is there, since most installs are per repository.
func (r *Runner) printInstall(repoDir, home string, local, global installStatus) {
	fmt.Println("Installed for your agents")
	behind := false
	row := func(label string, st installStatus) {
		switch {
		case st.Error != "":
			fmt.Printf("  %-18s could not be read: %s\n", label, st.Error)
		case !st.Installed:
			fmt.Printf("  %-18s nothing installed. `%s install --hooks` wires your agents to enola.\n", label, r.name())
		case len(st.Changes) == 0:
			fmt.Printf("  %-18s current with this binary\n", label)
		default:
			behind = true
			verb := "differ"
			if len(st.Changes) == 1 {
				verb = "differs"
			}
			fmt.Printf("  %-18s %s %s from what this binary writes\n", label, plural(len(st.Changes), "file"), verb)
			for _, c := range st.Changes {
				fmt.Printf("  %-18s   %s\n", "", c)
			}
		}
	}
	row("this repository", local)
	if global.Installed || global.Error != "" {
		row("~ (all projects)", global)
	}
	if behind {
		var cmds []string
		if len(local.Changes) > 0 {
			cmds = append(cmds, "`"+r.name()+" install`")
		}
		if len(global.Changes) > 0 {
			cmds = append(cmds, "`"+r.name()+" install --global`")
		}
		fmt.Printf("  Run %s to bring them in line. Hooks an earlier install wrote are\n", strings.Join(cmds, " and "))
		fmt.Println("  kept; restart your agent session afterwards.")
	}
	fmt.Println()
}

// enclosingFolderHooks finds the nearest directory above repoDir that is a folder of
// repositories with enola's hooks configured, and the record of those hooks. Found by
// walking up, never by name: where someone keeps their repositories is theirs.
func enclosingFolderHooks(repoDir string) (dir string, state hookstate.State, ok bool) {
	for d := filepath.Dir(repoDir); ; d = filepath.Dir(d) {
		if workspace.IsFolderOfRepos(d) && hooksConfiguredIn(d) != "" {
			return d, hookstate.Load(hookOutputDir(d)), true
		}
		if parent := filepath.Dir(d); parent == d {
			return "", hookstate.State{}, false
		}
	}
}

// folderHooksRan says where this repository's sessions start when an enclosing folder
// of repositories has enola hooks that have run, or "" when none has.
func folderHooksRan(repoDir string) string {
	dir, st, ok := enclosingFolderHooks(repoDir)
	if !ok || !st.Fired(hookstate.EventSessionStart) {
		return ""
	}
	return fmt.Sprintf("Sessions start in %s instead, a folder of repositories with its own enola hooks, "+
		"last run %s ago.", displayPath(dir), humanSince(st.Get(hookstate.EventSessionStart).LastFired))
}

// displayPath shortens a path under the home directory to ~/..., for reading only.
func displayPath(p string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if rel, err := filepath.Rel(home, p); err == nil && !strings.HasPrefix(rel, "..") {
			if rel == "." {
				return "~"
			}
			return filepath.Join("~", rel)
		}
	}
	return p
}

// printWrapped prints a paragraph indented under a doctor section.
func printWrapped(text string) {
	for _, line := range wrap(text, 76) {
		fmt.Println("  " + line)
	}
}
