package explain

import (
	"fmt"
	"strings"
)

// summaryWidth is the column the summary's prose wraps at, so it reads in an
// 80-column terminal without the terminal breaking words.
const summaryWidth = 78

// Render returns the summary as plain text for a terminal. The sections come in
// the same order every time, so a reader who has seen one summary can find
// their way around the next.
//
// A statement enola is certain of is printed as it is. One it is less sure of
// carries its certainty, "(likely)" or "(a guess)", so the hedges stand out
// rather than being lost among a label on every line.
func (s *Summary) Render() string {
	var b strings.Builder

	repo := s.RepoPath
	if repo == "" {
		repo = "(unknown)"
	}
	rule := strings.Repeat("═", 60)
	fmt.Fprintf(&b, "%s\n", rule)
	fmt.Fprintf(&b, " Repository summary: %s\n", repo)
	fmt.Fprintf(&b, "%s\n\n", rule)

	more := ""
	if s.MoreFindings > 0 {
		more = plural(s.MoreFindings, "more kind of finding is", "more kinds of finding are") + " in the full report."
	}
	section(&b, "What this is", s.About, "")
	section(&b, "Structure: "+s.Health.Verdict, s.Health.Items, "")
	section(&b, "Start reading here", s.StartHere, "")
	section(&b, "Careful when changing", s.Careful, "")
	section(&b, "Worth a look (most certain first)", s.WorthALook, more)
	section(&b, "Take with a grain of salt", s.Caveats, "")
	section(&b, "Next", s.Next, "")
	return b.String()
}

// section prints a heading, its items and an optional closing line. An item
// about a named module or symbol puts the name on its own line and the
// statement under it; an item with no subject is the statement alone.
func section(b *strings.Builder, title string, items []Item, footer string) {
	b.WriteString(title + "\n")
	for _, it := range items {
		text := it.Text
		if it.Certainty != "" && it.Certainty != Certain {
			text += " (" + string(it.Certainty) + ")"
		}
		if it.Subject == "" {
			wrap(b, text, "  ", "  ")
			continue
		}
		head := "  " + it.Subject
		if it.Location != "" {
			if loc := "  (" + it.Location + ")"; len(head)+len(loc) <= summaryWidth {
				head += loc
			} else {
				head += "\n    at " + it.Location
			}
		}
		b.WriteString(head + "\n")
		wrap(b, text, "    ", "    ")
		if it.Next != "" {
			b.WriteString("    See: " + it.Next + "\n")
		}
	}
	if footer != "" {
		wrap(b, footer, "  ", "  ")
	}
	b.WriteString("\n")
}

// wrap writes text as lines no wider than summaryWidth, the first indented by
// first and the rest by rest. A word longer than the line is written whole.
func wrap(b *strings.Builder, text, first, rest string) {
	line := first
	empty := true
	for _, w := range strings.Fields(text) {
		if !empty && len(line)+1+len(w) > summaryWidth {
			b.WriteString(line + "\n")
			line, empty = rest, true
		}
		if !empty {
			line += " "
		}
		line += w
		empty = false
	}
	b.WriteString(line + "\n")
}
