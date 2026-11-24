package locate

import (
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// How a name was matched to symbols.
const (
	// NameOne is a name exactly one declaration answers to.
	NameOne = "one"
	// NameSeveral is a name more than one declaration answers to. All are returned and
	// none is picked: which one a caller meant is in its own text, not in the graph.
	NameSeveral = "several"
	// NameNone is a name no symbol in the graph answers to.
	NameNone = "none"
)

// NameResult is what a name resolves to.
type NameResult struct {
	Name    string        `json:"name"`
	Match   string        `json:"match"`
	Symbols []NamedSymbol `json:"symbols,omitempty"`
}

// NamedSymbol is a symbol a name resolved to, with the version of its declaration line,
// so a caller can store it as a stamp without a second lookup.
type NamedSymbol struct {
	Symbol
	CitedHash  string `json:"cited_hash,omitempty"`
	CitedLines int    `json:"cited_lines,omitempty"`
}

// Names resolves each name to the symbols it names.
//
// Notes and memories name code by its identifier, not by its line: `nodeFor`,
// `Graph.nodeFor`, `internal/facts.Graph.nodeFor`. A symbol answers to a name when its
// qualified name is the name or ends in it at a `.` or `/` boundary, case included, so
// `nodeFor` finds `Graph.nodeFor` but not `Graph.nodeForID` or `NodeFor`. Qualifying the
// name narrows it; a trailing `()` is ignored. One call resolves every name, which a substring search over the
// facts cannot do.
func (ix *Index) Names(names []string) []NameResult {
	byLast := map[string][]Symbol{}
	for _, f := range ix.files {
		for _, s := range ix.byFile[f] {
			byLast[lastSegment(s.Name)] = append(byLast[lastSegment(s.Name)], s)
		}
	}
	out := make([]NameResult, 0, len(names))
	for _, name := range names {
		r := NameResult{Name: name, Match: NameNone}
		name = strings.TrimSuffix(strings.TrimSpace(name), "()")
		for _, s := range byLast[lastSegment(name)] {
			if namesSymbol(s.Name, name) {
				r.Symbols = append(r.Symbols, ix.named(s))
			}
		}
		sort.SliceStable(r.Symbols, func(i, j int) bool {
			a, b := r.Symbols[i], r.Symbols[j]
			if a.File != b.File {
				return a.File < b.File
			}
			return a.Line < b.Line
		})
		switch len(r.Symbols) {
		case 0:
		case 1:
			r.Match = NameOne
		default:
			r.Match = NameSeveral
		}
		out = append(out, r)
	}
	return out
}

// named is s with the version of its declaration line, cited as locate cites one line.
func (ix *Index) named(s Symbol) NamedSymbol {
	n := NamedSymbol{Symbol: s}
	content, ok := ix.read(s.File)
	if !ok || !s.spanned() {
		return n
	}
	from, to := citedRange(s.Line, s.Line, &s)
	if lines, inside := facts.NormalizedLines(content, from, to); inside && len(lines) > 0 {
		n.CitedHash, n.CitedLines = facts.HashLines(lines), len(lines)
	}
	return n
}

// namesSymbol reports whether name names the symbol called qualified: the whole of it, or
// its end at a `.` or `/` boundary.
func namesSymbol(qualified, name string) bool {
	if name == "" {
		return false
	}
	if qualified == name {
		return true
	}
	if !strings.HasSuffix(qualified, name) {
		return false
	}
	b := qualified[len(qualified)-len(name)-1]
	return b == '.' || b == '/'
}

// lastSegment is the part of a name after its last `.` or `/`.
func lastSegment(name string) string {
	return name[strings.LastIndexAny(name, "./")+1:]
}
