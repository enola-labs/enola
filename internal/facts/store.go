package facts

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/enola-labs/enola/internal/factpath"
)

// Store provides in-memory storage and querying of facts with JSONL persistence.
type Store struct {
	mu    sync.RWMutex
	facts []Fact

	// Indexes for fast lookups
	byKind map[string][]int // kind -> indices into facts
	byFile map[string][]int // file -> indices into facts
	// byName maps a name to the facts carrying it, in a form sized for how names
	// actually distribute: on a kernel-sized graph 1.9M names index 2.06M facts and
	// 98.2% of them carry exactly one. A map[string][]int spends a 24-byte slice
	// header per name plus a separate heap object holding one int; this spends four
	// bytes and no object. A non-negative value IS the fact index; a negative value
	// v means ^v indexes byNameDup, which holds the full index list for that name.
	// Measured at that shape: 167 MiB against 71.
	byName    map[string]int32
	byNameDup [][]int32
	byRepo    map[string][]int // repo label -> indices into facts

	// Graph provides adjacency-list traversal over fact relations
	graph *Graph

	// intern canonicalizes the two string fields that repeat heavily across facts,
	// so N equal strings share one backing array instead of holding N. See Add.
	// Released by BuildGraph and lazily recreated if facts are added afterwards.
	intern map[string]string

	// frozen records that Freeze has run, so structurally identical Props maps and
	// Relations slices are now SHARED between facts. See Freeze for what that
	// forbids.
	frozen bool
	// frozenUpTo is how many facts the last Freeze shared, so another Freeze starts
	// after them. Anything that rebuilds the slice resets it to 0.
	frozenUpTo int

	// sharing, while set, holds the shapes ReadJSONLFileShared has already shared
	// facts[:sharedUpTo] onto, so Freeze carries on from there rather than encoding
	// every fact a second time. Freeze drops it.
	sharing    *sharer
	sharedUpTo int

	// targetAliases maps a relation target that names no fact to the one fact it
	// refers to in another repository, keyed by the target exactly as written. See
	// SetTargetAliases.
	targetAliases map[string]FactKey

	// docTexts holds the whole comment of each fact whose PropDoc summarizes more
	// than it says, keyed by the fact's identity. TakeDocTexts fills it from the
	// facts' own PropDocText, which it removes, so the comment's full text reaches
	// the docs renderer without riding on the fact into facts.jsonl or any tool's
	// output.
	docTexts map[FactKey]string
}

// FactKey is a fact's identity: the four fields its id is computed from.
type FactKey struct {
	Repo, Kind, Name, File string
}

// SetTargetAliases records which fact a relation target names when the target is
// not any fact's name: a consumer names a symbol of another loaded repository the
// way its own source spells it (a Go import path, an npm package), while the
// provider's fact carries its repo-relative name. WriteJSONL resolves target_id
// through these when name resolution finds nothing, so the id is exact without
// renaming the target, which would make it collide with same-named facts of the
// consumer. A later call replaces the whole table; nil clears it.
func (s *Store) SetTargetAliases(aliases map[string]FactKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.targetAliases = aliases
}

// NewStore creates an empty fact store.
func NewStore() *Store {
	return &Store{
		byKind: make(map[string][]int),
		byFile: make(map[string][]int),
		byName: make(map[string]int32),
		byRepo: make(map[string][]int),
	}
}

// canonical returns the interned copy of s, adding it if this is the first sighting.
// Callers must hold the write lock.
func (s *Store) canonical(str string) string {
	if str == "" {
		return str
	}
	if s.intern == nil {
		s.intern = make(map[string]string)
	}
	if c, ok := s.intern[str]; ok {
		return c
	}
	s.intern[str] = str
	return str
}

// Add adds facts to the store.
//
// File and Relation.Target are interned on the way in. Both are populations of a few
// distinct values repeated across a great many facts, and every extractor produces
// each occurrence as its own allocation — a parser has no way to know it has emitted
// that path or that callee before. Measured on the Linux kernel (1.89M facts): 1.89M
// File strings holding 60 MiB of bytes that are only 1 MiB distinct, and 5.42M
// relation targets holding 157 MiB that are only 46 MiB distinct. Interning collapses
// both to one allocation per distinct value.
//
// Name is deliberately NOT interned: it is near-unique by construction (1.83M distinct
// of 1.89M on the same corpus), so a map over it would cost more than it saved.
//
// Interning changes only which backing array a string header points at. Equal strings
// stay equal, the fields keep their types, and nothing downstream can observe the
// difference — including WriteJSONL, whose output is unchanged byte for byte.
//
// File — and the Name of the kinds whose name IS a path (see pathShapedName) — is also
// normalised to forward slashes here. That is the LAST line of defence rather than the
// first: paths are normalised at the walker, where the host filesystem enters, and the
// extractors build on them with internal/factpath. This catches whatever still slips
// through, because the failure mode it guards against is silent — a backslash path
// does not error anywhere, it simply stops being equal to the path everything else
// derived, and a declared layer order quietly classifies nothing (issue #242).
//
// Relation.Target is deliberately NOT normalised, and neither is a symbol Name: PHP
// namespaces are backslash-separated, so those fields carry real backslashes that mean
// something other than a directory boundary.
func (s *Store) Add(ff ...Fact) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range ff {
		s.addLocked(f)
	}
}

// addLocked is Add for one fact, returning its index. The caller must hold s.mu.
func (s *Store) addLocked(f Fact) int {
	f.File = s.canonical(factpath.Slash(f.File))
	if pathShapedName[f.Kind] {
		f.Name = factpath.Slash(f.Name)
	}
	if len(f.Relations) > 0 {
		// Rewrite in place: Relations is the caller's slice, but Add already
		// takes ownership of the fact (it is stored, not copied), and the
		// targets are being replaced with equal values.
		for i := range f.Relations {
			f.Relations[i].Target = s.canonical(f.Relations[i].Target)
		}
	}

	idx := len(s.facts)
	s.facts = append(s.facts, f)
	s.byKind[f.Kind] = append(s.byKind[f.Kind], idx)
	if f.File != "" {
		s.byFile[f.File] = append(s.byFile[f.File], idx)
	}
	if f.Name != "" {
		s.indexName(f.Name, idx)
	}
	if f.Repo != "" {
		s.byRepo[f.Repo] = append(s.byRepo[f.Repo], idx)
	}
	return idx
}

// indexName records that facts[idx] carries name. The caller must hold s.mu.
func (s *Store) indexName(name string, idx int) {
	v, seen := s.byName[name]
	switch {
	case !seen:
		s.byName[name] = int32(idx)
	case v >= 0:
		s.byNameDup = append(s.byNameDup, []int32{v, int32(idx)})
		s.byName[name] = ^int32(len(s.byNameDup) - 1)
	default:
		d := ^v
		s.byNameDup[d] = append(s.byNameDup[d], int32(idx))
	}
}

// namedIdxs returns the indices of every fact carrying name. The single-fact case,
// which is the overwhelming majority, comes back as (idx, nil, true) and touches no
// slice at all; a shared name comes back as (first, rest, true). Callers iterate
// first and then rest. The caller must hold s.mu, and must not retain rest.
func (s *Store) namedIdxs(name string) (first int, rest []int32, ok bool) {
	v, seen := s.byName[name]
	if !seen {
		return 0, nil, false
	}
	if v >= 0 {
		return int(v), nil, true
	}
	all := s.byNameDup[^v]
	return int(all[0]), all[1:], true
}

// appendNamedIdxs appends every index carrying name to dst.
// The caller must hold s.mu.
func (s *Store) appendNamedIdxs(dst []int, name string) []int {
	first, rest, ok := s.namedIdxs(name)
	if !ok {
		return dst
	}
	dst = append(dst, first)
	for _, i := range rest {
		dst = append(dst, int(i))
	}
	return dst
}

// namedCount returns how many facts carry name. The caller must hold s.mu.
func (s *Store) namedCount(name string) int {
	v, seen := s.byName[name]
	switch {
	case !seen:
		return 0
	case v >= 0:
		return 1
	default:
		return len(s.byNameDup[^v])
	}
}

// collectByName returns the facts carrying name. The caller must hold s.mu.
func (s *Store) collectByName(name string) []Fact {
	first, rest, ok := s.namedIdxs(name)
	if !ok {
		return nil
	}
	out := make([]Fact, 0, 1+len(rest))
	if first < len(s.facts) {
		out = append(out, s.facts[first])
	}
	for _, i := range rest {
		if int(i) < len(s.facts) {
			out = append(out, s.facts[i])
		}
	}
	return out
}

// All returns an independent copy of every fact in the store — including each fact's
// Props map and Relations slice, which are copied rather than aliased.
//
// Copying those two is what makes the word "independent" true. A plain slice copy
// duplicates the Fact structs, but Props is a map and Relations is a slice header, so
// every copy still pointed into the ORIGINAL store: a caller that added a prop or
// appended a relation wrote through into the store it had copied from. That store is
// often the published snapshot bundle, which concurrent MCP readers are traversing.
// The append-mode path in engine.GenerateSnapshot copies the previous bundle precisely
// so it can mutate the result safely — binders delete props (unmatchedroutes) and
// annotators add them (the metrics/orphans passes) — and it was relying on
// a guarantee this method did not provide.
//
// Freeze makes that guarantee load-bearing rather than merely correct: once a store is
// frozen, one Props map is shared by ~9 facts on average, so a write through an
// uncopied reference would corrupt all of them and the published bundle besides.
//
// Callers that only iterate and never retain should use FactsRef instead; this one
// allocates proportionally to the fact set.
func (s *Store) All() []Fact {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]Fact, len(s.facts))
	copy(result, s.facts)
	for i := range result {
		if len(result[i].Relations) > 0 {
			rels := make([]Relation, len(result[i].Relations))
			copy(rels, result[i].Relations)
			result[i].Relations = rels
		}
		if len(result[i].Props) > 0 {
			result[i].Props = result[i].CloneProps()
		}
	}
	return result
}

// DetachFrom gives every fact from startIdx on its own Relations slice, in
// compareRelations order, and its own Props map: the state a fact has always had
// when a cluster carried it from one turn to the next (Freeze sorted its relations,
// then All copied both apart). A cluster that keeps appending into one store instead
// of copying it every turn calls this at the end of each turn but the last, so the
// union the linkers see is the same, fact for fact, at a cost linear in the turn
// rather than in the union.
func (s *Store) DetachFrom(startIdx int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := startIdx; i < len(s.facts); i++ {
		f := &s.facts[i]
		if len(f.Relations) > 0 {
			sortRelations(f.Relations)
			rels := make([]Relation, len(f.Relations))
			copy(rels, f.Relations)
			f.Relations = rels
		}
		if len(f.Props) > 0 {
			f.Props = f.CloneProps()
		}
	}
}

// FactsRef returns the store's underlying fact slice WITHOUT copying it. Unlike
// All, the caller must treat the result as read-only and must not retain it past
// the next mutation (Add/Clear/RemoveWhere may replace or grow the backing
// array). It exists so the engine can attach facts to the live snapshot it owns
// without doubling memory; callers that need an independent, stable copy (e.g.
// pinned baselines) must use All instead.
func (s *Store) FactsRef() []Fact {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.facts
}

// RepoLabels returns the distinct, non-empty repo labels present in the store,
// derived in O(repos) from the byRepo index rather than by scanning every fact.
// Order is unspecified.
func (s *Store) RepoLabels() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	labels := make([]string, 0, len(s.byRepo))
	for repo := range s.byRepo {
		if repo != "" {
			labels = append(labels, repo)
		}
	}
	return labels
}

// Count returns the number of facts in the store.
func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.facts)
}

// CountFilesWithFacts returns how many of the given walked files produced at
// least one fact, applying prefix to each path first (the repo-label prefix used
// in append mode; "" in single-repo mode). This is the intersection of "files
// enumerated by the walker" with "files present in the graph", so it is a true
// parsing-coverage numerator that never exceeds the seen count — unlike a raw
// count of distinct fact file-keys, which also includes module/directory keys and
// config files scanned outside the walker.
func (s *Store) CountFilesWithFacts(files []string, prefix string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, f := range files {
		key := prefix + filepath.ToSlash(f)
		if idxs, ok := s.byFile[key]; ok && len(idxs) > 0 {
			n++
		}
	}
	return n
}

// FilesWithFacts returns the subset of the given walked files that produced at
// least one fact, keyed by the walked (un-prefixed) path. Same membership rule
// as CountFilesWithFacts — prefix is the repo-label prefix append mode stamps
// on fact files — returned as a set because the census needs to place each
// walked file in exactly one bucket rather than only count the parsed ones.
func (s *Store) FilesWithFacts(files []string, prefix string) map[string]bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	parsed := make(map[string]bool)
	for _, f := range files {
		key := prefix + filepath.ToSlash(f)
		if idxs, ok := s.byFile[key]; ok && len(idxs) > 0 {
			parsed[f] = true
		}
	}
	return parsed
}

// SourceBytesWithFacts returns the total on-disk size of the walked files that
// produced at least one fact — the same set CountFilesWithFacts counts, measured
// rather than counted. root is the repo root that files are relative to.
//
// This is the corpus figure the value model in pkg/status is anchored to, and it
// deliberately measures only *parsed* files. Summing every walked file instead
// folds in whatever else lives in the tree — images, media, vendored databases,
// stale artifacts — which on a real repo is a 39x difference, and none of it is
// source an agent would have read. Unreadable files are skipped, not estimated.
func (s *Store) SourceBytesWithFacts(files []string, prefix, root string) int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var total int64
	for _, f := range files {
		key := prefix + filepath.ToSlash(f)
		if idxs, ok := s.byFile[key]; !ok || len(idxs) == 0 {
			continue
		}
		p := f
		if !filepath.IsAbs(p) && root != "" {
			p = filepath.Join(root, f)
		}
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			total += info.Size()
		}
	}
	return total
}

// ByKind returns all facts of the given kind.
func (s *Store) ByKind(kind string) []Fact {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.collectByIndex(s.byKind[kind])
}

// OfKind yields the facts of the given kind without copying them into a slice, as
// ByKind does: ByKind(symbol) on a large graph is a copy of most of the store, made by
// each of a dozen passes that only read it, and on runtime that was 2.5 GB of a run's
// allocation.
//
// It reads the facts as they were when iteration began, like ByKind, so facts added
// during the loop are not yielded. Unlike ByKind it holds no copy: a loop that writes
// facts of this kind while iterating may see some of its writes. Use ByKind there.
func (s *Store) OfKind(kind string) iter.Seq[Fact] {
	return func(yield func(Fact) bool) {
		s.mu.RLock()
		idx, ff := s.byKind[kind], s.facts
		s.mu.RUnlock()
		for _, i := range idx {
			if i < len(ff) && !yield(ff[i]) {
				return
			}
		}
	}
}

// ByFile returns all facts for the given file.
func (s *Store) ByFile(file string) []Fact {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.collectByIndex(s.byFile[file])
}

// ByName returns all facts with the given name.
func (s *Store) ByName(name string) []Fact {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.collectByName(name)
}

// ByRepo returns all facts for the given repo label.
func (s *Store) ByRepo(repo string) []Fact {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.collectByIndex(s.byRepo[repo])
}

// CountByRepo returns how many facts carry the given repo label, in O(1) from the
// byRepo index without materializing (and copying) the facts themselves.
func (s *Store) CountByRepo(repo string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.byRepo[repo])
}

// HasFileOutside reports whether any fact of the given kind labelled repo names a
// file that is not under prefix, stopping at the first one. It reads the byRepo
// index, so it costs nothing for a repo the store does not hold.
func (s *Store) HasFileOutside(repo, kind, prefix string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, i := range s.byRepo[repo] {
		f := &s.facts[i]
		if f.Kind == kind && f.File != "" && !strings.HasPrefix(f.File, prefix) {
			return true
		}
	}
	return false
}

// ByRelation returns all facts that have a relation of the given kind.
func (s *Store) ByRelation(relKind string) []Fact {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []Fact
	for _, f := range s.facts {
		for _, r := range f.Relations {
			if r.Kind == relKind {
				result = append(result, f)
				break
			}
		}
	}
	return result
}

// Query returns facts matching all provided filter criteria.
// Empty filter values are ignored (match all).
func (s *Store) Query(kind, file, name, relKind string) []Fact {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Use the kind index when available to avoid a full scan.
	candidates := s.facts
	if kind != "" {
		if idxs, ok := s.byKind[kind]; ok {
			return s.queryFromIndices(idxs, kind, file, name, relKind)
		}
		return nil // kind specified but no facts of that kind exist
	}

	var result []Fact
	for _, f := range candidates {
		if file != "" && f.File != file {
			continue
		}
		if name != "" && !strings.Contains(strings.ToLower(f.Name), strings.ToLower(name)) {
			continue
		}
		if relKind != "" {
			hasRel := false
			for _, r := range f.Relations {
				if r.Kind == relKind {
					hasRel = true
					break
				}
			}
			if !hasRel {
				continue
			}
		}
		result = append(result, f)
	}
	return result
}

// queryFromIndices applies file/name/relKind filters over a pre-selected index
// slice, avoiding a full scan of s.facts when a kind (or file) index is used.
// kind is already matched by the caller's index selection and is not re-checked.
func (s *Store) queryFromIndices(indices []int, kind, file, name, relKind string) []Fact {
	nameLower := strings.ToLower(name)
	var result []Fact
	for _, idx := range indices {
		if idx >= len(s.facts) {
			continue
		}
		f := s.facts[idx]
		if file != "" && f.File != file {
			continue
		}
		if name != "" && !strings.Contains(strings.ToLower(f.Name), nameLower) {
			continue
		}
		if relKind != "" {
			hasRel := false
			for _, r := range f.Relations {
				if r.Kind == relKind {
					hasRel = true
					break
				}
			}
			if !hasRel {
				continue
			}
		}
		result = append(result, f)
	}
	return result
}

// QueryOpts holds the full set of query filters for QueryAdvanced.
// Multi-value filters within a dimension are OR-combined; filters across
// different dimensions are AND-combined.
type QueryOpts struct {
	Kind       string   // single kind filter (exact match)
	Kinds      []string // multi-kind filter (OR with Kind)
	File       string   // exact file filter
	Files      []string // multi-file filter (OR with File)
	FilePrefix string   // file path prefix filter (e.g. "internal/server")
	Name       string   // substring name filter
	Names      []string // exact name batch filter (OR)
	Repo       string   // repo label filter (exact match, for multi-repo mode)
	RelKind    string   // relation kind filter
	Prop       string   // property name filter
	PropValue  string   // property value filter (requires Prop)
	PropOp     string   // how PropValue compares: PropOps; "" is eq
	Offset     int      // number of results to skip
	Limit      int      // max results to return (0 = default 100, max 500)
}

// eachMatch calls visit for every fact matching opts, in store order, ignoring
// Offset and Limit. It holds the read lock throughout: visit must not call back into
// the store, and must neither keep nor modify the fact it is handed.
func (s *Store) eachMatch(opts QueryOpts, visit func(*Fact)) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Merge single and multi-value filters into sets for efficient lookup.
	kindSet := mergeIntoSet(opts.Kind, opts.Kinds)
	fileSet := mergeIntoSet(opts.File, opts.Files)
	nameSet := make(map[string]struct{}, len(opts.Names))
	for _, n := range opts.Names {
		if n != "" {
			nameSet[n] = struct{}{}
		}
	}

	nameLower := strings.ToLower(opts.Name)

	// Select the narrowest available index as the candidate set to avoid a
	// full O(N) scan when a high-selectivity filter is present.
	//
	// Priority: single-kind > single-file > exact-name batch > full scan.
	// Multi-kind and multi-file filters still fall back to the full slice
	// because building a union of index slices is only worthwhile when the
	// union is significantly smaller than N, which is hard to determine
	// cheaply; the remaining filters then trim the result.
	type iterMode int
	const (
		iterFull      iterMode = iota // scan all s.facts
		iterKindIndex                 // scan byKind[kind] indices
		iterFileIndex                 // scan byFile[file] indices
		iterNameUnion                 // scan union of byName[name] indices
	)

	mode := iterFull
	var indexSlice []int // for iterKindIndex / iterFileIndex

	if len(kindSet) == 1 {
		for k := range kindSet {
			if idxs, ok := s.byKind[k]; ok {
				indexSlice = idxs
				mode = iterKindIndex
			} else {
				// Kind filter specified but no facts of that kind — fast exit.
				return
			}
		}
	} else if len(fileSet) == 1 && opts.FilePrefix == "" {
		for f := range fileSet {
			if idxs, ok := s.byFile[f]; ok {
				indexSlice = idxs
				mode = iterFileIndex
			} else {
				return
			}
		}
	} else if len(nameSet) > 0 && opts.Name == "" {
		// Exact-name batch: union of byName index entries.
		// Only switch to this mode when no substring filter is also active,
		// to keep the logic simple.
		total := 0
		for n := range nameSet {
			total += s.namedCount(n)
		}
		if total < len(s.facts) {
			union := make([]int, 0, total)
			for n := range nameSet {
				union = s.appendNamedIdxs(union, n)
			}
			// nameSet is a map, so the union came out in a different order on every
			// call: offset paging over a names= query could skip or repeat facts. Store
			// order, as every other mode yields.
			sort.Ints(union)
			indexSlice = union
			mode = iterNameUnion
		}
	}

	// factAt retrieves a fact by absolute index in s.facts, regardless of mode.
	filterFact := func(f Fact) bool {
		// Kind filter
		if len(kindSet) > 0 {
			if _, ok := kindSet[f.Kind]; !ok {
				return false
			}
		}

		// Repo filter
		if opts.Repo != "" && f.Repo != opts.Repo {
			return false
		}

		// File filter (exact match set OR prefix)
		if len(fileSet) > 0 || opts.FilePrefix != "" {
			fileMatch := false
			if len(fileSet) > 0 {
				_, fileMatch = fileSet[f.File]
			}
			if !fileMatch && opts.FilePrefix != "" {
				fileMatch = strings.HasPrefix(f.File, opts.FilePrefix)
			}
			if !fileMatch {
				return false
			}
		}

		// Name filter: substring (Name) OR exact batch (Names)
		if opts.Name != "" || len(nameSet) > 0 {
			nameMatch := false
			if opts.Name != "" && strings.Contains(strings.ToLower(f.Name), nameLower) {
				nameMatch = true
			}
			if !nameMatch && len(nameSet) > 0 {
				_, nameMatch = nameSet[f.Name]
			}
			if !nameMatch {
				return false
			}
		}

		// Relation kind filter
		if opts.RelKind != "" {
			hasRel := false
			for _, r := range f.Relations {
				if r.Kind == opts.RelKind {
					hasRel = true
					break
				}
			}
			if !hasRel {
				return false
			}
		}

		// Property filter
		if opts.Prop != "" {
			v, ok := f.Prop(opts.Prop)
			if !ok {
				return false
			}
			if opts.PropValue != "" && !PropMatches(v, opts.PropOp, opts.PropValue) {
				return false
			}
		}

		return true
	}

	switch mode {
	case iterKindIndex, iterFileIndex, iterNameUnion:
		for _, idx := range indexSlice {
			if idx >= len(s.facts) {
				continue
			}
			if filterFact(s.facts[idx]) {
				visit(&s.facts[idx])
			}
		}
	default:
		for i := range s.facts {
			if filterFact(s.facts[i]) {
				visit(&s.facts[i])
			}
		}
	}
}

// QueryAdvanced returns facts matching the provided filter options along with
// the total count of matches before offset/limit are applied.
func (s *Store) QueryAdvanced(opts QueryOpts) ([]Fact, int) {
	var matched []Fact
	s.eachMatch(opts, func(f *Fact) { matched = append(matched, *f) })

	total := len(matched)

	// Apply offset
	if opts.Offset > 0 {
		if opts.Offset >= len(matched) {
			return nil, total
		}
		matched = matched[opts.Offset:]
	}

	// Apply limit
	limit := opts.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	if len(matched) > limit {
		matched = matched[:limit]
	}

	return matched, total
}

// QueryEach calls visit for every fact matching opts from Offset on, with no Limit,
// and returns the total number of matches, as QueryAdvanced counts them. It copies
// nothing, so a caller that only aggregates does not pay for a slice of every match.
// visit runs under the store's read lock: it must not call back into the store, and
// must neither keep nor modify the fact it is handed.
func (s *Store) QueryEach(opts QueryOpts, visit func(*Fact)) int {
	total := 0
	s.eachMatch(opts, func(f *Fact) {
		total++
		if total > opts.Offset {
			visit(f)
		}
	})
	return total
}

// LookupByExactName returns all facts with the given exact name using the index.
func (s *Store) LookupByExactName(name string) []Fact {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.collectByName(name)
}

// ReverseLookup returns all facts that have a relation targeting the given name.
// If relKind is non-empty, only relations of that kind are considered.
// When the graph index is available it delegates to the O(1) reverse map;
// otherwise it falls back to a linear scan of all facts.
func (s *Store) ReverseLookup(targetName, relKind string) []Fact {
	s.mu.RLock()
	g := s.graph
	s.mu.RUnlock()

	if g != nil {
		return g.ReverseFacts(targetName, relKind)
	}

	// Graph not yet built — linear scan fallback.
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []Fact
	for _, f := range s.facts {
		for _, r := range f.Relations {
			if r.Target == targetName && (relKind == "" || r.Kind == relKind) {
				result = append(result, f)
				break
			}
		}
	}
	return result
}

// mergeIntoSet combines a single value and a slice into a set.
// Empty strings are ignored.
func mergeIntoSet(single string, multi []string) map[string]struct{} {
	set := make(map[string]struct{}, len(multi)+1)
	if single != "" {
		set[single] = struct{}{}
	}
	for _, v := range multi {
		if v != "" {
			set[v] = struct{}{}
		}
	}
	if len(set) == 0 {
		return nil
	}
	return set
}

// SetRepoRange sets the Repo field for facts at indices [startIdx, current
// length) without modifying file paths. Used in non-append mode so the repo
// filter works even for single-repo snapshots.
func (s *Store) SetRepoRange(startIdx int, repo string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := startIdx; i < len(s.facts); i++ {
		f := &s.facts[i]
		if f.Repo == "" {
			f.Repo = repo
			s.byRepo[repo] = append(s.byRepo[repo], i)
		}
	}
}

// TagRange sets the Repo field and prefixes File paths for facts added at
// indices [startIdx, current length). Used by the engine in append mode to
// namespace facts from different repositories.
func (s *Store) TagRange(startIdx int, repo, filePrefix string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := startIdx; i < len(s.facts); i++ {
		f := &s.facts[i]
		// Index under the label once. SetRepoRange runs over the same range
		// first and has already indexed every fact it labelled, so re-adding
		// here listed each appended fact twice under its repo: ByRepo returned
		// doubles and CountByRepo counted them, for every repo but the first in
		// a union.
		if f.Repo != repo {
			if f.Repo != "" {
				s.removeFromIndex(s.byRepo, f.Repo, i)
			}
			s.byRepo[repo] = append(s.byRepo[repo], i)
		}
		f.Repo = repo
		if f.File != "" {
			oldFile := f.File
			f.File = filePrefix + f.File
			f.RepoPrefixed = true
			// Update byFile index: remove old key, add new key.
			s.removeFromIndex(s.byFile, oldFile, i)
			s.byFile[f.File] = append(s.byFile[f.File], i)
		}
	}
}

// SetPropRange sets a prop on every fact added at indices [startIdx, current
// length) whose File satisfies match, and returns how many it touched. match is
// called once per distinct file by the caller's own memoization, so it may be
// expensive (the engine reads file headers through it).
func (s *Store) SetPropRange(startIdx int, key string, value any, match func(file string) bool) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for i := startIdx; i < len(s.facts); i++ {
		f := &s.facts[i]
		if f.File == "" || !match(f.File) {
			continue
		}
		if f.Props == nil {
			f.Props = map[string]any{}
		}
		f.SetProp(key, value)
		n++
	}
	return n
}

func (s *Store) removeFromIndex(idx map[string][]int, key string, target int) {
	indices := idx[key]
	for j, v := range indices {
		if v == target {
			idx[key] = append(indices[:j], indices[j+1:]...)
			break
		}
	}
	if len(idx[key]) == 0 {
		delete(idx, key)
	}
}

// TagUntagged sets the Repo field and prefixes File paths for all facts that
// belong to the given repo (or have an empty Repo). This is used when entering
// append mode to retroactively label and prefix facts from a prior single-repo
// snapshot so they become filterable alongside newly appended facts.
func (s *Store) TagUntagged(repo, filePrefix string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	// The whole doc comments are keyed by repo and file too, and must follow their
	// facts: left under the old key, the first repository of a cluster, tagged here
	// when the second arrives, rendered its comments flattened to the one-line summary.
	var moved map[FactKey]FactKey
	for i := range s.facts {
		f := &s.facts[i]

		// Skip facts belonging to a DIFFERENT repo (from a prior append).
		if f.Repo != "" && f.Repo != repo {
			continue
		}
		before := FactKey{Repo: f.Repo, Kind: f.Kind, Name: f.Name, File: f.File}

		// Set Repo if not already set.
		if f.Repo == "" {
			f.Repo = repo
			s.byRepo[repo] = append(s.byRepo[repo], i)
		}

		// Prefix the file path unless it was prefixed already. Looking for the prefix in
		// the path would leave repository beta's own beta/code.py unprefixed here, and so
		// different from what it becomes when beta is appended second.
		if f.File != "" && !f.RepoPrefixed {
			oldFile := f.File
			f.File = filePrefix + f.File
			f.RepoPrefixed = true
			s.removeFromIndex(s.byFile, oldFile, i)
			s.byFile[f.File] = append(s.byFile[f.File], i)
			count++
		}

		if after := (FactKey{Repo: f.Repo, Kind: f.Kind, Name: f.Name, File: f.File}); after != before {
			if _, ok := s.docTexts[before]; ok {
				if moved == nil {
					moved = map[FactKey]FactKey{}
				}
				moved[before] = after
			}
		}
	}
	// Every text is read before any is deleted: two facts can share an old key.
	texts := make(map[FactKey]string, len(moved))
	for before, after := range moved {
		texts[after] = s.docTexts[before]
	}
	for before := range moved {
		delete(s.docTexts, before)
	}
	for after, text := range texts {
		s.docTexts[after] = text
	}
	return count
}

// RemoveWhere removes every fact for which pred returns true and rebuilds all
// indices from the surviving facts. The graph index is invalidated (set to nil)
// because removing facts shifts slice positions; callers should rebuild it via
// BuildGraph afterwards. Returns the number of facts removed.
//
// This is used to drop previously-synthesized facts (e.g. cross-repo links)
// before recomputing them, so repeated append/link cycles stay idempotent.
func (s *Store) RemoveWhere(pred func(Fact) bool) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Find the first match before copying anything. Binders and the cross-repo
	// linker call this at the start of every snapshot to drop output from a previous
	// pass, and on a single-repository snapshot there usually is none: copying the
	// whole store to learn that cost four full copies per snapshot, which put about
	// 650 MiB on gitlab's peak heap.
	first := -1
	for i := range s.facts {
		if pred(s.facts[i]) {
			first = i
			break
		}
	}
	if first < 0 {
		return 0
	}
	kept := make([]Fact, first, len(s.facts)-1)
	copy(kept, s.facts[:first])
	removed := 1
	for _, f := range s.facts[first+1:] {
		if pred(f) {
			removed++
			continue
		}
		kept = append(kept, f)
	}

	// Rebuild facts slice and all indices from scratch.
	s.facts = kept
	s.frozenUpTo = 0
	s.byKind = make(map[string][]int)
	s.byFile = make(map[string][]int)
	s.byName = make(map[string]int32)
	s.byNameDup = nil
	s.byRepo = make(map[string][]int)
	for idx, f := range s.facts {
		s.byKind[f.Kind] = append(s.byKind[f.Kind], idx)
		if f.File != "" {
			s.byFile[f.File] = append(s.byFile[f.File], idx)
		}
		if f.Name != "" {
			s.indexName(f.Name, idx)
		}
		if f.Repo != "" {
			s.byRepo[f.Repo] = append(s.byRepo[f.Repo], idx)
		}
	}
	s.graph = nil
	return removed
}

// UpdateWhere applies mutate to a pointer to every stored fact, letting callers
// modify fields or Props in place under the store lock. It does NOT touch the
// kind/file/name/repo indexes or the graph, so it is intended only for property
// tweaks that change none of those indexed fields; callers that change indexed
// fields must rebuild via RemoveWhere/Add instead.
func (s *Store) UpdateRange(startIdx int, mutate func(*Fact)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := startIdx; i < len(s.facts); i++ {
		mutate(&s.facts[i])
	}
}

func (s *Store) UpdateWhere(mutate func(*Fact)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.facts {
		mutate(&s.facts[i])
	}
}

// Modules returns all module facts.
func (s *Store) Modules() []Fact {
	return s.ByKind(KindModule)
}

// Symbols returns all symbol facts.
func (s *Store) Symbols() []Fact {
	return s.ByKind(KindSymbol)
}

// Clear removes all facts from the store.
func (s *Store) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.facts = nil
	s.byKind = make(map[string][]int)
	s.byFile = make(map[string][]int)
	s.byName = make(map[string]int32)
	s.byNameDup = nil
	s.byRepo = make(map[string][]int)
	s.graph = nil
	s.intern = nil
	s.frozen, s.frozenUpTo = false, 0
	s.sharing, s.sharedUpTo = nil, 0
	s.targetAliases = nil
	s.docTexts = nil
}

// BuildGraph constructs the adjacency-list graph index from the current facts.
// Call this after all facts have been added and tagged (e.g. after snapshot generation).
func (s *Store) BuildGraph() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.graph = NewGraphWithAliases(s.facts, s.targetAliases)
	// The interning table has done its job: the strings it canonicalized are held by
	// the facts themselves now, and the map is pure overhead (1.3M entries on the
	// kernel) for the rest of the store's life. Add recreates it if more facts
	// arrive — interning is an optimization, so a fresh table is merely less
	// effective, never incorrect.
	s.intern = nil
}

// Graph returns the current graph index, or nil if BuildGraph has not been called.
func (s *Store) Graph() *Graph {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.graph
}

// WriteJSONL writes all facts as JSONL to the given writer in a deterministic
// order. Extractors emit facts (and within-fact relations) in map-iteration
// order, which varies run to run; sorting here makes facts.jsonl byte-stable for
// a given commit, so the snapshot is reproducible and incremental regenerations
// don't churn the file. Relations are sorted on a copy here; Freeze puts the store's
// own into the same order (see sortRelations), so a frozen store already matches.
//
// This is also where each fact's id and each relation's target_id are computed
// (see wire.go and id.go). They are written and never stored, so no internal
// reader can come to depend on them.
//
// The lines are sorted without holding them all. Each line is marshalled once to keep
// its first jsonlKeyLen bytes, the facts are sorted by those, the few runs whose lines
// share all of them and run longer are marshalled again and sorted in full, and the
// lines are marshalled a last time in that order as they are written. Holding every
// line instead cost the size of facts.jsonl in heap at the end of a run: 6.8 GB of a
// 14.7 GB live peak on a 100-repository cluster. The order is the one a byte-wise sort
// of the whole lines gives, so facts.jsonl and every id derived from it are unchanged.
func (s *Store) WriteJSONL(w io.Writer) error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	order, err := s.jsonlOrder()
	if err != nil {
		return err
	}
	n := len(order)

	// Written a window at a time: the window's chunks are marshalled side by side and
	// then written in order, so what is held is the window, not the file.
	bw := bufio.NewWriterSize(w, 1<<20)
	window := jsonlChunk * runtime.GOMAXPROCS(0)
	out := make([][]byte, (window+jsonlChunk-1)/jsonlChunk)
	for start := 0; start < n; start += window {
		part := order[start:min(start+window, n)]
		if err := inChunks(len(part), func(lo, hi int, ws *wireScratch) error {
			buf := out[lo/jsonlChunk][:0]
			for _, i := range part[lo:hi] {
				b, err := s.wireLine(s.facts[i], ws)
				if err != nil {
					return err
				}
				buf = append(buf, b[:len(b)+1]...) // the newline the encoder ended it with
			}
			out[lo/jsonlChunk] = buf
			return nil
		}); err != nil {
			return err
		}
		for c := 0; c*jsonlChunk < len(part); c++ {
			if _, err := bw.Write(out[c]); err != nil {
				return err
			}
		}
	}
	return bw.Flush()
}

// jsonlOrder returns the fact indices in the order facts.jsonl lists the facts (see
// WriteJSONL). The caller must hold s.mu.
func (s *Store) jsonlOrder() ([]int32, error) {
	n := len(s.facts)
	keys := make([]byte, n*jsonlKeyLen)
	long := make([]bool, n) // the line runs past its key
	if err := inChunks(n, func(lo, hi int, ws *wireScratch) error {
		for i := lo; i < hi; i++ {
			// The scalar fields a line opens with are usually the whole key; the
			// line is marshalled for it only when they are shorter.
			b, err := ws.head(&s.facts[i])
			if err != nil {
				return err
			}
			// A line always runs past its head, so a key taken from a head is long
			// even when the head ends exactly at the key's end.
			long[i] = true
			if len(b) < jsonlKeyLen {
				if b, err = s.wireLine(s.facts[i], ws); err != nil {
					return err
				}
				long[i] = len(b) > jsonlKeyLen
			}
			copy(keys[i*jsonlKeyLen:(i+1)*jsonlKeyLen], b)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	key := func(i int32) []byte { return keys[int(i)*jsonlKeyLen : (int(i)+1)*jsonlKeyLen] }

	// A key is the line's first bytes, zero-padded; JSON holds no zero byte, so a line
	// that ends inside the key sorts before every longer line sharing its bytes, as it
	// does in full.
	order := make([]int32, n)
	for i := range order {
		order[i] = int32(i)
	}
	slices.SortFunc(order, func(a, b int32) int { return bytes.Compare(key(a), key(b)) })

	// Equal keys are equal lines unless a line runs on; those runs are sorted in full.
	ws := newWireScratch()
	for lo := 0; lo < n; {
		hi, anyLong := lo+1, long[order[lo]]
		for hi < n && bytes.Equal(key(order[lo]), key(order[hi])) {
			anyLong = anyLong || long[order[hi]]
			hi++
		}
		if anyLong && hi-lo > 1 {
			if err := s.sortRunByLine(order[lo:hi], ws); err != nil {
				return nil, err
			}
		}
		lo = hi
	}
	return order, nil
}

// SortLikeJSONL puts the facts in the order facts.jsonl lists them, so a store built
// by a generate holds them as a store read back from that file does. Extractors emit
// in map-iteration order, so without it a built store's order differs from one run to
// the next and from a restored store's, and so does everything that lists facts in
// store order: an offset-paged query read across an unload and restore skipped some
// facts and repeated others. The facts are moved, not changed, so a frozen store may
// be sorted; Freeze sorts relations, which are part of a line, so sort after it.
// The indexes and the graph are remapped in place rather than rebuilt.
func (s *Store) SortLikeJSONL() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	order, err := s.jsonlOrder()
	if err != nil {
		return err
	}
	s.applyOrder(order)
	return nil
}

// SortByLocation puts the facts in an order that depends only on where each one is:
// repository, kind, name, file, line, end line, then the method, role and language
// extraction gave it. Linking runs over the store in its order, and binders and explainers that
// keep the first or last of several same-named facts (a Ruby class reopened across
// files, a Go method on several receivers) keep whichever that order puts there. A
// generate held the facts in extraction order and a store restored from facts.jsonl
// in that file's order, so a refresh, which links a restored store, bound some of them
// differently from a full generate of the same trees. Neither key is what linking
// writes, so both paths sort to the same order.
func (s *Store) SortByLocation() {
	s.mu.Lock()
	defer s.mu.Unlock()
	order := make([]int32, len(s.facts))
	for i := range order {
		order[i] = int32(i)
	}
	slices.SortStableFunc(order, func(a, b int32) int {
		fa, fb := &s.facts[a], &s.facts[b]
		if c := strings.Compare(fa.Repo, fb.Repo); c != 0 {
			return c
		}
		if c := strings.Compare(fa.Kind, fb.Kind); c != 0 {
			return c
		}
		if c := strings.Compare(fa.Name, fb.Name); c != 0 {
			return c
		}
		if c := strings.Compare(fa.File, fb.File); c != 0 {
			return c
		}
		if fa.Line != fb.Line {
			return fa.Line - fb.Line
		}
		if fa.EndLine != fb.EndLine {
			return fa.EndLine - fb.EndLine
		}
		// Extraction sets these and linking does not: routes on one path and line
		// differ by method and role, modules two extractors emit by language.
		for _, key := range locationTieProps {
			if c := strings.Compare(fa.PropString(key), fb.PropString(key)); c != 0 {
				return c
			}
		}
		return 0
	})
	s.applyOrder(order)
}

// locationTieProps break ties between facts at one location (see SortByLocation).
var locationTieProps = []string{"method", "role", "language"}

// applyOrder moves the facts so facts[to] is the fact that was at order[to], and
// remaps every index and the graph. Caller holds s.mu.
func (s *Store) applyOrder(order []int32) {
	inv := make([]int32, len(order))
	moved := false
	for to, from := range order {
		inv[from] = int32(to)
		moved = moved || int(from) != to
	}
	if !moved {
		return
	}
	// facts[to] = old facts[order[to]], one cycle at a time, marking order as it goes.
	for start := range order {
		if order[start] < 0 {
			continue
		}
		held := s.facts[start]
		at := start
		for {
			from := int(order[at])
			order[at] = -1
			if from == start {
				s.facts[at] = held
				break
			}
			s.facts[at] = s.facts[from]
			at = from
		}
	}

	remap := func(idxs []int) {
		for i, x := range idxs {
			idxs[i] = int(inv[x])
		}
		slices.Sort(idxs)
	}
	for _, m := range []map[string][]int{s.byKind, s.byFile, s.byRepo} {
		for _, idxs := range m {
			remap(idxs)
		}
	}
	for name, v := range s.byName {
		if v >= 0 {
			s.byName[name] = inv[v]
		}
	}
	for _, idxs := range s.byNameDup {
		for i, x := range idxs {
			idxs[i] = inv[x]
		}
		slices.Sort(idxs)
	}
	if s.graph != nil {
		s.graph.remapFacts(s.facts, inv)
	}
}

// jsonlChunk is how many facts one worker marshals at a time.
const jsonlChunk = 4096

// inChunks runs fn over [0, n) in chunks of jsonlChunk, GOMAXPROCS at a time, each
// worker with its own scratch, and returns the first error. Chunks start at multiples
// of jsonlChunk.
func inChunks(n int, fn func(lo, hi int, ws *wireScratch) error) error {
	var next atomic.Int64
	var mu sync.Mutex
	var first error
	var wg sync.WaitGroup
	for w := min(runtime.GOMAXPROCS(0), (n+jsonlChunk-1)/jsonlChunk); w > 0; w-- {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ws := newWireScratch()
			for {
				lo := int(next.Add(jsonlChunk)) - jsonlChunk
				if lo >= n {
					return
				}
				if err := fn(lo, min(lo+jsonlChunk, n), ws); err != nil {
					mu.Lock()
					if first == nil {
						first = err
					}
					mu.Unlock()
					return
				}
			}
		}()
	}
	wg.Wait()
	return first
}

func newWireScratch() *wireScratch {
	ws := &wireScratch{rels: make([]wireRelation, 0, 8), props: make(map[string]any, 16)}
	ws.enc = json.NewEncoder(&ws.buf)
	return ws
}

// wireHead is the scalar fields a facts.jsonl line opens with, in its order and under
// its tags: a Fact's fields before props. TestWireHead_IsALinesPrefix holds it to that.
type wireHead struct {
	Kind         string `json:"kind"`
	Name         string `json:"name"`
	File         string `json:"file,omitempty"`
	Line         int    `json:"line,omitempty"`
	EndLine      int    `json:"end_line,omitempty"`
	Column       int    `json:"column,omitempty"`
	EndColumn    int    `json:"end_column,omitempty"`
	Repo         string `json:"repo,omitempty"`
	RepoPrefixed bool   `json:"repo_prefixed,omitempty"`
}

// head returns the bytes f's line opens with: its head fields without the closing
// brace, which the line continues past. Valid until ws is used again.
func (ws *wireScratch) head(f *Fact) ([]byte, error) {
	ws.buf.Reset()
	// Through a pointer to ws's own, so the call boxes no copy of it.
	ws.hd = wireHead{f.Kind, f.Name, f.File, f.Line, f.EndLine, f.Column, f.EndColumn, f.Repo, f.RepoPrefixed}
	if err := ws.enc.Encode(&ws.hd); err != nil {
		return nil, fmt.Errorf("encoding fact %q: %w", f.Name, err)
	}
	b := ws.buf.Bytes()
	return b[:len(b)-2], nil // "}\n"
}

// jsonlKeyLen is how much of a line the first sort compares. Lines begin with the
// kind and the name, so most differ within it.
const jsonlKeyLen = 96

// wireScratch is reused from one line to the next: a line is marshalled before the
// next one overwrites it, and a per-fact allocation would be paid tens of millions of
// times on the largest graphs.
type wireScratch struct {
	rels  []wireRelation
	ids   []byte
	props map[string]any
	// The encoder writes into buf, which a line is read from before the next is
	// encoded: json.Marshal would allocate every line's bytes afresh.
	buf bytes.Buffer
	enc *json.Encoder
	hd  wireHead
}

// sortRunByLine sorts a run of facts whose keys are equal by their whole lines.
//
// Most are told apart by their heads: two lines whose heads differ, neither being a
// prefix of the other, compare as their heads do. Only the rest are marshalled, once
// each; at 96 bytes half the lines of a large repository share their key with another,
// and marshalling all of those again cost a third of the function's allocations.
func (s *Store) sortRunByLine(run []int32, ws *wireScratch) error {
	heads := make(map[int32][]byte, len(run))
	for _, i := range run {
		h, err := ws.head(&s.facts[i])
		if err != nil {
			return err
		}
		heads[i] = bytes.Clone(h)
	}
	lines := map[int32][]byte{}
	var failed error
	line := func(i int32) []byte {
		if b, ok := lines[i]; ok {
			return b
		}
		b, err := s.wireLine(s.facts[i], ws)
		if err != nil && failed == nil {
			failed = err
		}
		b = bytes.Clone(b)
		lines[i] = b
		return b
	}
	slices.SortStableFunc(run, func(a, b int32) int {
		ha, hb := heads[a], heads[b]
		if n := min(len(ha), len(hb)); !bytes.Equal(ha[:n], hb[:n]) {
			return bytes.Compare(ha, hb)
		}
		return bytes.Compare(line(a), line(b))
	})
	return failed
}

// wireLine marshals f as its facts.jsonl line, without the newline, computing the ids
// WriteJSONL describes. Called with s.mu held for reading. The line is in ws's buffer,
// followed there by its newline, and valid until ws is used again.
func (s *Store) wireLine(f Fact, ws *wireScratch) ([]byte, error) {
	rels := ws.rels[:0]
	for _, r := range f.Relations {
		wr := wireRelation{Relation: r}
		if t := s.targetFactFor(r.Target, f.Repo); t >= 0 {
			if tf := s.facts[t]; LandsByName(f, r, tf) {
				wr.TargetID, ws.ids = factIDInto(ws.ids, tf.Repo, tf.Kind, tf.Name, tf.IdentityFile())
			}
		} else if k, ok := s.targetAliases[r.Target]; ok {
			// A repository's facts are prefixed together, so any of them answers for k.
			prefixed := false
			if in := s.byRepo[k.Repo]; len(in) > 0 {
				prefixed = s.facts[in[0]].RepoPrefixed
			}
			wr.TargetID, ws.ids = factIDInto(ws.ids, k.Repo, k.Kind, k.Name, identityFile(k.Repo, k.File, prefixed))
		}
		rels = append(rels, wr)
	}
	ws.rels = rels
	if len(rels) > 1 {
		slices.SortFunc(rels, func(a, b wireRelation) int { return compareRelations(a.Relation, b.Relation) })
	}
	// f is a copy, and its own Relations field is shadowed by the one below, so
	// neither the store's facts nor their relation slices are touched.
	var factID string
	factID, ws.ids = factIDInto(ws.ids, f.Repo, f.Kind, f.Name, f.IdentityFile())
	if f.Kind == KindRoute {
		// f is a copy, and withFactRefIDs copies before widening, so the store's
		// shared Props map is never written.
		f.Props, ws.ids = withFactRefIDs(f.Props, f.Repo, f.File, f.RepoPrefixed, ws.ids)
	}
	ws.buf.Reset()
	if err := ws.enc.Encode(wireFact{FactJSON: f.jsonViewInto(ws.props), Relations: rels, ID: factID}); err != nil {
		return nil, fmt.Errorf("encoding fact %q: %w", f.Name, err)
	}
	b := ws.buf.Bytes()
	return b[:len(b)-1], nil
}

// WriteJSONLFile writes all facts as JSONL to the given file path.
func (s *Store) WriteJSONLFile(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("creating %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	bw := bufio.NewWriter(f)
	if err := s.WriteJSONL(bw); err != nil {
		return err
	}
	return bw.Flush()
}

// ReadJSONL reads facts from a JSONL reader and adds them to the store.
func (s *Store) ReadJSONL(r io.Reader) error {
	return s.readJSONL(r, false)
}

// readJSONL is ReadJSONL, sharing each fact as it is added when share is set.
func (s *Store) readJSONL(r io.Reader, share bool) error {
	scanner := bufio.NewScanner(r)
	// Allow large lines
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var f Fact
		if err := json.Unmarshal(line, &f); err != nil {
			return fmt.Errorf("decoding fact: %w", err)
		}
		dropWireIDs(&f)
		f.liftIdentProps()
		s.mu.Lock()
		idx := s.addLocked(f)
		if share {
			s.sharing.share(&s.facts[idx])
			s.sharedUpTo = idx + 1
		}
		s.mu.Unlock()
	}
	return scanner.Err()
}

// ReadJSONLFileShared reads path as ReadJSONLFile does, and shares each fact's Props
// and Relations with an equal earlier fact's as it goes: what Freeze does, done while
// reading. Decoding gives every fact its own Props map, and on a kernel-sized graph
// nine in ten of them repeat one already read; sharing them only at Freeze held every
// copy alive at once first, which put a restore's peak near twice what it settles at.
//
// It carries Freeze's precondition from the first fact it reads: nothing may write a
// fact's Props or Relations again, and the store must be Frozen before it is
// published. Freeze then shares only what was added after the read.
//
// The fact array is sized from the file's line count, so it is allocated once rather
// than doubled its way up, which held the outgrown arrays as garbage at the peak and
// left up to half of the last one unused for as long as the store lived.
func (s *Store) ReadJSONLFileShared(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	return s.ReadJSONLShared(f)
}

// ReadJSONLShared is ReadJSONLFileShared over a file already open, read from its
// start. A caller that must read the facts and the files beside them from one write
// opens them all first.
func (s *Store) ReadJSONLShared(f *os.File) error {
	path := f.Name()
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	lines, err := countLines(f)
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}

	s.mu.Lock()
	grown := make([]Fact, len(s.facts), len(s.facts)+lines)
	copy(grown, s.facts)
	s.facts = grown
	if s.sharing == nil {
		s.sharing = newSharer()
	}
	for i := s.sharedUpTo; i < len(s.facts); i++ {
		s.sharing.share(&s.facts[i])
	}
	s.sharedUpTo = len(s.facts)
	s.mu.Unlock()

	return s.readJSONL(f, true)
}

// countLines counts the lines in r, a final line without its newline included. It is an
// upper bound on the facts in a JSONL file, which may hold blank lines.
func countLines(r io.Reader) (int, error) {
	buf := make([]byte, 256*1024)
	n, last := 0, byte('\n')
	for {
		k, err := r.Read(buf)
		if k > 0 {
			n += bytes.Count(buf[:k], []byte{'\n'})
			last = buf[k-1]
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, err
		}
	}
	if last != '\n' {
		n++
	}
	return n, nil
}

// MarkLegacyRepoPrefixes sets RepoPrefixed on a multi-repo store read from a file written
// before facts carried it, where it is guessed from the path as those builds did. A store
// in which any fact carries the mark already is left alone, and so is a single-repo one.
func (s *Store) MarkLegacyRepoPrefixes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.facts {
		if s.facts[i].RepoPrefixed {
			return 0
		}
	}
	n := 0
	for i := range s.facts {
		f := &s.facts[i]
		if f.Repo != "" && strings.HasPrefix(f.File, f.Repo+"/") {
			f.RepoPrefixed = true
			n++
		}
	}
	return n
}

// ReadJSONLFile reads facts from a JSONL file and adds them to the store.
func (s *Store) ReadJSONLFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	return s.ReadJSONL(f)
}

func (s *Store) collectByIndex(indices []int) []Fact {
	result := make([]Fact, 0, len(indices))
	for _, idx := range indices {
		if idx < len(s.facts) {
			result = append(result, s.facts[idx])
		}
	}
	return result
}

// TakeDocTexts moves PropDocText off every fact from startIdx on into the store's
// side table, keyed by the fact's identity, and returns how many it moved. Call it
// once the facts carry their final repo label and file path, since those are part
// of the key.
//
// It records every text before it deletes any, because an extractor may hand two
// facts the same Props map, and deleting from the first would hide the text from
// the second.
func (s *Store) TakeDocTexts(startIdx int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for i := startIdx; i < len(s.facts); i++ {
		f := &s.facts[i]
		if text, _ := f.Props[PropDocText].(string); text != "" {
			if s.docTexts == nil {
				s.docTexts = make(map[FactKey]string)
			}
			s.docTexts[FactKey{Repo: f.Repo, Kind: f.Kind, Name: f.Name, File: f.File}] = text
			n++
		}
	}
	for i := startIdx; i < len(s.facts); i++ {
		delete(s.facts[i].Props, PropDocText)
	}
	return n
}

// SetDocText records text as the whole comment of the fact with key k: a restore that
// will be extended reads the texts back from docs.jsonl, since facts.jsonl carries only
// the one-line PropDoc.
func (s *Store) SetDocText(k FactKey, text string) {
	if text == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.docTexts == nil {
		s.docTexts = make(map[FactKey]string)
	}
	s.docTexts[k] = text
}

// DocText returns the whole comment of the fact with key k, or "" when its PropDoc
// is all there is.
func (s *Store) DocText(k FactKey) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.docTexts[k]
}

// RemoveDocTexts drops the doc texts of every fact labelled repo. A repository
// replaced inside a union loses its facts through RemoveWhere, and its texts here.
func (s *Store) RemoveDocTexts(repo string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.docTexts {
		if k.Repo == repo {
			delete(s.docTexts, k)
		}
	}
}

// CarryDocTexts copies into s the doc texts held by from whose key keep accepts.
// Append mode uses it to carry the other repositories' texts forward with their facts.
func (s *Store) CarryDocTexts(from *Store, keep func(FactKey) bool) {
	from.mu.RLock()
	defer from.mu.RUnlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, text := range from.docTexts {
		if !keep(k) {
			continue
		}
		if s.docTexts == nil {
			s.docTexts = make(map[FactKey]string, len(from.docTexts))
		}
		s.docTexts[k] = text
	}
}
