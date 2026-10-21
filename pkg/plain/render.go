package plain

import (
	"fmt"
	"strings"
)

// Width is the column a summary's prose wraps at, so it reads in an 80-column
// terminal without the terminal breaking words.
const Width = 78

// Header writes the banner a summary opens with: "<title>: <subject>" between
// two rules.
func Header(b *strings.Builder, title, subject string) {
	if subject == "" {
		subject = "(unknown)"
	}
	rule := strings.Repeat("═", 60)
	fmt.Fprintf(b, "%s\n", rule)
	fmt.Fprintf(b, " %s: %s\n", title, subject)
	fmt.Fprintf(b, "%s\n\n", rule)
}

// Section writes a heading, its items and an optional closing line. An item
// about a named module or symbol puts the name on its own line and the
// statement under it; an item with no subject is the statement alone.
//
// A statement enola is certain of is printed as it is. One it is less sure of
// carries its certainty, "(likely)" or "(a guess)", so the hedges stand out
// rather than being lost among a label on every line.
func Section(b *strings.Builder, title string, items []Item, footer string) {
	b.WriteString(title + "\n")
	for _, it := range items {
		text := it.Text
		if it.Certainty != "" && it.Certainty != Certain {
			text += " (" + string(it.Certainty) + ")"
		}
		if it.Subject == "" {
			Wrap(b, text, "  ", "  ")
			continue
		}
		head := "  " + it.Subject
		if it.Location != "" {
			if loc := "  (" + it.Location + ")"; len(head)+len(loc) <= Width {
				head += loc
			} else {
				head += "\n    at " + it.Location
			}
		}
		b.WriteString(head + "\n")
		Wrap(b, text, "    ", "    ")
		if it.Next != "" {
			b.WriteString("    See: " + it.Next + "\n")
		}
	}
	if footer != "" {
		Wrap(b, footer, "  ", "  ")
	}
	b.WriteString("\n")
}

// Wrap writes text as lines no wider than Width, the first indented by first
// and the rest by rest. A word longer than the line is written whole.
func Wrap(b *strings.Builder, text, first, rest string) {
	line := first
	empty := true
	for _, w := range strings.Fields(text) {
		if !empty && len(line)+1+len(w) > Width {
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
