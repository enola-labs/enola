// Package resolve maps a name a caller typed to a graph node, for every tool and command that starts from one.
package resolve

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// Resolver resolves names against one loaded graph.
type Resolver struct {
	Store *facts.Store
	// RepoPaths maps repository label to absolute root; empty in single-repo mode.
	RepoPaths map[string]string
	// RepoPath is the snapshot's own repository root.
	RepoPath string
}

// NodeName resolves input to an exact fact name.
func (r Resolver) NodeName(input string) (string, *NameResolution, error) {
	return r.resolveNodeName(r.Store, input)
}

// RankedCandidates returns the facts matching input, most relevant first.
func (r Resolver) RankedCandidates(input string) []ScoredCandidate {
	return r.rankedCandidatesFor(r.Store, input)
}

// PrefixRepoLabel rewrites "<repo> <term>" to "repo:<repo> <term>" for a known label.
func (r Resolver) PrefixRepoLabel(input string) string { return r.maybePrefixRepoLabel(input) }

// NormalizeToRelative converts an absolute path to the store-relative path facts use.
func (r Resolver) NormalizeToRelative(p string) string { return r.normalizeToRelative(p) }

// ExpandFilePrefix returns the label-qualified forms of a file prefix that match facts.
func (r Resolver) ExpandFilePrefix(prefix string) []string { return r.expandFilePrefix(prefix) }

// RepoLabels returns the known repository labels, or nil.
func (r Resolver) RepoLabels() []string { return r.repoLabels() }

// AmbiguousMatchThreshold is the candidate count at or above which
// resolveNodeName refuses to guess and forces the caller to re-invoke with an
// exact name.
const AmbiguousMatchThreshold = 3

// MaxAlternatives caps how many candidate names are echoed back in a
// NameResolution so the response stays readable.
const MaxAlternatives = 10

// maxCandidates caps how many ranked, scored candidates are surfaced in a
// NameResolution.
const maxCandidates = 3

// AutoPickConfidence is the pickConfidence threshold above which an ambiguous
// resolution (at or above AmbiguousMatchThreshold matches) is resolved
// automatically to its top-scoring candidate instead of being refused.
const AutoPickConfidence = 0.80

// NameResolution reports how a user-provided name was resolved to a concrete
// fact name. It is surfaced in tool responses ONLY when the input matched more
// than one fact (i.e. Ambiguous is true), so callers can detect and correct a
// possibly-wrong pick. Matched is empty when the match count crossed
// AmbiguousMatchThreshold and no candidate scored confidently enough to
// auto-pick; the caller should then choose from Candidates (optionally using the
// repo:/kind:/file: scope prefixes) or re-invoke with an exact name.
type NameResolution struct {
	Query        string            `json:"query"`
	Matched      string            `json:"matched,omitempty"`
	Alternatives []string          `json:"alternatives,omitempty"`
	Candidates   []ScoredCandidate `json:"candidates,omitempty"`
	Confidence   float64           `json:"confidence,omitempty"`
	AutoPicked   bool              `json:"auto_picked,omitempty"`
	Ambiguous    bool              `json:"ambiguous"`
}

// resolveNodeName resolves a user-provided name to an exact fact name.
//
// The input may carry scope prefixes — repo:<label>, kind:<k>, file:<prefix> —
// to disambiguate an otherwise-ambiguous term (see parseScopedQuery). A plain
// input keeps the legacy substring-match behavior.
//
// It returns the resolved name and, when resolution was ambiguous (more than one
// fact matched and no confident pick existed), a non-nil *NameResolution
// describing the ambiguity (ranked Candidates with scores + a Confidence). For
// exact matches, single matches, and confident suffix-exact matches the
// resolution is nil.
//
// When the candidate count reaches AmbiguousMatchThreshold the method picks the
// top-scoring candidate only if its pickConfidence exceeds AutoPickConfidence;
// otherwise it returns an empty name with a resolution-only response so the
// caller can choose from Candidates or re-invoke with a scoped/exact name.
func (r Resolver) resolveNodeName(store *facts.Store, input string) (string, *NameResolution, error) {
	input = r.maybePrefixRepoLabel(input)
	query := input
	sq := parseScopedQuery(input)
	scoped := sq.Repo != "" || len(sq.Kinds) > 0 || sq.FilePrefix != "" || sq.SymbolKind != ""

	term := r.normalizeToRelative(sq.Term)

	// Try exact match first (unscoped only — a scope filter signals the caller
	// wants the candidate set narrowed, not bypassed).
	if !scoped {
		if exact := store.LookupByExactName(term); len(exact) > 0 {
			return exact[0].Name, nil, nil
		}
	}

	results := r.gatherCandidates(store, sq, term)
	if len(results) == 0 {
		// Fallback 1: exact name match on KindService facts (service nodes are
		// named after repo labels and are often missed by substring search).
		for _, svc := range store.ByKind(facts.KindService) {
			if strings.EqualFold(svc.Name, term) {
				return svc.Name, nil, nil
			}
		}
		// Fallback 2: input matches a known repo label whose service node may
		// have an empty name (primary repo) or hasn't been loaded yet.
		if name, ok := r.resolveRepoLabelToServiceNode(store, term); ok {
			return name, nil, nil
		}
		// Fallback 3: the term is a FILE basename (e.g. "auth_routes") with no
		// fact named after it — resolve to the symbol(s) declared in that file, so
		// file-shaped targets are pathable. A single owner resolves directly;
		// several are surfaced as candidates.
		if fileMatches := r.resolveByFileBasename(store, sq, term); len(fileMatches) > 0 {
			if len(fileMatches) == 1 {
				return fileMatches[0].Name, nil, nil
			}
			ranked := rankCandidates(fileMatches, sq)
			return "", &NameResolution{
				Query:        query,
				Alternatives: candidateNames(ranked, ""),
				Candidates:   topCandidates(ranked),
				Ambiguous:    true,
			}, nil
		}
		if sugg := r.suggestNames(store, sq, term); len(sugg) > 0 {
			return "", nil, fmt.Errorf("no facts matching %q; did you mean: %s "+
				"(tip: scope with repo:/kind:/file:)", query, strings.Join(sugg, ", "))
		}
		return "", nil, fmt.Errorf("no facts matching %q", query)
	}
	if len(results) == 1 {
		return results[0].Name, nil, nil
	}

	// A service node whose name exactly matches the term is a confident pick.
	for _, svc := range store.ByKind(facts.KindService) {
		if strings.EqualFold(svc.Name, term) {
			return svc.Name, nil, nil
		}
	}

	// Multiple matches: rank by relevance score and judge how decisively the top
	// candidate wins.
	ranked := rankCandidates(results, sq)
	confidence := pickConfidence(ranked, sq.Term)
	top := ranked[0].Name

	// In multi-repo mode, if the top-tier matches span 2+ repos and no repo:
	// scope was given, refuse to silently guess the user's repo — surface the
	// candidates (which carry their repo) so the caller can pin it down.
	if r.crossRepoAmbiguous(ranked, sq) {
		return "", &NameResolution{
			Query:        query,
			Alternatives: candidateNames(ranked, ""),
			Candidates:   topCandidates(ranked),
			Confidence:   confidence,
			Ambiguous:    true,
		}, nil
	}

	// One candidate clearly dominates (e.g. a unique suffix-exact name among
	// substring matches). Auto-resolve to it, but surface the resolution with its
	// confidence and the alternatives so the caller can see — and override — the
	// pick rather than it being silent.
	if confidence > AutoPickConfidence {
		return top, &NameResolution{
			Query:        query,
			Matched:      top,
			Alternatives: candidateNames(ranked, top),
			Candidates:   topCandidates(ranked),
			Confidence:   confidence,
			AutoPicked:   true,
			Ambiguous:    true,
		}, nil
	}

	// Below the ambiguity threshold, return the best guess (flagged ambiguous),
	// preserving the long-standing "small ambiguity → pick anyway" behavior.
	if len(results) < AmbiguousMatchThreshold {
		return top, &NameResolution{
			Query:        query,
			Matched:      top,
			Alternatives: candidateNames(ranked, top),
			Candidates:   topCandidates(ranked),
			Confidence:   confidence,
			Ambiguous:    true,
		}, nil
	}

	// Too ambiguous to guess: surface ranked candidates and refuse to pick.
	return "", &NameResolution{
		Query:        query,
		Alternatives: candidateNames(ranked, ""),
		Candidates:   topCandidates(ranked),
		Confidence:   confidence,
		Ambiguous:    true,
	}, nil
}

// candidateLimit caps how many candidates a scoped query carries into ranking.
const candidateLimit = 500

// gatherCandidates returns the facts matching a (possibly scoped) query. Unscoped
// inputs use the legacy substring Query to preserve existing semantics. Scoped
// inputs filter by repo, kind and file prefix (expanding the file prefix across
// repos in multi-repo mode) with an optional symbol_kind post-filter, and keep the
// candidateLimit best-ranked distinct names. They are the best, not the first the
// store happens to hold: a broad term matches more than the limit, and the exact
// name it was typed for must not be cut because it sits late in the store.
func (r Resolver) gatherCandidates(store *facts.Store, sq scopedQuery, term string) []facts.Fact {
	scoped := sq.Repo != "" || len(sq.Kinds) > 0 || sq.FilePrefix != "" || sq.SymbolKind != ""
	if !scoped {
		return store.Query("", "", term, "")
	}

	prefixes := []string{""}
	if sq.FilePrefix != "" {
		prefixes = r.expandFilePrefix(sq.FilePrefix)
	}

	best := make([]scored, 0, 2*candidateLimit)
	// Once the limit is full, its last entry is the bar a new candidate has to clear.
	var bar *scored
	for _, pfx := range prefixes {
		store.QueryEach(facts.QueryOpts{
			Repo:       sq.Repo,
			Kinds:      sq.Kinds,
			FilePrefix: pfx,
			Name:       term,
		}, func(f *facts.Fact) {
			if sq.SymbolKind != "" {
				if sk, _ := f.PropAny("symbol_kind").(string); sk != sq.SymbolKind {
					return
				}
			}
			c := scored{fact: *f, score: scoreCandidate(*f, sq)}
			if bar != nil && !c.before(*bar) {
				return
			}
			best = append(best, c)
			// Compact as it fills, so a term matching the whole store holds a bounded set.
			if len(best) == cap(best) {
				best = keepBest(best, candidateLimit)
				if len(best) == candidateLimit {
					last := best[candidateLimit-1]
					bar = &last
				}
			}
		})
	}
	best = keepBest(best, candidateLimit)
	out := make([]facts.Fact, 0, len(best))
	for _, c := range best {
		out = append(out, c.fact)
	}
	return out
}

// topCandidates returns up to maxCandidates ranked candidates.
func topCandidates(ranked []ScoredCandidate) []ScoredCandidate {
	if len(ranked) > maxCandidates {
		return ranked[:maxCandidates]
	}
	return ranked
}

// resolveRepoLabelToServiceNode maps a user-supplied label to the corresponding
// KindService fact name. Handles appended repos (label in RepoPaths) and the
// primary repo (base name of Snapshot.Meta.RepoPath), whose service node has
// Repo == "" and Name == "".
func (r Resolver) resolveRepoLabelToServiceNode(store *facts.Store, input string) (string, bool) {
	services := store.ByKind(facts.KindService)
	inputLower := strings.ToLower(input)

	for label := range r.RepoPaths {
		if strings.ToLower(label) == inputLower {
			for _, svc := range services {
				if strings.ToLower(svc.Repo) == inputLower || strings.ToLower(svc.Name) == inputLower {
					return svc.Name, true
				}
			}
		}
	}

	if r.RepoPath != "" {
		if strings.ToLower(filepath.Base(r.RepoPath)) == inputLower {
			for _, svc := range services {
				if svc.Repo == "" {
					return svc.Name, true
				}
			}
		}
	}

	return "", false
}

// maybePrefixRepoLabel rewrites "<repo> <term>" → "repo:<repo> <term>" when the
// first whitespace token is exactly a known repo label and a remainder follows.
// It is a no-op when the input has no remainder, or when the first token is
// already a scope token. This lets a bare
// "go-auth AuthHandler" resolve the same as "repo:go-auth AuthHandler".
func (r Resolver) maybePrefixRepoLabel(input string) string {
	fields := strings.Fields(input)
	if len(fields) < 2 {
		return input
	}
	if _, _, ok := splitScopeToken(fields[0]); ok {
		return input // already scoped
	}
	if paths := r.RepoPaths; paths[fields[0]] != "" {
		return "repo:" + input
	}
	// A single-repo graph has a path map only when restored from disk, so ask the facts too.
	if r.Store != nil && slices.Contains(r.Store.RepoLabels(), fields[0]) {
		return "repo:" + input
	}
	return input
}

// crossRepoAmbiguous reports whether, in multi-repo mode with no repo: scope, the
// candidates sharing the top match tier span 2+ repos — meaning auto-picking one
// would silently guess the user's repo. A unique top-tier match (e.g. a single
// suffix-exact name in one repo) is NOT cross-repo ambiguous and still resolves.
func (r Resolver) crossRepoAmbiguous(ranked []ScoredCandidate, sq scopedQuery) bool {
	if sq.Repo != "" || len(ranked) < 2 || len(r.RepoPaths) < 2 {
		return false
	}
	term := strings.ToLower(sq.Term)
	topTier := matchTier(ranked[0].Name, term)
	repos := make(map[string]struct{})
	for _, c := range ranked {
		if matchTier(c.Name, term) != topTier {
			break // ranked is tier-sorted; stop at the first lower tier
		}
		repos[c.Repo] = struct{}{}
	}
	return len(repos) >= 2
}

// suggestNames does a relaxed substring search to recover near-misses when an
// input matched nothing exactly. It searches on the longest alphanumeric run of
// the term (and its last dotted segment) and returns the 5 nearest fact names,
// repo-qualified in multi-repo mode, so a no-match error is never a dead end.
func (r Resolver) suggestNames(store *facts.Store, sq scopedQuery, term string) []string {
	probe := longestAlnumRun(term)
	if seg := lastSegment(term); len(seg) > len(probe) {
		probe = longestAlnumRun(seg)
	}
	// A path's longest word is usually a directory every name shares. Its base
	// name, without a source extension, is the part that says what was meant.
	if i := strings.LastIndexByte(term, '/'); i >= 0 {
		base := term[i+1:]
		if j := strings.LastIndexByte(base, '.'); j > 0 && codeExtensions[strings.ToLower(base[j+1:])] {
			base = base[:j]
		}
		if run := longestAlnumRun(base); len(run) >= 3 {
			probe = run
		}
	}
	if len(probe) < 3 {
		return nil
	}
	multiRepo := len(r.RepoPaths) > 1
	// The shortest names holding the probe, then by name: the nearest misses, and
	// the same five whatever order the store holds them in.
	closer := func(a, b string) bool {
		if len(a) != len(b) {
			return len(a) < len(b)
		}
		return a < b
	}
	const limit = 5
	out := make([]string, 0, limit+1)
	store.QueryEach(facts.QueryOpts{Name: probe, Repo: sq.Repo}, func(m *facts.Fact) {
		name := m.Name
		if multiRepo && m.Repo != "" {
			name = "repo:" + m.Repo + " " + m.Name
		}
		if len(out) == limit && !closer(name, out[limit-1]) {
			return
		}
		at, dup := slices.BinarySearchFunc(out, name, func(have, want string) int {
			switch {
			case have == want:
				return 0
			case closer(have, want):
				return -1
			}
			return 1
		})
		if dup {
			return
		}
		out = slices.Insert(out, at, name)
		if len(out) > limit {
			out = out[:limit]
		}
	})
	return out
}

// resolveByFileBasename maps a file-basename term to the symbols declared in the
// matching file(s). Enola does not name a fact after a source file, so a target
// like "auth_routes" (the file auth_routes.go) is otherwise unresolvable for
// graph tools; this returns the pathable symbol nodes that live in that file.
// Honors a repo: scope when present. Returns distinct symbol facts.
func (r Resolver) resolveByFileBasename(store *facts.Store, sq scopedQuery, term string) []facts.Fact {
	lt := strings.ToLower(term)
	seen := make(map[string]struct{})
	var out []facts.Fact
	for _, f := range store.ByKind(facts.KindSymbol) {
		if f.File == "" || !fileBaseMatches(f.File, lt) {
			continue
		}
		if sq.Repo != "" && !strings.EqualFold(f.Repo, sq.Repo) {
			continue
		}
		if _, dup := seen[f.Name]; dup {
			continue
		}
		seen[f.Name] = struct{}{}
		out = append(out, f)
	}
	return out
}

// fileBaseMatches reports whether lowerTerm equals path's basename, with or
// without a trailing source extension (case-insensitive). "a/b/auth_routes.go"
// matches both "auth_routes.go" and "auth_routes"; it never matches a bare
// extension like "go".
func fileBaseMatches(path, lowerTerm string) bool {
	base := strings.ToLower(path)
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	if base == lowerTerm {
		return true
	}
	if i := strings.LastIndexByte(base, '.'); i > 0 && codeExtensions[base[i+1:]] {
		return base[:i] == lowerTerm
	}
	return false
}

// rankedCandidatesFor returns the facts matching an input (after repo-prefix
// auto-detection and scope parsing), ranked most-relevant first. Used by find_path
// to consider more candidates than the capped set carried in a NameResolution.
func (r Resolver) rankedCandidatesFor(store *facts.Store, input string) []ScoredCandidate {
	input = r.maybePrefixRepoLabel(input)
	sq := parseScopedQuery(input)
	term := r.normalizeToRelative(sq.Term)
	return rankCandidates(r.gatherCandidates(store, sq, term), sq)
}

// longestAlnumRun returns the longest maximal run of [A-Za-z0-9_] in s. Used to
// derive a robust substring probe from a dotted/spaced term.
func longestAlnumRun(s string) string {
	best, start := "", -1
	flush := func(end int) {
		if start >= 0 && end-start > len(best) {
			best = s[start:end]
		}
		start = -1
	}
	for i, r := range s {
		if r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			if start < 0 {
				start = i
			}
		} else {
			flush(i)
		}
	}
	flush(len(s))
	return best
}

// candidateNames collects up to MaxAlternatives distinct names from ranked, best
// first, excluding exclude (the chosen match) when set.
func candidateNames(ranked []ScoredCandidate, exclude string) []string {
	names := make([]string, 0, min(len(ranked), MaxAlternatives))
	for _, c := range ranked {
		if c.Name == exclude || slices.Contains(names, c.Name) {
			continue
		}
		names = append(names, c.Name)
		if len(names) >= MaxAlternatives {
			break
		}
	}
	return names
}

// normalizeToRelative converts an absolute filesystem path to a store-relative
// path by stripping known repo root prefixes. If the path is already relative
// or doesn't match any known repo root, it is returned unchanged.
func (r Resolver) normalizeToRelative(p string) string {
	if !filepath.IsAbs(p) {
		return p
	}

	// Try multi-repo paths first (populated in append mode).
	for label, absRoot := range r.RepoPaths {
		rel, err := filepath.Rel(absRoot, p)
		if err == nil && !strings.HasPrefix(rel, "..") {
			if !r.filesCarryLabel(label) {
				return filepath.ToSlash(rel)
			}
			// Prefix with repo label so it matches the prefixed fact files.
			return filepath.ToSlash(filepath.Join(label, rel))
		}
	}

	// Fall back to the single-snapshot repo path.
	if r.RepoPath != "" {
		rel, err := filepath.Rel(r.RepoPath, p)
		if err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.ToSlash(rel)
		}
	}

	return p
}

// filesCarryLabel reports whether the facts of repo label store their files under
// "<label>/". Append mode prefixes them; a single repository restored from disk has
// a label map and bare files, so the map alone does not say. One symbol outside the
// prefix settles it. Only symbols are asked: a symbol's file is the source file it
// was read from, where a fact a binder or linker adds names whatever path its
// author gave it. That leaves a repository whose every source file sits in a
// directory named after its own label read as prefixed.
func (r Resolver) filesCarryLabel(label string) bool {
	return r.Store == nil || !r.Store.HasFileOutside(label, facts.KindSymbol, label+"/")
}

// repoLabels returns the known repo labels from multi-repo mode, or nil.
func (r Resolver) repoLabels() []string {
	rp := r.RepoPaths
	if len(rp) == 0 {
		return nil
	}
	labels := make([]string, 0, len(rp))
	for l := range rp {
		labels = append(labels, l)
	}
	return labels
}

// expandFilePrefix expands a relative file prefix for multi-repo mode.
// When repoPaths are configured and the prefix doesn't already start with a
// known repo label, it returns all "{label}/{prefix}" variants that have
// matches in the store. If only one repo matches, it returns that single
// expanded prefix. If multiple repos match, it returns all variants.
// In single-repo mode or when the prefix already has a repo label, it returns
// the input unchanged.
func (r Resolver) expandFilePrefix(prefix string) []string {
	if prefix == "" || filepath.IsAbs(prefix) {
		return []string{prefix}
	}

	repoPaths := r.RepoPaths
	if len(repoPaths) == 0 {
		return []string{prefix}
	}

	// Check if prefix already starts with a known repo label.
	for label := range repoPaths {
		if prefix == label || strings.HasPrefix(prefix, label+"/") {
			return []string{prefix}
		}
	}

	// Try prefixing with each repo label and check for matches.
	store := r.Store
	var expanded []string
	for label := range repoPaths {
		candidate := label + "/" + prefix
		// Quick check: does the store have any facts with this file prefix?
		_, total := store.QueryAdvanced(facts.QueryOpts{FilePrefix: candidate, Limit: 1})
		if total > 0 {
			expanded = append(expanded, candidate)
		}
	}

	if len(expanded) == 0 {
		// No matches with any repo label; return original (maybe it matches as-is).
		return []string{prefix}
	}
	return expanded
}
