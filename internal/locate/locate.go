// Package locate maps places in source to the symbols the graph holds there, and a
// remembered symbol to where it is now.
//
// Anything that keeps facts about code across time (an agent's memory, a review
// bot, an issue tracker) holds them as locations: a `file:line` a finding pointed at,
// the lines a fix changed. A location is a poor key. The line moves when anything above
// it changes, and two notes about one function rarely name it the same way. The fact id
// is a good key, and the body hash says whether the code behind it changed. This
// package is the conversion between the two, so the caller never has to rebuild it from
// facts.jsonl.
package locate

import (
	"path"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// Symbol is one symbol fact as a lookup reports it.
type Symbol struct {
	ID         string `json:"id"`
	Repo       string `json:"repo,omitempty"`
	Name       string `json:"name"`
	SymbolKind string `json:"symbol_kind,omitempty"`
	File       string `json:"file"`
	Line       int    `json:"line"`
	EndLine    int    `json:"end_line,omitempty"`
	BodyHash   string `json:"body_hash,omitempty"`
	// DeclHash tells this declaration from others sharing its id (facts.PropDeclHash).
	DeclHash string `json:"decl_hash,omitempty"`
	// SpanStart is where the code starts when that is above Line: its decorators.
	SpanStart int `json:"span_start,omitempty"`
}

func (s Symbol) spanned() bool { return s.EndLine >= s.Line && s.Line > 0 }

// first is the first line of the symbol's code, its decorators included.
func (s Symbol) first() int {
	if s.SpanStart > 0 {
		return s.SpanStart
	}
	return s.Line
}

func (s Symbol) contains(line int) bool { return s.spanned() && s.first() <= line && line <= s.EndLine }

// How a location was matched to a symbol.
const (
	// MatchSpan is the innermost symbol whose span holds the line: exact.
	MatchSpan = "span"
	// MatchNearest is the nearest declaration above the line, for a file whose
	// extractor measures no spans. Usually the enclosing declaration, but a line after a
	// function's end is credited to it too.
	MatchNearest = "nearest"
	// MatchNone is a line no symbol's span holds: imports, top-level code, blank lines.
	MatchNone = "none"
	// MatchNoFile is a file the graph has no symbols for.
	MatchNoFile = "no_file"
)

// Index is the graph's symbols, by file and by identity.
type Index struct {
	byFile map[string][]Symbol // sorted by line
	files  []string            // sorted
	byID   map[string][]Symbol
	byMove map[string][]Symbol
	// spans records the files where at least one symbol has a span. Their extractor
	// measures spans, so a line no span holds is genuinely outside every symbol rather
	// than unmeasured.
	spans map[string]bool
	// source reads a graph file's current content; nil when there is no source to read
	// (a stored snapshot, a recorded revision). See SetSource.
	source  Source
	sources map[string][]byte
}

// Source reads the current content of a file the graph names, given the repository
// label its facts carry. ok is false when it cannot be read.
type Source func(repo, file string) (content []byte, ok bool)

// SetSource lets the index read files, which is what the cited-lines version needs: the
// facts hold a body's hash but not its lines. Without a source the index answers from
// the facts alone and reports no cited version.
func (ix *Index) SetSource(src Source) {
	ix.source = src
	ix.sources = map[string][]byte{}
}

// read returns a graph file's content through the source, once per file per index.
func (ix *Index) read(file string) ([]byte, bool) {
	if ix.source == nil {
		return nil, false
	}
	if content, seen := ix.sources[file]; seen {
		return content, content != nil
	}
	repo := ""
	if ss := ix.byFile[file]; len(ss) > 0 {
		repo = ss[0].Repo
	}
	content, ok := ix.source(repo, file)
	if !ok {
		content = nil
	}
	ix.sources[file] = content
	return content, ok
}

// NewIndex indexes the symbol facts among ff. It keeps only the fields a lookup reports,
// so the caller may hand it a store's own slice.
func NewIndex(ff []facts.Fact) *Index {
	ix := &Index{
		byFile: map[string][]Symbol{},
		byID:   map[string][]Symbol{},
		byMove: map[string][]Symbol{},
		spans:  map[string]bool{},
	}
	for i := range ff {
		f := &ff[i]
		if f.Kind != facts.KindSymbol || f.File == "" || f.Line <= 0 {
			continue
		}
		s := Symbol{
			ID:         f.Identity(),
			Repo:       f.Repo,
			Name:       f.Name,
			SymbolKind: f.PropString("symbol_kind"),
			File:       f.File,
			Line:       f.Line,
			EndLine:    f.EndLine,
			BodyHash:   f.PropString(facts.PropBodyHash),
			DeclHash:   f.PropString(facts.PropDeclHash),
		}
		if first := f.SpanStart(); first != f.Line {
			s.SpanStart = first
		}
		ix.byFile[s.File] = append(ix.byFile[s.File], s)
		ix.byID[s.ID] = append(ix.byID[s.ID], s)
		if s.BodyHash != "" {
			ix.byMove[moveKey(s)] = append(ix.byMove[moveKey(s)], s)
		}
		if s.spanned() {
			ix.spans[s.File] = true
		}
	}
	for file, ss := range ix.byFile {
		sort.SliceStable(ss, func(i, j int) bool { return ss[i].first() < ss[j].first() })
		ix.files = append(ix.files, file)
	}
	sort.Strings(ix.files)
	return ix
}

// moveKey is what a symbol keeps when it moves: its declared name and its body. The
// kind is left out so a stamp that did not record it can still be found; the declared
// name and body together already make a false pairing rare.
func moveKey(s Symbol) string {
	return declared(s.Name) + "\x00" + s.BodyHash
}

func declared(name string) string { return facts.DeclaredName(name) }

// ResolveFile maps a path as a caller wrote it to the file the graph knows it as.
//
// Callers hold paths in whatever form they were given: relative to the repository,
// relative to a package inside it, or absolute. Exact wins; otherwise the one graph file
// that ends with the given path, or that the given path ends with. Two candidates are not
// guessed between: the result is "" and the candidates are returned.
func (ix *Index) ResolveFile(p string) (string, []string) {
	p = strings.TrimPrefix(path.Clean(strings.ReplaceAll(p, "\\", "/")), "./")
	if _, ok := ix.byFile[p]; ok {
		return p, nil
	}
	var cands []string
	for _, f := range ix.files {
		if strings.HasSuffix(f, "/"+p) || strings.HasSuffix(p, "/"+f) {
			cands = append(cands, f)
		}
	}
	if len(cands) == 1 {
		return cands[0], nil
	}
	return "", cands
}

// At returns the symbol holding line in file, already resolved by ResolveFile, and how
// it was matched.
func (ix *Index) At(file string, line int) (Symbol, string) {
	ss, ok := ix.byFile[file]
	if !ok {
		return Symbol{}, MatchNoFile
	}
	if ix.spans[file] {
		best, found := Symbol{}, false
		for _, s := range ss {
			if s.first() > line {
				break
			}
			if !s.contains(line) {
				continue
			}
			// Innermost: the shortest span holding the line. Spans nest, so on a tie the
			// later start is the inner one.
			if !found || s.EndLine-s.first() < best.EndLine-best.first() ||
				(s.EndLine-s.first() == best.EndLine-best.first() && s.first() > best.first()) {
				best, found = s, true
			}
		}
		if found {
			return best, MatchSpan
		}
		return Symbol{}, MatchNone
	}
	i := sort.Search(len(ss), func(i int) bool { return ss[i].Line > line })
	if i == 0 {
		return Symbol{}, MatchNone
	}
	return ss[i-1], MatchNearest
}
