package check

import (
	"strings"

	"github.com/enola-labs/enola/pkg/plain"
)

// Render returns the summary as plain text for a terminal, in the same order
// on every outcome.
func (s *Summary) Render() string {
	var b strings.Builder
	plain.Header(&b, "Change summary", s.Repo)

	more := ""
	if s.MoreWorse > 0 {
		more = plain.Plural(s.MoreWorse, "more is", "more are") + " in the full verdict."
	}
	plain.Section(&b, "Result: "+s.Result.Outcome, s.Result.Items, "")
	plain.Section(&b, "What you changed", s.Changed, "")
	plain.Section(&b, "Got worse", s.Worse, more)
	plain.Section(&b, "Got better", s.Better, "")
	plain.Section(&b, "Neither better nor worse", s.Neither, "")
	plain.Section(&b, "Could not see", s.CouldNotSee, "")
	plain.Section(&b, "Next", s.Next, "")
	return b.String()
}
