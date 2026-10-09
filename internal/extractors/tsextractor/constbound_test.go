package tsextractor

import "testing"

// A loop fixed where the code is written raises loop_depth and no scaling depth.
func TestTsConstantLoopsAddNoScalingDepth(t *testing.T) {
	cases := map[string]string{
		"all-caps constant": `export function r(rows) {
  for (const row of rows) { for (const p of PACKAGES) { use(row, p) } }
}`,
		"member constant": `export function r(rows) {
  for (const row of rows) { Config.STOP_WORDS.forEach(w => use(row, w)) }
}`,
		"view of a constant": `export function r(rows) {
  for (const row of rows) { for (const [k, v] of Object.entries(FONT_FAMILY)) { use(row, k, v) } }
}`,
		"literal bound": `export function r(rows) {
  for (const row of rows) { for (let i = 0; i < 4; i++) { use(row, i) } }
}`,
		"arithmetic over constants": `export function r(rows) {
  for (const row of rows) { for (let x = 0; x < GRID_COLUMNS - 8; x++) { use(row, x) } }
}`,
		"local literal": `export function r(rows) {
  const prefixes = ["a/", "b/", "c/"];
  for (const row of rows) { if (prefixes.some(p => row.startsWith(p))) { use(row) } }
}`,
	}
	for name, src := range cases {
		f := tsExtractFunc(t, src, "src.r")
		if got := tsIntProp(t, f, "loop_depth"); got != 2 {
			t.Errorf("%s: loop_depth = %d, want 2", name, got)
		}
		if got := tsIntProp(t, f, "scaling_loop_depth"); got != 1 {
			t.Errorf("%s: scaling_loop_depth = %d, want 1", name, got)
		}
	}
}

// A name that only looks constant keeps counting.
func TestTsLoopsThatOnlyLookConstantStillScale(t *testing.T) {
	cases := map[string]string{
		"mixed-case name":          `export function r(rows, packages) { for (const row of rows) { for (const p of packages) { use(row, p) } } }`,
		"bound on a length":        `export function r(rows, xs) { for (const row of rows) { for (let i = 0; i < xs.length; i++) { use(row, i) } } }`,
		"compound condition":       `export function r(rows) { for (const row of rows) { for (let i = 0; i < 4 && more(row); i++) { use(row, i) } } }`,
		"local that is pushed to":  `export function r(rows) { const seen = []; for (const row of rows) { seen.push(row); for (const s of seen) { use(row, s) } } }`,
		"local that is a let":      `export function r(rows) { let acc = [1]; acc = load(); for (const row of rows) { for (const a of acc) { use(row, a) } } }`,
		"descending from a length": `export function r(rows, xs) { for (const row of rows) { for (let i = xs.length - 1; i >= 0; i--) { use(row, i) } } }`,
		"single capital":           `export function r(rows, N) { for (const row of rows) { for (let i = 0; i < N; i++) { use(row, i) } } }`,
	}
	for name, src := range cases {
		f := tsExtractFunc(t, src, "src.r")
		if got := tsIntProp(t, f, "scaling_loop_depth"); got != 2 {
			t.Errorf("%s: scaling_loop_depth = %d, want 2", name, got)
		}
	}
}

// A call inside a constant loop runs a fixed number of times.
func TestTsCallInConstantLoopIsNotAnNPlusOneCandidate(t *testing.T) {
	f := tsExtractFunc(t, `export function r() {
  for (const p of PACKAGES) { publish(p) }
}`, "src.r")
	if cil := tsStrSlice(f, "calls_in_scaling_loop"); tsContains(cil, "src.publish") {
		t.Errorf("calls_in_scaling_loop = %v: publish runs once per entry of a constant", cil)
	}
}
