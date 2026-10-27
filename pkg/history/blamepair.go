package history

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// pairDelta finds, among a revision's matching additions and removals, the ones that are
// the same symbol, and returns the rest unpaired.
//
// A fact line holds the fact's line number, so a function that only shifted down by one
// leaves the graph as one line and comes back as another. Reported verbatim that is a
// removal and an addition, which reads as "deleted and re-created" and is indistinguishable
// from a real rewrite. Pairing by identity (kind, name, file, repo) turns it back into one
// symbol that changed, and the body version says how: shifted when the body is the same,
// rewritten when it is not.
//
// A symbol moved to another file has a new identity, so identity cannot pair it. Its body
// version and its name inside its module can: a line that left and a line that arrived
// with the same kind, declared name and body hash are reported as a move. The other end of a move often does not
// match the pattern (blaming the new path finds the arrival, not the departure), so it is
// looked up among all the revision's changes, not only the matching ones.
func pairDelta(added, removed, prev, cur []string, needle string) (outAdded, outRemoved []string, pairs []BlamePair) {
	if len(added) == 0 && len(removed) == 0 {
		return added, removed, nil
	}
	adds := parseFactLines(added)
	rems := parseFactLines(removed)

	byIdentity := map[string][]int{}
	for i, r := range rems {
		if r.ok {
			byIdentity[r.identity()] = append(byIdentity[r.identity()], i)
		}
	}
	for i := range adds {
		a := &adds[i]
		if !a.ok {
			continue
		}
		cands := byIdentity[a.identity()]
		if len(cands) == 0 {
			continue
		}
		j := cands[0]
		byIdentity[a.identity()] = cands[1:]
		pairs = append(pairs, BlamePair{Kind: identityPairKind(rems[j], *a), From: rems[j].raw, To: a.raw})
		a.paired, rems[j].paired = true, true
	}

	// Moves between two matching lines first, then a matching line against the rest of
	// the revision's changes.
	byBody := map[string][]int{}
	for i, r := range rems {
		if r.movable() {
			byBody[r.moveKey()] = append(byBody[r.moveKey()], i)
		}
	}
	for i := range adds {
		a := &adds[i]
		if !a.movable() {
			continue
		}
		if cands := byBody[a.moveKey()]; len(cands) > 0 {
			j := cands[0]
			byBody[a.moveKey()] = cands[1:]
			pairs = append(pairs, BlamePair{Kind: PairMoved, From: rems[j].raw, To: a.raw})
			a.paired, rems[j].paired = true, true
		}
	}

	var otherAdded, otherRemoved map[string][]factLine
	others := func() {
		if otherAdded != nil {
			return
		}
		allAdded, allRemoved := matchDelta(prev, cur, "")
		otherAdded = movableOutside(allAdded, needle)
		otherRemoved = movableOutside(allRemoved, needle)
	}
	for i := range adds {
		a := &adds[i]
		if !a.movable() {
			continue
		}
		others()
		if cands := otherRemoved[a.moveKey()]; len(cands) > 0 {
			otherRemoved[a.moveKey()] = cands[1:]
			pairs = append(pairs, BlamePair{Kind: PairMoved, From: cands[0].raw, To: a.raw, net: 1})
			a.paired = true
		}
	}
	for i := range rems {
		r := &rems[i]
		if !r.movable() {
			continue
		}
		others()
		if cands := otherAdded[r.moveKey()]; len(cands) > 0 {
			otherAdded[r.moveKey()] = cands[1:]
			pairs = append(pairs, BlamePair{Kind: PairMoved, From: r.raw, To: cands[0].raw, net: -1})
			r.paired = true
		}
	}

	for _, a := range adds {
		if !a.paired {
			outAdded = append(outAdded, a.raw)
		}
	}
	for _, r := range rems {
		if !r.paired {
			outRemoved = append(outRemoved, r.raw)
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].To != pairs[j].To {
			return pairs[i].To < pairs[j].To
		}
		return pairs[i].From < pairs[j].From
	})
	return outAdded, outRemoved, pairs
}

func identityPairKind(from, to factLine) string {
	switch {
	case from.body == "" || to.body == "":
		return PairChanged
	case from.body == to.body:
		return PairShifted
	default:
		return PairRewritten
	}
}

// movableOutside indexes the changed lines that do not match the pattern by move key.
func movableOutside(lines []string, needle string) map[string][]factLine {
	out := map[string][]factLine{}
	for _, l := range lines {
		if !strings.Contains(l, `"`+facts.PropBodyHash+`"`) || strings.Contains(strings.ToLower(l), needle) {
			continue
		}
		f := parseFactLine(l)
		if f.movable() {
			out[f.moveKey()] = append(out[f.moveKey()], f)
		}
	}
	return out
}

// factLine is the part of a canonical fact line that pairing reads.
type factLine struct {
	raw                    string
	kind, name, file, repo string
	body                   string
	ok, paired             bool
}

func parseFactLines(lines []string) []factLine {
	out := make([]factLine, len(lines))
	for i, l := range lines {
		out[i] = parseFactLine(l)
	}
	return out
}

func parseFactLine(line string) factLine {
	var f struct {
		Kind  string `json:"kind"`
		Name  string `json:"name"`
		File  string `json:"file"`
		Repo  string `json:"repo"`
		Props struct {
			Body string `json:"body_hash"`
		} `json:"props"`
	}
	if err := json.Unmarshal([]byte(line), &f); err != nil || f.Kind == "" || f.Name == "" {
		return factLine{raw: line}
	}
	return factLine{raw: line, kind: f.Kind, name: f.Name, file: f.File, repo: f.Repo, body: f.Props.Body, ok: true}
}

func (f factLine) identity() string {
	return f.repo + "\x00" + f.kind + "\x00" + f.name + "\x00" + f.file
}

func (f factLine) movable() bool { return f.ok && !f.paired && f.body != "" }

// moveKey is what a moved symbol keeps: its kind, its name inside its module and its
// body. The name is required as well as the body, because a short body is shared by
// unrelated symbols, and pairing those would invent a move. The whole declared name, not
// the last segment, because one-line members are where bodies collide most: two props
// interfaces each declaring `title?: string;` are two members named `title`, and only
// their parents tell them apart.
func (f factLine) moveKey() string {
	return f.kind + "\x00" + declaredName(f.name) + "\x00" + f.body
}

// declaredName is a symbol's name without its module: `ElementStore.add` for
// `packages/element/src.ElementStore.add`. A module is a path, and the symbol starts
// after the first dot past its last slash.
func declaredName(name string) string {
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	if i := strings.IndexByte(name, '.'); i >= 0 {
		return name[i+1:]
	}
	return name
}
