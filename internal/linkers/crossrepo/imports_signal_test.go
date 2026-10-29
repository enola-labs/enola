package crossrepo

import (
	"sort"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
	importsignal "github.com/enola-labs/enola/internal/linkers/crossrepo/signals/imports"
	"github.com/enola-labs/enola/internal/linkers/vocab"
	"github.com/enola-labs/enola/pkg/plugin"
)

// A repository is named where a package manager puts a repository's name, in the
// importer's language family. A deeper path segment, or a package of another
// language, that only spells a loaded repository's label is not an import of it.
func TestImports_PositionAndLanguage(t *testing.T) {
	code := func(repo, lang string) facts.Fact {
		return facts.Fact{Kind: facts.KindSymbol, Name: repo + ".Main", Repo: repo, File: repo + "/main",
			Props: map[string]any{"symbol_kind": facts.SymbolClass, "language": lang}}
	}
	imp := func(repo, lang, target string) facts.Fact {
		return facts.Fact{Kind: facts.KindDependency, Name: repo + "/x -> " + target, Repo: repo, File: repo + "/x",
			Props:     map[string]any{"language": lang},
			Relations: []facts.Relation{{Kind: facts.RelImports, Target: target}}}
	}
	in := []facts.Fact{
		code("core", "ruby"), code("core", "ruby"),
		code("billing", "go"), code("ledger", "go"),
		code("web", "typescript"), code("ui-kit", "typescript"),
		code("app", "ruby"),
		// Real: a Go module path naming a loaded Go repository, a scoped package.
		imp("billing", "go", "github.com/acme/ledger/pkg/money"),
		imp("web", "typescript", "@acme/ui-kit"),
		// Not: deeper segments, and packages of another language.
		imp("billing", "go", "github.com/other-org/core/errors"),
		imp("web", "typescript", "@dnd-kit/core"),
		imp("web", "typescript", "undici/lib/core/symbols"),
		imp("app", "ruby", "rspec/core/rake_task"),
	}
	var edges []string
	for _, f := range ComputeLinks(in, nil, []plugin.CrossRepoSignal{importsignal.New()}, vocab.Default()) {
		if f.Kind == facts.KindDependency && f.PropString("type") == "cross_repo" {
			edges = append(edges, f.Name)
		}
	}
	sort.Strings(edges)
	want := []string{"billing -> ledger", "web -> ui-kit"}
	if len(edges) != len(want) || edges[0] != want[0] || edges[1] != want[1] {
		t.Errorf("edges = %v, want %v", edges, want)
	}
}
