package diff

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// layerViolation is a declared-layer finding: every violation of one layer pair shares
// the title, and only the evidence tells them apart.
func layerViolation(evidence ...string) facts.Insight {
	in := facts.Insight{Source: "layers", Confidence: 1.0, Title: "Layer violation: store -> delivery (declared)"}
	for _, e := range evidence {
		in.Evidence = append(in.Evidence, facts.Evidence{Fact: e})
	}
	return in
}

// A new violation of a layer pair the baseline already violates twice must be new, not
// incidental, whatever its evidence sorts like. Its evidence ("notify", "store/cache")
// sorts before both old ones, which used to shift the positional pairing by one: the new
// finding paired with an old one, and the last old one, citing nothing the change
// touched, was left over and filed as incidental, so `--fail-on=layers` passed.
func TestCompute_NewMemberOfViolatedGroupIsNew(t *testing.T) {
	oldNotify := layerViolation("store/legacy", "notify")
	oldWeb := layerViolation("store/legacy", "web")
	added := layerViolation("store/cache", "notify")

	base := snap(
		[]facts.Fact{mod("store/legacy", "store/legacy.go", "notify", "web"), mod("notify", "notify/n.go"), mod("web", "web/w.go")},
		[]facts.Insight{oldNotify, oldWeb},
	)
	cur := snap(
		[]facts.Fact{
			mod("store/legacy", "store/legacy.go", "notify", "web"), mod("notify", "notify/n.go"), mod("web", "web/w.go"),
			mod("store/cache", "store/cache/c.go", "notify"),
		},
		[]facts.Insight{oldNotify, oldWeb, added},
	)

	d := Compute(base, cur)

	if len(d.FindingsNew) != 1 || sortedEvidenceEntities(d.FindingsNew[0]) != sortedEvidenceEntities(added) {
		t.Fatalf("FindingsNew = %+v, want only the store/cache violation", d.FindingsNew)
	}
	if len(d.FindingsNewIncidental) != 0 {
		t.Errorf("FindingsNewIncidental = %+v, want none", d.FindingsNewIncidental)
	}
	if len(d.FindingsChanged) != 0 {
		t.Errorf("FindingsChanged = %+v, want none: both old violations are unchanged", d.FindingsChanged)
	}
}

// The mirror case: removing one violation of a pair that keeps others resolves that
// one, and leaves the survivors unchanged.
func TestCompute_RemovedMemberOfViolatedGroupIsResolved(t *testing.T) {
	gone := layerViolation("store/cache", "notify")
	keptNotify := layerViolation("store/legacy", "notify")
	keptWeb := layerViolation("store/legacy", "web")

	base := snap(
		[]facts.Fact{
			mod("store/legacy", "store/legacy.go", "notify", "web"), mod("notify", "notify/n.go"), mod("web", "web/w.go"),
			mod("store/cache", "store/cache/c.go", "notify"),
		},
		[]facts.Insight{gone, keptNotify, keptWeb},
	)
	cur := snap(
		[]facts.Fact{mod("store/legacy", "store/legacy.go", "notify", "web"), mod("notify", "notify/n.go"), mod("web", "web/w.go")},
		[]facts.Insight{keptNotify, keptWeb},
	)

	d := Compute(base, cur)

	if len(d.FindingsResolved) != 1 || sortedEvidenceEntities(d.FindingsResolved[0]) != sortedEvidenceEntities(gone) {
		t.Fatalf("FindingsResolved = %+v, want only the store/cache violation", d.FindingsResolved)
	}
	if len(d.FindingsResolvedIncidental)+len(d.FindingsChanged) != 0 {
		t.Errorf("incidental %+v, changed %+v: want none", d.FindingsResolvedIncidental, d.FindingsChanged)
	}
}

func TestPairInsightGroup(t *testing.T) {
	a, b, c := layerViolation("a"), layerViolation("b"), layerViolation("c")
	cases := []struct {
		name         string
		base, cur    []facts.Insight
		curToBase    []int
		unpairedBase []int
	}{
		{"identical", []facts.Insight{a, b}, []facts.Insight{a, b}, []int{0, 1}, nil},
		{"one added in front", []facts.Insight{b, c}, []facts.Insight{a, b, c}, []int{-1, 0, 1}, nil},
		{"one removed in front", []facts.Insight{a, b, c}, []facts.Insight{b, c}, []int{1, 2}, []int{0}},
		// A lone finding whose evidence moved is still the same finding.
		{"lone member moved", []facts.Insight{a}, []facts.Insight{b}, []int{0}, nil},
		{"all new", nil, []facts.Insight{a, b}, []int{-1, -1}, nil},
		{"all gone", []facts.Insight{a, b}, nil, []int{}, []int{0, 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := pairInsightGroup(tc.base, tc.cur)
			if len(p.curToBase) != len(tc.curToBase) {
				t.Fatalf("curToBase = %v, want %v", p.curToBase, tc.curToBase)
			}
			for i := range tc.curToBase {
				if p.curToBase[i] != tc.curToBase[i] {
					t.Fatalf("curToBase = %v, want %v", p.curToBase, tc.curToBase)
				}
			}
			if len(p.unpairedBase) != len(tc.unpairedBase) {
				t.Fatalf("unpairedBase = %v, want %v", p.unpairedBase, tc.unpairedBase)
			}
			for i := range tc.unpairedBase {
				if p.unpairedBase[i] != tc.unpairedBase[i] {
					t.Fatalf("unpairedBase = %v, want %v", p.unpairedBase, tc.unpairedBase)
				}
			}
		})
	}
}
