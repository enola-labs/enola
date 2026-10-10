package rubyextractor

import (
	sitter "github.com/tree-sitter/go-tree-sitter"
)

// This file is what the Ruby walker knows about a loop beyond that it is one:
// whether it adds a factor of the input, and whether a call in it is made once per
// element. loop_depth already leaves out a loop over a literal or a constant. The
// rest was missing, so a call-in-loop finding on Ruby took its exponent from the
// function's deepest nest, wherever the call sat, and a parent-then-children walk
// read as quadratic.

// rubyBatchIterators yield a batch of the receiver per round, so a call made
// directly in the block is the batched call: one per batch is what batching is.
// find_in_batches and in_batches are not here because they are not counted as
// loops at all (see rubyIterators).
var rubyBatchIterators = map[string]bool{
	"each_slice": true, "in_groups_of": true, "in_groups": true,
}

// rubyLoop is one enclosing loop of the walk.
type rubyLoop struct {
	vars  []string // what the loop binds per round: block parameters, a `for` variable
	batch bool     // a batch iterator: its own calls run once per batch
}

// pushLoop enters a loop. scales says whether it adds a factor of the input: a
// loop over what an enclosing loop bound does not, since the nest as a whole
// visits each element once.
func (w *rubyWalker) pushLoop(l rubyLoop, scales bool) {
	w.loops = append(w.loops, l)
	w.loopScales = append(w.loopScales, scales)
	if !scales {
		return
	}
	w.scalingDepth++
	if w.metrics != nil && w.scalingDepth > w.metrics.scalingLoopDepth {
		w.metrics.scalingLoopDepth = w.scalingDepth
	}
}

func (w *rubyWalker) popLoop() {
	n := len(w.loops) - 1
	if w.loopScales[n] {
		w.scalingDepth--
	}
	w.loops, w.loopScales = w.loops[:n], w.loopScales[:n]
}

// rootedAtLoopVar reports whether an expression is reached through what an
// enclosing loop bound: `post.comments`, `batch`, `row[:items].compact`.
func (w *rubyWalker) rootedAtLoopVar(n *sitter.Node) bool {
	for n != nil {
		switch n.Kind() {
		case "identifier":
			name := rubyText(n, w.src)
			for _, l := range w.loops {
				for _, v := range l.vars {
					if v == name {
						return true
					}
				}
			}
			return false
		case "call":
			n = n.ChildByFieldName("receiver")
		case "element_reference":
			n = n.ChildByFieldName("object")
		case "parenthesized_statements":
			n = n.NamedChild(0)
		default:
			return false
		}
	}
	return false
}

// blockParamNames returns every name a block binds: `|key, (a, b)|` is three.
func blockParamNames(block *sitter.Node, src []byte) []string {
	if block == nil {
		return nil
	}
	var out []string
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil {
			return
		}
		if n.Kind() == "identifier" {
			out = append(out, rubyText(n, src))
			return
		}
		for i := uint(0); i < n.NamedChildCount(); i++ {
			walk(n.NamedChild(i))
		}
	}
	walk(block.ChildByFieldName("parameters"))
	return out
}

// recordScalingCall notes a call made inside a loop that repeats, with the
// nesting of the scaling loops around it. A call made directly in a batch
// iterator's block is left out: it runs once per batch.
func (w *rubyWalker) recordScalingCall(target string) {
	if n := len(w.loops); n > 0 && w.loops[n-1].batch {
		return
	}
	m := w.metrics
	if m.scalingCallDepth == nil {
		m.scalingCallDepth = make(map[string]int)
	}
	if d, seen := m.scalingCallDepth[target]; !seen {
		m.callsInScalingLoop = append(m.callsInScalingLoop, target)
		m.scalingCallDepth[target] = w.scalingDepth
	} else if w.scalingDepth > d {
		m.scalingCallDepth[target] = w.scalingDepth
	}
}
