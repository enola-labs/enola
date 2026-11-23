package kmpswift

import (
	"context"
	"slices"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func kotlinFn(name, file, framework, facade string) facts.Fact {
	return facts.Fact{Kind: facts.KindSymbol, Name: name, File: file, Repo: "app", Props: map[string]any{
		"symbol_kind": facts.SymbolFunc, "language": "kotlin",
		facts.PropKMPFramework: framework, facts.PropKotlinFacade: facade,
	}}
}

func swiftCaller(name, file string, calls ...string) facts.Fact {
	return facts.Fact{Kind: facts.KindSymbol, Name: name, File: file, Repo: "app", Props: map[string]any{
		"symbol_kind": facts.SymbolMethod, "language": "swift", facts.PropKotlinFacadeCalls: calls,
	}}
}

func swiftImport(file, module string) facts.Fact {
	return facts.Fact{Kind: facts.KindDependency, Name: "ios -> " + module, File: file, Repo: "app",
		Props:     map[string]any{"language": "swift"},
		Relations: []facts.Relation{{Kind: facts.RelImports, Target: module}}}
}

func callsOf(store *facts.Store, name string) []string {
	var out []string
	for _, f := range store.FactsRef() {
		if f.Name != name {
			continue
		}
		for _, r := range f.Relations {
			if r.Kind == facts.RelCalls {
				out = append(out, r.Target)
			}
		}
	}
	slices.Sort(out)
	return out
}

// TestKMPSwiftCallBindsThroughTheImportedFramework: a Swift call on a facade
// binds to the function of the framework its file imports, including a name
// Kotlin/Native prefixed with `do`; a file importing no such framework, a
// facade of another framework, and an ambiguous member bind nothing.
func TestKMPSwiftCallBindsThroughTheImportedFramework(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		kotlinFn("shared/src/iosMain/kotlin/app.MainViewController", "shared/src/iosMain/kotlin/app/main.ios.kt", "shared", "Main_iosKt"),
		kotlinFn("shared/src/iosMain/kotlin/app.initApp", "shared/src/iosMain/kotlin/app/main.ios.kt", "shared", "Main_iosKt"),
		kotlinFn("other/src/iosMain/kotlin/o.helper", "other/src/iosMain/kotlin/o/Util.kt", "other", "UtilKt"),
		kotlinFn("shared/src/iosMain/kotlin/a.dup", "shared/src/iosMain/kotlin/a/Util.kt", "shared", "UtilKt"),
		kotlinFn("shared/src/iosMain/kotlin/b.dup", "shared/src/iosMain/kotlin/b/Util.kt", "shared", "UtilKt"),
		swiftImport("ios/ContentView.swift", "shared"),
		swiftCaller("ios.ContentView.make", "ios/ContentView.swift",
			"Main_iosKt.MainViewController", "Main_iosKt.doInitApp", "Main_iosKt.initApp", "UtilKt.helper", "UtilKt.dup"),
		swiftCaller("ios.Other.make", "ios/Other.swift", "Main_iosKt.MainViewController"),
	)
	b := New()
	for range 2 { // idempotent
		if err := b.Bind(context.Background(), store); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"shared/src/iosMain/kotlin/app.MainViewController", "shared/src/iosMain/kotlin/app.initApp"}
	if got := callsOf(store, "ios.ContentView.make"); !slices.Equal(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	if got := callsOf(store, "ios.Other.make"); len(got) != 0 {
		t.Fatalf("a file importing no framework bound %v", got)
	}
}

func TestObjCExportName(t *testing.T) {
	for in, want := range map[string]string{
		"initApp": "doInitApp", "init": "doInit", "newsFeed": "newsFeed", "newItem": "doNewItem",
		"copy": "doCopy", "copyright": "copyright", "mutableCopyOf": "doMutableCopyOf", "handle": "handle",
	} {
		if got := objcExportName(in); got != want {
			t.Errorf("objcExportName(%q) = %q, want %q", in, got, want)
		}
	}
}
