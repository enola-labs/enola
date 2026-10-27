package locate

import "sort"

// Stamp is a symbol as something remembered it: its id and the version of its body at
// the time. Name and SymbolKind are optional; without them a symbol that moved to
// another file cannot be found again, because the id is a hash and does not say what it
// named.
type Stamp struct {
	ID         string `json:"id"`
	BodyHash   string `json:"body_hash,omitempty"`
	Name       string `json:"name,omitempty"`
	SymbolKind string `json:"symbol_kind,omitempty"`
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
)

// CheckResult is what became of one stamp.
type CheckResult struct {
	Stamp      Stamp    `json:"stamp"`
	Status     string   `json:"status"`
	Symbol     *Symbol  `json:"symbol,omitempty"`
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
	r := CheckResult{Stamp: st}
	if same := ix.byID[st.ID]; len(same) > 0 {
		// An id can cover several facts (the same declaration recorded twice); the one
		// with the stamped body is the stamped one if it is still there.
		s := same[0]
		for _, c := range same {
			if c.BodyHash == st.BodyHash {
				s = c
				break
			}
		}
		r.Symbol = &s
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
