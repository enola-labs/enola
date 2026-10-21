// Package plain is the shared vocabulary of enola's plain-language summaries:
// the statement they are made of, how sure enola is of it, and how a summary is
// laid out in a terminal.
//
// Each command decides what its summary says. This package decides how it
// reads, so a reader who has seen one summary can find their way around the
// next, whichever command printed it.
package plain

import (
	"fmt"
	"strings"
)

// Item is one plain-language statement, optionally about a named module or
// symbol and with the command that shows the detail behind it.
type Item struct {
	Subject   string    `json:"subject,omitempty"`
	Location  string    `json:"location,omitempty"`
	Text      string    `json:"text"`
	Certainty Certainty `json:"certainty,omitempty"`
	Next      string    `json:"next,omitempty"`
}

// Certainty is how sure enola is of a statement, in words rather than a score.
type Certainty string

const (
	Certain Certainty = "certain"
	Likely  Certainty = "likely"
	Guess   Certainty = "a guess"
)

// CertaintyOf maps a finding's confidence to words.
func CertaintyOf(confidence float64) Certainty {
	switch {
	case confidence >= 0.9:
		return Certain
	case confidence >= 0.6:
		return Likely
	default:
		return Guess
	}
}

// Rank orders certainties most certain first, for sorting.
func (c Certainty) Rank() int {
	switch c {
	case Certain:
		return 0
	case Likely:
		return 1
	default:
		return 2
	}
}

// MaxItems caps every list in a summary. Three is what a reader new to the
// code can act on; the full report holds the rest.
const MaxItems = 3

// Plural writes n with the singular or plural form: "1 module", "3 modules".
func Plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// AndList joins words as English does: "a", "a and b", "a, b and c".
func AndList(words []string) string {
	switch len(words) {
	case 0:
		return ""
	case 1:
		return words[0]
	}
	return strings.Join(words[:len(words)-1], ", ") + " and " + words[len(words)-1]
}
