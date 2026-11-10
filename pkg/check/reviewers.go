package check

import (
	"sort"

	"github.com/enola-labs/enola/internal/explainers/common"
	"github.com/enola-labs/enola/internal/facts"
)

// MajorMinorDependency is the relationship Bird et al. found in Windows Vista: the
// person changing a module barely knows it, but owns something that DEPENDS on it.
//
// 52% of Vista binaries had one, far above a degree-preserving rewired null model over
// 10,000 random contribution graphs — so it is not what random assignment of developers
// to components would produce. Cataldo et al., cited there, found that changing a
// depending component without coordinating with the owner of what it depends on raises
// the fault rate. That is the whole reason this section exists.
type MajorMinorDependency struct {
	// Dependent is the module the actor owns, and Share their proportion of it.
	Dependent string  `json:"dependent"`
	Share     float64 `json:"share"`
}

// ReviewerRoute is what the verdict reports for one module the change touched.
type ReviewerRoute struct {
	Module string `json:"module"`
	// Owner and OwnerShare are the paper's `Ownership` — the largest single share and
	// who holds it. Owner is empty when nobody clears ownedThreshold, which is itself
	// the finding: no clear point of contact.
	Owner      string  `json:"owner,omitempty"`
	OwnerShare float64 `json:"owner_share,omitempty"`
	// Minor and Total are the paper's contributor counts for this module.
	Minor int `json:"minor"`
	Total int `json:"total"`
	// Commits is how many commits in the window touched this module.
	Commits int `json:"commits"`

	// ActorShare is the actor's own proportion of this module, and ActorIsMinor
	// whether that falls under MinorThreshold. Both are zero-valued when the run
	// could not tell who the actor is.
	ActorShare   float64 `json:"actor_share,omitempty"`
	ActorIsMinor bool    `json:"actor_is_minor,omitempty"`

	// ViaDependents are the major-minor-dependency hits: modules the actor owns that
	// import this one. Non-empty only when ActorIsMinor.
	ViaDependents []MajorMinorDependency `json:"via_dependents,omitempty"`
}

// Reviewers is the routing report as a whole, carried on the verdict.
type Reviewers struct {
	// Actor is whose change this is, "" when the run could not tell.
	Actor string `json:"actor,omitempty"`
	// ActorUnknown marks an actor who committed nowhere in the window — an unset git
	// identity, a CI robot, or a name the history spells differently. Said once here
	// rather than as "you are a minor contributor" on every module they touched.
	ActorUnknown bool `json:"actor_unknown,omitempty"`
	// Window and Cause describe the measurement, mirroring Authorship.
	Window int             `json:"window"`
	Cause  string          `json:"cause,omitempty"`
	Routes []ReviewerRoute `json:"routes,omitempty"`
}

// AttachReviewers reports who owns the modules this change touched, and — where the
// actor is a minor contributor to one while owning something that depends on it —
// names the reviewer to route to.
//
// It is STEERING, never grading, and the distinction is load-bearing enough to state
// twice. Everything here is derived from git history rather than from the code, so it
// moves when nobody has changed a line; and the contributor split it rests on is a
// correlation with defects measured on Windows Vista, not a rule this repository
// declared. A verdict's Status, ExitCode, Failures and Breaches must be identical with
// and without this call — TestReviewersNeverGrade holds that.
//
// Modelled on AttachGuidance, including the status guard: a verdict that declined to
// grade has no delta worth routing.
func AttachReviewers(v Verdict, store *facts.Store, a *Authorship, actor string) Verdict {
	if store == nil || v.Diff == nil || a == nil {
		return v
	}
	switch v.Status {
	case StatusClean, StatusRegression, StatusPartialClean, StatusPartialRegression:
	default:
		return v
	}

	r := &Reviewers{Actor: actor, Window: a.Window, Cause: a.Cause}
	// An actor the window never saw holds 0% of everything, which is arithmetically
	// minor everywhere and informative nowhere. Drop the actor framing and report
	// owners only; the join cannot fire for them either, since majority anywhere
	// requires commits somewhere.
	if actor != "" && !a.Knows(actor) {
		r.ActorUnknown = true
		actor = ""
	}

	prod, test := common.ModuleNameSets(store)
	dirs := common.NewModuleDirs(store)
	touched := map[string]bool{}
	for _, cf := range changedFiles(v.Diff) {
		// changedFiles yields fact File paths, which ARE repo-prefixed in an
		// append-mode snapshot — unlike the git-log paths ReadAuthorship resolved.
		// Carrying the repo through is what lets ModuleDirCandidates strip it.
		mod := common.ResolveModule(facts.Fact{File: cf.Path, Repo: cf.Repo}, prod, test, dirs)
		if prod[mod] {
			touched[mod] = true
		}
	}
	if len(touched) == 0 {
		v.Reviewers = r
		return v
	}

	// Reverse the module graph once: dependents[M] is every module importing M. The
	// forward graph is what BuildModuleGraph hands back, and the join needs the other
	// direction — a module the actor owns that reaches DOWN into what they just edited.
	dependents := map[string][]string{}
	if actor != "" {
		for src, targets := range common.BuildModuleGraph(store) {
			for _, t := range targets {
				dependents[t] = append(dependents[t], src)
			}
		}
	}

	names := make([]string, 0, len(touched))
	for m := range touched {
		names = append(names, m)
	}
	sort.Strings(names)

	for _, mod := range names {
		ma, ok := a.Module(mod)
		if !ok {
			continue // untouched in the window: nothing measured, so nothing claimed
		}
		route := ReviewerRoute{
			Module:  mod,
			Minor:   ma.Minor,
			Total:   ma.Total,
			Commits: ma.Commits,
		}
		// A sparse module names no owner: see minModuleCommits. The contributor
		// counts still print, so the line reports what little was measured rather
		// than vanishing.
		if !ma.Sparse && ma.TopShare >= ownedThreshold {
			route.Owner, route.OwnerShare = ma.TopAuthor, ma.TopShare
		}
		if actor != "" {
			route.ActorShare = ma.Shares[actor]
			route.ActorIsMinor = ma.IsMinor(actor)
			if route.ActorIsMinor {
				route.ViaDependents = majorMinorDependencies(a, actor, dependents[mod])
			}
		}
		r.Routes = append(r.Routes, route)
	}

	v.Reviewers = r
	return v
}

// majorMinorDependencies keeps the dependents of a module that the actor is a MAJOR
// contributor to. The direction is the whole point: a module the actor owns which
// imports the one they just edited as a stranger. Reversing it would report the
// harmless case — an owner reaching into code that depends on them.
func majorMinorDependencies(a *Authorship, actor string, dependents []string) []MajorMinorDependency {
	var out []MajorMinorDependency
	seen := map[string]bool{}
	for _, dep := range dependents {
		if seen[dep] {
			continue
		}
		seen[dep] = true
		da, ok := a.Module(dep)
		if !ok || !da.IsMajor(actor) {
			continue
		}
		out = append(out, MajorMinorDependency{Dependent: dep, Share: da.Shares[actor]})
	}
	// Largest stake first: the module the actor owns most strongly is the one whose
	// expertise they are declining to apply.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Share != out[j].Share {
			return out[i].Share > out[j].Share
		}
		return out[i].Dependent < out[j].Dependent
	})
	return out
}
