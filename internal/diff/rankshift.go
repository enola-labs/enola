package diff

import "github.com/enola-labs/enola/internal/facts"

// hotspotsExplainerName is the explainer whose findings rankJudge can judge. Named
// here rather than imported, as intentExplainerName is: diff reads findings, it does
// not depend on the passes that produce them.
const hotspotsExplainerName = "hotspots"

// rankJudge tells a finding that entered or left a capped, ranked list because its own
// measure moved from one that did so only because others around it moved.
//
// The structural-cause test asks whether the change touched anything a finding cites,
// and a hotspot cites its neighbours: up to five callers and five callees. A refactor
// that moves code out of a large class touches most of what sits near it, so on a real
// one (a few hundred lines out of a 12,000-line component) two hotspots with exactly
// their old scores were reported as new and one as resolved: three functions tied at
// the cut of a top-20 list, reordered. A hotspot's score is its own architectural
// fan-in times fan-out and nothing else, so the exact question is whether those two
// numbers changed; if neither did, its place in the list moved for someone else.
//
// The graphs are built only when a hotspot finding is to be judged.
type rankJudge struct {
	base, cur *facts.Snapshot
	bg, cg    *facts.Graph
}

// onlyReRanked reports a hotspot finding whose subject has the same fan-in and fan-out
// on both sides. A subject missing on either side was added or removed, which is a
// change of its own.
func (j *rankJudge) onlyReRanked(in facts.Insight) bool {
	if in.Source != hotspotsExplainerName || len(in.Evidence) == 0 || in.Evidence[0].Symbol == "" {
		return false
	}
	if j.base == nil || j.cur == nil {
		return false
	}
	if j.bg == nil {
		j.bg, j.cg = facts.NewGraph(j.base.Facts), facts.NewGraph(j.cur.Facts)
	}
	name := in.Evidence[0].Symbol
	bk, ok := symbolKey(j.bg, j.base.Facts, name)
	if !ok {
		return false
	}
	ck, ok := symbolKey(j.cg, j.cur.Facts, name)
	if !ok {
		return false
	}
	return j.bg.ArchitecturalFanIn(bk) == j.cg.ArchitecturalFanIn(ck) &&
		j.bg.ArchitecturalFanOut(bk) == j.cg.ArchitecturalFanOut(ck)
}

// symbolKey is the graph key of the symbol named name, as the explainer scored it.
func symbolKey(g *facts.Graph, ff []facts.Fact, name string) (string, bool) {
	for i := range ff {
		if ff[i].Kind == facts.KindSymbol && ff[i].Name == name {
			return g.KeyOf(&ff[i]), true
		}
	}
	return "", false
}
