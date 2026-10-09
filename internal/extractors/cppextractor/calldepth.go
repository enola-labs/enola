package cppextractor

// Per-call loop depth: how deep, counting only loops that scale with the input, is
// the deepest loop each in-loop call sits in.
//
// scaling_loop_depth is one number for the whole function, its deepest nest. A
// call-in-loop finding needs the depth of the loop the CALL is in, and the two
// differ whenever the function has a deeper nest somewhere else:
//
//	for (row of rows) { await save(row) }          // the call: depth 1
//	for (a of xs) { for (b of ys) { total += 1 } } // the function: depth 2
//
// Reporting the function's depth for the call printed one query per row as O(n²).

// noteCallDepth records that target is called at the given scaling depth, keeping
// the deepest seen. A depth of 0 (a call only ever inside a `while (true)`, which
// repeats without scaling) is the map's zero value and needs no entry.
func noteCallDepth(m map[string]int, target string, depth int) map[string]int {
	if depth <= m[target] {
		return m
	}
	if m == nil {
		m = make(map[string]int)
	}
	m[target] = depth
	return m
}

// callDepths lays the recorded depths out in the order of calls, so the prop is a
// slice parallel to calls_in_scaling_loop and as deterministic as it is.
func callDepths(calls []string, m map[string]int) []int {
	out := make([]int, len(calls))
	for i, c := range calls {
		out[i] = m[c]
	}
	return out
}
