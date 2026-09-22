package goextractor

import "testing"

// Loop shapes reduced from the functions this repository's own high tier reports.
// Each case names the function it came from; the labeled corpus and the reasoning
// behind each verdict live in internal/perf/testdata/high_tier_labels.jsonl.
//
// `want` is the scaling depth the shape HAS. `today` is what the extractor emits for
// it now. Where the two differ, the gap is the over-count the corpus measured, and
// closing it is the work. A case whose want and today agree is a control: it must not
// move.
type scalingCase struct {
	name  string // the shape
	from  string // the real function it was reduced from
	src   string
	fn    string // fact name to read the prop off
	want  int    // the depth the shape actually has
	today int    // the depth the extractor emits now
}

var scalingCorpus = []scalingCase{
	{
		name: "dimensions/hierarchical", from: "internal/providers.pairAcrossProviders",
		fn: "pkg.Walk", want: 1, today: 1,
		src: `package pkg

type Rel struct{ Kind string }
type Fact struct{ Relations []Rel }

func Walk(kept [][]Fact) int {
	n := 0
	for _, ff := range kept {
		for _, f := range ff {
			for _, rel := range f.Relations {
				if rel.Kind != "" {
					n++
				}
			}
		}
	}
	return n
}
`,
	},
	{
		name: "dimensions/unrelated", from: "internal/extractors/goextractor.extractRoutes",
		fn: "pkg.Routes", want: 1, today: 2,
		src: `package pkg

func Routes(decls []string, seedsFor func(string) []string, stmts []string) int {
	n := 0
	for _, d := range decls {
		for _, seed := range seedsFor(d) {
			for _, s := range stmts {
				if seed != "" && s != "" {
					n++
				}
			}
		}
	}
	return n
}
`,
	},
	{
		name: "fixed-bound/const-condition", from: "internal/facts.Graph.buildImpactSummary",
		fn: "pkg.Summary", want: 1, today: 1,
		src: `package pkg

func Summary(byDepth map[int][]string) int {
	n := 0
	for d := 1; d <= 10; d++ {
		for _, s := range byDepth[d] {
			if s != "" {
				n++
			}
		}
	}
	return n
}
`,
	},
	{
		name: "fixed-bound/const-by-local-map", from: "internal/facts.Graph.buildImpactSummary",
		fn: "pkg.Tally", want: 1, today: 2,
		src: `package pkg

func Tally(byDepth map[int][]string) int {
	n := 0
	for d := 1; d <= 10; d++ {
		kinds := map[string]int{}
		for _, s := range byDepth[d] {
			kinds[s]++
		}
		for _, c := range kinds {
			n += c
		}
	}
	return n
}
`,
	},
	{
		name: "fixed-bound/local-literal-slice", from: "internal/facts.Graph.GovernedByPage",
		fn: "pkg.Forms", want: 1, today: 2,
		src: `package pkg

import "strings"

func Forms(files []string, repo string) int {
	n := 0
	for _, f := range files {
		forms := []string{f}
		if t := strings.TrimPrefix(f, repo+"/"); t != f {
			forms = append(forms, t)
		}
		for _, form := range forms {
			if form != "" {
				n++
			}
		}
	}
	return n
}
`,
	},
	{
		name: "fixed-bound/package-level-slice", from: "internal/perf.isBoundedFanout",
		fn: "pkg.Match", want: 1, today: 2,
		src: `package pkg

import "strings"

var keywords = []string{"Query", "Find", "Save", "Exec"}

func Match(calls []string) int {
	n := 0
	for _, c := range calls {
		for _, k := range keywords {
			if strings.Contains(c, k) {
				n++
			}
		}
	}
	return n
}
`,
	},
	{
		name: "fixed-bound/variadic-literal", from: "internal/extractors/dartextractor.childOfKind",
		fn: "pkg.ChildOfKind", want: 1, today: 2,
		src: `package pkg

func ChildOfKind(children []string, kinds ...string) string {
	for _, c := range children {
		for _, k := range kinds {
			if c == k {
				return c
			}
		}
	}
	return ""
}

func Caller(children []string) string { return ChildOfKind(children, "a", "b") }
`,
	},
	{
		name: "monotonic-index", from: "internal/extractors/dotnetextractor.stripLiterals",
		fn: "pkg.Strip", want: 1, today: 2,
		src: `package pkg

func Strip(s string) string {
	b := []byte(s)
	for i := 0; i < len(b); i++ {
		if b[i] != '"' {
			continue
		}
		i++
		for i < len(b) && b[i] != '"' {
			b[i] = ' '
			i++
		}
	}
	return string(b)
}
`,
	},
	{
		name: "partition/const-guarded-scan", from: "internal/facts.graphBuilder.dedup",
		fn: "pkg.Dedup", want: 1, today: 3,
		src: `package pkg

const scanLimit = 16

func Dedup(off []int, order, tgt []int) int {
	dropped := 0
	for s := 0; s+1 < len(off); s++ {
		run := order[off[s]:off[s+1]]
		if len(run) > scanLimit {
			continue
		}
		for i := 1; i < len(run); i++ {
			for j := 0; j < i; j++ {
				if tgt[run[i]] == tgt[run[j]] {
					dropped++
					break
				}
			}
		}
	}
	return dropped
}
`,
	},
	{
		name: "terminal/inner-loop-returns", from: "internal/explainers/importclosure.Graph.Path",
		fn: "pkg.Path", want: 1, today: 3,
		src: `package pkg

func Path(edges map[string][]string, entry, target string) []string {
	parent := map[string]string{entry: ""}
	queue := []string{entry}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, next := range edges[cur] {
			if _, ok := parent[next]; ok {
				continue
			}
			parent[next] = cur
			if next == target {
				var rev []string
				for f := target; f != ""; f = parent[f] {
					rev = append(rev, f)
				}
				return rev
			}
			queue = append(queue, next)
		}
	}
	return nil
}
`,
	},
	// --- controls: these scale, and must keep their depth ---
	{
		name: "scales/nested-full-scan", from: "internal/explainers/intentcheck.claimVerdicts",
		fn: "pkg.Claims", want: 2, today: 2,
		src: `package pkg

type Fact struct{ Kind, Repo string }

func Claims(all []Fact) int {
	n := 0
	for _, f := range all {
		if f.Kind != "claim" {
			continue
		}
		for _, g := range all {
			if g.Repo == f.Repo {
				n++
			}
		}
	}
	return n
}
`,
	},
	{
		name: "scales/all-pairs", from: "pkg/history.reduce",
		fn: "pkg.Reduce", want: 2, today: 2,
		src: `package pkg

func Reduce(candidates []int, reachable func(a, b int) bool) []int {
	redundant := map[int]bool{}
	for _, from := range candidates {
		for _, other := range candidates {
			if from != other && reachable(from, other) {
				redundant[other] = true
			}
		}
	}
	out := candidates[:0]
	for _, c := range candidates {
		if !redundant[c] {
			out = append(out, c)
		}
	}
	return out
}
`,
	},
}

// TestScalingDepthCorpus pins what the extractor emits for each shape TODAY. It is the
// before-picture: the cases where today != want are the over-counts, and a change that
// closes one edits its `today` to match its `want`. A control (want == today) that
// moves is a true positive lost.
func TestScalingDepthCorpus(t *testing.T) {
	for _, tc := range scalingCorpus {
		t.Run(tc.name, func(t *testing.T) {
			ff := extractAll(t, map[string]string{"pkg/x.go": tc.src})
			got := scalingLoopDepthOf(ff, tc.fn)
			if got == -1 {
				t.Fatalf("%s: no symbol %q extracted", tc.from, tc.fn)
			}
			if got != tc.today {
				t.Errorf("%s (%s): scaling_loop_depth = %d, pinned at %d (shape actually has %d)",
					tc.name, tc.from, got, tc.today, tc.want)
			}
		})
	}
}

// TestScalingDepthCorpus_ControlsHoldDepth states the one-directional rule on its own,
// so it fails loudly rather than as one row of the table: a shape that really does
// scale must never be discounted.
func TestScalingDepthCorpus_ControlsHoldDepth(t *testing.T) {
	for _, tc := range scalingCorpus {
		if tc.want != tc.today {
			continue // an over-count, not a control
		}
		t.Run(tc.name, func(t *testing.T) {
			ff := extractAll(t, map[string]string{"pkg/x.go": tc.src})
			if got := scalingLoopDepthOf(ff, tc.fn); got < tc.want {
				t.Errorf("%s (%s): scaling_loop_depth dropped to %d, want at least %d",
					tc.name, tc.from, got, tc.want)
			}
		})
	}
}
