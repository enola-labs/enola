package history

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const OriginLocal = "local"

type UnionRevision struct {
	Entry   Entry
	Origins []string
	Local   bool
	Record  *ShareRecord
}

func LocalUnion(entries []Entry) []UnionRevision {
	out := make([]UnionRevision, 0, len(entries))
	for _, e := range entries {
		out = append(out, UnionRevision{Entry: e, Origins: []string{localOrigin(e)}, Local: true})
	}
	return out
}

func localOrigin(e Entry) string {
	if e.Origin != "" {
		return OriginLocal + " (pulled from " + e.Origin + ")"
	}
	return OriginLocal
}

func BuildUnion(local []Entry, sh *Share, repo string) []UnionRevision {
	byID := map[string]int{}
	var out []UnionRevision
	for _, e := range local {
		if e.ID != "" {
			if _, dup := byID[e.ID]; dup {
				continue
			}
			byID[e.ID] = len(out)
		}
		out = append(out, UnionRevision{Entry: e, Origins: []string{localOrigin(e)}, Local: true})
	}
	if sh != nil {
		for _, rec := range sh.RevisionsFor(repo) {
			rec := rec
			origin := "store:" + rec.Source
			if i, dup := byID[rec.ID]; dup {
				out[i].Origins = append(out[i].Origins, origin)
				if out[i].Record == nil {
					out[i].Record = &rec
				}
				continue
			}
			byID[rec.ID] = len(out)
			out = append(out, UnionRevision{Entry: sh.EntryFor(rec), Origins: []string{origin}, Record: &rec})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return unionBefore(out[i], out[j]) })
	return out
}

func UnionRepo(local []Entry, sh *Share, requested string) (string, error) {
	if requested != "" {
		return requested, nil
	}
	for i := len(local) - 1; i >= 0; i-- {
		if local[i].Repo != "" {
			return local[i].Repo, nil
		}
	}
	if sh == nil {
		return "", nil
	}
	repos := sh.Repos()
	switch len(repos) {
	case 0:
		return "", fmt.Errorf("the store at %s holds no revisions", sh.Dir)
	case 1:
		return repos[0], nil
	}
	return "", fmt.Errorf(
		"cannot tell which repository is meant: the local history is empty and the store holds %d (%s). Snapshot the repository once so its identity is recorded, or pass --repo",
		len(repos), strings.Join(repos, ", "))
}

// compareInstant orders two timestamps: negative when a is earlier, positive when
// later, zero when they are the same instant. Compared as instants where both parse
// (see SortedByTime for why text is not enough), and as text otherwise, so an
// unparseable timestamp is placed where its text says rather than dropped.
func compareInstant(a, b string) int {
	ta, erra := time.Parse(time.RFC3339, a)
	tb, errb := time.Parse(time.RFC3339, b)
	switch {
	case erra == nil && errb == nil:
		return ta.Compare(tb)
	case a != b:
		return strings.Compare(a, b)
	}
	return 0
}

// unionBefore orders two revisions on the union's timeline.
//
// Time decides, and a revision is stamped to the second, so an agent loop lands
// several in one. Those ties are broken by the order the revisions were RECORDED in,
// which is the only evidence of which came first: within a local history that is
// the sequence number, and within one source's chain in a store it is the line.
// Breaking them by id instead put two snapshots taken within a second in hash order,
// and blame then read the later one first and reported its facts as removed.
//
// Recorded order only exists within one source, so a tie between revisions from
// different sources is broken by the source's name: there is no truth to recover
// there, only a need for the same answer every time. Within a source, the revisions
// held locally come before the ones only the store holds, since those are the ones
// that were pulled, or written, first. The id is last, for whatever is left.
func unionBefore(a, b UnionRevision) bool {
	if c := compareInstant(a.Entry.At, b.Entry.At); c != 0 {
		return c < 0
	}
	// Entry.Origin is empty for this machine's own revisions, and names the source
	// for a pulled revision and for one read from the store.
	if a.Entry.Origin != b.Entry.Origin {
		return a.Entry.Origin < b.Entry.Origin
	}
	if a.Local != b.Local {
		return a.Local
	}
	if pa, pb := recordedPosition(a), recordedPosition(b); pa != pb {
		return pa < pb
	}
	return a.Entry.ID < b.Entry.ID
}

// recordedPosition is where a revision sits in the order its source recorded it.
func recordedPosition(u UnionRevision) int {
	if !u.Local && u.Record != nil {
		return u.Record.Line
	}
	return u.Entry.Seq
}

func UnionEntries(revs []UnionRevision) []Entry {
	out := make([]Entry, 0, len(revs))
	for _, u := range revs {
		out = append(out, u.Entry)
	}
	return out
}

func (u UnionRevision) Lines(localRoot string, sh *Share) (factLines, insightLines []string, err error) {
	if u.Local && u.Entry.Blob != nil {
		factLines, insightLines, _, err = LoadLines(localRoot, u.Entry.Blob.Segment, u.Entry.Blob.Member)
		if err == nil {
			return factLines, insightLines, nil
		}
		if !errors.Is(err, ErrThinned) {
			return nil, nil, err
		}
	}
	if sh != nil && u.Record != nil {
		p, err := sh.LoadPayload(u.Record.ID)
		if err != nil {
			return nil, nil, err
		}
		return p.FactLines, p.InsightLines, nil
	}
	return nil, nil, ErrThinned
}
