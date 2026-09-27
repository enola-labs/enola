package metrics

import (
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// How "abstract type" is read differs by language, and a response says so only for the
// languages among the packages it reports.
func TestAbstractnessNoteNamesOnlyTheReportedPackagesLanguages(t *testing.T) {
	store := facts.NewStore()
	sym := func(name, module, lang string) facts.Fact {
		return facts.Fact{Kind: facts.KindSymbol, Name: name, Props: map[string]any{"language": lang},
			Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: module}}}
	}
	store.Add(
		facts.Fact{Kind: facts.KindModule, Name: "web/ui"},
		facts.Fact{Kind: facts.KindModule, Name: "svc/core"},
		sym("web/ui.Button", "web/ui", "typescript"),
		sym("svc/core.Run", "svc/core", "go"),
	)

	note := abstractnessNote(store, []PackageMetric{{Package: "web/ui"}, {Package: "svc/core"}})
	if !strings.Contains(note, "TypeScript/JS") || strings.Contains(note, "Scala") || strings.Contains(note, "Python") {
		t.Errorf("note for TypeScript and Go packages = %q", note)
	}
	if note := abstractnessNote(store, []PackageMetric{{Package: "svc/core"}}); note != "" {
		t.Errorf("a Go-only result carries an abstractness note: %q", note)
	}
}
