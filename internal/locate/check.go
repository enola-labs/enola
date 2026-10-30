package locate

import (
	"sort"

	"github.com/enola-labs/enola/internal/facts"
)

// Stamp is a symbol as something remembered it: its id and the version of its body at
// the time. Name and SymbolKind are optional; without them a symbol that moved to
// another file cannot be found again, because the id is a hash and does not say what it
// named.
type Stamp struct {
	ID       string `json:"id"`
	BodyHash string `json:"body_hash,omitempty"`
	// DeclHash says which declaration the stamp means when several share its id. Present
	// only if they already did when it was stamped.
	DeclHash   string `json:"decl_hash,omitempty"`
	Name       string `json:"name,omitempty"`
	SymbolKind string `json:"symbol_kind,omitempty"`
	// CitedHash and CitedLines are the version of the lines the remembered location
	// cited, as locate reported them (LocationResult). Optional.
	CitedHash  string `json:"cited_hash,omitempty"`
	CitedLines int    `json:"cited_lines,omitempty"`
}

// What became of a stamped symbol.
const (
	// StatusCurrent is the same symbol with the same body: what was remembered about it
	// still describes the code.
	StatusCurrent = "current"
	// StatusRewritten is the same symbol with a different body.
	StatusRewritten = "rewritten"
	// StatusPresent is the same symbol where either side has no body version, so it
	// cannot be told whether it changed.
	StatusPresent = "present"
	// StatusMoved is a symbol under a new id, in another file or package, with the same
	// declared name and the same body.
	StatusMoved = "moved"
	// StatusGone is a symbol no longer in the graph. Candidates lists symbols with the
	// same declared name, which may be it moved and rewritten at once.
	StatusGone = "gone"
	// StatusAmbiguous is a stamp that cannot be tied to one symbol: Candidates lists what it
	// may be, and Symbol is empty. Saying so is the answer; picking one would report on the
	// wrong code.
	StatusAmbiguous = "ambiguous"
)

// What became of the lines a stamp cited, beside what became of its function.
const (
	// CitedIntact is the cited lines still inside the function's body, wherever in it:
	// whatever changed in the function, it was not them.
	CitedIntact = "intact"
	// CitedChanged is the cited lines no longer found in the function's body.
	CitedChanged = "changed"
)

// CheckResult is what became of one stamp.
type CheckResult struct {
	Stamp  Stamp   `json:"stamp"`
	Status string  `json:"status"`
	Symbol *Symbol `json:"symbol,omitempty"`
	// Cited is what became of the cited lines, when the stamp carries their version and
	// the symbol is still there: CitedIntact or CitedChanged. Empty when that cannot be
	// told (no cited version, no source to read, a symbol with no span, or gone).
	Cited      string   `json:"cited,omitempty"`
	Candidates []Symbol `json:"candidates,omitempty"`
}

// maxCandidates caps the same-name symbols a gone stamp lists.
const maxCandidates = 5

// Check reports, for each stamp, whether the symbol is still there and still the same.
func (ix *Index) Check(stamps []Stamp) []CheckResult {
	out := make([]CheckResult, 0, len(stamps))
	for _, st := range stamps {
		out = append(out, ix.check(st))
	}
	return out
}

func (ix *Index) check(st Stamp) CheckResult {
	r := ix.checkSymbol(st)
	r.Cited = ix.cited(st, r)
	return r
}

// cited reports whether the stamp's cited lines are still in the symbol it resolved to.
//
// A function's version changes with any edit to it, and a note about three lines of a
// four-hundred-line handler is not made wrong by an edit two hundred lines away. The
// cited lines are looked for as a run anywhere in the current body, so code inserted
// above them, or the function moving, leaves them intact.
func (ix *Index) cited(st Stamp, r CheckResult) string {
	if st.CitedHash == "" || st.CitedLines <= 0 || r.Symbol == nil {
		return ""
	}
	switch r.Status {
	case StatusCurrent, StatusMoved:
		return CitedIntact // the same body holds the same lines
	case StatusRewritten:
	default:
		return ""
	}
	content, ok := ix.read(r.Symbol.File)
	if !ok || !r.Symbol.spanned() {
		return ""
	}
	body, inside := facts.NormalizedLines(content, r.Symbol.first(), r.Symbol.EndLine)
	if !inside {
		return ""
	}
	for i := 0; i+st.CitedLines <= len(body); i++ {
		if facts.HashLines(body[i:i+st.CitedLines]) == st.CitedHash {
			return CitedIntact
		}
	}
	return CitedChanged
}

func (ix *Index) checkSymbol(st Stamp) CheckResult {
	r := CheckResult{Stamp: st}
	if same := ix.byID[st.ID]; len(same) > 0 {
		s, candidates := ix.resolveDeclaration(st, same)
		if s == nil {
			r.Status, r.Candidates = StatusAmbiguous, candidates
			return r
		}
		r.Symbol = s
		switch {
		case st.BodyHash == "" || s.BodyHash == "":
			r.Status = StatusPresent
		case s.BodyHash == st.BodyHash:
			r.Status = StatusCurrent
		default:
			r.Status = StatusRewritten
		}
		return r
	}

	r.Status = StatusGone
	if st.Name == "" {
		return r
	}
	if st.BodyHash != "" {
		var moved []Symbol
		for _, c := range ix.byMove[moveKey(Symbol{Name: st.Name, BodyHash: st.BodyHash})] {
			if st.SymbolKind == "" || c.SymbolKind == st.SymbolKind {
				moved = append(moved, c)
			}
		}
		if len(moved) == 1 {
			r.Status, r.Symbol = StatusMoved, &moved[0]
			return r
		}
		if len(moved) > 1 {
			// Two copies of one body under one name: a move cannot be told from a copy,
			// so both are reported and neither is picked.
			r.Candidates = moved
			return r
		}
	}
	r.Candidates = ix.sameDeclaredName(st)
	return r
}

// resolveDeclaration picks, among the declarations holding a stamp's id, the one it means:
// the stamped body if it is still there, else the stamped signature, else the stamped
// kind. It returns nil and what remains when those do not settle it.
func (ix *Index) resolveDeclaration(st Stamp, same []Symbol) (*Symbol, []Symbol) {
	if st.BodyHash != "" {
		for i := range same {
			if same[i].BodyHash == st.BodyHash {
				return &same[i], nil
			}
		}
	}
	if len(same) == 1 {
		if !ix.mayBeSameDeclaration(st, same[0]) {
			return nil, same
		}
		return &same[0], nil
	}
	remaining := same
	if byDecl := filterSymbols(remaining, func(s Symbol) bool { return st.DeclHash != "" && s.DeclHash == st.DeclHash }); len(byDecl) > 0 {
		remaining = byDecl
	}
	if byKind := filterSymbols(remaining, func(s Symbol) bool { return st.SymbolKind != "" && s.SymbolKind == st.SymbolKind }); len(byKind) > 0 {
		remaining = byKind
	}
	if len(remaining) == 1 {
		return &remaining[0], nil
	}
	return nil, remaining
}

// mayBeSameDeclaration checks a stamp that named one of several declarations against the
// only one left. A symbol alone under its id carries no decl_hash, so its signature is
// read from the source when there is one; without source only the kind can object. A
// mismatch is a sibling that survived, or the stamped one with a new signature, and the
// two look alike.
func (ix *Index) mayBeSameDeclaration(st Stamp, s Symbol) bool {
	if st.DeclHash == "" {
		return true
	}
	if st.SymbolKind != "" && s.SymbolKind != "" && st.SymbolKind != s.SymbolKind {
		return false
	}
	if s.DeclHash != "" {
		return s.DeclHash == st.DeclHash
	}
	if content, ok := ix.read(s.File); ok && s.spanned() {
		if h := facts.DeclHash(content, s.Line, s.EndLine); h != "" {
			return h == st.DeclHash
		}
	}
	return true
}

func filterSymbols(ss []Symbol, keep func(Symbol) bool) []Symbol {
	var out []Symbol
	for _, s := range ss {
		if keep(s) {
			out = append(out, s)
		}
	}
	return out
}

func (ix *Index) sameDeclaredName(st Stamp) []Symbol {
	want := declared(st.Name)
	var out []Symbol
	for _, file := range ix.files {
		for _, s := range ix.byFile[file] {
			if declared(s.Name) == want && (st.SymbolKind == "" || s.SymbolKind == st.SymbolKind) {
				out = append(out, s)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	if len(out) > maxCandidates {
		out = out[:maxCandidates]
	}
	return out
}
