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

// In a multi-repo snapshot a Go library and a TypeScript app can both have a package
// named "adapters": the Go one's metrics carry no TypeScript note.
func TestAbstractnessNoteKeepsRepositoriesApart(t *testing.T) {
	store := facts.NewStore()
	sym := func(repo, name, module, lang string) facts.Fact {
		return facts.Fact{Kind: facts.KindSymbol, Name: name, Repo: repo, Props: map[string]any{"language": lang},
			Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: module}}}
	}
	store.Add(
		facts.Fact{Kind: facts.KindModule, Name: "adapters", Repo: "auth"},
		facts.Fact{Kind: facts.KindModule, Name: "adapters", Repo: "web"},
		sym("auth", "adapters.Handler", "adapters", "go"),
		sym("web", "adapters.toDTO", "adapters", "typescript"),
	)
	if note := abstractnessNote(store, []PackageMetric{{Package: "adapters", Repo: "auth"}}); note != "" {
		t.Errorf("the Go library's adapters carry another repository's note: %q", note)
	}
	if note := abstractnessNote(store, []PackageMetric{{Package: "adapters", Repo: "web"}}); !strings.Contains(note, "TypeScript/JS") {
		t.Errorf("the TypeScript app's adapters lost its note: %q", note)
	}
}
