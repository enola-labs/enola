package server

import (
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// snapshot_receipt follows the unresolved-edge count with its breakdown by reason,
// the same one llm_context.md prints, so the count names its next step.
func TestRenderReceipt_UnresolvedReasons(t *testing.T) {
	r := facts.Receipt{Quality: facts.ReceiptQuality{Coverage: &facts.CoverageSummary{
		ServicesTotal: 4, UnresolvedEdges: 144,
		UnresolvedReasons: []facts.ReasonCount{{Reason: "path_unknown", Count: 132}, {Reason: "method_mismatch", Count: 12}},
	}}}
	out := renderReceipt(r)
	if !strings.Contains(out, "144 unresolved edge(s) (by reason: `path_unknown` 132, `method_mismatch` 12)") {
		t.Errorf("receipt lacks the reason breakdown:\n%s", out)
	}
}
