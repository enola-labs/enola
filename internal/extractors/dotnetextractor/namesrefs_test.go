package dotnetextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// An attribute, typeof, default or nameof names a type without constructing or
// calling it. The edge survives the merge pass only when the type is declared in the
// repository, so the framework's own attributes ([HttpGet]) leave none, and `[Test]`
// does not catch an unrelated class named Test. A class the
// compiler or a MEF container loads by attribute is framework_registered.
func TestExtract_TypeNamesAndToolchainRoots(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"src/Lib/Lib.csproj": `<Project Sdk="Microsoft.NET.Sdk"></Project>`,
		"src/Lib/Polyfills.cs": `namespace Acme;
internal sealed class UnconditionalSuppressMessageAttribute : System.Attribute { }
internal readonly struct ForceYielding { }
public sealed class Marker { }
public sealed class Registry { }
public sealed class Test { }
`,
		"src/Lib/Uses.cs": `namespace Acme;
public static class Uses
{
    [UnconditionalSuppressMessage("Trimming", "IL2026")]
    public static void Annotated() { }

    [HttpGet("/x")]
    [Test]
    public static void Framework() { }

    public static async Task Yield() { await default(ForceYielding); }

    public static System.Type Kind() => typeof(Marker);

    public static string Name() => nameof(Registry);
}
`,
		"src/Lib/Analyzers.cs": `namespace Acme;
[DiagnosticAnalyzer(LanguageNames.CSharp)]
public sealed class Suppressor : DiagnosticSuppressor { }

[ExportCodeFixProvider(LanguageNames.CSharp)]
public sealed class Fixer : CodeFixProvider { }

public sealed class Plain { }
`,
	})
	for sym, want := range map[string]string{
		"src/Lib.Uses.Annotated": "src/Lib.UnconditionalSuppressMessageAttribute",
		"src/Lib.Uses.Yield":     "src/Lib.ForceYielding",
		"src/Lib.Uses.Kind":      "src/Lib.Marker",
		"src/Lib.Uses.Name":      "src/Lib.Registry",
	} {
		f := factByName(ff, sym)
		if f == nil {
			t.Fatalf("missing %s; got %v", sym, names(ff, facts.KindSymbol))
		}
		if !hasRel(f, facts.RelNames, want) {
			t.Errorf("%s: missing names %s; got %v", sym, want, f.Relations)
		}
	}
	if f := factByName(ff, "src/Lib.Uses.Framework"); f != nil {
		for _, r := range f.Relations {
			if r.Kind == facts.RelNames {
				t.Errorf("a framework attribute must leave no names edge; got %v", r)
			}
		}
	}
	for sym, want := range map[string]bool{
		"src/Lib.Suppressor": true, "src/Lib.Fixer": true, "src/Lib.Plain": false,
	} {
		f := factByName(ff, sym)
		if f == nil {
			t.Fatalf("missing %s", sym)
		}
		if got := f.PropBool("framework_registered"); got != want {
			t.Errorf("%s: framework_registered = %v, want %v", sym, got, want)
		}
	}
}

// A names edge binds only to a type the file can see: its namespace, an enclosing
// one, or a namespace it opens with `using`. NUnit's [Test] in a test file does not
// reach a viewer helper class that happens to be called TestAttribute, and typeof(T)
// inside a generic method names the type parameter, not a class named T.
func TestExtract_TypeNamesFollowVisibility(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"src/App/App.csproj": `<Project Sdk="Microsoft.NET.Sdk"></Project>`,
		"src/App/Viewer.cs": `namespace App.Viewer.Forms;
public sealed class TestAttribute : System.Attribute { }
public sealed class T { }
`,
		"src/App/Specs.cs": `namespace App.Specs;
public sealed class Specs
{
    [Test]
    public void Unseen() { }

    public System.Type Param<T>() => typeof(T);
}
`,
		"src/App/Seen.cs": `using App.Viewer.Forms;
namespace App.Other;
public sealed class Seen
{
    [Test]
    public void Visible() { }
}
`,
	})
	unseen := factByName(ff, "src/App.Specs.Unseen")
	param := factByName(ff, "src/App.Specs.Param")
	visible := factByName(ff, "src/App.Seen.Visible")
	if unseen == nil || param == nil || visible == nil {
		t.Fatalf("missing symbols; got %v", names(ff, facts.KindSymbol))
	}
	for _, f := range []*facts.Fact{unseen, param} {
		for _, r := range f.Relations {
			if r.Kind == facts.RelNames {
				t.Errorf("%s: unexpected names edge %v", f.Name, r)
			}
		}
	}
	if !hasRel(visible, facts.RelNames, "src/App.TestAttribute") {
		t.Errorf("a [Test] the file can see through its using must bind; got %v", visible.Relations)
	}
}
