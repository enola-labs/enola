package plain

import (
	"strings"
	"testing"
)

func TestWrap_StaysWithinWidth(t *testing.T) {
	var b strings.Builder
	Wrap(&b, strings.Repeat("word ", 40), "  ", "    ")
	lines := strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected wrapping, got %q", b.String())
	}
	for i, l := range lines {
		if len(l) > Width {
			t.Errorf("line %d is %d wide: %q", i, len(l), l)
		}
	}
	if !strings.HasPrefix(lines[1], "    word") {
		t.Errorf("continuation indent: %q", lines[1])
	}
}

func TestSection_LongSubjectPutsLocationOnItsOwnLine(t *testing.T) {
	var b strings.Builder
	long := strings.Repeat("a", Width)
	Section(&b, "Start reading here", []Item{{Subject: long, Location: "x.ts:3", Text: "t", Certainty: Certain}}, "")
	if !strings.Contains(b.String(), "  "+long+"\n    at x.ts:3\n") {
		t.Errorf("got:\n%s", b.String())
	}
}

func TestSection_LocationWithoutSubject(t *testing.T) {
	var b strings.Builder
	Section(&b, "S", []Item{{Text: "t.", Location: "a/b.go", Certainty: Certain}}, "")
	if b.String() != "S\n  t.\n    at a/b.go\n\n" {
		t.Errorf("got %q", b.String())
	}
}

func TestSection_OnlyHedgesAreLabelled(t *testing.T) {
	var b strings.Builder
	Section(&b, "S", []Item{
		{Text: "sure.", Certainty: Certain},
		{Text: "maybe.", Certainty: Likely},
		{Text: "hunch.", Certainty: Guess},
	}, "footer.")
	want := "S\n  sure.\n  maybe. (likely)\n  hunch. (a guess)\n  footer.\n\n"
	if b.String() != want {
		t.Errorf("got %q, want %q", b.String(), want)
	}
}

func TestCertainty(t *testing.T) {
	for conf, want := range map[float64]Certainty{1: Certain, 0.9: Certain, 0.8: Likely, 0.6: Likely, 0.5: Guess} {
		if got := CertaintyOf(conf); got != want {
			t.Errorf("CertaintyOf(%v) = %q, want %q", conf, got, want)
		}
	}
	if Certain.Rank() >= Likely.Rank() || Likely.Rank() >= Guess.Rank() {
		t.Error("ranks must order certain, likely, a guess")
	}
}

func TestPluralAndList(t *testing.T) {
	if Plural(1, "module", "modules") != "1 module" || Plural(3, "module", "modules") != "3 modules" {
		t.Error("Plural")
	}
	for in, want := range map[string]string{"": "", "a": "a", "a,b": "a and b", "a,b,c": "a, b and c"} {
		words := strings.Split(in, ",")
		if in == "" {
			words = nil
		}
		if got := AndList(words); got != want {
			t.Errorf("AndList(%q) = %q, want %q", in, got, want)
		}
	}
}
