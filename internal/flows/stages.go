package flows

import (
	"fmt"
	"strings"
)

const (
	// stageDepth and stageFanout bound the stage tree: three levels of at most
	// four steps is what a reader takes in as the shape of a flow. The rest of
	// the reach is in the metrics.
	stageDepth  = 3
	stageFanout = 4
	// stageShare is the least a callee must add, as a share of the flow, to be
	// a stage. Below it a callee is a helper the flow uses, not a step it takes.
	// In a flow of a few dozen symbols the share rounds to nothing and every
	// callee that reaches anything is a step.
	stageShare = 0.05
	stageFloor = 1
)

// stages builds a flow's stage tree, in pre-order.
//
// Under each step the callees are taken greedily by what they add: the one
// reaching the most symbols first, then the one reaching the most that the
// first did not. What a step adds is its share of the flow, and its own steps
// divide that share between them, so the numbers down a branch never exceed
// the number above them.
//
// A callee is measured with every step already placed removed from the graph,
// because calls loop. Measured plainly, a helper that calls back into the
// flow's entry reaches the whole flow and outranks the step that does the work.
func (a *Analysis) stages(f *Flow) []Stage {
	threshold := int(stageShare * float64(len(f.reach)))
	if threshold < stageFloor {
		threshold = stageFloor
	}
	placed := map[int32]bool{f.entry: true}
	var out []Stage

	type step struct {
		id    int32
		share map[int32]bool
	}
	var expand func(parent int32, parentID string, share map[int32]bool, depth int)
	expand = func(parent int32, parentID string, share map[int32]bool, depth int) {
		type candidate struct {
			id    int32
			reach []int32
		}
		var candidates []candidate
		a.expand(parent)
		for _, callee := range a.nodes[parent].callees {
			if placed[callee] || !share[callee] {
				continue
			}
			walked, _, _ := a.walk(callee, placed)
			reach := walked[:0]
			for _, id := range walked {
				if share[id] {
					reach = append(reach, id)
				}
			}
			candidates = append(candidates, candidate{callee, reach})
		}
		covered := map[int32]bool{}
		var chosen []step
		for len(chosen) < stageFanout {
			best, bestGain := -1, 0
			for i, c := range candidates {
				if placed[c.id] {
					continue
				}
				gain := 0
				for _, id := range c.reach {
					if !covered[id] {
						gain++
					}
				}
				// Candidates are in name order, so a tie keeps the first.
				if gain > bestGain {
					best, bestGain = i, gain
				}
			}
			if best < 0 || bestGain < threshold {
				break
			}
			c := candidates[best]
			placed[c.id] = true
			own := make(map[int32]bool, bestGain)
			for _, id := range c.reach {
				if !covered[id] {
					covered[id] = true
					own[id] = true
				}
			}
			chosen = append(chosen, step{c.id, own})
			out = append(out, Stage{Depth: depth, Parent: parentID, Ref: a.ref(c.id), Reach: bestGain})
		}
		if depth >= stageDepth {
			return
		}
		// Recursing after the whole level is placed keeps a step's children from
		// claiming a sibling. The steps were appended level by level, so they are
		// put back in pre-order below.
		for _, s := range chosen {
			expand(s.id, a.ref(s.id).ID, s.share, depth+1)
		}
	}
	whole := make(map[int32]bool, len(f.reach))
	for _, id := range f.reach {
		whole[id] = true
	}
	expand(f.entry, "", whole, 1)

	out = preorder(out)
	for i := range out {
		out[i].Order = i + 1
	}
	return out
}

// preorder rearranges stages appended level by level into tree order, keeping
// the order siblings were chosen in.
func preorder(stages []Stage) []Stage {
	children := map[string][]Stage{}
	for _, s := range stages {
		children[s.Parent] = append(children[s.Parent], s)
	}
	out := make([]Stage, 0, len(stages))
	var visit func(parent string)
	visit = func(parent string) {
		for _, s := range children[parent] {
			out = append(out, s)
			visit(s.ID)
		}
	}
	visit("")
	return out
}

// summarize writes the paragraph that describes a flow, from its own fields and
// nothing else: the same flow always reads the same.
func summarize(f *Flow) string {
	var b strings.Builder
	entry := ShortName(f.Entry.Name)
	switch f.Mechanism {
	case MechanismRoute:
		fmt.Fprintf(&b, "HTTP %s enters at %s", f.Name, entry)
	case MechanismMain:
		fmt.Fprintf(&b, "The program starts at %s", entry)
	case MechanismRegistered:
		fmt.Fprintf(&b, "The framework invokes %s", entry)
	default:
		fmt.Fprintf(&b, "A %s enters at %s", f.Mechanism, entry)
	}
	if f.Entry.Doc != "" {
		fmt.Fprintf(&b, " (%q)", f.Entry.Doc)
	}
	b.WriteString(".")

	var top []Stage
	for _, s := range f.Stages {
		if s.Depth == 1 {
			top = append(top, s)
		}
	}
	switch {
	case len(top) == 1:
		fmt.Fprintf(&b, " It hands off to %s", ShortName(top[0].Name))
		var next []string
		for _, s := range f.Stages {
			if s.Depth == 2 {
				next = append(next, ShortName(s.Name))
			}
		}
		if len(next) > 0 {
			fmt.Fprintf(&b, ", which calls %s", joinAnd(next))
		}
		b.WriteString(".")
	case len(top) > 1:
		names := make([]string, len(top))
		for i, s := range top {
			names[i] = ShortName(s.Name)
		}
		fmt.Fprintf(&b, " It calls %s.", joinAnd(names))
	}

	if len(f.Modules) > 1 {
		shown := f.Modules
		if len(shown) > 3 {
			shown = shown[:3]
		}
		parts := make([]string, len(shown))
		for i, m := range shown {
			switch {
			case i == 0 && m.Symbols == 1:
				parts[i] = fmt.Sprintf("%s (1 symbol)", m.Name)
			case i == 0:
				parts[i] = fmt.Sprintf("%s (%d symbols)", m.Name, m.Symbols)
			default:
				parts[i] = fmt.Sprintf("%s (%d)", m.Name, m.Symbols)
			}
		}
		fmt.Fprintf(&b, " Most of its %d symbols sit in %s.", f.Metrics.Reach, joinAnd(parts))
	}
	return b.String()
}

// ShortName drops the directory part of a path-qualified symbol name:
// `pkg/api/recall.recall` reads as `recall.recall`. A name with no slash, as
// Ruby's and PHP's are, is returned whole.
func ShortName(name string) string {
	if i := strings.LastIndex(name, "/"); i >= 0 {
		return name[i+1:]
	}
	return name
}

func joinAnd(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}
