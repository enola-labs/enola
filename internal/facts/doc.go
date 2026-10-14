package facts

import "strings"

// PropDoc is the first paragraph of the comment a symbol's author wrote on it:
// a docstring, or the comment block above a declaration. It is descriptive,
// and what a reader is given when a symbol is named without its source.
//
// It is the one prop that is prose. A reworded comment changes it and changes
// nothing about the architecture, which is why the diff leaves it out of what
// it compares.
const PropDoc = "doc"

// docLimit caps a doc prop. A first paragraph is usually a sentence; the cap is
// for the ones that are a page, since every symbol in a graph may carry one.
const docLimit = 300

// DocSummary reduces a comment's text to what PropDoc holds: its first
// paragraph on one line, cut at a word when it runs past the cap. It returns ""
// for a comment with no text.
//
// The text is made valid UTF-8 first. A comment saved in another encoding (a
// Windows-1252 apostrophe in a C# file) holds bytes that are not, and an invalid
// byte is written to facts.jsonl as `\ufffd` while the same text read back from the
// extractor cache is written as the replacement character itself: one snapshot,
// two encodings, and a cold run that does not reproduce on a warm one.
func DocSummary(text string) string {
	text = strings.ToValidUTF8(text, "\uFFFD")
	var words []string
	started := false
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			if started {
				break
			}
			continue
		}
		started = true
		words = append(words, fields...)
	}
	doc := strings.Join(words, " ")
	if len(doc) <= docLimit {
		return doc
	}
	cut := strings.LastIndexByte(doc[:docLimit+1], ' ')
	if cut <= 0 {
		// One unbroken run: cut on a rune boundary.
		cut = docLimit
		for cut > 0 && doc[cut]&0xC0 == 0x80 {
			cut--
		}
	}
	return strings.TrimRight(doc[:cut], " ") + "..."
}

// PropFrameworkRoot names the mechanism through which a framework invokes a
// symbol: route, job, mailer, hook, task or callback. The framework-roots binder
// writes it and the flow ranking reads it, so it lives here beside the fact
// model rather than in either.
const PropFrameworkRoot = "root"
