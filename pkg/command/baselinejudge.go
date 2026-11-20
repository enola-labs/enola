package command

import (
	"github.com/enola-labs/enola/internal/diff"
	"github.com/enola-labs/enola/internal/engine"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/pkg/bootstrap"
	"github.com/enola-labs/enola/pkg/check"
)

// baselineState is what the metadata on disk says about a pinned baseline, before
// anything is parsed: whether `enola check` could grade against it.
type baselineState string

const (
	// baselineAbsent: nothing is pinned, so there is nothing to grade against.
	baselineAbsent baselineState = "absent"
	// baselineComparable: no blocking comparability warning from the metadata.
	baselineComparable baselineState = "comparable"
	// baselineNotComparable: the gate would decline; Reason names the kinds.
	baselineNotComparable baselineState = "not_comparable"
	// baselineUnknown: a baseline exists but this tree could not be described.
	baselineUnknown baselineState = "unknown"
)

// baselineJudgement is the one answer doctor and `baseline show` give about a pinned
// baseline. Reason is empty only for a comparable baseline.
type baselineJudgement struct {
	State  baselineState
	Reason string
}

// judgeBaseline answers "can the gate grade against the baseline in baseDir?" for
// repoPaths, from metadata alone: no files are parsed, so a report stays a report.
// It is the verdict `enola check` reaches minus what only a parse can show (facts that
// moved), with the member set read from the baseline's members.json, so a cluster
// baseline that measured a repository the config no longer names is declined here as
// the gate declines it.
func judgeBaseline(eng *bootstrap.Engine, baseDir string, repoPaths []string) baselineJudgement {
	base, err := readSnapshotMeta(baseDir)
	if err != nil {
		return baselineJudgement{State: baselineAbsent, Reason: "no baseline pinned"}
	}
	membership := membershipComparability(baseDir, repoPaths)
	kinds := check.BlockingKinds(membership)
	for _, repoPath := range repoPaths {
		meta := base
		if len(repoPaths) > 1 {
			// A member the baseline never measured has no meta to compare; the gate
			// treats a member that joined as growth, not as a reason to decline.
			if meta, err = engine.ReadMemberMeta(baseDir, repoPath); err != nil {
				continue
			}
		}
		current := eng.CurrentMeta(repoPath)
		if current == nil {
			return baselineJudgement{State: baselineUnknown, Reason: repoPath + " could not be described"}
		}
		kinds = appendKinds(kinds, metaBlockingKinds(*meta, *current))
	}
	if len(kinds) == 0 {
		return baselineJudgement{State: baselineComparable}
	}
	v := check.Evaluate(&diff.SnapshotDiff{Comparability: diff.Comparability{Kinds: kinds}}, check.Policy{})
	reason := v.DeclineReason()
	if len(membership.Warnings) > 0 {
		reason += ": " + membership.Warnings[0]
	}
	return baselineJudgement{State: baselineNotComparable, Reason: reason}
}

// metaBlockingKinds is the gate's blocking classification of two metas. The provider
// set is left out: a current meta describes a run that has not happened, so it cannot
// say which providers will run, and an equal config hash already proves the same
// providers are configured.
func metaBlockingKinds(base, current facts.SnapshotMeta) []diff.WarningKind {
	var out []diff.WarningKind
	for _, kind := range check.BlockingKinds(diff.CompareMeta(base, current)) {
		if kind != diff.WarnProviderSet {
			out = append(out, kind)
		}
	}
	return out
}

func appendKinds(kinds, more []diff.WarningKind) []diff.WarningKind {
	for _, k := range more {
		seen := false
		for _, have := range kinds {
			seen = seen || have == k
		}
		if !seen {
			kinds = append(kinds, k)
		}
	}
	return kinds
}
