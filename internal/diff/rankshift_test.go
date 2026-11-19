package diff

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// caller is a symbol calling each of targets.
func caller(name string, targets ...string) facts.Fact {
	f := sym(name, name+".go", 1)
	for _, t := range targets {
		f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelCalls, Target: t})
	}
	return f
}

func hotspot(name string) facts.Insight {
	return facts.Insight{Source: hotspotsExplainerName, Title: "Call-graph hotspot: " + name, Confidence: 0.7,
		Evidence: []facts.Evidence{
			{Symbol: name, File: name + ".go", Detail: "score"},
			{Symbol: "neighbour", Detail: "calls into " + name},
		}}
}

// A hotspot that enters the list with its own fan-in and fan-out unchanged entered it
// because another left: an incidental shift, though the change touched a neighbour it
// cites. One whose own degree changed is new.
func TestRankShiftIsIncidental(t *testing.T) {
	// hub is called by a, b and neighbour, and calls x, y, z. neighbour is touched (it
	// gains a call elsewhere); hub's degrees are the same on both sides.
	base := []facts.Fact{
		caller("a", "hub"), caller("b", "hub"), caller("neighbour", "hub"),
		caller("hub", "x", "y", "z"), sym("x", "x.go", 1), sym("y", "y.go", 1), sym("z", "z.go", 1), sym("w", "w.go", 1),
	}
	cur := append([]facts.Fact(nil), base...)
	cur[2] = caller("neighbour", "hub", "w")

	d := Compute(snap(base, nil), snap(cur, []facts.Insight{hotspot("hub")}))
	if len(d.FindingsNew) != 0 || len(d.FindingsNewIncidental) != 1 {
		t.Errorf("re-ranked hotspot: new %v, incidental %v; want it incidental", titles(d.FindingsNew), titles(d.FindingsNewIncidental))
	}

	// Now hub gains a caller: its own score moved.
	cur2 := append(append([]facts.Fact(nil), cur...), caller("c", "hub"))
	d = Compute(snap(base, nil), snap(cur2, []facts.Insight{hotspot("hub")}))
	if len(d.FindingsNew) != 1 {
		t.Errorf("hotspot whose fan-in grew: new %v, incidental %v; want it new", titles(d.FindingsNew), titles(d.FindingsNewIncidental))
	}

	// The same on the way out: unchanged degrees, left the list, incidental.
	d = Compute(snap(base, []facts.Insight{hotspot("hub")}), snap(cur, nil))
	if len(d.FindingsResolved) != 0 || len(d.FindingsResolvedIncidental) != 1 {
		t.Errorf("hotspot that left the list unchanged: resolved %v, incidental %v", titles(d.FindingsResolved), titles(d.FindingsResolvedIncidental))
	}
}

// Only hotspots are judged by their own measure; another explainer's finding citing a
// touched fact keeps the structural-cause test.
func TestRankShiftLeavesOtherExplainersAlone(t *testing.T) {
	base := []facts.Fact{caller("a", "hub"), caller("hub", "x"), sym("x", "x.go", 1)}
	cur := []facts.Fact{caller("a", "hub", "x"), caller("hub", "x"), sym("x", "x.go", 1)}
	in := hotspot("hub")
	in.Source = "god-class"
	in.Evidence = []facts.Evidence{{Symbol: "a", File: "a.go"}}
	d := Compute(snap(base, nil), snap(cur, []facts.Insight{in}))
	if len(d.FindingsNew) != 1 {
		t.Errorf("a god-class finding citing a touched symbol: new %v, incidental %v", titles(d.FindingsNew), titles(d.FindingsNewIncidental))
	}
}

func titles(ins []facts.Insight) []string {
	out := make([]string, len(ins))
	for i, in := range ins {
		out[i] = in.Title
	}
	return out
}
