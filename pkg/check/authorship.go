package check

import (
	"bytes"
	"fmt"
	"os/exec"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/explainers/common"
	"github.com/enola-labs/enola/internal/facts"
)

// MinorThreshold is the commit share below which a contributor counts as MINOR to a
// module — someone who has touched it but does not know it.
//
// The 5% figure is Bird, Nagappan, Murphy, Gall & Devanbu, "Don't Touch My Code!
// Examining the Effects of Ownership on Software Quality" (ESEC/FSE 2011), where it was
// chosen by inspecting the ownership distribution of Windows Vista binaries — components
// averaging some 900 commits across 17 engineers.
//
// It is NOT validated here, and nothing in enola grades on it. On a three-person
// repository every contributor clears 5% and the split says nothing; on a large
// open-source tree almost nobody does. That is exactly why this number steers a reader
// toward a reviewer and never reaches a verdict's status. See AttachReviewers.
const MinorThreshold = 0.05

// ownedThreshold is the share the top contributor must hold before a module is
// described as having a clear owner. The paper reports `Ownership` as a continuous
// proportion rather than a cutoff; this is a presentation choice, used only to decide
// whether the routing line reads "owner: X" or "no single contributor above 50%".
const ownedThreshold = 0.50

// minModuleCommits is how many commits a module must have in the window before its
// shares are read as evidence of anything.
//
// Without a floor the arithmetic produces its most confident claims on its weakest
// data: a module with one commit in the window has a contributor holding 100% of it,
// and "you own this" is then said about a single drive-by edit. Measured on chatwoot,
// the top major-minor-dependency reported was a module with exactly one commit behind
// its 100%. The paper never had to draw this line — its components averaged some 900
// commits — and a repository of ordinary directories is nothing like that.
//
// Five is deliberately low. It is not a claim that five commits make an expert; it is
// the point below which a proportion stops describing a distribution at all.
const minModuleCommits = 5

// Causes an authorship read can decline for, reported rather than hidden: a run that
// silently measured nothing would print "no owners" for a repository full of them.
const (
	AuthorshipNoGit   = "no_git"
	AuthorshipShallow = "shallow"
	AuthorshipEmpty   = "empty_window"
)

// ModuleAuthorship is the paper's four measures for one module, over the window that
// produced them.
type ModuleAuthorship struct {
	Module string `json:"module"`
	// Shares is each contributor's proportion of the module's commits in the window.
	Shares map[string]float64 `json:"shares"`
	// Commits is the module's commit count in the window — the denominator of Shares,
	// carried so a reader can tell a 50% share of two commits from 50% of two hundred.
	Commits int `json:"commits"`
	// Total, Major and Minor are the paper's contributor counts.
	Total int `json:"total"`
	Major int `json:"major"`
	Minor int `json:"minor"`
	// TopAuthor and TopShare are the paper's `Ownership`: the single largest share and
	// who holds it.
	TopAuthor string  `json:"top_author"`
	TopShare  float64 `json:"top_share"`
	// Sparse marks a module with too few commits in the window for its shares to mean
	// anything. Its numbers are still carried — they are what was measured — but no
	// caller may read ownership out of them. See minModuleCommits.
	Sparse bool `json:"sparse,omitempty"`
}

// IsMinor reports whether an author is a minor contributor to this module. An author
// with no commits at all in the window is minor by the same test — they hold 0%.
func (m ModuleAuthorship) IsMinor(author string) bool {
	return m.Shares[author] < MinorThreshold
}

// IsMajor is NOT the negation of IsMinor, in two ways that both matter.
//
// The questions get asked about different modules — minor in the one being edited,
// major in the one that depends on it — and collapsing them into one predicate at a
// call site is how the major-minor-dependency join gets inverted.
//
// And a sparse module has no majors while its sole contributor is not minor there
// either: holding all of three commits is neither expertise nor estrangement. The
// floor belongs on this side because this is the predicate the "you own X" half of the
// join rests on; whether somebody is a STRANGER to thin code is still answerable, and
// IsMinor still answers it.
func (m ModuleAuthorship) IsMajor(author string) bool {
	return !m.Sparse && m.Shares[author] >= MinorThreshold
}

// Authorship is who has been committing to which module, over a bounded window of
// recent history.
//
// It is read at CHECK time and never enters a snapshot. Every number here moves with
// the repository's history rather than with its code, so putting one in a fact or a
// finding would make two snapshots of the same commit differ — the property the whole
// gate rests on. See the invariants in AttachReviewers.
type Authorship struct {
	// Window is how many commits were read. Reported alongside every measure, because
	// "owner of this module" means nothing without the span it was measured over.
	Window int `json:"window"`
	// Commits is how many commits the window actually yielded, which is lower than
	// Window on a repository younger than it.
	Commits int `json:"commits"`
	// Cause is why the read is empty or partial, one of the Authorship* constants, or
	// "" when the measurement is whole.
	Cause string `json:"cause,omitempty"`

	byModule map[string]ModuleAuthorship
	authors  map[string]bool
}

// Knows reports whether an author committed anywhere in the window.
//
// The distinction this draws is between "a stranger to THIS module" and "not a
// contributor to this repository at all". The second is usually not a person: it is an
// unset git identity, a CI robot, or a name spelled differently from the history. Both
// hold 0% of every module, so without this the routing section tells a build server it
// is a minor contributor to everything it touches.
func (a *Authorship) Knows(author string) bool {
	return a != nil && a.authors[author]
}

// Module returns the measures for a module, and whether it had any commits in the
// window. A module the window never touched is absent rather than zeroed: "nobody has
// changed this in 500 commits" and "this module has no owner" are different statements.
func (a *Authorship) Module(name string) (ModuleAuthorship, bool) {
	if a == nil {
		return ModuleAuthorship{}, false
	}
	m, ok := a.byModule[name]
	return m, ok
}

// ReadAuthorship measures per-module commit shares over the last `window` commits of
// repo, mapping each changed path to its module through the store's module namespace.
//
// One `git log` call, bounded by the window, so the cost does not grow with the age of
// the repository and nothing needs caching. Author names come from %aN, which applies
// .mailmap, so a contributor committing under two spellings is counted once.
//
// A repository git cannot read, or a shallow clone whose window is longer than its
// history, returns an Authorship carrying the cause rather than an error: routing is
// advisory, and a gate must not fail because it could not offer advice.
func ReadAuthorship(repo string, window int, store *facts.Store) *Authorship {
	a := &Authorship{Window: window, byModule: map[string]ModuleAuthorship{}, authors: map[string]bool{}}
	if window <= 0 || store == nil {
		a.Cause = AuthorshipEmpty
		return a
	}
	if exec.Command("git", "-C", repo, "rev-parse", "--is-inside-work-tree").Run() != nil {
		a.Cause = AuthorshipNoGit
		return a
	}

	out, err := exec.Command("git", "-C", repo, "log", "--no-merges",
		"-n", fmt.Sprint(window), "--format=%x00%aN", "--name-only").Output()
	if err != nil {
		a.Cause = AuthorshipNoGit
		return a
	}

	prod, test := common.ModuleNameSets(store)
	dirs := common.NewModuleDirs(store)

	// counts[module][author] — a commit contributes ONE unit to a module however many
	// of that module's files it touched. Weighting by file count would let a
	// tree-wide rename outrank years of real work on a package.
	counts := map[string]map[string]int{}
	moduleCommits := map[string]int{}

	author := ""
	touched := map[string]bool{}
	flush := func() {
		if author == "" {
			return
		}
		for mod := range touched {
			if counts[mod] == nil {
				counts[mod] = map[string]int{}
			}
			counts[mod][author]++
			moduleCommits[mod]++
		}
	}

	for _, raw := range bytes.Split(out, []byte{'\n'}) {
		line := string(raw)
		if strings.HasPrefix(line, "\x00") {
			flush()
			author = strings.TrimSpace(line[1:])
			a.authors[author] = true
			touched = map[string]bool{}
			a.Commits++
			continue
		}
		if line == "" || author == "" {
			continue
		}
		// Resolve through the same path the module graph uses, so a routing line names
		// a module that graph also has an edge for. A path outside every known module
		// (a top-level README, a deleted tree) resolves to a name nothing matches and
		// simply never joins.
		// Repo is deliberately empty: git log emits repository-relative paths, which
		// are already in module-name space. Setting the label would ask
		// ModuleDirCandidates to strip a prefix these paths never carry.
		mod := common.ResolveModule(facts.Fact{File: line}, prod, test, dirs)
		if !prod[mod] {
			continue // test scaffolding, or no module at all
		}
		touched[mod] = true
	}
	flush()

	if a.Commits == 0 {
		a.Cause = AuthorshipEmpty
		return a
	}
	// A shallow clone that ran out of history before the window closed measured a
	// shorter span than it was asked for. Said once, here, rather than per module.
	if a.Commits < window {
		if sh, err := exec.Command("git", "-C", repo, "rev-parse", "--is-shallow-repository").Output(); err == nil &&
			strings.TrimSpace(string(sh)) == "true" {
			a.Cause = AuthorshipShallow
		}
	}

	for mod, authors := range counts {
		a.byModule[mod] = summarise(mod, authors, moduleCommits[mod])
	}
	return a
}

// summarise turns one module's raw per-author commit counts into the paper's measures.
// Split out so the aggregation is testable without a repository.
func summarise(module string, authors map[string]int, commits int) ModuleAuthorship {
	m := ModuleAuthorship{
		Module:  module,
		Shares:  make(map[string]float64, len(authors)),
		Commits: commits,
		Total:   len(authors),
		Sparse:  commits < minModuleCommits,
	}
	if commits == 0 {
		return m
	}
	// Names sorted so the top author is decided the same way on every run when two
	// contributors hold an identical share.
	names := make([]string, 0, len(authors))
	for name := range authors {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		share := float64(authors[name]) / float64(commits)
		m.Shares[name] = share
		if share >= MinorThreshold {
			m.Major++
		} else {
			m.Minor++
		}
		if share > m.TopShare {
			m.TopShare, m.TopAuthor = share, name
		}
	}
	return m
}

// Actor is whose change this is: the identity the routing section speaks to when it
// says "you are a minor contributor here".
//
// override wins; otherwise git's configured user name, which is what will land in %aN
// when this work is committed. Returns "" when neither is available, and the caller
// degrades to reporting owners without addressing anyone.
func Actor(repo, override string) string {
	if strings.TrimSpace(override) != "" {
		return strings.TrimSpace(override)
	}
	out, err := exec.Command("git", "-C", repo, "config", "user.name").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
