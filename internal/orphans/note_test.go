package orphans

import (
	"strings"
	"testing"
)

// The response note carries a language's caveats only when that language is among the
// symbols the call considered. Sent whole it was about 5,000 characters on every call,
// mostly about languages the graph did not hold.
func TestResponseNoteCarriesOnlyThePresentLanguages(t *testing.T) {
	sym := func(lang, pkg string) symInput {
		return symInput{Name: pkg + ".F", Kind: "function", File: pkg + "/f", Package: pkg, Language: lang}
	}
	syms := []symInput{sym("go", "svc"), sym("python", "tools"), sym("cpp", "native"), sym("typescript", "web")}
	all := options{Mode: "both", Visibility: "all"}

	note := responseNote(syms, all)
	for _, want := range []string{"CONFIDENCE:", "PYTHON:", "C/C++:", "TYPESCRIPT/JS:"} {
		if !strings.Contains(note, want) {
			t.Errorf("note lacks %q for a population holding it", want)
		}
	}
	for _, absent := range []string{"RUBY:", "SCALA:"} {
		if strings.Contains(note, absent) {
			t.Errorf("note carries %q with no such symbols", absent)
		}
	}

	goOnly := options{Mode: "both", Visibility: "all", Package: "svc"}
	if note := responseNote(syms, goOnly); strings.Contains(note, "PYTHON:") || strings.Contains(note, "C/C++:") || strings.Contains(note, "TYPESCRIPT/JS:") {
		t.Errorf("a call scoped to Go code still carries other languages' caveats: %q", note)
	}
}
