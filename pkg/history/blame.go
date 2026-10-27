package history

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Blame answers the question a timeline exists for: WHEN did this appear, and when did it
// go away?
//
// "When did internal/server start importing internal/extractors" is unanswerable from a
// snapshot, however good the snapshot is — it is a question about the past, and a snapshot
// has none. It is answerable here by scanning the stored patches, which is why the patches
// are verbatim canonical lines rather than a compact encoding: the search is a substring
// match over text.
type Blame struct {
	// Pattern is what was searched for.
	Pattern string
	// Events are the appearances and disappearances, oldest first.
	Events []BlameEvent
	// Scanned is how many revisions were examined, and Skipped how many could not be
	// (their contents were dropped by retention). A blame that silently searched half the
	// history would answer "never" for something it simply could not see.
	Scanned int
	Skipped int
	Pruned  int
}

// BlameEvent is one appearance or disappearance.
type BlameEvent struct {
	Entry Entry
	// Added and Removed are the matching canonical lines that came or went at this
	// revision. Verbatim, because the line IS the fact — a symbol's file and signature, an
	// edge's endpoints — and paraphrasing it would lose the detail the question was about.
	Added   []string
	Removed []string
	// Changed pairs a line that left with the line that replaced it, when both are the
	// same symbol: one that only shifted, one whose body was rewritten, or one that moved
	// to another file. A paired line is in neither Added nor Removed, because nothing
	// came or went; a symbol changed.
	Changed []BlamePair `json:"changed,omitempty"`

	Origins []string `json:"origins,omitempty"`
}

// Kinds of BlamePair.
const (
	// PairShifted is one symbol whose body is the same: it moved within its file, or
	// something about it other than its code changed.
	PairShifted = "shifted"
	// PairRewritten is one symbol whose body changed.
	PairRewritten = "rewritten"
	// PairChanged is one symbol whose line changed where either side carries no body
	// version, so a shift cannot be told from a rewrite.
	PairChanged = "changed"
	// PairMoved is a symbol that left one file or name and arrived in another with the
	// same body and the same bare name. The id is keyed on the file, so it is a new id;
	// the pair is what lets a reader follow the function across it.
	PairMoved = "moved"
)

// BlamePair is a removed and an added line that are the same symbol.
type BlamePair struct {
	Kind string `json:"kind"`
	From string `json:"from"`
	To   string `json:"to"`

	// net is what the pair changes about Present: a move whose other end the pattern
	// does not match brought the symbol into the matched set, or took it out.
	net int
}

// BlameOptions narrows a blame.
type BlameOptions struct {
	// Findings searches the recorded FINDINGS rather than the facts — "when did this
	// cycle first appear" instead of "when did this symbol".
	Findings bool
	// FirstOnly stops at the first appearance. The common question ("when was this
	// introduced?") does not need the rest, and on a long history the rest is most of the
	// work.
	FirstOnly bool
}

// BlameLines walks the history forward and reports where lines matching pattern entered or
// left the graph.
//
// Forward, reconstructing as it goes, rather than scanning each stored patch independently
// — and the difference is not an optimisation, it is correctness. A segment BASE is a patch
// against the empty set, so every line in the graph appears as an addition in it. A scanner
// that read patches on their own would therefore report every symbol in the repository as
// "introduced" at each segment boundary, which on a long history is most of its answers.
// Comparing the matching subset of one reconstructed revision against the next asks the
// question that was actually asked: did this appear BETWEEN these two observations?
func BlameLines(root string, entries []Entry, pattern string, opts BlameOptions) (*Blame, error) {
	return BlameUnion(root, LocalUnion(entries), nil, pattern, opts)
}

func BlameUnion(localRoot string, revs []UnionRevision, sh *Share, pattern string, opts BlameOptions) (*Blame, error) {
	if pattern == "" {
		return nil, errors.New("blame needs something to look for")
	}
	needle := canonicalNeedle(pattern)
	b := &Blame{Pattern: pattern}

	var prev []string
	for _, u := range revs {
		factLines, insightLines, err := u.Lines(localRoot, sh)
		if err != nil {
			var pruned *PrunedError
			var gap *GapError
			switch {
			case errors.As(err, &pruned):
				b.Pruned++
				continue
			case errors.As(err, &gap), errors.Is(err, ErrThinned):
				b.Skipped++
				continue
			}
			return nil, fmt.Errorf("revision %s: %w", u.Entry.Short(), err)
		}
		b.Scanned++

		cur := factLines
		if opts.Findings {
			cur = insightLines
		}
		added, removed := matchDelta(prev, cur, needle)
		var changed []BlamePair
		if !opts.Findings {
			added, removed, changed = pairDelta(added, removed, prev, cur, needle)
		}
		prev = cur

		if len(added) == 0 && len(removed) == 0 && len(changed) == 0 {
			continue
		}
		b.Events = append(b.Events, BlameEvent{Entry: u.Entry, Added: added, Removed: removed, Changed: changed, Origins: u.Origins})
		if opts.FirstOnly && len(added) > 0 {
			break
		}
	}
	return b, nil
}

// matchDelta returns the matching lines present in cur and not prev, and vice versa.
//
// The comparison is over the MATCHING SUBSETS, not the whole sets: a revision that changed
// a thousand unrelated facts and left this one alone must produce no event, or a blame
// becomes a log.
func matchDelta(prev, cur []string, needle string) (added, removed []string) {
	prevMatch := matching(prev, needle)
	curMatch := matching(cur, needle)

	for line, n := range curMatch {
		for i := n - prevMatch[line]; i > 0; i-- {
			added = append(added, line)
		}
	}
	for line, n := range prevMatch {
		for i := n - curMatch[line]; i > 0; i-- {
			removed = append(removed, line)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	return added, removed
}

// canonicalNeedle puts a search pattern into the same encoding as the lines it will be
// compared against.
//
// Go's encoding/json escapes `<`, `>` and `&` as \u003c, \u003e and \u0026 — an HTML-safety
// default — so a dependency fact is stored with its arrow as `pkg/command -\u003e pkg/history`.
// Searching for the arrow a person would actually type therefore matches nothing at all:
// the most natural way to ask about an EDGE, which is the question blame exists for,
// silently answers "never". Found on the first attempt to use it that way.
//
// This is not special-casing the search; it is applying the writer's encoding to the query
// so both sides are in one alphabet. A pattern containing none of those characters passes
// through unchanged.
func canonicalNeedle(pattern string) string {
	// Assembled from a prefix rather than written out, so the sequences cannot be mistaken
	// for escapes by anything editing this file.
	const u = `\u00`
	r := strings.NewReplacer("<", u+"3c", ">", u+"3e", "&", u+"26")
	return strings.ToLower(r.Replace(pattern))
}

// matching counts the lines containing needle, case-insensitively.
//
// Case-insensitive because the caller is a person typing a name they half-remember, and a
// blame that answers "never" because the module is spelled `internal/Server` is worse than
// one that occasionally matches too much — the matches are printed, so too much is
// visible and too little is not.
func matching(lines []string, needle string) map[string]int {
	out := map[string]int{}
	for _, l := range lines {
		if strings.Contains(strings.ToLower(l), needle) {
			out[l]++
		}
	}
	return out
}

// Introduced returns the revision where the pattern first appeared, or false when it never
// did within the scanned range.
func (b *Blame) Introduced() (Entry, bool) {
	for _, ev := range b.Events {
		if len(ev.Added) > 0 {
			return ev.Entry, true
		}
	}
	return Entry{}, false
}

// Present reports whether anything matching was in the graph at the end of the walk.
func (b *Blame) Present() bool {
	present := 0
	for _, ev := range b.Events {
		present += len(ev.Added) - len(ev.Removed)
		for _, p := range ev.Changed {
			present += p.net
		}
	}
	return present > 0
}
