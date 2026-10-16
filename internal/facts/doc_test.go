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

// A comment holding bytes that are not UTF-8 is stored as valid UTF-8, so the doc is
// written the same way whether or not it went through the extractor cache.
func TestDocSummary_InvalidUTF8(t *testing.T) {
	got := DocSummary("Sets the specified window\x92s show state.")
	if !utf8.ValidString(got) {
		t.Fatalf("doc %q is not valid UTF-8", got)
	}
	if want := "Sets the specified window�s show state."; got != want {
		t.Errorf("doc = %q, want %q", got, want)
	}
}

// DocText keeps the whole comment: its lines without their shared indentation, one
// blank line between paragraphs and none at either end.
func TestDocText(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"", ""},
		{"\n  \n", ""},
		{"Places an order.", "Places an order."},
		{"Places an order.\n\n    Args:\n        id: the order\n\n\n    Returns nothing.\n", "Places an order.\n\nArgs:\n    id: the order\n\nReturns nothing."},
		{"\n  First.\n  Second.\n", "First.\nSecond."},
	} {
		if got := DocText(c.in); got != c.want {
			t.Errorf("DocText(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	long := strings.Repeat("word ", docTextLimit/5+10)
	got := DocText(long)
	if len(got) > docTextLimit || !strings.HasSuffix(got, "word...") {
		t.Errorf("a long comment is cut at a word within the cap: len %d, ends %q", len(got), got[len(got)-10:])
	}
	if again := DocText(got); again != got {
		t.Error("DocText changes a text it returned")
	}
}

// doc_text is set only when the comment says more than doc: a second paragraph, or a
// first one cut at the cap. A one-paragraph comment over several lines is all in doc.
func TestSetDoc(t *testing.T) {
	for _, c := range []struct {
		in, doc, full string
	}{
		{"", "", ""},
		{"Places an order\nand charges for it.", "Places an order and charges for it.", ""},
		{"Places an order.\n\nMore detail.", "Places an order.", "Places an order.\n\nMore detail."},
		{strings.Repeat("word ", 80), DocSummary(strings.Repeat("word ", 80)), strings.TrimSpace(strings.Repeat("word ", 80))},
		{"Trailing newline.\n\nKept out.\n", "Trailing newline.", "Trailing newline.\n\nKept out."},
	} {
		var f Fact
		SetDoc(&f, c.in)
		if got := f.PropString(PropDoc); got != c.doc {
			t.Errorf("SetDoc(%q): doc = %q, want %q", c.in, got, c.doc)
		}
		if got := f.PropString(PropDocText); got != c.full {
			t.Errorf("SetDoc(%q): doc_text = %q, want %q", c.in, got, c.full)
		}
		props := map[string]any{}
		SetDocProp(props, c.in)
		if got, _ := props[PropDocText].(string); got != c.full {
			t.Errorf("SetDocProp(%q): doc_text = %q, want %q", c.in, got, c.full)
		}
	}
}

// TakeDocTexts moves doc_text off the facts into the side table, under the fact's
// final key, even when two facts share one Props map.
func TestStore_TakeDocTexts(t *testing.T) {
	shared := map[string]any{PropDoc: "A.", PropDocText: "A.\n\nMore."}
	s := NewStore()
	s.Add(
		Fact{Kind: KindSymbol, Name: "a.One", File: "a/a.go", Props: shared},
		Fact{Kind: KindSymbol, Name: "a.Two", File: "a/a.go", Props: shared},
		Fact{Kind: KindSymbol, Name: "a.Bare", File: "a/a.go", Props: map[string]any{PropDoc: "Bare."}},
	)
	s.SetRepoRange(0, "shop")
	if n := s.TakeDocTexts(0); n != 2 {
		t.Fatalf("TakeDocTexts moved %d, want 2", n)
	}
	for _, f := range s.All() {
		if _, has := f.Prop(PropDocText); has {
			t.Errorf("%s still carries doc_text", f.Name)
		}
		if f.PropString(PropDoc) == "" {
			t.Errorf("%s lost its doc", f.Name)
		}
	}
	for _, name := range []string{"a.One", "a.Two"} {
		if got := s.DocText(FactKey{Repo: "shop", Kind: KindSymbol, Name: name, File: "a/a.go"}); got != "A.\n\nMore." {
			t.Errorf("DocText(%s) = %q", name, got)
		}
	}
	if got := s.DocText(FactKey{Repo: "shop", Kind: KindSymbol, Name: "a.Bare", File: "a/a.go"}); got != "" {
		t.Errorf("a fact whose doc is all there is has no doc text, got %q", got)
	}

	carried := NewStore()
	carried.CarryDocTexts(s, func(k FactKey) bool { return k.Name != "a.Two" })
	if carried.DocText(FactKey{Repo: "shop", Kind: KindSymbol, Name: "a.One", File: "a/a.go"}) == "" ||
		carried.DocText(FactKey{Repo: "shop", Kind: KindSymbol, Name: "a.Two", File: "a/a.go"}) != "" {
		t.Error("CarryDocTexts carries exactly the texts keep accepts")
	}
}
