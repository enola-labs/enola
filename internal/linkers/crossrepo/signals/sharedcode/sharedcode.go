// Package sharedcode links repos that declare the same distinctive types — vendored or
// copy-pasted source, rather than a call or an import.
//
// This is the one SYMMETRIC signal: two repos declaring the same type names says
// nothing about which depends on which. So it never invents a direction. When a
// directional signal has already oriented the pair it annotates that edge; when
// nothing has, it records a symmetric coupling that carries no relation and stays out
// of the traversable graph. Shared code is not a dependency, and unlike a dependency it
// does not compose across hops — a repo calling one side of a copy-paste pair does not
// thereby reach the other.
package sharedcode

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/linkers/vocab"
	"github.com/enola-labs/enola/pkg/plugin"
)

// Via is the evidence label this signal reports under, and the ONE via that does not
// establish a direction: two repos declaring the same type names says nothing about
// which depends on which. Every other via — import, http, http-client, grpc, and any
// signal added later — is directional by construction, which is why the sink tests for
// "reported by a symmetric signal" rather than enumerating the directional kinds. An
// enumeration silently mis-classified new signals: it once omitted "grpc", so a pair
// linked only by gRPC calls still got a fabricated reverse edge.
const Via = facts.ViaSharedSymbols

// Signal detects repos sharing distinctive type declarations.
type Signal struct {
	v *vocab.Set
}

// New returns the signal, judging identities under the given vocabulary.
func New(v *vocab.Set) *Signal { return &Signal{v: v} }

func (s *Signal) Name() string { return "shared-code" }

func (s *Signal) Phase() plugin.SignalPhase { return plugin.PhaseSymmetric }

// s.v.Thresholds.MinSharedSymbols is the fewest distinct distinctive type identities two repos
// must share before a shared-symbol edge is drawn. Set above 2 so an incidental
// name collision (a `JsonParser` both repos happen to define) cannot fabricate a
// dependency, while genuinely shared/vendored code (a protocol header copied
// between repos) shares many.

// s.v.Thresholds.MaxVocabRepoShare bounds how widely an unqualified type identity may be
// declared before it is read as shared domain vocabulary rather than evidence
// that any specific pair shares code. Vendored/shared source (an onelab protocol
// header) lands in the handful of repos that copied it; a domain type every
// service in a fleet independently models ("Translation", "Category", "Filter")
// lands in most of them. An identity declared in more than this fraction of all
// loaded repos is the latter, so it is dropped as a pairwise coupling signal —
// otherwise a fleet of same-language services fabricates a near-complete mesh of
// edges from parallel modeling alone.

// s.v.Thresholds.MinReposForVocabFilter guards the vocabulary filter so it only applies once
// enough repos are loaded that "a majority of repos" is a meaningful bar. With
// two or three repos a genuinely vendored header can legitimately appear in most
// of them, so below this count the filter stays off and the small-repo-set
// behavior (including every 2-repo test fixture) is preserved unchanged.

// isUbiquitousIdentity reports whether an unqualified identity declared in
// declaringRepos of totalRepos loaded repos is too widespread to be a pairwise
// coupling signal — see s.v.Thresholds.MaxVocabRepoShare. Namespace-qualified identities never
// reach here: "::" is a strong shared-source marker and bypasses the filter.
func (s *Signal) isUbiquitousIdentity(declaringRepos, totalRepos int) bool {
	if totalRepos < s.v.Thresholds.MinReposForVocabFilter {
		return false
	}
	return float64(declaringRepos) > s.v.Thresholds.MaxVocabRepoShare*float64(totalRepos)
}

// conventionSuffixes are the trailing words a framework's naming convention appends
// to a component name to derive a companion type. They carry no information of their
// own, so they are stripped before an identity's segments are judged.

// isConventionalComponentName reports whether an identity is built purely from the
// generic UI-component vocabulary — a name every app of the framework produces for
// its own unrelated component. It strips one convention suffix ("SidebarProps" ->
// "Footer"), splits the remainder on camelCase boundaries, and rejects the identity
// only when EVERY segment is generic vocabulary.
//
// A single distinctive segment is enough to save a name, which is what keeps a
// disambiguating prefix meaningful: "TListRow" splits to T/List/Row, and though
// List and Row are generic, the deliberate "T" type prefix is not — so the identity
// survives, as it should, because such a name really is shared code rather than a
// coincidence. The stripped core is also deliberately NOT re-checked against the
// minimum-length rule: "TileProps" reduces to a 4-character "Like" yet names real
// shared code.
func (s *Signal) isConventionalComponentName(id string) bool {
	core := id
	for _, suffix := range s.v.ConventionSuffixes {
		if len(core) > len(suffix) && strings.HasSuffix(core, suffix) {
			core = core[:len(core)-len(suffix)]
			break
		}
	}
	segments := splitCamelCase(core)
	if len(segments) == 0 {
		return false
	}
	for _, seg := range segments {
		lower := strings.ToLower(seg)
		if !s.v.GenericComponentNames[lower] && !s.v.GenericTypeNames[lower] {
			return false
		}
	}
	return true
}

// splitCamelCase breaks an identifier on lower-to-upper transitions, so "PanelSection"
// yields ["Form", "Header"]. Runs of capitals stay together ("HTTPServer" -> ["HTTP",
// "Server"]) so an acronym is not shredded into single letters.
func splitCamelCase(s string) []string {
	runes := []rune(s)
	var out []string
	start := 0
	for i := 1; i < len(runes); i++ {
		boundary := unicode.IsUpper(runes[i]) &&
			(!unicode.IsUpper(runes[i-1]) || (i+1 < len(runes) && unicode.IsLower(runes[i+1])))
		if boundary {
			out = append(out, string(runes[start:i]))
			start = i
		}
	}
	if start < len(runes) {
		out = append(out, string(runes[start:]))
	}
	return out
}

// s.v.Thresholds.MinFileSimilarity is the least line-set overlap the two files declaring a shared type
// name must have for that name to count as evidence of shared CODE. Measured on real
// repo pairs, file-level overlap separates the two populations cleanly: genuinely
// vendored files score at or near 1.0, while same-name-different-code declarations sit
// far below — so the threshold sits between them rather than near either.
//
// Comparing FILES rather than declaration bodies is deliberate: copied code travels as
// whole files, and facts carry no end line, so a body would have to be guessed. The
// tradeoff is a missed match when a genuinely shared declaration sits inside an
// otherwise-different file.

// s.v.Thresholds.MaxComparedFileBytes skips files too large to be worth hashing line-by-line; a
// vendored bundle or generated blob should not dominate link time.

// s.v.Thresholds.MaxFactsPerIdentityRepo bounds how many declaring facts are kept per (identity,
// repo). A class can be reopened across several files; keeping a few lets verification
// pick the best-matching pair without letting a widely-reopened name explode the
// comparison count.

// fileComparer scores how similar two source files are, memoizing both the per-file
// line sets and the per-pair score. Several shared identities usually resolve to the
// same file pair, and every file is read at most once.
type fileComparer struct {
	v     *vocab.Set
	src   plugin.SignalInput
	lines map[string]map[string]bool // fact file path -> normalized line set (nil = unreadable)
	score map[string]float64         // "fileA\x00fileB" -> similarity
}

func (s *Signal) newFileComparer(src plugin.SignalInput) *fileComparer {
	return &fileComparer{
		v:     s.v,
		src:   src,
		lines: map[string]map[string]bool{},
		score: map[string]float64{},
	}
}

// linesOf returns the normalized line set of a fact's file, or nil when unreadable.
func (fc *fileComparer) linesOf(f facts.Fact) map[string]bool {
	if set, ok := fc.lines[f.File]; ok {
		return set
	}
	var set map[string]bool
	if text, ok := fc.src.ReadSource(f); ok && len(text) <= fc.v.Thresholds.MaxComparedFileBytes {
		set = tokenSet(text)
	}
	fc.lines[f.File] = set
	return set
}

// similar reports whether the files declaring a and b are alike enough to treat the
// shared name as shared code.
func (fc *fileComparer) similar(a, b facts.Fact) bool {
	key := a.File + "\x00" + b.File
	if b.File < a.File {
		key = b.File + "\x00" + a.File
	}
	if score, ok := fc.score[key]; ok {
		return score >= fc.v.Thresholds.MinFileSimilarity
	}
	score := jaccard(fc.linesOf(a), fc.linesOf(b))
	fc.score[key] = score
	return score >= fc.v.Thresholds.MinFileSimilarity
}

// tokenSet reduces source to the set of identifiers and punctuation it contains.
//
// Tokens rather than lines: a copy that drifted usually keeps its structure but edits
// something inside many lines — a renamed constant, a changed argument — and whole-line
// comparison scores that as a near-total mismatch. Calibrated against a
// character-diff ratio over real repo pairs, line-set overlap disagreed on 4 of 13
// sampled declarations (every one a false negative, including two files ~95% identical),
// while token-set overlap agreed on all 13. Comments are deliberately NOT stripped —
// that would need per-language syntax, and a copied file carries its comments anyway.
func tokenSet(text string) map[string]bool {
	set := map[string]bool{}
	for _, tok := range tokenPattern.FindAllString(text, -1) {
		set[tok] = true
	}
	return set
}

// tokenPattern matches an identifier (letters, digits, underscore, not leading with a
// digit) or any single non-space character, so operators and punctuation count too.
var tokenPattern = regexp.MustCompile(`[A-Za-z_][A-Za-z_0-9]*|[^\sA-Za-z_0-9]`)

// jaccard is the intersection-over-union of two token sets, 0 when either is empty. Set
// overlap (rather than a diff) makes the score independent of how the code was
// reordered, which is what a drifted copy usually looks like.
func jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for line := range a {
		if b[line] {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// verifyIdentity returns the first pairing of the declarations of id in repos a and b
// whose files are similar enough to count. A class reopened across files gives several
// candidates; one shared file is enough to evidence shared code.
func (s *Signal) verifyIdentity(byRepo map[string][]facts.Fact, a, b string, fc *fileComparer) (facts.Fact, facts.Fact, bool) {
	for _, fa := range byRepo[a] {
		for _, fb := range byRepo[b] {
			if fc.similar(fa, fb) {
				return fa, fb, true
			}
		}
	}
	return facts.Fact{}, facts.Fact{}, false
}

// sharedFileSample is a BucketSharedFiles value: a shared type and the files, in repos
// a and b, whose declarations matched.
func sharedFileSample(id string, fa, fb facts.Fact) string {
	return fmt.Sprintf("%s: %s:%d <-> %s:%d", id, fa.File, fa.Line, fb.File, fb.Line)
}

// linkSharedSymbols connects repos that declare enough of the same distinctive types.
// Name matching alone only produces CANDIDATES: two repos declaring the same domain class may
// share code or may merely share a domain vocabulary, and the name cannot tell them
// apart. When src is non-nil each candidate is verified by comparing the files that
// declare it, and only verified identities count toward the threshold. A nil src skips
// verification and falls back to name-only matching.
func (s *Signal) Contribute(in plugin.SignalInput, out plugin.EvidenceSink) {
	all := in.Facts()
	repoCount := len(in.Repos())

	// identity -> repo -> the facts declaring it there. The facts (not just a repo
	// set) are kept so verification can reach the declaring file.
	idToRepos := map[string]map[string][]facts.Fact{}
	for _, f := range all {
		if f.Kind != facts.KindSymbol || f.Repo == "" || !isTypeSymbol(f) {
			continue
		}
		if s.isNonContractSharedFile(f.File) {
			continue // auto-generated class, not portable contract surface
		}
		id := typeIdentity(f.Name, in.ModuleNames(f.Repo))
		if !s.isDistinctiveIdentity(id) {
			continue
		}
		if idToRepos[id] == nil {
			idToRepos[id] = map[string][]facts.Fact{}
		}
		if len(idToRepos[id][f.Repo]) < s.v.Thresholds.MaxFactsPerIdentityRepo {
			idToRepos[id][f.Repo] = append(idToRepos[id][f.Repo], f)
		}
	}

	// For each identity shared by 2+ repos, record it against every repo pair — but
	// only when the shared identity is a trustworthy coupling signal for that pair.
	// A namespace-qualified identity (contains "::", the mark of vendored/shared
	// source) always counts, language-independent. Any other name — bare, or dotted
	// because it names a nested type — counts only between same-language repos: two
	// apps written in different languages sharing a plain domain type name (e.g.
	// Kotlin and Swift both declaring "LoginViewModel", or both nesting
	// "RegisterUseCase.ValidationError") is parallel modeling of the same product,
	// not shared code, and must not fabricate a dependency. A bare name is dropped
	// entirely when it is declared across too much of the fleet (isUbiquitousIdentity):
	// a distinctive type most services in a large multi-repo set independently model
	// is shared vocabulary, not evidence that any specific pair shares code.
	// pairShared["a\x00b"] (a<b) -> set of shared identities.
	pairShared := map[string]map[string]bool{}
	for id, repos := range idToRepos {
		if len(repos) < 2 {
			continue
		}
		qualified := isNamespaceQualified(id)
		if !qualified && s.isUbiquitousIdentity(len(repos), repoCount) {
			continue // shared domain vocabulary across the fleet, not pairwise coupling
		}
		rs := make([]string, 0, len(repos))
		for r := range repos {
			rs = append(rs, r)
		}
		sort.Strings(rs)
		for i := 0; i < len(rs); i++ {
			for j := i + 1; j < len(rs); j++ {
				if !qualified && in.PrimaryLanguage(rs[i]) != in.PrimaryLanguage(rs[j]) {
					continue
				}
				key := rs[i] + "\x00" + rs[j]
				if pairShared[key] == nil {
					pairShared[key] = map[string]bool{}
				}
				pairShared[key][id] = true
			}
		}
	}

	// Verify the candidates against source. Up to here a match means only that both
	// repos declare the name; now the files behind it decide whether that name is
	// evidence of shared code. nameMatches keeps the pre-verification tally so the
	// gap between "shares code" and "shares a vocabulary" stays reportable.
	nameMatches := map[string]int{}
	for key, ids := range pairShared {
		nameMatches[key] = len(ids)
	}
	// matchedFiles[key] lists, per verified identity, the declarations that matched.
	matchedFiles := map[string][]string{}
	if in.HasSource() {
		fc := s.newFileComparer(in)
		for key, ids := range pairShared {
			a, b, _ := strings.Cut(key, "\x00")
			verified := map[string]bool{}
			for id := range ids {
				if fa, fb, ok := s.verifyIdentity(idToRepos[id], a, b, fc); ok {
					verified[id] = true
					matchedFiles[key] = append(matchedFiles[key], sharedFileSample(id, fa, fb))
				}
			}
			pairShared[key] = verified
		}
	}

	// Materialize an edge for each pair over the threshold — counted in VERIFIED
	// identities, so a pair sharing many names but no code drops out entirely. Shared
	// code is inherently symmetric, so the default is a bidirectional pair — but when
	// an earlier linker has already established a DIRECTION for this pair (an import
	// or HTTP call one way and not the other), that direction is authoritative and
	// the reverse edge would contradict it. A library monorepo whose types a
	// consuming app also declares must not come out depending on that app.
	for key, ids := range pairShared {
		if len(ids) < s.v.Thresholds.MinSharedSymbols {
			continue
		}
		a, b, _ := strings.Cut(key, "\x00")
		pairs, directional := out.DirectedPairs(a, b)
		if !directional {
			// Nothing to annotate: the repos share type names and nothing else. Record
			// a symmetric coupling rather than manufacturing a dependency edge.
			c := out.Coupling(a, b)
			c.Via(Via)
			c.Unverified(plugin.BucketSymbols, nameMatches[key])
			for id := range ids {
				c.Sample(plugin.BucketSymbols, id)
			}
			for _, f := range matchedFiles[key] {
				c.Sample(plugin.BucketSharedFiles, f)
			}
			continue
		}
		for _, pair := range pairs {
			e := out.Edge(pair[0], pair[1])
			e.Via(Via)
			e.Unverified(plugin.BucketSymbols, nameMatches[key])
			for id := range ids {
				e.Sample(plugin.BucketSymbols, id)
			}
			for _, f := range matchedFiles[key] {
				e.Sample(plugin.BucketSharedFiles, f)
			}
		}
	}
}

// isTypeSymbol reports whether a symbol fact is a type-like declaration (the
// portable "contract surface"), excluding functions, methods, variables, etc.
func isTypeSymbol(f facts.Fact) bool {
	switch f.PropString("symbol_kind") {
	case facts.SymbolClass, facts.SymbolStruct, facts.SymbolInterface, facts.SymbolEnum:
		return true
	}
	return false
}

// typeIdentity strips the repo-specific "<module>." directory prefix from a
// symbol's name, returning the portable namespace/type-qualified remainder that
// is shared across repos (e.g. "src/common.onelab::Foo" -> "onelab::Foo",
// "Common.onelab::Foo" -> "onelab::Foo"). The repo's own module names are used so
// the differing directory layouts of two repos do not defeat the match.
func typeIdentity(name string, modules []string) string {
	for _, m := range modules { // longest first
		if len(name) > len(m)+1 && strings.HasPrefix(name, m+".") {
			return name[len(m)+1:]
		}
	}
	// Fallback: strip up to the first "." when no module matched.
	if i := strings.IndexByte(name, '.'); i >= 0 && i+1 < len(name) {
		return name[i+1:]
	}
	return name
}

// isDistinctiveIdentity filters out identities too generic to safely link on. A
// namespaced identity (containing "::") is always kept; anything else — including a
// dotted nested type name — is kept only if it is reasonably long and not a common
// generic type name.
func (s *Signal) isDistinctiveIdentity(id string) bool {
	if id == "" {
		return false
	}
	// Framework-convention boilerplate is checked before the namespace bypass so a
	// qualified convention name (ApplicationCable::Connection) is excluded too.
	if s.v.FrameworkConventions[id] {
		return false
	}
	if isNamespaceQualified(id) {
		return true
	}
	if len(id) < 5 {
		return false
	}
	if s.v.GenericTypeNames[strings.ToLower(id)] {
		return false
	}
	// A name assembled purely from generic component vocabulary ("SidebarProps",
	// "PanelSectionProps") is naming convention rather than shared code.
	return !s.isConventionalComponentName(id)
}

// nonContractPathMarkers are path fragments identifying files whose declarations are
// scaffolding rather than portable contract surface: Rails migrations (generator-
// derived names like InitSchema/CreateFooBars that coincide across apps that ran the
// same migration); the test/story/mock/fixture tree, where a throwaway local type
// is routinely declared with the same obvious name in every repo; and generated
// code — client/model/mock stubs a codegen tool (oapi-codegen, protoc, mockery)
// derives from a shared contract. Two services that consume the SAME upstream API
// each generate identically-named client and model types, so those coincide across
// every consumer of that API. That is shared upstream contract, not shared code
// between the consumers, and the real dependency (each consumer -> that API) is
// already captured by HTTP/import linking; counting the generated stubs only
// fabricates a spurious edge between the consumers themselves.

// isNonContractSharedFile reports whether a file holds declarations that are not
// portable contract surface for the shared-symbol signal, so they must not fabricate
// or inflate a cross-repo edge. Directory markers are matched against a
// leading-slash-normalised path so they also hit a repo-root-relative path.
func (s *Signal) isNonContractSharedFile(file string) bool {
	rooted := file
	if !strings.HasPrefix(rooted, "/") {
		rooted = "/" + rooted
	}
	for _, marker := range s.v.NonContractPaths {
		if strings.Contains(rooted, marker) {
			return true
		}
	}
	return false
}

// isNamespaceQualified reports whether a type identity carries a namespace (only
// "::" does). A namespaced identity shared across repos is a strong vendored/
// shared-source signal, independent of language; an unqualified one is a bare type
// name that two repos may coincidentally share.
//
// A "." does NOT qualify. typeIdentity has already stripped the module prefix, so
// any dot left in the identity is type *nesting* — "Outer.Inner", which Kotlin and
// Swift both emit for a nested declaration — not a namespace. Treating it as one
// let two parallel apps sharing only a domain vocabulary fabricate a dependency.
func isNamespaceQualified(id string) bool {
	return strings.Contains(id, "::")
}
