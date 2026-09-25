package command

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/enola-labs/enola/internal/diff"
	"github.com/enola-labs/enola/internal/engine"
	"github.com/enola-labs/enola/internal/hookstate"
	"github.com/enola-labs/enola/internal/providers"
	"github.com/enola-labs/enola/internal/updatecheck"
	"github.com/enola-labs/enola/pkg/bootstrap"
	"github.com/enola-labs/enola/pkg/check"
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
				"hooks last run?\n\nFlags:\n")
		fs.PrintDefaults()
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
	baselineIssue := r.baselineUsability(repoDir, outDir)

	memLimit, memSource := bootstrap.MemoryLimit()
	// Zero — "nothing to say" — for a binary this manifest does not describe, so the
	// --json contract carries the same silence the text output does rather than an
	// answer to a question that was not asked. See Runner.selfUpgrades.
	var update updatecheck.Notice
	if r.selfUpgrades() {
		update = updatecheck.For(engine.ExtractorVersion())
	}

	if *asJSON {
		out := map[string]any{
			"repo":              repoDir,
			"hooks_configured":  installed,
			"state":             state,
			"baseline_unusable": baselineIssue,
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
	fmt.Println("Baseline")
	if baselineIssue == "" {
		fmt.Println("  comparable — the gate can grade against it")
	} else {
		fmt.Printf("  NOT COMPARABLE: %s\n", baselineIssue)
		fmt.Println("  Nothing will be graded against it until it is re-pinned:")
		fmt.Printf("      %s baseline pin\n", r.name())
	}
	fmt.Println()

	fmt.Println("Session hooks")

	if !installed {
		fmt.Println("  not configured for this repository.")
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

// baselineUsability returns why the pinned baseline could not be graded against, or
// "" when it can. It answers from metadata alone — no files are parsed — so `doctor`
// stays a report rather than becoming a snapshot.
func (r *Runner) baselineUsability(repoDir, outDir string) string {
	base, err := bootstrap.LoadSnapshotDir(engine.ResolveBaselineDir(outDir, "pinned"))
	if err != nil {
		return "" // no baseline pinned; the hooks section covers that case
	}
	eng, cfg, err := r.newEngine(bootstrap.Options{ConfigPath: configForRepo(repoDir)})
	if err != nil {
		return ""
	}
	cfg.Repo, cfg.Repos = repoDir, nil
	current := eng.CurrentMeta(repoDir)
	if current == nil {
		return ""
	}
	v := check.Evaluate(&diff.SnapshotDiff{Comparability: diff.CompareMeta(base.Meta, *current)}, check.Policy{})
	return v.DeclineReason()
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
