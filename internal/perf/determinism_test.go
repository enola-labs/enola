package perf

import (
	"testing"
)

// A call CYCLE must produce the same compounded depths on every run.
//
// computeEffectiveDepths cuts cycles by returning the in-progress function's own scaling
// depth, so the memoised result depends on which member the DFS entered from. Ranging
// over the function map directly made that entry point vary per run: a mutually-recursive
// function was labelled O(n³) on one run and O(n³+) on the next, from an unchanged tree.
// insights.json stopped being byte-stable on 7 of 38 corpus repositories.
//
// Run repeatedly because Go randomises map iteration order per range: a single pass can
// pass by luck.
func TestComputeEffectiveDepths_CycleIsDeterministic(t *testing.T) {
	// a -> b -> c -> a, each looping, so every member sees the others through the cycle.
	funcs := map[string]funcInfo{
		"a": {Name: "a", LoopDepth: 1, CallsInLoop: []string{"b"}},
		"b": {Name: "b", LoopDepth: 1, CallsInLoop: []string{"c"}},
		"c": {Name: "c", LoopDepth: 1, CallsInLoop: []string{"a"}},
		// A non-cyclic caller, to prove the fix does not simply flatten everything.
		"d": {Name: "d", LoopDepth: 1, CallsInLoop: []string{"a"}},
	}

	first := computeEffectiveDepths(funcs)
	for i := 0; i < 200; i++ {
		got := computeEffectiveDepths(funcs)
		for name, want := range first {
			if got[name] != want {
				t.Fatalf("run %d: depth for %q = %d, want %d — entry order is leaking into the result",
					i, name, got[name], want)
			}
		}
	}

	// The acyclic caller must still compound over the cycle rather than being zeroed.
	if first["d"] <= 1 {
		t.Errorf("depth for the acyclic caller = %d, want > 1 — determinism must not cost the compounding", first["d"])
	}
}

// Mutual recursion between two functions is the shape seen in the field (an AST walker
// calling a helper that calls back into it), and the one that actually drifted.
func TestComputeEffectiveDepths_MutualRecursionIsStable(t *testing.T) {
	funcs := map[string]funcInfo{
		"walkFileScopeCalls": {Name: "walkFileScopeCalls", LoopDepth: 2, CallsInLoop: []string{"walkTypeBody"}},
		"walkTypeBody":       {Name: "walkTypeBody", LoopDepth: 1, CallsInLoop: []string{"walkFileScopeCalls"}},
	}

	first := computeEffectiveDepths(funcs)
	for i := 0; i < 200; i++ {
		got := computeEffectiveDepths(funcs)
		for name, want := range first {
			if got[name] != want {
				t.Fatalf("run %d: %q = %d, want %d", i, name, got[name], want)
			}
		}
	}
}
