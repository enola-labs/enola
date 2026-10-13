package httphandler

import (
	"context"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func class(name string) facts.Fact {
	return facts.Fact{Kind: facts.KindSymbol, Name: name, Repo: "r",
		Props: map[string]any{"symbol_kind": facts.SymbolClass}}
}

func page(path, file, target string) facts.Fact {
	return facts.Fact{Kind: facts.KindRoute, Name: path, File: file, Repo: "r",
		Props:     map[string]any{"method": "GET", "handler": target},
		Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: "lib"}, {Kind: facts.RelHandledBy, Target: target}}}
}

// settled binds the facts and returns route path -> handled_by target, "" for none.
func settled(t *testing.T, ff ...facts.Fact) map[string]string {
	t.Helper()
	store := facts.NewStore()
	store.Add(ff...)
	if err := New().Bind(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, f := range store.ByKind(facts.KindRoute) {
		out[f.Name] = ""
		for _, r := range f.Relations {
			if r.Kind == facts.RelHandledBy {
				out[f.Name] = r.Target
			}
		}
		if !f.HasRelation(facts.RelDeclares, "lib") {
			t.Errorf("%s lost its declares relation", f.Name)
		}
	}
	return out
}

func TestSettleTargets_BareNameResolvesToItsOnlyType(t *testing.T) {
	got := settled(t,
		class("lib/screens.HomeScreen"),
		page("/", "lib/routing/router.dart", "HomeScreen"),
		page("/gone", "lib/routing/router.dart", "PackagePage"),
	)
	if got["/"] != "lib/screens.HomeScreen" {
		t.Errorf("/ handled_by = %q, want lib/screens.HomeScreen", got["/"])
	}
	if got["/gone"] != "" {
		t.Errorf("/gone handled_by = %q, want none: nothing declares PackagePage", got["/gone"])
	}
}

func TestSettleTargets_BareNameTakesTheNearestOfSeveral(t *testing.T) {
	got := settled(t,
		class("examples/app/lib/pages.HomePage"),
		class("examples/other/lib/pages.HomePage"),
		class("pkg/example/lib.Start"),
		class("pkg/example/lib/others.Start"),
		page("/start", "pkg/example/lib/main.dart", "Start"),
		class("a/lib.Twin"),
		class("b/lib.Twin"),
		page("/", "examples/app/lib/main.dart", "HomePage"),
		page("/twin", "c/lib/main.dart", "Twin"),
	)
	if got["/"] != "examples/app/lib/pages.HomePage" {
		t.Errorf("/ handled_by = %q, want the HomePage of its own app", got["/"])
	}
	if got["/start"] != "pkg/example/lib.Start" {
		t.Errorf("/start handled_by = %q, want the Start of its own directory", got["/start"])
	}
	if got["/twin"] != "" {
		t.Errorf("/twin handled_by = %q, want none: neither Twin is nearer", got["/twin"])
	}
}

func TestSettleTargets_PackageNameResolvesBelowThePackage(t *testing.T) {
	got := settled(t,
		class("libs/components/src/anon-layout.AnonLayoutWrapperComponent"),
		class("apps/web/src.AnonLayoutWrapperComponent"),
		class("src/app/pages.Declared"),
		page("/", "apps/web/src/app/routing.ts", "libs/components/src.AnonLayoutWrapperComponent"),
		page("/lib", "src/app/routing.ts", "src/app.NbLoginComponent"),
		page("/kept", "src/app/routing.ts", "src/app/pages.Declared"),
	)
	if want := "libs/components/src/anon-layout.AnonLayoutWrapperComponent"; got["/"] != want {
		t.Errorf("/ handled_by = %q, want %q", got["/"], want)
	}
	if got["/lib"] != "" {
		t.Errorf("/lib handled_by = %q, want none: the component comes from a dependency", got["/lib"])
	}
	if got["/kept"] != "src/app/pages.Declared" {
		t.Errorf("/kept handled_by = %q, want it untouched", got["/kept"])
	}
}
