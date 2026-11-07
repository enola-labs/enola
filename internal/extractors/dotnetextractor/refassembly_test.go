package dotnetextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A reference assembly (a project directory named ref) redeclares an implementation's
// public types with stub bodies. A reference to the type binds to the implementation
// whichever order the two were indexed in, and the pair does not make the simple name
// ambiguous: dotnet/runtime's System.Runtime/ref otherwise moved about 13,500 edges
// from System.Private.CoreLib onto stubs, and dropped calls through Span and ReadOnlySpan.
func TestReferenceAssemblyStubDoesNotShadowImplementation(t *testing.T) {
	impl := []byte(`
namespace System;
public readonly partial struct Span
{
    public Span Slice(int start) => this;
}
`)
	stub := []byte(`
namespace System;
public readonly partial struct Span
{
    public Span Slice(int start) { throw null; }
}
`)
	caller := []byte(`
namespace App;
public class Reader
{
    public void Read(Span s) { var t = new Span(); }
}
`)
	for _, order := range []string{"stub first", "implementation first"} {
		t.Run(order, func(t *testing.T) {
			var ff []facts.Fact
			implFacts := extractFileAST(impl, "lib/CoreLib/src/System/Span.cs")
			stubFacts := extractFileAST(stub, "lib/System.Runtime/ref/System.Runtime.cs")
			if order == "stub first" {
				ff = append(append(ff, stubFacts...), implFacts...)
			} else {
				ff = append(append(ff, implFacts...), stubFacts...)
			}
			ff = append(ff, extractFileAST(caller, "app/Reader.cs")...)
			ff = mergePartialTypes(ff)
			resolveCSharpTargets(ff)

			read := factByName(ff, "app.Reader.Read")
			if read == nil {
				t.Fatalf("missing app.Reader.Read; got %v", names(ff, facts.KindSymbol))
			}
			if !hasRel(read, facts.RelInstantiates, "lib/CoreLib/src/System.Span") {
				t.Errorf("new Span() does not bind to the implementation; got %v", read.Relations)
			}
			if hasRel(read, facts.RelInstantiates, "lib/System.Runtime/ref.Span") {
				t.Errorf("new Span() binds to the reference-assembly stub; got %v", read.Relations)
			}
		})
	}
}

func TestInReferenceAssembly(t *testing.T) {
	for file, want := range map[string]bool{
		"src/libraries/System.Runtime/ref/System.Runtime.cs": true,
		"ref/Api.cs": true,
		"src/libraries/System.Runtime/src/System/Span.cs": false,
		"src/prefix/Api.cs": false,
		"src/Refs/Api.cs":   false,
	} {
		if got := inReferenceAssembly(file); got != want {
			t.Errorf("inReferenceAssembly(%q) = %v, want %v", file, got, want)
		}
	}
}
