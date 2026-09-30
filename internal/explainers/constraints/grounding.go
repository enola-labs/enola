package constraints

import (
	"fmt"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// Component membership resolves an edge's target by exact fact name, and a
// file-granular import target names no fact: module facts are named by directory
// (`lib`), symbol facts by `dir.symbol` (`lib.app`), while the TS extractor
// measures `require('./application')` as an imports edge onto the PATH
// `lib/application`. So the dominant dependency mechanism of a classic Node
// codebase was measured and unenforceable, and the silence read as compliance
// (finding 0010).
//
// The grounding below is the contained fix: a target that names no fact, but
// does name a file this repository measured, joins the component that file
// belongs to. It is strictly subordinate — every caller asks exact-name
// membership first and only falls back here — so no rule whose target already
// grounds by name can change verdict. It is scoped to one repository, because an
// extension-less path is only meaningful relative to the tree it was written in.
// And it applies to path-targeting edges only, which pathTargetEdge decides: a
// calls or implements target is a symbol name, never a path, so resolving one as
// a file could only ever be a guess.

// resolutionLevelProp and markupDeclaredLevel mirror the extractor vocabulary a
// markup pass stamps on the facts it emits. Spelled here rather than imported so
// this explainer keeps depending only on facts, exactly as the binders do.
const (
	resolutionLevelProp = "resolution_level"
	markupDeclaredLevel = "markup-declared"
)

// pathTargetEdge reports whether an edge's target is a PATH rather than a fact
// name, which is the precondition for resolving it against measured files. An
// imports target always is. A depends_on target is one exactly when the fact
// declaring it was itself declared in markup: `data-controller="dropdown"` is
// written in an ERB view and the Ruby pass resolves it to the controller FILE by
// naming convention, so the edge lands on a path for the same reason an import
// does — and for the same reason it names no member. Every other depends_on
// target is a symbol: an association, a Terraform address, a project reference.
// Resolving one of those as a file could only ever be a guess.
func pathTargetEdge(rel facts.Relation, from facts.Fact) bool {
	if rel.Kind == facts.RelImports {
		return true
	}
	return rel.Kind == facts.RelDependsOn && from.PropString(resolutionLevelProp) == markupDeclaredLevel
}

// importModuleExts are the source extensions an extension-less import target may
// have elided, in the resolver's own order (mirroring the TS extractor's).
var importModuleExts = []string{".ts", ".tsx", ".js", ".jsx", ".mjs", ".vue", ".svelte", ".gts", ".gjs"}

// pythonModuleExts and pythonPackageIndex are the same two questions for a
// Python importer: `import inventory.stock` is measured as the path
// inventory/stock, which is inventory/stock.py, or the package directory's
// __init__.py. Without them every in-repository Python import named "nothing
// measured", so a file-level breach went unverdicted and was caught, if at all,
// only on the derived module edge, which has no line to point at.
var (
	pythonModuleExts   = []string{".py", ".pyi"}
	pythonPackageIndex = "/__init__.py"
)

// moduleExtsFor picks the extensions an importer's own language would elide. A
// TypeScript import of "./stock" never means stock.py, so the two sets are never
// merged: a stray file of the other language must not ground an edge.
func moduleExtsFor(importer string) (exts []string, index func(ext string) string) {
	if strings.HasSuffix(importer, ".py") || strings.HasSuffix(importer, ".pyi") {
		return pythonModuleExts, func(string) string { return pythonPackageIndex }
	}
	return importModuleExts, func(ext string) string { return "/index" + ext }
}

// groundSkipConfidence caps the ungroundable-target advisory, for the same
// reason the reach-skip advisory sits at 0.4: it reports that no verdict was
// reached for those targets, and silence there must never read as compliance.
const groundSkipConfidence = 0.4

// grounding indexes what each repository measured: every file, and every fact
// name. The names half is what makes "resolves to nothing measured" a statement
// about the snapshot rather than about one component.
//
// memberFiles is the third index, and it is what keeps the file join from
// widening a predicate: the files each component actually measured a member in.
// A path component's match globs ARE its claim about files, so joining a
// resolved path to them says what the declaration said. A predicate component's
// claim is about one measured fact, and its match globs are a scope the
// predicate narrows — so a resolved path inside the globs is not inside the
// component unless the component measured a member there. Without that, a
// component declaring `match: app/components/**` AND
// `where: {superclass: ViewComponent::Base}` answered for its whole glob as an
// edge target: 41 imports edges in the monolith resolve to a file under those
// globs that hosts no member at all, and each one drew a 1.0 verdict on code
// the declaration excluded.
type grounding struct {
	files       map[string]map[string]bool
	names       map[string]bool
	memberFiles map[string]map[string]bool
}

func newGrounding(store *facts.Store, memberFacts map[string][]facts.Fact) *grounding {
	g := &grounding{files: map[string]map[string]bool{}, names: map[string]bool{}, memberFiles: map[string]map[string]bool{}}
	for name, ff := range memberFacts {
		set := map[string]bool{}
		for _, f := range ff {
			if f.File == "" {
				continue
			}
			// Both forms, because resolvedPathIn applies its test to the
			// label-prefixed path and then to the repo-relative one — the same
			// double join matchConstraintFile makes, for the same reason.
			set[f.File] = true
			if f.Repo != "" {
				set[strings.TrimPrefix(f.File, f.Repo+"/")] = true
			}
		}
		g.memberFiles[name] = set
	}
	// FactsRef, not All: this only reads, and retains nothing but two string sets.
	for _, f := range store.FactsRef() {
		if f.Name != "" {
			g.names[f.Name] = true
		}
		if f.File == "" {
			continue
		}
		set := g.files[f.Repo]
		if set == nil {
			set = map[string]bool{}
			g.files[f.Repo] = set
		}
		set[f.File] = true
	}
	return g
}

// resolve returns the measured file an import target names, in the canonical
// form the fact carries it. Both the bare and the repo-labelled shapes are
// tried, because a target is written repo-relative while an append-mode
// snapshot's files are label-prefixed.
func (g *grounding) resolve(target, repo, importer string) (string, bool) {
	set := g.files[repo]
	if set == nil || target == "" {
		return "", false
	}
	bases := []string{target}
	if repo != "" {
		bases = append(bases, repo+"/"+target)
	}
	exts, index := moduleExtsFor(importer)
	for _, base := range bases {
		if set[base] {
			return base, true
		}
		for _, ext := range exts {
			if p := base + ext; set[p] {
				return p, true
			}
		}
		// A directory import resolves to its index file, exactly as the module
		// resolver the extractor mirrors does.
		for _, ext := range exts {
			if p := base + index(ext); set[p] {
				return p, true
			}
		}
	}
	return "", false
}

// inComponent reports whether a path-targeting edge whose target named no member
// resolves to a measured file inside the named component. A name-narrowed or
// pattern-less component never grounds this way: the join is the component's own
// match globs, and a component that declares none has nothing to join against.
// A predicate component ANDs its predicate in the only way a file can carry it —
// the component measured a member in that file — for the same reason
// resolveMembership ANDs it: every narrowing on a component narrows, and a join
// that dropped one would let the rule judge code the declaration excluded.
func (g *grounding) inComponent(rel facts.Relation, from facts.Fact, name string, components map[string]component) bool {
	if !pathTargetEdge(rel, from) || name == "" {
		return false
	}
	c, declared := components[name]
	if !declared || c.namePattern != "" || len(c.match) == 0 {
		return false
	}
	if c.service != "" && from.Repo != c.service {
		return false
	}
	hosts := g.memberFiles[name]
	return g.resolvedPathIn(rel, from, func(path string) bool {
		if !matchConstraintPath(path, c.match) {
			return false
		}
		return !c.predicated() || hosts[path]
	})
}

// resolvedPathIn resolves a path target to a measured file and applies the
// caller's test to it, in both the label-prefixed and repo-relative forms — the
// same double join matchConstraintFile uses, because a single trimmed form
// mis-fires when a real path starts with the repo's own name.
func (g *grounding) resolvedPathIn(rel facts.Relation, from facts.Fact, ok func(string) bool) bool {
	if !pathTargetEdge(rel, from) {
		return false
	}
	path, resolved := g.resolve(rel.Target, from.Repo, from.File)
	if !resolved {
		return false
	}
	if ok(path) {
		return true
	}
	if from.Repo != "" {
		if trimmed := strings.TrimPrefix(path, from.Repo+"/"); trimmed != path {
			return ok(trimmed)
		}
	}
	return false
}

// resolves reports whether a path target names a measured file at all —
// the resolvability question, asked only after the target failed to name a fact.
func (g *grounding) resolves(rel facts.Relation, from facts.Fact) bool {
	if !pathTargetEdge(rel, from) {
		return false
	}
	_, ok := g.resolve(rel.Target, from.Repo, from.File)
	return ok
}

// groundedMembers names the members a path-targeting edge lands on when its
// target names no member but resolves to a file those members were measured in.
// An inbound edge demanded of a member is satisfied by reaching the file the
// member lives in, since that is the only thing the target can name.
func groundedMembers(rel facts.Relation, from facts.Fact, memberFacts []facts.Fact, g *grounding) []string {
	if !pathTargetEdge(rel, from) {
		return nil
	}
	path, ok := g.resolve(rel.Target, from.Repo, from.File)
	if !ok {
		return nil
	}
	var out []string
	for _, m := range memberFacts {
		if m.File == path {
			out = append(out, m.Name)
		}
	}
	return out
}

// ungroundable reports a path-targeting edge whose target names no measured fact
// AND resolves to no measured file — a target this snapshot cannot answer for at
// all. Why it cannot differs by edge kind, which groundSkipDiagnosis words.
// Callers count these rather than dropping them, because a target nobody could
// resolve is a verdict not reached.
func (g *grounding) ungroundable(rel facts.Relation, from facts.Fact) bool {
	if !pathTargetEdge(rel, from) || g.names[rel.Target] {
		return false
	}
	_, resolved := g.resolve(rel.Target, from.Repo, from.File)
	return !resolved
}

// groundSkipDiagnosis words why a rule's targets resolved to nothing, and what a
// reader can do about it. The two edge kinds grounding admits fail for opposite
// reasons, so one sentence covering both would diagnose neither.
//
// An imports target is quoted from the source, so it commonly names a package
// this snapshot never held, and a wider snapshot is the lever that reaches it.
//
// A markup-declared target is a path the declaring markup's own naming convention
// produced, so it is inside this tree by construction: no wider snapshot holds
// it, and it resolved to nothing because the tree holds no such file, or because
// no extractor pass claimed the file that is there — the Ruby pass builds a
// binding's target from the repository's file list, which is not the same set as
// the files an extractor parsed into facts.
func groundSkipDiagnosis(via string) (cause string, actions []string) {
	if via == facts.RelImports {
		return "typically a package outside the tree", []string{
			"Ignore the count when the targets are third-party packages the rule was never meant to reach",
			"Widen the snapshot to the repository holding those files if the rule should verdict them",
		}
	}
	return "each one an in-tree path the declaring markup's naming convention produced, so no wider snapshot holds it", []string{
		"Fix the binding when the tree holds no such file — an identifier resolving to nothing binds nothing at runtime either",
		"Extend extraction to that file kind when the file is there and no pass claimed it — widening the snapshot cannot reach a path already inside the tree",
	}
}

// groundSkipInsight names how many path targets a rule reached no verdict on,
// with a sample, so the residue is counted rather than swallowed.
func groundSkipInsight(r rule, targets map[string]bool) facts.Insight {
	named := make([]string, 0, len(targets))
	for t := range targets {
		named = append(named, t)
	}
	sort.Strings(named)
	sample := named
	if len(sample) > groundSkipSample {
		sample = sample[:groundSkipSample]
	}
	cause, actions := groundSkipDiagnosis(r.via)
	return facts.Insight{
		Title:       fmt.Sprintf("Constraint %s reached no verdict on %d %s target(s) naming nothing measured", r.id, len(named), r.via),
		Description: fmt.Sprintf("The rule walks %s edges, and %d of their targets name neither a measured fact nor a measured file in the declaring repository — %s. Those edges were skipped rather than verdicted: nothing was resolved, so nothing is claimed. This advisory exists so that the skip cannot be read as compliance. Sample: %s. Because: %s", r.via, len(named), cause, strings.Join(sample, ", "), r.because),
		Confidence:  groundSkipConfidence,
		Evidence:    []facts.Evidence{{Fact: "rule: " + r.id, Detail: "declared in " + r.source}},
		Actions:     actions,
	}
}

// groundSkipSample bounds the named sample: the count is the finding, the names
// are there to make it checkable.
const groundSkipSample = 8

// How a verdict words the basis it reached each end of an edge on lives in
// basis.go: one three-state vocabulary — exact, owned, grounded — used at both
// ends, replacing the three boolean helpers that stood here.
