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

// PropDocText is the whole comment PropDoc summarizes, set only when it says more
// than PropDoc: a second paragraph, or a first one cut at the cap. It exists for the
// docs renderer. The engine moves it off the fact into the store's side table as
// soon as the fact is labeled (Store.TakeDocTexts), so facts.jsonl, the diff, the
// MCP tools and every explainer see only PropDoc.
const PropDocText = "doc_text"

// docTextLimit caps PropDocText. A doc comment longer than this is a manual pasted
// above a function, and a reader is better served by its opening than by all of it.
const docTextLimit = 8 << 10

// DocText reduces a comment's text to what PropDocText holds: its lines without
// the indentation they share, blank-line runs collapsed to one and none at either
// end, cut at a word past the cap. It returns "" for a comment with no text. The
// first line is not counted for the shared indentation, since a docstring's first
// line starts right after its quotes.
func DocText(text string) string {
	text = strings.ToValidUTF8(text, "\uFFFD")
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	indent := -1
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " \t\r")
		if i == 0 || lines[i] == "" {
			continue
		}
		n := len(lines[i]) - len(strings.TrimLeft(lines[i], " \t"))
		if indent < 0 || n < indent {
			indent = n
		}
	}
	var out []string
	blank := false
	for i, line := range lines {
		switch {
		case i == 0:
			line = strings.TrimLeft(line, " \t")
		case indent > 0 && len(line) >= indent:
			line = line[indent:]
		}
		if line == "" {
			blank = len(out) > 0
			continue
		}
		if blank {
			out = append(out, "")
			blank = false
		}
		out = append(out, line)
	}
	doc := strings.Join(out, "\n")
	if len(doc) <= docTextLimit {
		return doc
	}
	// The "..." counts toward the cap, so a text DocText returned is returned as it is.
	limit := docTextLimit - len("...")
	cut := strings.LastIndexAny(doc[:limit+1], " \n")
	if cut <= 0 {
		cut = limit
		for cut > 0 && doc[cut]&0xC0 == 0x80 {
			cut--
		}
	}
	return strings.TrimRight(doc[:cut], " \n") + "..."
}

// docProps returns PropDoc and, when the comment says more than it, PropDocText for
// a comment's text. The text is passed through DocText, which leaves one it already
// returned unchanged.
func docProps(text string) (doc, full string) {
	text = DocText(text)
	doc = DocSummary(text)
	if doc == "" {
		return "", ""
	}
	if strings.Join(strings.Fields(text), " ") == doc {
		return doc, ""
	}
	return doc, text
}

// SetDoc sets PropDoc, and PropDocText when it is needed, on f from a comment's
// whole text. It sets nothing for a comment with no text.
func SetDoc(f *Fact, text string) {
	doc, full := docProps(text)
	if doc == "" {
		return
	}
	f.SetProp(PropDoc, doc)
	if full != "" {
		f.SetProp(PropDocText, full)
	}
}

// SetDocProp is SetDoc for a props map not yet on a fact.
func SetDocProp(props map[string]any, text string) {
	doc, full := docProps(text)
	if doc == "" {
		return
	}
	props[PropDoc] = doc
	if full != "" {
		props[PropDocText] = full
	}
}

// CopyDoc gives dst the doc src carries, PropDocText included, replacing dst's.
func CopyDoc(dst *Fact, src Fact) {
	doc := src.PropString(PropDoc)
	if doc == "" {
		return
	}
	dst.SetProp(PropDoc, doc)
	if full := src.PropString(PropDocText); full != "" {
		dst.SetProp(PropDocText, full)
	} else {
		dst.DelProp(PropDocText)
	}
}

// PropFrameworkRoot names the mechanism through which a framework invokes a
// symbol: route, job, mailer, hook, task or callback. The framework-roots binder
// writes it and the flow ranking reads it, so it lives here beside the fact
// model rather than in either.
const PropFrameworkRoot = "root"
