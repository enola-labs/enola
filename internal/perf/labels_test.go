package perf

import (
	"bufio"
	"encoding/json"
	"os"
	"testing"
)

// The labeled corpus behind the precision work on this analyzer.
//
// Every row is a finding the HIGH tier reported for this repository, read by hand and
// judged. `scales` means the nesting really does grow with the size of the input:
// removing the finding would lose a true risk. `bounded` means the exponent counts a
// loop that cannot grow with the input, so the finding is an over-count.
//
// The corpus exists because "the high tier got smaller" is not by itself an
// improvement. A discount that is too aggressive shrinks it the same way a correct one
// does, and nothing else in this repository or in the benchmark harness can tell those
// two apart: bench-arch grades architecture claims and bench-compare grades fact
// counts, neither grades whether a performance finding is true.
//
// The rule the corpus enforces is one-directional. No `scales` row may stop being
// reported. `bounded` rows are the budget: they are what the work is allowed to remove.
type label struct {
	Symbol  string `json:"symbol"`
	File    string `json:"file"`
	Line    int    `json:"line"`
	Kind    string `json:"kind"`
	BigO    string `json:"big_o"`
	Verdict string `json:"verdict"`
	Class   string `json:"class"`
	Why     string `json:"why"`
	// Sample says which round produced the row. "high-tier" is the original sample of
	// what the analyzer reported; "removed" is a second sample, drawn from the
	// findings a later change stopped reporting and read the same way. The second
	// exists because a cut has to be audited from the other side: the first sample
	// shows what SHOULD go, and only the second shows what actually went.
	Sample string `json:"sample"`
}

// Counts at the time of labeling, against enola at main d82f0343: 250 high findings,
// of which 35 were sampled across all three kinds and every package that produced one.
// 3 of 35 survived scrutiny. Pinned so the sample cannot be edited without the headline
// moving with it.
const (
	labeledTotal   = 45
	labeledScales  = 3
	labeledBounded = 42
	// Of the 250 high findings, 35 were sampled and read. Of the 170 that later
	// stopped being high, 10 were sampled and read; all ten were over-counts.
	labeledHighTier = 35
	labeledRemoved  = 10
)

// knownClasses are the over-count shapes the sample found. A `bounded` row must name
// one: an unexplained over-count is a row nobody has actually read.
var knownClasses = map[string]bool{
	"dimensions":           true, // nested loops over unrelated collections; the product is not the input size
	"fixed-bound":          true, // a loop bounded by a constant, a literal collection or a variadic passed literals
	"monotonic-index":      true, // nested loops sharing one cursor that only advances
	"partition":            true, // outer over groups, inner over that one group's members
	"explicit-cap":         true, // the code guards its own traversal size and returns early
	"terminal":             true, // the inner loop runs once in total because its block returns
	"pure-call":            true, // call-in-loop on a call that does no I/O
	"tree-walk":            true, // recursion reached through a loop over child nodes
	"supervisor":           true, // a bare for{} retry loop, not a per-item N+1
	"compound-drops-bound": true, // the local loop is bounded; compounding across the call graph ignored that
}

func loadLabels(t *testing.T) []label {
	t.Helper()
	f, err := os.Open("testdata/high_tier_labels.jsonl")
	if err != nil {
		t.Fatalf("open corpus: %v", err)
	}
	defer func() { _ = f.Close() }()

	var out []label
	s := bufio.NewScanner(f)
	for s.Scan() {
		if len(s.Bytes()) == 0 {
			continue
		}
		var l label
		if err := json.Unmarshal(s.Bytes(), &l); err != nil {
			t.Fatalf("line %d: %v", len(out)+1, err)
		}
		out = append(out, l)
	}
	if err := s.Err(); err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	return out
}

func TestLabelCorpus_ShapeAndCounts(t *testing.T) {
	labels := loadLabels(t)
	if len(labels) != labeledTotal {
		t.Fatalf("corpus holds %d rows, want %d — update the pinned counts with the sample", len(labels), labeledTotal)
	}

	seen := map[string]bool{}
	scales, bounded := 0, 0
	for _, l := range labels {
		if l.Symbol == "" || l.File == "" || l.Line == 0 || l.Kind == "" || l.Why == "" {
			t.Errorf("%s: incomplete row", l.Symbol)
		}
		if seen[l.Symbol] {
			t.Errorf("%s: labeled twice", l.Symbol)
		}
		seen[l.Symbol] = true

		switch l.Verdict {
		case "scales":
			scales++
			if l.Class != "" {
				t.Errorf("%s: a true positive names an over-count class %q", l.Symbol, l.Class)
			}
		case "bounded":
			bounded++
			if !knownClasses[l.Class] {
				t.Errorf("%s: over-count class %q is not one the sample identified", l.Symbol, l.Class)
			}
		default:
			t.Errorf("%s: verdict %q is neither scales nor bounded", l.Symbol, l.Verdict)
		}

		switch l.Kind {
		case "nested-loop", "compounded", "call-in-loop", "recursion":
		default:
			t.Errorf("%s: kind %q is not one this analyzer emits", l.Symbol, l.Kind)
		}

		switch l.Sample {
		case "high-tier", "removed":
		default:
			t.Errorf("%s: sample %q is neither round", l.Symbol, l.Sample)
		}
	}

	rounds := map[string]int{}
	for _, l := range labels {
		rounds[l.Sample]++
	}
	if rounds["high-tier"] != labeledHighTier || rounds["removed"] != labeledRemoved {
		t.Errorf("rounds are %d high-tier / %d removed, want %d / %d",
			rounds["high-tier"], rounds["removed"], labeledHighTier, labeledRemoved)
	}

	if scales != labeledScales || bounded != labeledBounded {
		t.Errorf("corpus splits %d scales / %d bounded, want %d / %d", scales, bounded, labeledScales, labeledBounded)
	}
}

// ScalingSymbols is the set no change to this analyzer may stop reporting. The
// extractor-side and analyzer-side corpus tests assert the shapes; this names the real
// functions those shapes were reduced from, so the connection survives a refactor of
// either side.
func TestLabelCorpus_ScalingSymbolsNamed(t *testing.T) {
	want := map[string]bool{
		"internal/explainers/intentcheck.claimVerdicts":          false,
		"internal/linkers/binders/messagingcontract.Binder.Bind": false,
		"pkg/history.reduce": false,
	}
	for _, l := range loadLabels(t) {
		if l.Verdict != "scales" {
			continue
		}
		if _, ok := want[l.Symbol]; !ok {
			t.Errorf("%s is labeled scales but is not in the protected set", l.Symbol)
			continue
		}
		want[l.Symbol] = true
	}
	for sym, found := range want {
		if !found {
			t.Errorf("%s left the protected set; a true positive may no longer be reported", sym)
		}
	}
}
