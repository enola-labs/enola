package cli

import (
	"fmt"
	"os"
)

// ShowDashboardHint reports whether a one-line dashboard discovery hint belongs in
// this process's output. Hints are useful in a terminal but are noise in CI logs,
// redirected scripts, and agent automation. Hook subcommands never call this helper;
// the environment and terminal checks keep the boundary explicit for other callers.
func ShowDashboardHint(out *os.File) bool {
	return shouldShowDashboardHint(os.Getenv("CI"), os.Getenv("ENOLA_NO_PROMPTS"), isTerminal(out))
}

func shouldShowDashboardHint(ci, noPrompts string, outputTTY bool) bool {
	return ci == "" && noPrompts == "" && outputTTY
}

func isTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// DashboardHint renders the one-line pointer to the dashboard that every action
// persisting snapshot artifacts (--generate, --refresh, check --write) prints on
// success. It is the single source for that wording so the three call sites — in
// two different packages, cmd/enola and pkg/command — cannot drift from each other.
// target is the repo or config path the action itself resolved, or "" to use the
// dashboard's own default.
func DashboardHint(binName, target string) string {
	open := binName + " dashboard --open"
	if target != "" {
		open += fmt.Sprintf(" %q", target)
	}
	return fmt.Sprintf("\nExplore this snapshot in your browser:\n  %s\nIt stays attached to the terminal; press Ctrl-C to stop it.\n", open)
}
