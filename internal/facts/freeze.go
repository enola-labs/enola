package facts

import (
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Freeze collapses structurally identical Props maps and Relations slices onto shared
// instances, so facts that describe the same shape stop each holding their own copy of
// it.
//
// Extraction produces the same shape over and over — a parser emitting
// {symbol_kind: function, language: c, static: true, exported: false, has_body: true,
// cyclomatic: 1} for one function has no way to know it has emitted that exact map
// 178,043 times already. Measured on a 1.89M-fact graph: 1,892,343 Props maps over
// 211,692 distinct values, and 1,891,081 Relations slices over 596,362 distinct values.
// Each of those maps costs a header plus a bucket plus a boxed value per non-string
// entry, which is why Props alone accounted for 858 MiB across 16.8M live objects —
// 44% of the retained heap and 57% of the objects.
//
// This is deduplication, not a change of representation: Props stays a map[string]any
// and Relations stays a []Relation. Nothing downstream can tell the difference by
// reading.
//
// # The precondition
//
// Sharing is only sound because the store is never written again. Freeze is called at
// the single publication point in engine.GenerateSnapshot (and in RestoreFromDir),
// after every binder, explainer and renderer has run, and the published bundle is
// immutable by contract — a regeneration builds a different store and swaps the
// pointer.
//
// Writing to a frozen fact's Props would now corrupt roughly nine unrelated facts
// rather than one, so the one path that legitimately carries frozen facts into a
// mutable store — append mode, which copies the previous bundle forward — goes through
// Store.All, which copies Props and Relations precisely so the copy can be mutated
// freely. See the comment there.
//
// Freeze is idempotent and safe to call on an already-frozen store.
func (s *Store) Freeze() {
	s.mu.Lock()
	defer s.mu.Unlock()

	sh, from := s.sharing, s.sharedUpTo
	if sh == nil {
		sh, from = newSharer(), 0
	}
	for i := from; i < len(s.facts); i++ {
		sh.share(&s.facts[i])
	}
	s.sharing, s.sharedUpTo = nil, 0
	s.frozen = true
}

// sharer is Freeze's memory of the shapes it has seen. It can run over a store all at
// once (Freeze) or one fact at a time as a store is read (ReadJSONLFileShared), which
// is the same sharing done before the duplicates pile up rather than after.
type sharer struct {
	// Keyed by an injective encoding of the value, so a key hit means equality and no
	// confirming comparison is needed. Values that cannot be encoded soundly are left
	// alone rather than guessed at (see appendPropValue).
	propSeen map[string]map[string]any
	relSeen  map[string][]Relation
	buf      []byte
	keys     *propKeys
}

func newSharer() *sharer {
	return &sharer{
		propSeen: make(map[string]map[string]any),
		relSeen:  make(map[string][]Relation),
		buf:      make([]byte, 0, 1024),
		keys:     &propKeys{buf: make([]string, 0, 64)},
	}
}

// share points f's Props and Relations at the first equal ones seen, or records f's
// as the ones later facts will share.
func (sh *sharer) share(f *Fact) {
	if len(f.Props) > 0 {
		if key, ok := appendProps(sh.buf[:0], f.Props, sh.keys); ok {
			sh.buf = key
			if canon, hit := sh.propSeen[string(key)]; hit {
				f.Props = canon
			} else {
				sh.propSeen[string(key)] = f.Props
			}
		}
	}

	if len(f.Relations) > 0 {
		sortRelations(f.Relations)
		key := appendRelations(sh.buf[:0], f.Relations)
		sh.buf = key
		if canon, hit := sh.relSeen[string(key)]; hit {
			f.Relations = canon
		} else {
			// Trim the capacity to the length before sharing. An append by a later
			// holder then always reallocates instead of writing into spare capacity
			// that other facts are now reading through — the quieter half of the
			// aliasing hazard, and the one a reader would never suspect.
			shared := f.Relations[:len(f.Relations):len(f.Relations)]
			f.Relations = shared
			sh.relSeen[string(key)] = shared
		}
	}
}

// compareRelations orders relations by kind, then target: the order facts.jsonl has
// always written them in, and so the order a restored graph holds them in.
func compareRelations(a, b Relation) int {
	if c := strings.Compare(a.Kind, b.Kind); c != 0 {
		return c
	}
	return strings.Compare(a.Target, b.Target)
}

// sortRelations puts rs in compareRelations order, in place. Extractors append
// relations in map-iteration order, so a graph published as built held them in an order
// that varied run to run and differed from the same graph restored from disk, and a
// tool listing a fact's relations answered differently by how the graph got there.
//
// Sorted in place, and only when out of order: a slice already sorted is never written,
// which is every slice a published store holds, so one shared into this store from
// another (a restore, an append) is read and never touched.
func sortRelations(rs []Relation) {
	if len(rs) > 1 && !slices.IsSortedFunc(rs, compareRelations) {
		slices.SortFunc(rs, compareRelations)
	}
}

// Frozen reports whether Freeze has been called. It exists for tests and for the
// engine's own assertions about when publication happens; nothing about reading a
// store depends on it.
func (s *Store) Frozen() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.frozen
}

// --- canonical encoding -----------------------------------------------------
//
// The encoding must be INJECTIVE: two values may share a key only if they are equal,
// because a key collision means two facts silently start sharing one map. Every string
// and every collection is therefore length-prefixed rather than delimited (a delimiter
// can appear inside a value), and every value carries a tag for its concrete Go type
// (so int(1), int64(1) and float64(1) never collide — a consumer asserting v.(int)
// would break on a shared map that holds an int64).

// Type tags. Distinct bytes, one per concrete type the encoder accepts.
const (
	tagNil        = 'z'
	tagString     = 's'
	tagTrue       = 'T'
	tagFalse      = 'F'
	tagInt        = 'i'
	tagInt32      = 'j'
	tagInt64      = 'k'
	tagFloat32    = 'e'
	tagFloat64    = 'f'
	tagStringSlic = 'S'
	tagAnySlice   = 'A'
	tagMapSlice   = 'M'
	tagMap        = 'm'
)

// propKeys is the sorted-key scratch shared by every appendProps in one Freeze.
//
// The encoding has to visit a map's keys in a fixed order, and the obvious way to get
// one is to collect and sort them — which allocated a slice for every fact encoded,
// 1.89M of them on the graph this file's own comment measures, each immediately
// discarded. Levels take the tail of one backing array and give it back on the way
// out, so a nested map (the []map[string]any case) has its own run and the level
// above keeps reading the keys it was given.
type propKeys struct{ buf []string }

func (s *propKeys) sorted(p map[string]any) []string {
	base := len(s.buf)
	for k := range p {
		s.buf = append(s.buf, k)
	}
	keys := s.buf[base:]
	sort.Strings(keys)
	return keys
}

// release returns a level's run. An append by a deeper level may have moved the
// backing array, which is harmless: the caller reads the run it was handed, and push
// and release stay balanced by length either way.
func (s *propKeys) release(keys []string) { s.buf = s.buf[:len(s.buf)-len(keys)] }

// appendProps appends an injective encoding of p to buf, returning false if p holds a
// value the encoder does not accept. A rejected fact keeps its own Props: leaving one
// map unshared costs a few hundred bytes, whereas guessing at an unknown type risks
// conflating two facts that differ.
func appendProps(buf []byte, p map[string]any, scratch *propKeys) ([]byte, bool) {
	keys := scratch.sorted(p)
	defer scratch.release(keys)

	buf = appendUint(buf, uint64(len(p)))
	for _, k := range keys {
		buf = appendLenString(buf, k)
		var ok bool
		buf, ok = appendPropValue(buf, p[k], scratch)
		if !ok {
			return buf, false
		}
	}
	return buf, true
}

// appendRelations appends an injective encoding of rr to buf. Relations are two
// strings and a count, so unlike Props there is no value type that can defeat it.
func appendRelations(buf []byte, rr []Relation) []byte {
	buf = appendUint(buf, uint64(len(rr)))
	for _, r := range rr {
		buf = appendLenString(buf, r.Kind)
		buf = appendLenString(buf, r.Target)
	}
	return buf
}

func appendPropValue(buf []byte, v any, scratch *propKeys) ([]byte, bool) {
	switch t := v.(type) {
	case nil:
		return append(buf, tagNil), true
	case string:
		return appendLenString(append(buf, tagString), t), true
	case bool:
		if t {
			return append(buf, tagTrue), true
		}
		return append(buf, tagFalse), true
	case int:
		return strconv.AppendInt(append(buf, tagInt), int64(t), 10), true
	case int32:
		return strconv.AppendInt(append(buf, tagInt32), int64(t), 10), true
	case int64:
		return strconv.AppendInt(append(buf, tagInt64), t, 10), true
	case float32:
		// 'g' with precision -1 is the shortest representation that round-trips
		// exactly, so distinct values always render distinctly.
		return strconv.AppendFloat(append(buf, tagFloat32), float64(t), 'g', -1, 32), true
	case float64:
		return strconv.AppendFloat(append(buf, tagFloat64), t, 'g', -1, 64), true
	case []string:
		buf = appendUint(append(buf, tagStringSlic), uint64(len(t)))
		for _, s := range t {
			buf = appendLenString(buf, s)
		}
		return buf, true
	case []any:
		// What a JSON array becomes on the way back in from a restored snapshot.
		buf = appendUint(append(buf, tagAnySlice), uint64(len(t)))
		for _, e := range t {
			var ok bool
			buf, ok = appendPropValue(buf, e, scratch)
			if !ok {
				return buf, false
			}
		}
		return buf, true
	case []map[string]any:
		buf = appendUint(append(buf, tagMapSlice), uint64(len(t)))
		for _, m := range t {
			var ok bool
			buf, ok = appendProps(buf, m, scratch)
			if !ok {
				return buf, false
			}
		}
		return buf, true
	case map[string]any:
		var ok bool
		buf, ok = appendProps(append(buf, tagMap), t, scratch)
		return buf, ok
	default:
		// An unrecognised type. Refusing is the only sound answer: two values this
		// encoder cannot describe might differ in a way it cannot see.
		return buf, false
	}
}

// appendLenString writes a length-prefixed string, so no value can imitate the
// boundary between two of them.
func appendLenString(buf []byte, s string) []byte {
	buf = appendUint(buf, uint64(len(s)))
	return append(buf, s...)
}

// appendUint writes an unambiguous decimal length followed by a separator. The
// separator is safe because the digits before it cannot contain one.
func appendUint(buf []byte, n uint64) []byte {
	buf = strconv.AppendUint(buf, n, 10)
	return append(buf, ':')
}
