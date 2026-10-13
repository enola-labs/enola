package facts

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestDocSummary(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"one line":           {"Place an order.", "Place an order."},
		"first paragraph":    {"Place an order.\n\nArgs:\n    cart: the cart.", "Place an order."},
		"wrapped paragraph":  {"\n    Place an order\n    for a cart.\n\n    More.\n", "Place an order for a cart."},
		"blank lines first":  {"\n\n  Place.  \n", "Place."},
		"inner whitespace":   {"Place\tan   order.", "Place an order."},
		"empty":              {"", ""},
		"whitespace only":    {"  \n\t\n", ""},
		"indented blank gap": {"One.\n   \nTwo.", "One."},
	} {
		if got := DocSummary(tc.in); got != tc.want {
			t.Errorf("%s: DocSummary(%q) = %q, want %q", name, tc.in, got, tc.want)
		}
	}
}

func TestDocSummary_CutsALongParagraphAtAWord(t *testing.T) {
	got := DocSummary(strings.Repeat("word ", 200))
	if len(got) > docLimit+3 || !strings.HasSuffix(got, "word...") {
		t.Errorf("len %d, ends %q", len(got), got[len(got)-10:])
	}
	// A run with no space to cut at must still end on a whole character.
	got = DocSummary(strings.Repeat("é", 400))
	if !utf8.ValidString(got) || len(got) > docLimit+3 {
		t.Errorf("unbroken run: valid %v, len %d", utf8.ValidString(got), len(got))
	}
}
