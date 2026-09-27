package perf

import (
	"strings"
	"testing"
)

// The legend defines only the kinds a response actually contains.
func TestKindLegendDefinesOnlyThePresentKinds(t *testing.T) {
	legend := kindLegend([]Finding{{Kind: "call-in-loop"}, {Kind: "call-in-loop"}})
	if !strings.Contains(legend, "call-in-loop: an I/O") || strings.Contains(legend, "nested-loop") {
		t.Errorf("legend = %q", legend)
	}
	if kindLegend(nil) != "" {
		t.Error("an empty result carries a legend")
	}
}
