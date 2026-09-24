package check

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A located finding shows the line it is about with its span underlined; one
// the extractor never positioned prints no frame, and neither does a position
// past the end of the file.
func TestWriteFindings_ShowsTheFrameWhenTheExtractorMeasuredOne(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "app", "models"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := "class Order\n  def get_total\n    lines.sum(:amount)\n  end\nend\n"
	if err := os.WriteFile(filepath.Join(dir, "app", "models", "order.rb"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	previous := frameRoot
	frameRoot = dir
	defer func() { frameRoot = previous }()

	var sb strings.Builder
	Verdict{}.writeFindings(&sb, []facts.Insight{
		{Title: "located", Source: "constraints", Confidence: 1, Evidence: []facts.Evidence{{
			File: "app/models/order.rb", Symbol: "Order#get_total", Detail: "name inside the forbidden pattern",
			Line: 2, EndLine: 4, Column: 3, EndColumn: 6}}},
		{Title: "unlocated", Source: "cycles", Confidence: 1, Evidence: []facts.Evidence{{
			File: "app/models/order.rb", Symbol: "Order", Detail: "no position measured"}}},
		{Title: "past the end", Source: "cycles", Confidence: 1, Evidence: []facts.Evidence{{
			File: "app/models/order.rb", Symbol: "Order", Detail: "stale line", Line: 99, Column: 1}}},
	})
	out := sb.String()
	if !strings.Contains(out, "app/models/order.rb:2\n") || !strings.Contains(out, "  def get_total") {
		t.Fatalf("the located finding must show its line:\n%s", out)
	}
	if !strings.Contains(out, "\n        ^^^^^^^^^^^^^^^^^^\n") && !strings.Contains(out, "^^^") {
		t.Fatalf("the span must be underlined:\n%s", out)
	}
	if strings.Count(out, "app/models/order.rb:") != 1 {
		t.Fatalf("only the positioned finding may print a frame:\n%s", out)
	}
}
