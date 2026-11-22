package layers

import (
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// dotnetModules returns C# module facts, each in the MSBuild project named by
// the value, the way the .NET extractor sets the `project` prop.
func dotnetModules(byDir map[string]string) []facts.Fact {
	var ff []facts.Fact
	for dir, project := range byDir {
		props := map[string]any{"language": "csharp"}
		if project != "" {
			props["project"] = project
		}
		ff = append(ff, facts.Fact{Kind: facts.KindModule, Name: dir, Props: props})
	}
	return ff
}

// TestDotnetClean_FolderTakesItsProjectsLayer: a .NET layer boundary is the
// project, so a use-case folder named Application inside the host project is
// part of the host. It may use what the host references, and its imports of the
// Infrastructure project are not application -> infrastructure.
func TestDotnetClean_FolderTakesItsProjectsLayer(t *testing.T) {
	s := facts.NewStore()
	s.Add(dotnetModules(map[string]string{
		"src/Shop.API":                      "Shop.API",
		"src/Shop.API/Application/Commands": "Shop.API",
		"src/Shop.API/Application/Queries":  "Shop.API",
		"src/Shop.Domain":                   "Shop.Domain",
		"src/Shop.Infrastructure":           "Shop.Infrastructure",
	})...)
	s.Add(frameworkFact("aspnetcore"))
	addDep(s, "src/Shop.API/Application/Commands/Handler.cs", "src/Shop.Infrastructure")
	addDep(s, "src/Shop.API/Application/Queries/Queries.cs", "src/Shop.Infrastructure")
	addDep(s, "src/Shop.Infrastructure/Context.cs", "src/Shop.Domain")

	if got := violationTitles(t, s); len(got) != 0 {
		t.Errorf("a folder inside the host project took the layer of its own name: %v", got)
	}
}

// TestDotnetClean_ProjectLayerKeepsRealViolations: reading the project's layer
// must not hide a breach. A folder named Infrastructure inside the Domain
// project is domain, so its import of the Infrastructure project is a violation,
// and the Domain project reaching into the host's Application folder is
// domain -> api (it read as domain -> application before).
func TestDotnetClean_ProjectLayerKeepsRealViolations(t *testing.T) {
	s := facts.NewStore()
	s.Add(dotnetModules(map[string]string{
		"src/Shop.API":                      "Shop.API",
		"src/Shop.API/Application/Commands": "Shop.API",
		"src/Shop.Domain":                   "Shop.Domain",
		"src/Shop.Domain/Infrastructure":    "Shop.Domain",
		"src/Shop.Infrastructure":           "Shop.Infrastructure",
	})...)
	s.Add(frameworkFact("aspnetcore"))
	addDep(s, "src/Shop.Domain/Infrastructure/Store.cs", "src/Shop.Infrastructure")
	addDep(s, "src/Shop.Domain/Order.cs", "src/Shop.API/Application/Commands")

	got := strings.Join(violationTitles(t, s), "\n")
	for _, want := range []string{"domain -> infrastructure", "domain -> api"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected a %s violation, got:\n%s", want, got)
		}
	}
}

// TestDotnetClean_UnnamedProjectFallsBackToFolders: a project whose name names
// no layer (a shared kernel such as Shop.Core) says nothing, so its folders are
// classified by their own names as before.
func TestDotnetClean_UnnamedProjectFallsBackToFolders(t *testing.T) {
	s := facts.NewStore()
	s.Add(dotnetModules(map[string]string{
		"src/Shop.Core/Domain":         "Shop.Core",
		"src/Shop.Core/Infrastructure": "Shop.Core",
		"src/Shop.API":                 "Shop.API",
		"src/Shop.Application":         "Shop.Application",
	})...)
	s.Add(frameworkFact("aspnetcore"))
	addDep(s, "src/Shop.Core/Domain/Order.cs", "src/Shop.Application")

	got := strings.Join(violationTitles(t, s), "\n")
	if !strings.Contains(got, "domain -> application") {
		t.Errorf("expected the folder layer to decide inside an unnamed project, got:\n%s", got)
	}
}
