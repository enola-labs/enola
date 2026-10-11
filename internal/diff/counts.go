package diff

import (
	"hash/fnv"
	"io"
	"sort"

	"github.com/enola-labs/enola/internal/facts"
)

// Counts is the delta expressed as counts, with no facts attached.
//
// It is what the architecture history's per-revision summary needs: five integers and
// two per-kind tallies. Compute produces the same numbers, but produces them by
// building the whole delta first — every added, removed and changed fact, and both
// sides' edge sets — which for a summary is the entire cost and none of the value.
//
// Measured on a warm dotnet/runtime snapshot before this existed: history recording held
// 647 MiB at the peak, 66% of the live heap, of which 386 MiB was loading the previous
// snapshot into a Store and 261 MiB was Compute's group indexes. On the kernel the same
// two steps account for most of a 3.9 GiB gap between the cold and warm peaks.
//
// The numbers are EXACT — this is a cheaper way to compute the same answer, not an
// estimate of it. TestCounts_MatchesCompute_* grade every field against Compute.
type Counts struct {
	FactsAdded   int
	FactsRemoved int
	FactsChanged int
	EdgesAdded   int
	EdgesRemoved int

	AddedByKind   map[string]int
	RemovedByKind map[string]int

	// TouchedNames is the set of fact names this change added, removed or altered,
	// plus the endpoints of every added or removed edge — the same set Compute
	// derives from the materialised delta, and the input findings attribution needs
	// to tell a real regression from an incidental one.
	//
	// Bounded by the size of the CHANGE, not of the snapshot, which is what makes it
	// affordable here: a warm re-snapshot of an untouched tree produces an empty set
	// and skips the pass that fills it.
	TouchedNames map[string]struct{}
}

// FactSource yields every fact of one snapshot, once per call to iterate. It is
// called TWICE (see collect), so a source reading a file must re-open it.
type FactSource func(fn func(facts.Fact) error) error

// JSONLSource reads facts from a JSONL stream, re-opening for each pass.
func JSONLSource(open func() (io.ReadCloser, error)) FactSource {
	return func(fn func(facts.Fact) error) error {
		r, err := open()
		if err != nil {
			return err
		}
		defer func() { _ = r.Close() }()
		return facts.ScanJSONL(r, fn)
	}
}

// SliceSource reads facts already in memory.
func SliceSource(ff []facts.Fact) FactSource {
	return func(fn func(facts.Fact) error) error {
		for _, f := range ff {
			if err := fn(f); err != nil {
				return err
			}
		}
		return nil
	}
}

// factRec is one fact reduced to what the counts need: no pointers and no strings, so
// a million of them are one allocation rather than a million.
type factRec struct {
	key   uint64 // hash of factKey — the fact's identity
	props uint64 // hash of propsJSON — what "changed" means
	kind  uint16 // index into the shared kind table
}

// member is a fact belonging to a group that shares a factKey. These keep the EXACT
// intraGroupOrder string rather than a hash of it, because members are paired
// positionally after sorting by it and a hash sorts in a different order — which
// changes which member pairs with which, and so changes the count of changed facts.
// Measured: on adversarial input, ordering groups by hash disagreed with Compute in
// 3.7% of rounds. Groups are rare, so keeping their strings costs almost nothing.
type member struct {
	ord   string
	props uint64
	kind  uint16
}

type kindTable struct {
	ids   map[string]uint16
	names []string
}

func newKindTable() *kindTable { return &kindTable{ids: make(map[string]uint16, 16)} }

func (t *kindTable) id(kind string) uint16 {
	if id, ok := t.ids[kind]; ok {
		return id
	}
	id := uint16(len(t.names))
	t.ids[kind] = id
	t.names = append(t.names, kind)
	return id
}

func hash64(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}

// side is one snapshot reduced to counting form.
type side struct {
	flat   []factRec           // one entry per fact, sorted by key after pass 1
	groups map[uint64][]member // only for keys with more than one member, filled in pass 2
	edges  []uint64            // edgeKey hashes, sorted and deduplicated
	kinds  *kindTable
}

// pass1 reduces every fact and collects the edge set. It cannot yet know which facts
// share a key, so it keeps no intraGroupOrder.
func (s *side) pass1(src FactSource) error {
	return src(func(f facts.Fact) error {
		s.flat = append(s.flat, factRec{
			key:   hash64(factKey(f)),
			props: hash64(propsJSON(f.Props)),
			kind:  s.kinds.id(f.Kind),
		})
		for _, r := range f.Relations {
			s.edges = append(s.edges, hash64(edgeKey(Edge{
				Source: f.Name, Kind: r.Kind, Target: r.Target, Repo: f.Repo,
			})))
		}
		return nil
	})
}

// pass2 revisits the source and materialises ordered groups for the given keys only.
// It is skipped entirely when no key on either side has more than one member, which
// is the common case for a repository with no overloads or conditional declarations.
func (s *side) pass2(src FactSource, keys map[uint64]bool) error {
	if len(keys) == 0 {
		return nil
	}
	s.groups = make(map[uint64][]member, len(keys))
	err := src(func(f facts.Fact) error {
		k := hash64(factKey(f))
		if !keys[k] {
			return nil
		}
		s.groups[k] = append(s.groups[k], member{
			ord:   intraGroupOrder(f),
			props: hash64(propsJSON(f.Props)),
			kind:  s.kinds.id(f.Kind),
		})
		return nil
	})
	if err != nil {
		return err
	}
	for _, g := range s.groups {
		sort.Slice(g, func(i, j int) bool { return g[i].ord < g[j].ord })
	}
	// The grouped keys are now accounted for twice; drop them from the flat run.
	kept := s.flat[:0]
	for _, r := range s.flat {
		if !keys[r.key] {
			kept = append(kept, r)
		}
	}
	s.flat = kept
	return nil
}

// dupKeys reports the keys held by more than one fact. flat must be sorted by key.
func (s *side) dupKeys() map[uint64]bool {
	out := map[uint64]bool{}
	for i := 1; i < len(s.flat); i++ {
		if s.flat[i].key == s.flat[i-1].key {
			out[s.flat[i].key] = true
		}
	}
	return out
}

func sortByKey(recs []factRec) {
	sort.Slice(recs, func(i, j int) bool { return recs[i].key < recs[j].key })
}

// sortDedup sorts hashes ascending and removes duplicates in place, turning a list of
// edge hashes into the SET that edgeSet builds with a map.
func sortDedup(hs []uint64) []uint64 {
	sort.Slice(hs, func(i, j int) bool { return hs[i] < hs[j] })
	out := hs[:0]
	for i, h := range hs {
		if i == 0 || h != hs[i-1] {
			out = append(out, h)
		}
	}
	return out
}

// CountAgainstJSONL computes the delta between a baseline snapshot stored as JSONL and
// the current facts, without materialising the baseline.
//
// # Why hashes
//
// Every string that identifies a fact — its key, its props — is reduced to a 64-bit
// FNV-1a hash, because holding the strings is the thing being avoided: factKey alone
// averages ~60 bytes and a kernel-sized snapshot has 1.9M of them per side.
//
// A collision would miscount by one. For 1.9M keys in a 64-bit space the expected
// number of collisions is n²/2^65 ≈ 1e-7, and the blast radius is one integer in a log
// line, not a fact in a graph. Compute — which diff_snapshot uses, and which must NAME
// the facts rather than count them — keeps the exact strings throughout.
//
// # Two passes
//
// Pass 1 reduces every fact; pass 2 revisits only the facts whose key is shared, to
// recover the exact intraGroupOrder that decides how group members pair up. A source
// backed by a file is therefore read twice. That is deliberate: the alternative is
// keeping an ordering string for every fact, which is the memory this exists to avoid,
// and pass 2 is skipped altogether when no key is shared.
func CountAgainstJSONL(open func() (io.ReadCloser, error), current []facts.Fact) (*Counts, error) {
	return count(JSONLSource(open), SliceSource(current))
}

// CountFacts is the general form: any FactSource for the baseline against the current
// facts in memory.
func CountFacts(baseline FactSource, current []facts.Fact) (*Counts, error) {
	return count(baseline, SliceSource(current))
}

// CountSnapshots is the in-memory form, for callers that already hold both sides (and
// for the differential test that grades this against Compute).
func CountSnapshots(baseline, current []facts.Fact) *Counts {
	c, err := count(SliceSource(baseline), SliceSource(current))
	if err != nil {
		// A slice source cannot fail; only a reader can.
		return &Counts{AddedByKind: map[string]int{}, RemovedByKind: map[string]int{}}
	}
	return c
}

func count(baseSrc, curSrc FactSource) (*Counts, error) {
	kinds := newKindTable() // shared, so a kind id means the same on both sides
	base := &side{kinds: kinds}
	cur := &side{kinds: kinds}

	if err := base.pass1(baseSrc); err != nil {
		return nil, err
	}
	if err := cur.pass1(curSrc); err != nil {
		return nil, err
	}
	sortByKey(base.flat)
	sortByKey(cur.flat)

	// A key needs exact ordering if it is shared on EITHER side: a key that is a
	// singleton in the baseline still pairs against member 0 of a current group, and
	// which fact that is depends on the group's order.
	shared := base.dupKeys()
	for k := range cur.dupKeys() {
		shared[k] = true
	}
	if err := base.pass2(baseSrc, shared); err != nil {
		return nil, err
	}
	if err := cur.pass2(curSrc, shared); err != nil {
		return nil, err
	}

	c, d := countSides(base, cur)
	if err := resolveNames(c, d, baseSrc, curSrc); err != nil {
		return nil, err
	}
	return c, nil
}

// movedSet records which identities and edges moved, so the names behind them can be
// resolved in one further pass without keeping a name for every fact. (Named for what
// it holds rather than "delta", which receipt.go already uses for something else.)
type movedSet struct {
	keys  map[uint64]bool
	edges map[uint64]bool
}

func (d *movedSet) empty() bool { return len(d.keys) == 0 && len(d.edges) == 0 }

func countSides(base, cur *side) (*Counts, *movedSet) {
	c := &Counts{AddedByKind: map[string]int{}, RemovedByKind: map[string]int{}}
	d := &movedSet{keys: map[uint64]bool{}, edges: map[uint64]bool{}}

	// Unshared keys: at most one member per side, so a merge-join over the sorted
	// runs settles added, removed and changed without materialising anything.
	i, j := 0, 0
	for i < len(base.flat) && j < len(cur.flat) {
		switch {
		case base.flat[i].key == cur.flat[j].key:
			if base.flat[i].props != cur.flat[j].props {
				c.FactsChanged++
				d.keys[cur.flat[j].key] = true
			}
			i++
			j++
		case base.flat[i].key < cur.flat[j].key:
			c.removed(base, base.flat[i].kind)
			d.keys[base.flat[i].key] = true
			i++
		default:
			c.added(cur, cur.flat[j].kind)
			d.keys[cur.flat[j].key] = true
			j++
		}
	}
	for ; i < len(base.flat); i++ {
		c.removed(base, base.flat[i].kind)
		d.keys[base.flat[i].key] = true
	}
	for ; j < len(cur.flat); j++ {
		c.added(cur, cur.flat[j].kind)
		d.keys[cur.flat[j].key] = true
	}

	// Shared keys: pair positionally in intraGroupOrder, exactly as Compute does.
	for k, cGroup := range cur.groups {
		bGroup := base.groups[k]
		for n, m := range cGroup {
			if n >= len(bGroup) {
				c.added(cur, m.kind)
				d.keys[k] = true
				continue
			}
			if bGroup[n].props != m.props {
				c.FactsChanged++
				d.keys[k] = true
			}
		}
	}
	for k, bGroup := range base.groups {
		for n := len(cur.groups[k]); n < len(bGroup); n++ {
			c.removed(base, bGroup[n].kind)
			d.keys[k] = true
		}
	}

	baseEdges, curEdges := sortDedup(base.edges), sortDedup(cur.edges)
	c.EdgesAdded, c.EdgesRemoved = countSetDelta(baseEdges, curEdges)
	collectSetDelta(baseEdges, curEdges, d.edges)
	return c, d
}

// collectSetDelta records the symmetric difference of two sorted, deduplicated hash
// sets into out.
func collectSetDelta(base, cur []uint64, out map[uint64]bool) {
	i, j := 0, 0
	for i < len(base) && j < len(cur) {
		switch {
		case base[i] == cur[j]:
			i++
			j++
		case base[i] < cur[j]:
			out[base[i]] = true
			i++
		default:
			out[cur[j]] = true
			j++
		}
	}
	for ; i < len(base); i++ {
		out[base[i]] = true
	}
	for ; j < len(cur); j++ {
		out[cur[j]] = true
	}
}

// resolveNames fills TouchedNames by revisiting both sources for just the identities
// and edges that moved. factKey embeds the fact's Name, so whichever side holds a
// changed key yields the same name Compute would have taken from the delta.
func resolveNames(c *Counts, d *movedSet, sources ...FactSource) error {
	c.TouchedNames = map[string]struct{}{}
	if d.empty() {
		return nil
	}
	add := func(n string) {
		if n != "" {
			c.TouchedNames[n] = struct{}{}
		}
	}
	for _, src := range sources {
		err := src(func(f facts.Fact) error {
			if d.keys[hash64(factKey(f))] {
				add(f.Name)
			}
			for _, r := range f.Relations {
				h := hash64(edgeKey(Edge{Source: f.Name, Kind: r.Kind, Target: r.Target, Repo: f.Repo}))
				if d.edges[h] {
					add(f.Name)
					add(r.Target)
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *Counts) added(s *side, kind uint16) {
	c.FactsAdded++
	c.AddedByKind[s.kinds.names[kind]]++
}

func (c *Counts) removed(s *side, kind uint16) {
	c.FactsRemoved++
	c.RemovedByKind[s.kinds.names[kind]]++
}

// countSetDelta returns |cur \ base| and |base \ cur| over two sorted, deduplicated
// hash sets.
func countSetDelta(base, cur []uint64) (added, removed int) {
	i, j := 0, 0
	for i < len(base) && j < len(cur) {
		switch {
		case base[i] == cur[j]:
			i++
			j++
		case base[i] < cur[j]:
			removed++
			i++
		default:
			added++
			j++
		}
	}
	return added + len(cur) - j, removed + len(base) - i
}

// FindingCounts is the findings half of a summary: how many findings appeared,
// cleared, or moved between two snapshots.
type FindingCounts struct {
	New      int
	Resolved int
	Changed  int
}

// ClassifyFindings counts appeared/cleared/altered findings, given the set of names
// the change structurally touched.
//
// It is the same grouping and the same attribution Compute applies — findings collide
// under one identity and are paired positionally, and an appearance only counts as a
// regression if the change touched something the finding cites. It is separated out so
// the history summary can reuse it without materialising the fact delta that Compute
// derives `touched` from; CountAgainstJSONL produces that set directly.
func ClassifyFindings(baseline, current []facts.Insight, touched map[string]struct{}) FindingCounts {
	var fc FindingCounts
	baseFind := groupInsightsByKey(baseline)
	curFind := groupInsightsByKey(current)

	for k, curGroup := range curFind {
		baseGroup := baseFind[k]
		for i, in := range curGroup {
			if i >= len(baseGroup) {
				if findingHasStructuralCause(in, touched) {
					fc.New++
				}
				continue
			}
			if insightChanged(baseGroup[i], in) {
				fc.Changed++
			}
		}
	}
	for k, baseGroup := range baseFind {
		curGroup := curFind[k]
		for i := len(curGroup); i < len(baseGroup); i++ {
			if findingHasStructuralCause(baseGroup[i], touched) {
				fc.Resolved++
			}
		}
	}
	return fc
}
