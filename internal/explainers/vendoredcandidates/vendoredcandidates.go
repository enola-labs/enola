// Package vendoredcandidates reports directories that LOOK like in-tree copies of
// somebody else's project, so the reader can decide whether to exclude them.
//
// It never excludes anything. That is the whole design, and it is a correction of
// an earlier attempt that did: a rule keyed on directory name plus a licence file
// pruned the tree automatically, and it deleted 671 files of a repository's own
// source along with the dependencies it was aiming at. Nothing said so — the code
// was simply absent from the graph, and the fact count went DOWN, which is the one
// direction a regression check reads as success.
//
// The lesson is not that the heuristic needed another pass. It is that a heuristic
// must not be wired to an irreversible action. enola already has the mechanism for
// exclusion — the `ignore:` globs, which the reader writes and can read back — so
// the only thing missing was DISCOVERY: knowing which directories are worth
// looking at. That is a report, and a report is allowed to be wrong.
//
// So every finding here is Informational (never gradeable, never a CI failure),
// states the evidence rather than a verdict, and carries the exact ignore glob to
// paste if the reader agrees. What is indexed stays exactly what the reader's
// config says is indexed.
package vendoredcandidates

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// candidateNames are the directory names that conventionally hold dependencies.
// Every one of them also names first-party code somewhere in the wild — a Go
// package called external, a Ruby module called third_party, a Python package
// called deps — which is exactly why this package reports rather than acts.
//
// UnderDependencyParent exposes the same vocabulary, so a consumer that wants to
// RANK by it rather than act on it reads one list instead of copying this one.
var candidateNames = map[string]bool{
	"third_party": true, "thirdparty": true, "3rdparty": true,
	"external": true, "externals": true, "extern": true,
	"deps": true, "contrib": true, "vendored": true,
	"Pods": true, "Carthage": true, "bower_components": true,
}

// licenceNames are the filenames a project uses to carry its licence, matched on
// the whole name rather than as a prefix: LICENSING.md is prose about licensing
// and licenses.go is source, and neither is a licence.
var licenceNames = map[string]bool{
	"license": true, "license.txt": true, "license.md": true, "license.rst": true,
	"licence": true, "licence.txt": true, "licence.md": true,
	"copying": true, "copying.txt": true, "copying.md": true,
	"license-mit": true, "license-apache": true, "unlicense": true, "copyright": true,
}

// ancestorDepth is how far above a licensed directory a candidate name may sit.
// One level is the common layout (external/brotli). Two covers the grouping
// directory real trees add — third_party/packages/<project>, or
// third_party/fonts.google.com/<font>.
const ancestorDepth = 2

// minFiles keeps the report quiet about a directory holding a licence and little
// else. A vendored dependency worth excluding is a body of code; a two-file
// directory is not worth a reader's attention either way.
const minFiles = 3

// Explainer reports candidate vendored directories.
type Explainer struct{}

// New creates a new Explainer.
func New() *Explainer { return &Explainer{} }

func (e *Explainer) Name() string { return "vendored-candidates" }

// Explain without the walked file names can say nothing: the evidence is a licence
// file, which no extractor parses and which therefore appears in no fact. Returning
// nothing is correct rather than degraded — the finding is unavailable, not wrong.
func (e *Explainer) Explain(context.Context, *facts.Store) ([]facts.Insight, error) {
	return nil, nil
}

// candidate is one directory that carries its own licence under a conventionally
// named parent, with what the fact store knows about it.
type candidate struct {
	dir       string // repo-relative directory
	licence   string // the licence file that is the evidence
	files     int    // indexed files beneath it
	languages []string
	inbound   int // references into it from outside the subtree
}

// ExplainFiles implements plugin.WalkAware.
func (e *Explainer) ExplainFiles(_ context.Context, store *facts.Store, walked []string) ([]facts.Insight, error) {
	// Pre-filtered against the WALKED names, which costs nothing, so a directory too
	// small to report never reaches the fact-store pass below. Without this a
	// repository with one two-file licensed directory paid the whole scan to emit
	// nothing — measured at +12.7s on one corpus repository that reported no finding
	// at all.
	cands := findCandidates(walked)
	if len(cands) == 0 {
		return nil, nil
	}
	describe(store, cands)

	kept := cands[:0]
	for _, c := range cands {
		if c.files >= minFiles {
			kept = append(kept, c)
		}
	}
	if len(kept) == 0 {
		return nil, nil
	}
	// Largest first: the directory worth a decision is the one carrying the most
	// code, and a reader scanning the list should meet it first.
	sort.Slice(kept, func(i, j int) bool {
		if kept[i].files != kept[j].files {
			return kept[i].files > kept[j].files
		}
		return kept[i].dir < kept[j].dir
	})

	total := 0
	evidence := make([]facts.Evidence, 0, len(kept))
	actions := make([]string, 0, len(kept)+1)
	for _, c := range kept {
		total += c.files
		detail := fmt.Sprintf("%d files", c.files)
		if len(c.languages) > 0 {
			detail += ", " + strings.Join(c.languages, "/")
		}
		detail += ", " + path.Base(c.licence)
		// The reference count is what separates a dependency nothing calls from one
		// the repository is built on. It is reported either way and gates nothing:
		// a vendored library IS often called, and that is not a reason to keep it.
		switch c.inbound {
		case 0:
			detail += ", no inbound refs"
		case 1:
			detail += ", 1 inbound ref"
		default:
			detail += fmt.Sprintf(", %d inbound refs", c.inbound)
		}
		evidence = append(evidence, facts.Evidence{File: c.licence, Detail: detail})
		actions = append(actions, fmt.Sprintf("  - \"%s/**\"", c.dir))
	}

	// Every candidate is listed, with no cap. A capped list would be the same
	// failure in a quieter form: code missing from the report rather than from the
	// graph, and no way for the reader to know what was left out.
	return []facts.Insight{{
		Title: fmt.Sprintf("%d director%s look like vendored third-party code (%d files)",
			len(kept), plural(len(kept)), total),
		Description: "Each directory below sits under a conventionally named parent " +
			"(third_party, external, contrib, deps, Pods, …) AND carries its own licence " +
			"file, which is what an in-tree copy of another project looks like. That is a " +
			"hint, not a verdict: the same names are used for first-party code, so read the " +
			"list before acting on it. Nothing has been excluded — these files are in this " +
			"snapshot. To leave them out of future ones, add the globs below to `ignore:` " +
			"in your enola config.",
		Confidence:    1.0,
		Informational: true,
		Evidence:      evidence,
		Actions:       append([]string{"Add to `ignore:` in the enola config any of these you agree are vendored:"}, actions...),
	}}, nil
}

func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

// findCandidates scans the walked names for directories that carry a licence file
// under a conventionally named ancestor.
//
// The candidate is the directory holding the LICENCE, never the conventionally
// named parent. That distinction is the whole correction: a container mixes
// dependencies with the repository's own code — one real tree holds eight licensed
// libraries beside four directories of first-party source — so naming the
// container would ask the reader to exclude both together.
func findCandidates(walked []string) []candidate {
	licenceDirs := make(map[string]string)
	for _, rel := range walked {
		base := rel[strings.LastIndex(rel, "/")+1:]
		if !licenceNames[strings.ToLower(base)] {
			continue
		}
		dir := path.Dir(rel)
		if dir == "." || !hasCandidateAncestor(dir) {
			continue
		}
		// First licence wins, so the report is stable regardless of walk order.
		if _, seen := licenceDirs[dir]; !seen {
			licenceDirs[dir] = rel
		}
	}
	out := make([]candidate, 0, len(licenceDirs))
	for dir, lic := range licenceDirs {
		out = append(out, candidate{dir: dir, licence: lic})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].dir < out[j].dir })
	return out
}

// hasCandidateAncestor reports whether one of the ancestorDepth segments directly
// above dir's own name is a conventional dependency directory name.
//
// Strictly above, never dir itself: a repository whose own contrib/ holds its
// LICENSE beside its own source is a licensed directory with a conventional name,
// and it is not vendoring. Only a licensed project INSIDE such a directory is.
func hasCandidateAncestor(dir string) bool {
	segs := strings.Split(dir, "/")
	for i := 1; i <= ancestorDepth; i++ {
		j := len(segs) - 1 - i
		if j < 0 {
			return false
		}
		if candidateNames[segs[j]] {
			return true
		}
	}
	return false
}

// describe fills in what the fact store knows about each candidate: how much
// indexed code it holds, in which languages, and how much of the repository
// outside it refers in.
//
// FactsRef, not All: All deep-copies every fact and clones its Props map and
// Relations slice, which on a large repository is the dominant cost of this
// explainer by an order of magnitude — and it was paid twice. Nothing here retains
// a fact past the call, which is exactly the contract FactsRef asks for.
func describe(store *facts.Store, cands []candidate) {
	if store == nil || len(cands) == 0 {
		return
	}
	// Match strings built once. Building them inside the per-fact loop allocated
	// twice per fact per candidate.
	prefix := make([]string, len(cands))
	infix := make([]string, len(cands))
	for i, c := range cands {
		prefix[i] = c.dir + "/"
		infix[i] = "/" + c.dir + "/"
	}
	langs := make([]map[string]bool, len(cands))
	for i := range cands {
		langs[i] = map[string]bool{}
	}
	// One walk of the facts, memoising the answer per distinct FILE — a repository
	// has far fewer files than facts, so the path matching runs once each.
	byFile := make(map[string]int)
	seenFile := make(map[string]bool)
	inSubtree := make(map[string]int)
	all := store.FactsRef()
	for i := range all {
		f := &all[i]
		idx, known := byFile[f.File]
		if !known {
			idx = matchCandidate(prefix, infix, f.File)
			byFile[f.File] = idx
		}
		if idx < 0 {
			continue
		}
		inSubtree[f.Name] = idx
		if !seenFile[f.File] {
			seenFile[f.File] = true
			cands[idx].files++
		}
		if l, ok := f.PropAny("language").(string); ok && l != "" {
			langs[idx][l] = true
		}
	}
	// A reference counts as inbound when its SOURCE lies outside the candidate and
	// its target inside. A fact within the subtree referring to another is the
	// dependency's own internal structure and says nothing about the repository.
	for i := range all {
		f := &all[i]
		if len(f.Relations) == 0 {
			continue
		}
		from, known := byFile[f.File]
		if !known {
			from = matchCandidate(prefix, infix, f.File)
			byFile[f.File] = from
		}
		for _, r := range f.Relations {
			if to, ok := inSubtree[r.Target]; ok && to != from {
				cands[to].inbound++
			}
		}
	}
	for i := range cands {
		for l := range langs[i] {
			cands[i].languages = append(cands[i].languages, l)
		}
		sort.Strings(cands[i].languages)
	}
}

// matchCandidate returns the index of the candidate whose directory contains file,
// or -1. In multi-repo mode a fact's File carries a repo prefix, so the match is a
// path-segment-aware suffix test as well as a prefix one.
func matchCandidate(prefix, infix []string, file string) int {
	if file == "" {
		return -1
	}
	for i := range prefix {
		if strings.HasPrefix(file, prefix[i]) || strings.Contains(file, infix[i]) {
			return i
		}
	}
	return -1
}

// UnderDependencyParent reports whether a repo-relative path passes through a
// directory conventionally used to hold other projects' code.
//
// It is deliberately weaker than what this explainer reports. A finding here needs
// the licence file too, because a container routinely mixes dependencies with the
// repository's own code — gmsh's `contrib/mobile/` is gmsh's. A caller that merely
// ORDERS its output can use the weaker test: over-matching costs a first-party
// directory some rank, where excluding on it would hide the code entirely.
func UnderDependencyParent(path string) bool {
	for _, seg := range strings.Split(strings.ReplaceAll(path, "\\", "/"), "/") {
		if candidateNames[seg] {
			return true
		}
	}
	return false
}
