package tsutil

import "testing"

func TestDocCommentText(t *testing.T) {
	for _, tc := range []struct {
		name     string
		comments []string
		want     string
	}{
		{"docblock to its first tag", []string{"/**\n * Places an order.\n *\n * @param id the order\n */"}, "\nPlaces an order.\n"},
		{"line run", []string{"// Cancels an order.", "// Refunds later."}, "Cancels an order.\nRefunds later."},
		{"licence block documents nothing", []string{"/* Copyright the authors. */"}, ""},
		{"directive ends the run", []string{"// The real description.", "// phpcs:ignore"}, ""},
		{"line comment above a docblock is not read", []string{"// stray", "/** Documented. */"}, "Documented."},
	} {
		got := DocCommentText(tc.comments, []string{"//", "#"}, func(l string) bool { return len(l) >= 6 && l[:6] == "phpcs:" })
		if got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestXMLDocSummary(t *testing.T) {
	got := XMLDocSummary(" <summary>\n Authenticates a user by name, see <see cref=\"IUserManager\"/>.\n </summary>\n <param name=\"request\">The request.</param>")
	want := "\n Authenticates a user by name, see IUserManager.\n "
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
