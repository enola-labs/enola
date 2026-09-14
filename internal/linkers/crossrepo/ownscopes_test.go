package crossrepo

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
	importsignal "github.com/enola-labs/enola/internal/linkers/crossrepo/signals/imports"
	"github.com/enola-labs/enola/internal/linkers/vocab"
	"github.com/enola-labs/enola/pkg/plugin"
)

func importEdges(t *testing.T, all []facts.Fact) map[string]bool {
	t.Helper()
	edges := map[string]bool{}
	for _, f := range ComputeLinks(all, nil, []plugin.CrossRepoSignal{importsignal.New()}, vocab.Default()) {
		if f.Kind == facts.KindDependency {
			edges[f.Name] = true
		}
	}
	return edges
}

// A service lists @example/sdk in package.json and imports it. The manifest dependency
// fact carries package_name @example/sdk; that names what the service depends on, not
// what it publishes, so the import still links to the repository that publishes it.
func TestOwnScopes_ADependencyIsNotAPublishedPackage(t *testing.T) {
	all := []facts.Fact{
		{Kind: facts.KindModule, Name: "src/catalog", Repo: "backend",
			Props: map[string]any{"package_name": "backend"}},
		{Kind: facts.KindDependency, Name: "pkg:npm/@example/sdk", Repo: "backend",
			Props: map[string]any{"package_name": "@example/sdk", "type": "package", "ecosystem": "npm"}},
		{Kind: facts.KindDependency, Name: "src/catalog -> @example/sdk", Repo: "backend",
			Relations: []facts.Relation{{Kind: facts.RelImports, Target: "@example/sdk"}}},
		{Kind: facts.KindModule, Name: "src", Repo: "sdk",
			Props: map[string]any{"package_name": "@example/sdk"}},
	}
	if edges := importEdges(t, all); !edges["backend -> sdk"] {
		t.Fatalf("a scoped package.json dependency suppressed the import edge it declares: %v", edges)
	}
}

// The guard the bug hid behind still holds: a repository publishing @acme/web that
// imports @acme/lib is importing a sibling package of its own project, not a repository
// that happens to be labeled acme.
func TestOwnScopes_ASiblingPackageIsStillSkipped(t *testing.T) {
	all := []facts.Fact{
		{Kind: facts.KindModule, Name: "src", Repo: "web",
			Props: map[string]any{"package_name": "@acme/web"}},
		{Kind: facts.KindDependency, Name: "src -> @acme/lib", Repo: "web",
			Relations: []facts.Relation{{Kind: facts.RelImports, Target: "@acme/lib"}}},
		{Kind: facts.KindModule, Name: "src", Repo: "acme",
			Props: map[string]any{"package_name": "acme"}},
	}
	if edges := importEdges(t, all); edges["web -> acme"] {
		t.Fatalf("a sibling package of the importing repository linked to a repo labeled like its scope: %v", edges)
	}
}
