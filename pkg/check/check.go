// Package check turns an architecture delta into a pass/fail verdict with an exit
// code, so the diff that `diff_snapshot` serves to an agent can also gate a commit
// or a CI job without an agent in the loop.
//
// It is deliberately a thin policy layer over internal/diff: Compute decides WHAT
// changed, Evaluate decides whether that is allowed to break a build. Evaluate is
// pure — same delta plus same policy always yields the same verdict — so a gate is
// as reproducible as the snapshot underneath it.
package check

import (
	"github.com/enola-labs/enola/internal/diff"
	"github.com/enola-labs/enola/internal/explainers/constraints"
	"github.com/enola-labs/enola/internal/facts"
)

// Status is the verdict. Its ExitCode is the process's contract with CI.
type Status string

const (
	// StatusClean — nothing the policy fails on. Advisory warnings may still print.
	StatusClean Status = "clean"
	// StatusRegression — the policy was violated.
	StatusRegression Status = "regression"
	// StatusUsageError — the caller's inputs were wrong in a way with a concrete
	// remedy (no baseline, an inverted snapshot pair).
	StatusUsageError Status = "usage_error"
	// StatusIncomparable — the two snapshots were not generated over equivalent
	// inputs, so the delta describes extraction differences rather than the change.
	// Distinct from StatusRegression on purpose: "I refuse to grade this" must never
	// be reported as "your change is bad".
	StatusIncomparable Status = "incomparable"

	StatusPartialClean      Status = "partial_clean"
	StatusPartialRegression Status = "partial_regression"
)

// ExitCode maps a status to the process exit code.
func (s Status) ExitCode() int {
	switch s {
	case StatusClean, StatusPartialClean:
		return 0
	case StatusRegression, StatusPartialRegression:
		return 1
	case StatusUsageError:
		return 2
	case StatusIncomparable:
		return 3
	default:
		return 2
	}
}

// DefaultFailExplainers is empty, and that is the design: enola measures, reports, and
// fails nothing until you name what should fail.
//
// It used to hold "cycles". A dependency cycle is exactly measurable, which made it a
// tempting default — but exactly measurable is not the same as universally unwanted.
// Go's own standard library is written in a language whose packages cannot form one and
// whose authors treat the restriction as a design constraint rather than a virtue; a
// Rails app assembles most of its graph at runtime and its community does not read a
// cycle between two app/ directories as a defect at all. Shipping a default that breaks
// those builds meant enola arrived asserting a position on someone else's architecture
// before it had measured anything, and the first thing a Go or Rails team had to learn
// was which flag turns the opinion off.
//
// So the gate now enforces only what the caller states: --fail-on names the explainers,
// --max-spillover bounds the scope check, and a run with neither reports its findings and
// exits 0. Nothing is hidden by that — an unenforced run says so in its own output rather
// than printing a green line that could be mistaken for an all-clear (see Render).
//
// Deprecated: retained so a consumer that referenced this symbol still compiles. It is
// empty and no longer consulted; set Policy.FailExplainers to enforce anything.
var DefaultFailExplainers []string

// DefaultMinConfidence is the floor applied WITHIN the failing explainers.
//
// It still does real work: the constraints explainer emits advisory-mode breaches at
// 0.9 and dead-selector component notes at 0.4 — both from a failing explainer, both
// deliberately below this floor, because their own descriptions say they report rather
// than enforce. The floor keeps them out of the gate without needing a second
// allow-list. (The cycles explainer's oversized "highly coupled module cluster" used
// to sit under the floor at 0.4 too. That was a defect, not a design: a cluster is a
// cycle, exactly as certain, and softening its confidence let a growing cycle drop
// under the gate — see the doctrine comment on cycles.maxCycleModules. It reports at
// 1.0 now.)
const DefaultMinConfidence = 1.0

// Measurement is a count the policy can gate on that the DELTA ALONE DOES NOT CARRY —
// something the caller computed by running its own analysis over the two snapshots.
//
// It exists so that grading stays in one place while measuring stays with whoever can
// measure. A caller that owns an analyzer the engine does not ship can report what it
// found; it does not get to decide what that means for the verdict, because two surfaces
// deciding separately is how they come to disagree about the same change.
type Measurement struct {
	// Name is the stable key a Threshold refers to.
	Name string `json:"name"`
	// Label is the human phrasing used in the verdict line ("net-new dead-code
	// orphan(s)"), so a breach reads as a sentence rather than a variable name.
	Label string `json:"label"`
	Count int    `json:"count"`
}

// Threshold turns a Measurement into a verdict contribution. A zero bound disables that
// severity, so {FailAt: 3} warns at nothing and fails at three.
//
// Both bounds live here rather than in the measuring code because the whole point is
// that one policy object describes what the gate enforces, whoever invoked it.
type Threshold struct {
	Measurement string `json:"measurement"`
	WarnAt      int    `json:"warn_at,omitempty"`
	FailAt      int    `json:"fail_at,omitempty"`
}

// Breach is a Measurement that met or exceeded one of its bounds.
type Breach struct {
	Measurement Measurement `json:"measurement"`
	// Fatal distinguishes a FailAt breach from a WarnAt one. A warning breach is
	// reported and does not change the exit code.
	Fatal bool `json:"fatal"`
}

// Policy decides which parts of a delta are allowed to fail a build.
type Policy struct {
	// FailExplainers are the explainer names whose new findings fail the gate.
	// EMPTY MEANS NOTHING FAILS: every new finding is reported as an advisory and the
	// verdict is clean. See DefaultFailExplainers for why the empty set is the default.
	FailExplainers []string `json:"fail_explainers"`
	// MinConfidence is the floor within FailExplainers. Zero means DefaultMinConfidence.
	MinConfidence float64 `json:"min_confidence"`
	// WarnOnly reports everything and still exits clean. Blocking incomparability and
	// usage errors are NOT suppressed by it — those are not judgements about the code,
	// they are statements that the gate could not run.
	WarnOnly bool `json:"warn_only"`
	// Thresholds gate on Measurements the caller supplies. EMPTY BY DEFAULT, which is
	// what keeps this additive: with no thresholds the verdict is byte-identical to
	// what it was before measurements existed, so a build that passes today still
	// passes. Anything that would newly fail a build has to be asked for.
	Thresholds []Threshold `json:"thresholds,omitempty"`
	// Suppressions is the repository's signed excuse ledger (LoadSuppressions).
	// A finding an entry selects lands in the verdict's Suppressed bucket and
	// never fails — reported, attributed, and out of the gate. Part of the
	// policy because it decides verdicts: same delta plus same policy, ledger
	// included, always yields the same verdict.
	Suppressions []Suppression `json:"suppressions,omitempty"`
}

// thresholdFor returns the bound configured for a measurement, if any.
func (p Policy) thresholdFor(name string) (Threshold, bool) {
	for _, t := range p.Thresholds {
		if t.Measurement == name {
			return t, true
		}
	}
	return Threshold{}, false
}

// resolved fills in the defaults, so a Verdict records the policy that was actually
// ENFORCED rather than the one the caller happened to type. It matters most for the
// floor: a caller who set FailExplainers and left MinConfidence at zero is gating at
// 1.00, and JSON reporting `min_confidence: 0` would tell a consumer the opposite.
// FailExplainers is carried through untouched — an empty list is now a real answer
// ("nothing could fail"), not a stand-in for a default.
func (p Policy) resolved() Policy {
	return Policy{
		FailExplainers: p.failExplainers(),
		MinConfidence:  p.minConfidence(),
		WarnOnly:       p.WarnOnly,
		Thresholds:     p.Thresholds,
		Suppressions:   p.Suppressions,
	}
}

func (p Policy) failExplainers() []string {
	return p.FailExplainers
}

// Enforcing reports whether this policy can fail anything at all. A run where it is
// false still grades and still reports; it just has no grounds to exit non-zero, and
// its output has to say so rather than print an unqualified PASS.
//
// Thresholds count: --max-spillover=0 with no --fail-on is a legitimate gate that
// enforces scope and no findings.
func (p Policy) Enforcing() bool {
	return len(p.FailExplainers) > 0 || len(p.Thresholds) > 0
}

func (p Policy) minConfidence() float64 {
	if p.MinConfidence == 0 {
		return DefaultMinConfidence
	}
	return p.MinConfidence
}

// fails reports whether one new finding violates the policy.
func (p Policy) fails(in facts.Insight) bool {
	// A descriptive finding is never a violation, whatever its confidence or explainer.
	// Without this, declaring a layer order for the first time fails the pull request
	// that declared it: the `layers` explainer emits an exact finding SAYING SO, and
	// --fail-on=layers would grade the description alongside the violations.
	if in.Informational {
		return false
	}
	if in.Confidence < p.minConfidence() {
		return false
	}
	// An unset Source cannot be matched against the allow-list. Treated as NOT failing:
	// findings from a snapshot written before Source was recorded would otherwise all
	// fail at once, which is a tooling artifact rather than a regression.
	for _, name := range p.failExplainers() {
		if in.Source == name {
			return true
		}
	}
	return false
}

// blockingKinds are the comparability warnings that make a delta meaningless rather
// than merely caveated: the numbers below them describe differences in how the two
// snapshots were produced, not the change under test.
// BlockingKinds returns the comparability warnings that make a delta untrustworthy —
// the ones that make the gate decline rather than grade.
//
// Exported so callers who need the classification without running the gate share it
// rather than re-deriving it: the session-start hook decides whether to refresh a
// baseline on exactly the grounds `enola check` would have refused to use it. Two
// copies of "which warnings are fatal" would drift, and the drift would be invisible.
func BlockingKinds(c diff.Comparability) []diff.WarningKind {
	var out []diff.WarningKind
	for _, k := range c.Kinds {
		if blockingKinds[k] {
			out = append(out, k)
		}
	}
	return out
}

var blockingKinds = map[diff.WarningKind]bool{
	diff.WarnDifferentRepo: true,
	// Same repository, different fact labels: nothing matches across the two sides, so
	// the delta is a fiction the gate must not grade. It reached production as a green
	// job reporting the entire repository as added and removed.
	diff.WarnRepoLabel:       true,
	diff.WarnVersionMismatch: true,
	diff.WarnExtractorSet:    true,
	// A provider is a fact source exactly as an extractor is, so a differing
	// ran-provider set invalidates the fact delta the same way.
	diff.WarnProviderSet: true,
	diff.WarnIgnoreGlobs: true,
	// Fail closed on a caveat this package cannot categorize — see diff.AddWarning.
	diff.WarnUnclassified: true,
}

// advisoryKinds caveat a delta that is still worth grading. A stale baseline is the
// important member: internal/diff treats staleness as deliberately-not-a-refusal,
// because a long-lived baseline is a legitimate way to measure a multi-day refactor,
// and only the caller knows which they meant. The gate honours that — it says exactly
// why the baseline is stale and what it means for the delta, then grades anyway.
var advisoryKinds = map[diff.WarningKind]bool{
	diff.WarnStaleBaseline: true,
	diff.WarnPreReceipt:    true,
	// The explainer set differed. Deliberately advisory rather than blocking: unlike a
	// differing EXTRACTOR set, the fact delta is untouched, so the change is still
	// gradeable and only the findings from the explainers that differ are misattributed.
	// Registering it here is what makes "advisory" mean reported-and-graded rather than
	// silent — a kind in neither map is carried in ComparabilityWarnings but named in
	// no summary line.
	diff.WarnExplainerSet: true,
}

// Verdict is the graded delta.
type Verdict struct {
	Status Status `json:"status"`
	Policy Policy `json:"policy"`

	// Census is what the run could not see, printed as one line under the
	// headline on every outcome; see AttachCensus.
	Census *Census `json:"census,omitempty"`

	// Law is the declared law's excuse summary — how many rules are declared,
	// how many breaches they raised, and how many of those were signed away by
	// a suppression or an exemption. Reported, never graded: see AttachLedger
	// for why a gate must not fail on its own unpopularity. Absent entirely
	// when the repository declares no rules, because undeclared is unasked and
	// a zeroed ledger would read as a clean bill of health.
	Law *LedgerSummary `json:"law,omitempty"`

	// Failures are the new findings that violated the policy.
	Failures []facts.Insight `json:"failures,omitempty"`
	// Advisories are new findings that did NOT violate it — reported so a clean exit
	// is still informative, never silent about a real structural change.
	Advisories []facts.Insight `json:"advisories,omitempty"`
	// Suppressed are findings a ledger entry excused: reported in their own
	// bucket — never failed, never folded into Advisories, because "someone
	// signed this away" and "below the policy" are different statements and a
	// reader auditing the ledger needs the first one visible.
	Suppressed []facts.Insight `json:"suppressed,omitempty"`
	Exempted   []facts.Insight `json:"exempted,omitempty"`
	// Resolved are findings the change cleared. Worth printing: a gate that only ever
	// reports bad news trains people to read it as noise.
	Resolved []facts.Insight `json:"resolved,omitempty"`
	// Incidental are findings that moved with no structural cause in this change —
	// a drifting statistical threshold or a re-ranked top-N. Never graded.
	Incidental []facts.Insight `json:"incidental,omitempty"`
	// Descriptive are new findings that describe the graph rather than complain about
	// it (facts.Insight.Informational). Kept out of Advisories rather than merged into
	// them: the advisory note explains why a finding did NOT fail, and neither of its
	// reasons — under the floor, or outside --fail-on — is true of these.
	Descriptive []facts.Insight `json:"descriptive,omitempty"`
	// Silenced are constraint breaches that stopped being reported because the
	// code they named left the component the rule binds, not because it complied.
	// Held out of Resolved deliberately: a rule losing its subject printed as a
	// win is CI reporting a regression as an improvement. Not graded — a member
	// leaving a component is often exactly the refactor intended — but never
	// silent, and never counted as good news.
	Silenced []facts.Insight `json:"silenced,omitempty"`
	// Undeclared are constraint breaches that stopped being reported because the
	// declaration changed — the rule deleted, its form swapped under a preserved
	// id, or the witness exempted — with the breaching code untouched. Held out
	// of Resolved for the same reason Silenced is: a law that stopped asking the
	// question is not an answer to it. Not graded; editing a declaration is a
	// legitimate act, and this section is what keeps it from reading as a fix.
	Undeclared []facts.Insight `json:"undeclared,omitempty"`
	// Declared are constraint breaches that started being reported because the
	// declaration arrived — a rule new to the graph, re-formed under its id, or
	// an exemption removed — on code the change did not touch. The mirror of
	// Undeclared. They are the baseline a rule starts from, not regressions the
	// change made, and a strict rule among them is graded by the strict pass
	// below exactly as before: declaring a strict rule over standing breaches
	// fails, declaring an advisory one reports. What this bucket changes is the
	// sentence: "this change declares a rule that 3,980 places already break" is
	// not "this change introduced 3,980 findings".
	Declared []facts.Insight `json:"declared,omitempty"`
	// Unattributed are constraint breaches this pair of snapshots has no standing
	// to judge: the witness's repository left a union snapshot, or the baseline
	// carried the finding without the declaration that produced it. Held out of
	// Resolved because "the code is no longer measured" is not "the code was
	// fixed", and a still-breaching witness printed as good news is the failure
	// this whole section exists to prevent. Not graded — neither is a regression
	// in the change — but never silent.
	Unattributed []facts.Insight `json:"unattributed,omitempty"`

	// Measurements are every count the caller supplied, gated or not. Breaches are the
	// subset that met a threshold; a fatal one makes the status a regression exactly as
	// a failing finding does, so both surfaces reach the same verdict from the same
	// numbers.
	Measurements []Measurement `json:"measurements,omitempty"`
	Breaches     []Breach      `json:"breaches,omitempty"`

	Intersection *IntersectionGrading `json:"intersection_grading,omitempty"`

	Guidance []constraints.GuidanceMatch `json:"guidance,omitempty"`

	// Reviewers is who owns the modules this change touched, and who to route it to.
	// Present only under --reviewers; nil otherwise, so the default verdict is
	// unchanged and no author name is read unless someone asked for one.
	//
	// Never graded, for two independent reasons. It is derived from git history rather
	// than from the code, so it moves while the architecture stands still — the one
	// thing a delta gate cannot have in its verdict. And the contributor split under it
	// is a correlation with defects measured on Windows Vista binaries, not a rule this
	// repository declared, and enola fails only what was declared. See AttachReviewers.
	Reviewers *Reviewers `json:"reviewers,omitempty"`

	// ComparabilityWarnings is every warning, verbatim and in full. Not split by
	// severity: diff.Comparability records kinds as a set rather than per-message, and
	// inventing an alignment to sort the prose would be a lie about which message
	// carried which kind. The kinds below name the verdict; these give the reasons.
	ComparabilityWarnings []string           `json:"comparability_warnings,omitempty"`
	BlockingKinds         []diff.WarningKind `json:"blocking_kinds,omitempty"`
	AdvisoryKinds         []diff.WarningKind `json:"advisory_kinds,omitempty"`

	// Structural tallies.
	EdgesAdded   int `json:"edges_added"`
	EdgesRemoved int `json:"edges_removed"`
	FactsAdded   int `json:"facts_added"`
	FactsRemoved int `json:"facts_removed"`
	// FactsChanged counts facts present in both snapshots whose own attributes moved
	// (a signature, an exported flag, a complexity metric) — a change that is neither an
	// addition nor a removal, and was previously reported as nothing at all.
	FactsChanged int `json:"facts_changed"`

	// AddedByKind / RemovedByKind break the fact tallies down per architectural kind, so
	// "+4 facts" reads as "one module and three symbols" without opening the full diff.
	AddedByKind   map[string]int `json:"added_by_kind,omitempty"`
	RemovedByKind map[string]int `json:"removed_by_kind,omitempty"`

	// EdgeKindsAdded / EdgeKindsRemoved break the edge tallies down per relation, which
	// is what makes the raw number readable: a large share of the edges any change adds
	// are `declares` — the mechanical one-per-new-symbol link to its module — and reading
	// "+56 coupling" without that split badly overstates what the change actually coupled.
	EdgeKindsAdded   map[string]int `json:"edge_kinds_added,omitempty"`
	EdgeKindsRemoved map[string]int `json:"edge_kinds_removed,omitempty"`

	// FindingsChanged are findings that survived under the same identity but whose
	// content moved — a god-class whose fan-in ticked up, a rollup whose count swung.
	// Carried because silence here reads as safety: the gate is the loop's own
	// collateral-damage instrument, and a finding that moved is something it saw.
	FindingsChanged []diff.InsightChange `json:"findings_changed,omitempty"`

	// Diff is the underlying delta, carried so --json emits one self-contained
	// document and so Render can delegate the detail view to internal/diff.
	Diff *diff.SnapshotDiff `json:"diff,omitempty"`

	// sources is where the files the findings name live on disk; see Sources.
	// Unexported, so it never reaches the JSON verdict: a path on the grading
	// machine means nothing to whoever reads the document.
	sources Sources
}

// ExitCode is the process exit code for this verdict.
func (v Verdict) ExitCode() int { return v.Status.ExitCode() }

// edgeKindCounts tallies edges by relation kind (the counterpart of diff.KindCounts).
func edgeKindCounts(edges []diff.Edge) map[string]int {
	m := make(map[string]int, len(edges))
	for _, e := range edges {
		m[e.Kind]++
	}
	return m
}

// Evaluate grades a delta against a policy.
//
// Precedence is blocking incomparability, then usage error, then regression. Blocking
// comes first because it is the more fundamental complaint: when the two snapshots were
// built over different inputs, "re-run generate_snapshot" (the remedy for an inverted
// pair) sends the caller down the wrong path. Nothing is hidden by the ordering — every
// warning is reported regardless of which one decided the status.
//
// Delta-scoped: a finding the baseline already carried is not graded. Callers that
// must also enforce strict-mode constraints — which fail on baselined violations
// too — pass the current snapshot's findings through EvaluateCurrent instead.
func Evaluate(d *diff.SnapshotDiff, p Policy, measurements ...Measurement) Verdict {
	return EvaluateCurrent(d, p, nil, measurements...)
}

// EvaluateCurrent grades a delta and, additionally, the current snapshot's
// strict-mode constraint violations. currentFindings is the current snapshot's
// FULL findings list: strict constraints are the one policy that opts out of
// the ratchet's delta scoping — a rule declared strict was decided to hold NOW,
// not merely to stop getting worse — so its violations fail whether or not the
// baseline already carried them, unless a ledger entry suppresses them. Every
// other finding in currentFindings is ignored; the delta remains the frame for
// everything the ratchet grades.
func EvaluateCurrent(d *diff.SnapshotDiff, p Policy, currentFindings []facts.Insight, measurements ...Measurement) Verdict {
	v := Verdict{Policy: p.resolved(), Diff: d}
	if d == nil {
		v.Status = StatusUsageError
		return v
	}

	v.ComparabilityWarnings = d.Comparability.Warnings
	for _, k := range d.Comparability.Kinds {
		switch {
		case blockingKinds[k]:
			v.BlockingKinds = append(v.BlockingKinds, k)
		case advisoryKinds[k]:
			v.AdvisoryKinds = append(v.AdvisoryKinds, k)
		}
	}

	graded := map[string]bool{}
	for _, in := range d.FindingsNew {
		graded[in.Title] = true
		switch {
		case in.Informational:
			v.Descriptive = append(v.Descriptive, in)
		case exemptedFinding(in):
			v.Exempted = append(v.Exempted, in)
		case p.suppressed(in):
			v.Suppressed = append(v.Suppressed, in)
		// A strict violation fails without consulting the explainer allow-list:
		// strict is declared per rule, and a --fail-on override that dropped
		// constraints must not quietly soften what a declaration said is law.
		case strictFinding(in) || p.fails(in):
			v.Failures = append(v.Failures, in)
		default:
			v.Advisories = append(v.Advisories, in)
		}
	}
	// Strict pass, after the delta so a strict violation that IS new is graded
	// once under its delta identity. currentFindings preserves snapshot order,
	// which the explainer already sorts, so the appended failures are as
	// deterministic as the delta's.
	for _, in := range currentFindings {
		if !strictFinding(in) || graded[in.Title] {
			continue
		}
		graded[in.Title] = true
		if p.suppressed(in) {
			v.Suppressed = append(v.Suppressed, in)
		} else {
			v.Failures = append(v.Failures, in)
		}
	}
	for _, in := range currentFindings {
		if !exemptedFinding(in) || graded[in.Title] {
			continue
		}
		graded[in.Title] = true
		v.Exempted = append(v.Exempted, in)
	}
	v.Resolved = d.FindingsResolved
	v.Silenced = d.FindingsSilenced
	v.Undeclared = withoutExempted(d.FindingsUndeclared, v.Exempted)
	for _, in := range d.FindingsDeclared {
		if graded[in.Title] {
			continue
		}
		graded[in.Title] = true
		switch {
		case exemptedFinding(in):
			v.Exempted = append(v.Exempted, in)
		case p.suppressed(in):
			v.Suppressed = append(v.Suppressed, in)
		default:
			v.Declared = append(v.Declared, in)
		}
	}
	v.Unattributed = withoutExempted(d.FindingsUnattributed, v.Exempted)
	v.Incidental = append(append([]facts.Insight{}, d.FindingsNewIncidental...), d.FindingsResolvedIncidental...)
	if len(v.Incidental) == 0 {
		v.Incidental = nil
	}

	v.EdgesAdded, v.EdgesRemoved = len(d.EdgesAdded), len(d.EdgesRemoved)
	v.FactsAdded, v.FactsRemoved = len(d.FactsAdded), len(d.FactsRemoved)
	v.FactsChanged = len(d.FactsChanged)
	v.AddedByKind = diff.KindCounts(d.FactsAdded)
	v.RemovedByKind = diff.KindCounts(d.FactsRemoved)
	v.EdgeKindsAdded = edgeKindCounts(d.EdgesAdded)
	v.EdgeKindsRemoved = edgeKindCounts(d.EdgesRemoved)
	v.FindingsChanged = d.FindingsChanged

	// Measurements are carried whether or not a threshold gates them: a caller that
	// measured something and got silence cannot tell "under the bound" from "never
	// looked", and that ambiguity is the same one the incidental bucket exists to avoid.
	v.Measurements = measurements
	fatalBreach := false
	for _, m := range measurements {
		t, ok := p.thresholdFor(m.Name)
		if !ok {
			continue
		}
		switch {
		case t.FailAt > 0 && m.Count >= t.FailAt:
			v.Breaches = append(v.Breaches, Breach{Measurement: m, Fatal: true})
			fatalBreach = true
		case t.WarnAt > 0 && m.Count >= t.WarnAt:
			v.Breaches = append(v.Breaches, Breach{Measurement: m})
		}
	}

	switch {
	case len(v.BlockingKinds) > 0:
		v.Status = StatusIncomparable
	case d.Comparability.HasKind(diff.WarnInvertedPair):
		v.Status = StatusUsageError
	case (len(v.Failures) > 0 || fatalBreach) && !p.WarnOnly:
		v.Status = StatusRegression
	default:
		v.Status = StatusClean
	}
	return v
}
