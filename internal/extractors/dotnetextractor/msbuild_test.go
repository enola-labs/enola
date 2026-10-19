package dotnetextractor

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// extractRepo writes files to a temp repository and runs the real Extract, so the
// MSBuild tests exercise the same path a snapshot does — these facts come from
// reading files off disk, which the in-memory helpers in the other test files
// deliberately bypass.
func extractRepo(t *testing.T, files map[string]string) []facts.Fact {
	t.Helper()
	root := t.TempDir()
	rels := make([]string, 0, len(files))
	for rel, body := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		rels = append(rels, rel)
	}
	sort.Strings(rels) // the engine hands over a stable order; so must the test
	ff, err := New().Extract(context.Background(), root, rels)
	if err != nil {
		t.Fatal(err)
	}
	return ff
}

// moduleNamed returns the single module fact with this name.
func moduleNamed(t *testing.T, ff []facts.Fact, name string) facts.Fact {
	t.Helper()
	var found []facts.Fact
	for _, f := range ff {
		if f.Kind == facts.KindModule && f.Name == name {
			found = append(found, f)
		}
	}
	if len(found) == 0 {
		t.Fatalf("no module %q; modules present: %v", name, moduleNames(ff))
	}
	if len(found) > 1 {
		t.Fatalf("module %q emitted %d times, want 1 — facts are name-keyed and "+
			"duplicates double-count fact_count", name, len(found))
	}
	return found[0]
}

func moduleNames(ff []facts.Fact) []string {
	var out []string
	for _, f := range ff {
		if f.Kind == facts.KindModule {
			out = append(out, f.Name)
		}
	}
	sort.Strings(out)
	return out
}

func dependsOn(f facts.Fact) []string {
	var out []string
	for _, r := range f.Relations {
		if r.Kind == facts.RelDependsOn {
			out = append(out, r.Target)
		}
	}
	sort.Strings(out)
	return out
}

const apiProj = `<Project Sdk="Microsoft.NET.Sdk">
  <PropertyGroup>
    <TargetFramework>net9.0</TargetFramework>
    <OutputType>Exe</OutputType>
  </PropertyGroup>
  <ItemGroup>
    <ProjectReference Include="..\Acme.Domain\Acme.Domain.csproj" />
    <PackageReference Include="Serilog" Version="4.2.0" />
  </ItemGroup>
</Project>`

func TestMSBuild_ProjectReferenceBecomesModuleEdge(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"src/Acme.Api/Acme.Api.csproj":       apiProj,
		"src/Acme.Api/Program.cs":            "namespace Acme.Api; public class Program { }",
		"src/Acme.Domain/Acme.Domain.csproj": `<Project Sdk="Microsoft.NET.Sdk"></Project>`,
		"src/Acme.Domain/Order.cs":           "namespace Acme.Domain; public class Order { }",
	})

	api := moduleNamed(t, ff, "src/Acme.Api")
	if got := dependsOn(api); len(got) != 1 || got[0] != "src/Acme.Domain" {
		t.Errorf("depends_on = %v, want [src/Acme.Domain]", got)
	}
	if api.Props["project"] != "Acme.Api" {
		t.Errorf("project = %v, want Acme.Api", api.Props["project"])
	}
	if api.Props["target_framework"] != "net9.0" || api.Props["output_type"] != "Exe" {
		t.Errorf("tfm/output = %v/%v", api.Props["target_framework"], api.Props["output_type"])
	}
	if api.Props["msbuild"] != true {
		t.Errorf("msbuild prop = %v, want true", api.Props["msbuild"])
	}
}

// The Include path uses backslashes on every host platform, so a plain
// filepath.Dir on macOS/Linux leaves them embedded and the edge points nowhere.
func TestMSBuild_BackslashIncludeResolves(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"src/A/A.csproj": `<Project Sdk="Microsoft.NET.Sdk"><ItemGroup>` +
			`<ProjectReference Include="..\..\lib\B\B.csproj" /></ItemGroup></Project>`,
		"src/A/A.cs":     "public class A { }",
		"lib/B/B.csproj": `<Project Sdk="Microsoft.NET.Sdk"></Project>`,
	})
	if got := dependsOn(moduleNamed(t, ff, "src/A")); len(got) != 1 || got[0] != "lib/B" {
		t.Errorf("depends_on = %v, want [lib/B]", got)
	}
}

// A project root that also holds sources must yield ONE module fact carrying both
// the source-derived and the project-derived props.
func TestMSBuild_ProjectRootWithSourcesIsOneModule(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"src/Acme.Api/Acme.Api.csproj": apiProj,
		"src/Acme.Api/Program.cs":      "namespace Acme.Api; public class Program { }",
	})
	m := moduleNamed(t, ff, "src/Acme.Api") // fails if emitted twice
	if m.Props["language"] != "csharp" || m.Props["project"] != "Acme.Api" {
		t.Errorf("props lost on merge: %v", m.Props)
	}
}

// The Giraffe case: an F#-only repository. Before this, Detect matched the .slnx,
// the extractor claimed the repo and emitted nothing at all.
func TestMSBuild_FSharpOnlyRepoStillEmitsProjectGraph(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"Giraffe.slnx": `<Solution>
  <Project Path="src/Giraffe/Giraffe.fsproj" />
  <Project Path="tests/Giraffe.Tests/Giraffe.Tests.fsproj" />
</Solution>`,
		"src/Giraffe/Giraffe.fsproj": `<Project Sdk="Microsoft.NET.Sdk">
  <PropertyGroup><TargetFramework>net8.0</TargetFramework></PropertyGroup>
</Project>`,
		"src/Giraffe/Core.fs": "module Giraffe.Core",
		"tests/Giraffe.Tests/Giraffe.Tests.fsproj": `<Project Sdk="Microsoft.NET.Sdk">
  <ItemGroup>
    <ProjectReference Include="../../src/Giraffe/Giraffe.fsproj" />
    <PackageReference Include="Microsoft.NET.Test.Sdk" Version="17.11.1" />
  </ItemGroup>
</Project>`,
	})

	if len(ff) == 0 {
		t.Fatal("F#-only repo emitted no facts at all — the silent-empty snapshot")
	}
	lib := moduleNamed(t, ff, "src/Giraffe")
	if lib.Props["language"] != "fsharp" {
		t.Errorf("language = %v, want fsharp", lib.Props["language"])
	}
	if lib.Props["solution"] != "Giraffe" {
		t.Errorf("solution = %v, want Giraffe", lib.Props["solution"])
	}

	tests := moduleNamed(t, ff, "tests/Giraffe.Tests")
	if got := dependsOn(tests); len(got) != 1 || got[0] != "src/Giraffe" {
		t.Errorf("depends_on = %v, want [src/Giraffe]", got)
	}
	// Microsoft.NET.Test.Sdk is the signal; the path heuristic would also say test
	// here, so assert the property that the SDK reference is what set it by using a
	// project whose path says nothing.
	if tests.Props[facts.PropModuleRole] != facts.ModuleRoleTest {
		t.Errorf("module_role = %v, want test", tests.Props[facts.PropModuleRole])
	}
}

func TestMSBuild_TestSdkMarksTestProjectOffPath(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		// Nothing in this path says "test".
		"src/Acme.Verification/Acme.Verification.csproj": `<Project Sdk="Microsoft.NET.Sdk"><ItemGroup>` +
			`<PackageReference Include="Microsoft.NET.Test.Sdk" Version="17.11.1" /></ItemGroup></Project>`,
		"src/Acme.Verification/Check.cs": "public class Check { }",
	})
	m := moduleNamed(t, ff, "src/Acme.Verification")
	if m.Props[facts.PropModuleRole] != facts.ModuleRoleTest {
		t.Errorf("module_role = %v, want test", m.Props[facts.PropModuleRole])
	}
}

func TestMSBuild_PackageReferenceBecomesNugetDependency(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"src/Acme.Api/Acme.Api.csproj": apiProj,
		"src/Acme.Api/Program.cs":      "public class Program { }",
	})
	var dep *facts.Fact
	for i := range ff {
		if ff[i].Kind == facts.KindDependency && ff[i].Props["package_manager"] == "nuget" {
			dep = &ff[i]
		}
	}
	if dep == nil {
		t.Fatal("no nuget dependency fact")
	}
	if dep.Props["import"] != "Serilog" || dep.Props["version"] != "4.2.0" {
		t.Errorf("dep props = %v", dep.Props)
	}
	if dep.Props["source"] != "external" {
		t.Errorf("source = %v, want external", dep.Props["source"])
	}
}

// A <Version> child element is the other legal spelling.
func TestMSBuild_PackageVersionAsChildElement(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"src/A/A.csproj": `<Project Sdk="Microsoft.NET.Sdk"><ItemGroup>` +
			`<PackageReference Include="Dapper"><Version>2.1.66</Version></PackageReference>` +
			`</ItemGroup></Project>`,
		"src/A/A.cs": "public class A { }",
	})
	for _, f := range ff {
		if f.Kind == facts.KindDependency && f.Props["import"] == "Dapper" {
			if f.Props["version"] != "2.1.66" {
				t.Errorf("version = %v, want 2.1.66", f.Props["version"])
			}
			return
		}
	}
	t.Fatal("no Dapper dependency fact")
}

// A legacy project puts every element in the 2003 MSBuild namespace, which is why
// the parser matches on Name.Local rather than unmarshalling into a struct.
func TestMSBuild_LegacyNamespacedProject(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"src/A/A.csproj": `<?xml version="1.0" encoding="utf-8"?>
<Project ToolsVersion="15.0" xmlns="http://schemas.microsoft.com/developer/msbuild/2003">
  <PropertyGroup><AssemblyName>Acme.Legacy</AssemblyName></PropertyGroup>
  <ItemGroup><ProjectReference Include="..\B\B.csproj" /></ItemGroup>
</Project>`,
		"src/A/A.cs":     "public class A { }",
		"src/B/B.csproj": `<Project Sdk="Microsoft.NET.Sdk"></Project>`,
	})
	m := moduleNamed(t, ff, "src/A")
	if m.Props["project"] != "Acme.Legacy" {
		t.Errorf("AssemblyName not read: project = %v", m.Props["project"])
	}
	if got := dependsOn(m); len(got) != 1 || got[0] != "src/B" {
		t.Errorf("depends_on = %v, want [src/B]", got)
	}
}

// Conditions are deliberately ignored, so a reference guarded by one still counts
// — but the same target reached twice must not produce two edges.
func TestMSBuild_ConditionalDuplicateRefIsOneEdge(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"src/A/A.csproj": `<Project Sdk="Microsoft.NET.Sdk">
  <ItemGroup Condition="'$(TargetFramework)' == 'net48'">
    <ProjectReference Include="..\B\B.csproj" />
  </ItemGroup>
  <ItemGroup Condition="'$(TargetFramework)' == 'net9.0'">
    <ProjectReference Include="..\B\B.csproj" />
  </ItemGroup>
</Project>`,
		"src/A/A.cs":     "public class A { }",
		"src/B/B.csproj": `<Project Sdk="Microsoft.NET.Sdk"></Project>`,
	})
	if got := dependsOn(moduleNamed(t, ff, "src/A")); len(got) != 1 {
		t.Errorf("depends_on = %v, want exactly one edge", got)
	}
}

// A reference escaping the repository root names a project that is not in this
// graph; drawing the edge would point at a module that can never exist.
func TestMSBuild_ReferenceAboveRepoRootIsDropped(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"A.csproj": `<Project Sdk="Microsoft.NET.Sdk"><ItemGroup>` +
			`<ProjectReference Include="..\Outside\Outside.csproj" /></ItemGroup></Project>`,
		"A.cs": "public class A { }",
	})
	if got := dependsOn(moduleNamed(t, ff, ".")); len(got) != 0 {
		t.Errorf("depends_on = %v, want none", got)
	}
}

// The legacy .sln text format, which predates .slnx and is still the majority in
// older trees.
func TestMSBuild_LegacySlnGrouping(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"Acme.sln": `Microsoft Visual Studio Solution File, Format Version 12.00
Project("{FAE04EC0-301F-11D3-BF4B-00C04F79EFBC}") = "Acme.Api", "src\Acme.Api\Acme.Api.csproj", "{1234}"
EndProject
Project("{2150E333-8FDC-42A3-9474-1A3956D46DE8}") = "Solution Items", "Solution Items", "{5678}"
EndProject
`,
		"src/Acme.Api/Acme.Api.csproj": `<Project Sdk="Microsoft.NET.Sdk"></Project>`,
		"src/Acme.Api/Program.cs":      "public class Program { }",
	})
	if got := moduleNamed(t, ff, "src/Acme.Api").Props["solution"]; got != "Acme" {
		t.Errorf("solution = %v, want Acme", got)
	}
}

// Only a class library that says it packs is published; an executable, a web app,
// a test project and a library relying on the SDK's packable default are not.
func TestMSBuild_PublishedMarksPackableClassLibraries(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"src/Acme.Sdk/Acme.Sdk.csproj":           `<Project Sdk="Microsoft.NET.Sdk"><PropertyGroup><PackageId>Acme.Sdk</PackageId></PropertyGroup></Project>`,
		"src/Acme.Sdk/Client.cs":                 "public class Client { }",
		"src/Acme.Cli/Acme.Cli.csproj":           apiProj,
		"src/Acme.Cli/Program.cs":                "public class Program { }",
		"src/Acme.Api/Acme.Api.csproj":           `<Project Sdk="Microsoft.NET.Sdk.Web"></Project>`,
		"src/Acme.Api/Startup.cs":                "public class Startup { }",
		"src/Acme.Internal/Acme.Internal.csproj": `<Project Sdk="Microsoft.NET.Sdk"><PropertyGroup><TargetFramework>net9.0</TargetFramework></PropertyGroup></Project>`,
		"src/Acme.Internal/Util.cs":              "public class Util { }",
		"src/Acme.Checks/Acme.Checks.csproj": `<Project Sdk="Microsoft.NET.Sdk"><ItemGroup>` +
			`<PackageReference Include="Microsoft.NET.Test.Sdk" Version="17.11.1" /></ItemGroup></Project>`,
		"src/Acme.Checks/Check.cs": "public class Check { }",
	})
	want := map[string]bool{
		"src/Acme.Sdk": true, "src/Acme.Cli": false, "src/Acme.Api": false,
		"src/Acme.Internal": false, "src/Acme.Checks": false,
	}
	for name, published := range want {
		if got := moduleNamed(t, ff, name).PropBool(facts.PropPublished); got != published {
			t.Errorf("%s: published = %v, want %v", name, got, published)
		}
	}
}
