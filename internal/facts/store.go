package facts

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/enola-labs/enola/internal/factpath"
)

// Store provides in-memory storage and querying of facts with JSONL persistence.
type Store struct {
	mu    sync.RWMutex
	facts []Fact

	// Indexes for fast lookups
	byKind map[string][]int // kind -> indices into facts
	byFile map[string][]int // file -> indices into facts
	byName map[string][]int // name -> indices into facts
	byRepo map[string][]int // repo label -> indices into facts

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
}

// NewStore creates an empty fact store.
func NewStore() *Store {
	return &Store{
		byKind: make(map[string][]int),
		byFile: make(map[string][]int),
		byName: make(map[string][]int),
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
			s.byName[f.Name] = append(s.byName[f.Name], idx)
		}
		if f.Repo != "" {
			s.byRepo[f.Repo] = append(s.byRepo[f.Repo], idx)
		}
	}
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
		if result[i].PropCount() > 0 {
			result[i].Props = result[i].CloneProps()
		}
	}
	return result
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
	return s.collectByIndex(s.byName[name])
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
	Offset     int      // number of results to skip
	Limit      int      // max results to return (0 = default 100, max 500)
}

// QueryAdvanced returns facts matching the provided filter options along with
// the total count of matches before offset/limit are applied.
func (s *Store) QueryAdvanced(opts QueryOpts) ([]Fact, int) {
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
				return nil, 0
			}
		}
	} else if len(fileSet) == 1 && opts.FilePrefix == "" {
		for f := range fileSet {
			if idxs, ok := s.byFile[f]; ok {
				indexSlice = idxs
				mode = iterFileIndex
			} else {
				return nil, 0
			}
		}
	} else if len(nameSet) > 0 && opts.Name == "" {
		// Exact-name batch: union of byName index entries.
		// Only switch to this mode when no substring filter is also active,
		// to keep the logic simple.
		total := 0
		for n := range nameSet {
			total += len(s.byName[n])
		}
		if total < len(s.facts) {
			union := make([]int, 0, total)
			for n := range nameSet {
				union = append(union, s.byName[n]...)
			}
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
			if opts.PropValue != "" && fmt.Sprintf("%v", v) != opts.PropValue {
				return false
			}
		}

		return true
	}

	var matched []Fact

	switch mode {
	case iterKindIndex, iterFileIndex, iterNameUnion:
		for _, idx := range indexSlice {
			if idx >= len(s.facts) {
				continue
			}
			if filterFact(s.facts[idx]) {
				matched = append(matched, s.facts[idx])
			}
		}
	default:
		for _, f := range s.facts {
			if filterFact(f) {
				matched = append(matched, f)
			}
		}
	}

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

// LookupByExactName returns all facts with the given exact name using the index.
func (s *Store) LookupByExactName(name string) []Fact {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.collectByIndex(s.byName[name])
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
	for i := range s.facts {
		f := &s.facts[i]

		// Skip facts belonging to a DIFFERENT repo (from a prior append).
		if f.Repo != "" && f.Repo != repo {
			continue
		}

		// Set Repo if not already set.
		if f.Repo == "" {
			f.Repo = repo
			s.byRepo[repo] = append(s.byRepo[repo], i)
		}

		// Prefix the file path if it doesn't already have the prefix.
		if f.File != "" && !strings.HasPrefix(f.File, filePrefix) {
			oldFile := f.File
			f.File = filePrefix + f.File
			s.removeFromIndex(s.byFile, oldFile, i)
			s.byFile[f.File] = append(s.byFile[f.File], i)
			count++
		}
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

	kept := s.facts[:0:0]
	removed := 0
	for _, f := range s.facts {
		if pred(f) {
			removed++
			continue
		}
		kept = append(kept, f)
	}
	if removed == 0 {
		return 0
	}

	// Rebuild facts slice and all indices from scratch.
	s.facts = kept
	s.byKind = make(map[string][]int)
	s.byFile = make(map[string][]int)
	s.byName = make(map[string][]int)
	s.byRepo = make(map[string][]int)
	for idx, f := range s.facts {
		s.byKind[f.Kind] = append(s.byKind[f.Kind], idx)
		if f.File != "" {
			s.byFile[f.File] = append(s.byFile[f.File], idx)
		}
		if f.Name != "" {
			s.byName[f.Name] = append(s.byName[f.Name], idx)
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
	s.byName = make(map[string][]int)
	s.byRepo = make(map[string][]int)
	s.graph = nil
	s.intern = nil
	s.frozen = false
}

// BuildGraph constructs the adjacency-list graph index from the current facts.
// Call this after all facts have been added and tagged (e.g. after snapshot generation).
func (s *Store) BuildGraph() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.graph = NewGraph(s.facts)
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

// Byte cost of the two wire-only id fields, for the size estimate: `,"id":"…"`
// with a 32-character value, and `,"target_id":"…"` likewise.
const (
	idFieldBytes       = len(`,"id":""`) + 2*idBytes
	idTargetFieldBytes = len(`,"target_id":""`) + 2*idBytes
)

// estimateJSONLSize guesses the serialized size of ff by marshalling a sample and
// extrapolating, so WriteJSONL can allocate its buffer once.
//
// It is a size hint and nothing more: too small merely restores the append growth it
// was avoiding, and too large wastes the difference until the buffer is released. The
// 10% margin and the sample size are chosen so that ordinary variation between facts
// does not push a real corpus over the estimate.
func estimateJSONLSize(ff []Fact) int {
	const sampleSize = 512
	if len(ff) == 0 {
		return 0
	}
	// Sample evenly across the slice rather than the first N: facts are grouped by
	// extractor and by file, so a prefix is a poor stand-in for the whole.
	stride := len(ff) / sampleSize
	if stride < 1 {
		stride = 1
	}
	total, n := 0, 0
	for i := 0; i < len(ff); i += stride {
		b, err := json.Marshal(ff[i])
		if err != nil {
			continue // the real pass reports it; the estimate just skips it
		}
		// A Fact marshals without the two id fields the wire shape adds, so
		// count them here. Assuming every relation resolves over-estimates (most
		// corpora resolve about half), which is the safe direction: too small
		// restores the append growth this exists to avoid.
		total += len(b) + 1 + idFieldBytes + idTargetFieldBytes*len(ff[i].Relations)
		n++
	}
	if n == 0 {
		return 0
	}
	return (total / n) * len(ff) * 11 / 10
}

// WriteJSONL writes all facts as JSONL to the given writer in a deterministic
// order. Extractors emit facts (and within-fact relations) in map-iteration
// order, which varies run to run; sorting here makes facts.jsonl byte-stable for
// a given commit, so the snapshot is reproducible and incremental regenerations
// don't churn the file. Relations are sorted on a copy so the in-memory store is
// left untouched.
//
// This is also where each fact's id and each relation's target_id are computed
// (see wire.go and id.go). They are written and never stored, so no internal
// reader can come to depend on them.
func (s *Store) WriteJSONL(w io.Writer) error {
	s.mu.RLock()

	// One contiguous buffer holding every marshalled line back to back, plus an index
	// of where each begins. The sort then reorders 8-byte offsets rather than moving
	// the lines, and the line bytes exist exactly once.
	//
	// Collecting a []string instead held each line TWICE — json.Marshal's []byte and
	// the string(b) copy of it — and made 1.89M separate allocations of it, on top of
	// whatever buffer the caller was writing into. Measured on a 1.89M-fact graph
	// whose output is 792 MiB, this function moved live heap from 1,212 to 5,016 MiB,
	// and the engine calls it twice per snapshot. That spike, not the graph, was what
	// drove peak footprint to 11.3 GB.
	// Pre-size the buffer from a sample of the first lines. Growing it by append
	// instead means repeatedly allocating a larger array and copying into it, which
	// at this size holds the old and the new one at once — the exact transient this
	// function exists to avoid — and leaves up to 25% slack in the survivor.
	// Over-estimating slightly is cheaper than either.
	buf := make([]byte, 0, estimateJSONLSize(s.facts))
	starts := make([]int, 0, len(s.facts)+1)
	// One scratch relation slice for the whole pass. Relations are marshalled
	// immediately, so the previous fact's are dead by the time the next one
	// overwrites them, and a per-fact allocation here would be paid 39M times on
	// the largest graph.
	rels := make([]wireRelation, 0, 8)
	var idScratch []byte
	var factID string
	for _, f := range s.facts {
		rels = rels[:0]
		for _, r := range f.Relations {
			wr := wireRelation{Relation: r}
			if t := s.targetFactFor(r.Target, f.Repo); t >= 0 {
				tf := s.facts[t]
				wr.TargetID, idScratch = factIDInto(idScratch, tf.Repo, tf.Kind, tf.Name, tf.File)
			}
			rels = append(rels, wr)
		}
		if len(rels) > 1 {
			sort.Slice(rels, func(i, j int) bool {
				if rels[i].Kind != rels[j].Kind {
					return rels[i].Kind < rels[j].Kind
				}
				return rels[i].Target < rels[j].Target
			})
		}
		// f is a copy, and its own Relations field is shadowed by the one below,
		// so neither the store's facts nor their relation slices are touched.
		factID, idScratch = factIDInto(idScratch, f.Repo, f.Kind, f.Name, f.File)
		b, err := json.Marshal(wireFact{Fact: f, Relations: rels, ID: factID})
		if err != nil {
			s.mu.RUnlock()
			return fmt.Errorf("encoding fact %q: %w", f.Name, err)
		}
		starts = append(starts, len(buf))
		buf = append(buf, b...)
	}
	starts = append(starts, len(buf)) // sentinel: one past the last line
	s.mu.RUnlock()

	// Sort the index, comparing the lines it points at. bytes.Compare orders byte by
	// byte and so did sort.Strings over the same lines, so the emitted order — and
	// therefore facts.jsonl and every snapshot ID derived from it — is unchanged.
	order := make([]int, len(starts)-1)
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool {
		x, y := order[a], order[b]
		return bytes.Compare(buf[starts[x]:starts[x+1]], buf[starts[y]:starts[y+1]]) < 0
	})

	bw := bufio.NewWriter(w)
	for _, i := range order {
		if _, err := bw.Write(buf[starts[i]:starts[i+1]]); err != nil {
			return err
		}
		if err := bw.WriteByte('\n'); err != nil {
			return err
		}
	}
	return bw.Flush()
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
		s.Add(f)
	}
	return scanner.Err()
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
