package mcputil

import (
	"strings"
	"testing"
)

// CapTokens is shared by every tool and knows none of them, so its notice must be
// true of all of them: no mode a tool may not have, and no mode it may already be in.
func TestCapTokensNoticeIsTrueOfEveryTool(t *testing.T) {
	long := strings.Repeat("line of output\n", 200)
	for _, isJSON := range []bool{false, true} {
		out := CapTokens(long, 10, isJSON)
		if !strings.Contains(out, "[truncated:") || !strings.Contains(out, "raise max_tokens") {
			t.Errorf("isJSON=%v: notice missing or without a remedy:\n%s", isJSON, out[len(out)-300:])
		}
		if strings.Contains(out, "output_mode=summary") {
			t.Errorf("isJSON=%v: notice prescribes output_mode=summary, which not every tool has", isJSON)
		}
	}
}
