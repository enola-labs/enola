package explain

import (
	"strings"

	"github.com/enola-labs/enola/pkg/plain"
)

// Render returns the summary as plain text for a terminal. The sections come in
// the same order every time, so a reader who has seen one summary can find
// their way around the next.
func (s *Summary) Render() string {
	var b strings.Builder
	plain.Header(&b, "Repository summary", s.RepoPath)

	more := ""
	if s.MoreFindings > 0 {
		more = plain.Plural(s.MoreFindings, "more kind of finding is", "more kinds of finding are") + " in the full report."
	}
	plain.Section(&b, "What this is", s.About, "")
	plain.Section(&b, "Structure: "+s.Health.Verdict, s.Health.Items, "")
	plain.Section(&b, "Start reading here", s.StartHere, "")
	plain.Section(&b, "Careful when changing", s.Careful, "")
	plain.Section(&b, "Worth a look (most certain first)", s.WorthALook, more)
	plain.Section(&b, "Take with a grain of salt", s.Caveats, "")
	plain.Section(&b, "Next", s.Next, "")
	return b.String()
}
