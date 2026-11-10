package layers

import (
	"context"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func layerIntent(repo, name string, order int, paths ...string) facts.Fact {
	return facts.Fact{Kind: facts.KindIntent, Repo: repo, Name: "layer " + name,
		Props: map[string]any{"intent_kind": "layer", "layer_name": name, "order": order, "paths": paths}}
}

func TestDeclaredLayers_WrongDirectionImportIsProofClass(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		layerIntent("svc", "handlers", 0, "app/handlers/**"),
		layerIntent("svc", "domain", 1, "app/domain/**"),
		facts.Fact{Kind: facts.KindModule, Repo: "svc", Name: "svc/app/handlers"},
		facts.Fact{Kind: facts.KindModule, Repo: "svc", Name: "svc/app/domain"},
		facts.Fact{Kind: facts.KindDependency, Repo: "svc", Name: "domain-imports-handlers",
			File:      "svc/app/domain/thing.go",
			Relations: []facts.Relation{{Kind: facts.RelImports, Target: "svc/app/handlers"}}},
	)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	var pattern, violation *facts.Insight
	for i := range insights {
		if strings.Contains(insights[i].Title, "declared (svc)") {
			pattern = &insights[i]
		}
		if strings.Contains(strings.ToLower(insights[i].Title), "violation") {
			violation = &insights[i]
		}
	}
	if pattern == nil || pattern.Confidence != 1.0 {
		t.Fatalf("declared pattern missing or not exact: %+v", insights)
	}
	if violation == nil || violation.Confidence != 1.0 {
		t.Fatalf("inner-imports-outer against a declaration must be a proof-class violation: %+v", insights)
	}
}

func TestDeclaredLayers_GlobDialectIsBounded(t *testing.T) {
	if !matchDeclaredLayerPath("app/handlers/api", []string{"app/handlers/**"}) {
		t.Fatal("subtree glob must match nested modules")
	}
	if !matchDeclaredLayerPath("app/handlers", []string{"app/handlers/**"}) {
		t.Fatal("subtree glob must match the prefix itself")
	}
	if matchDeclaredLayerPath("app/handlers2", []string{"app/handlers/**"}) {
		t.Fatal("prefix match must respect segment boundaries")
	}
	if !matchDeclaredLayerPath("lib/core", []string{"lib/core"}) {
		t.Fatal("exact form must match exactly")
	}
	if matchDeclaredLayerPath("lib/core/sub", []string{"lib/core"}) {
		t.Fatal("exact form must not match the subtree")
	}
}

// A declaration governs the codebase it was written for. Module names are
// repo-relative, so two repositories in one union both carry a module called
// app/domain — and the declaring repo's pattern then classified the OTHER
// repo's file, verdicting its wrong-direction import at confidence 1.0. That is
// exactly what a 22-repo union reported: one violation whose only evidence
// file was monolith/app/models/_coupling.rb, under the declaration of a
// different repository entirely.
func TestDeclaredLayers_VerdictsOnlyTheDeclaringRepo(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		layerIntent("svc", "handlers", 0, "app/handlers/**"),
		layerIntent("svc", "domain", 1, "app/domain/**"),
		facts.Fact{Kind: facts.KindModule, Repo: "svc", Name: "app/handlers"},
		facts.Fact{Kind: facts.KindModule, Repo: "svc", Name: "app/domain"},
		facts.Fact{Kind: facts.KindModule, Repo: "other", Name: "app/handlers"},
		facts.Fact{Kind: facts.KindModule, Repo: "other", Name: "app/domain"},
		facts.Fact{Kind: facts.KindDependency, Repo: "other", Name: "other-domain-imports-handlers",
			File:      "other/app/domain/thing.go",
			Relations: []facts.Relation{{Kind: facts.RelImports, Target: "app/handlers"}}},
	)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	for _, insight := range insights {
		if !strings.Contains(strings.ToLower(insight.Title), "violation") {
			continue
		}
		for _, ev := range insight.Evidence {
			if strings.HasPrefix(ev.File, "other/") {
				t.Fatalf("svc's declaration verdicted a file in other: %q (%+v)", ev.File, insight)
			}
		}
	}
}

// A Go package is named by its import path and declared by its directory: the
// declaration's paths classify it, and a wrong-direction import between two Go
// packages is still the proof-class violation it was when packages were named by
// directory.
func TestDeclaredLayers_GoPackagesAreClassifiedByDirectory(t *testing.T) {
	goPkg := func(name, file string) facts.Fact {
		return facts.Fact{Kind: facts.KindModule, Repo: "svc", Name: name, File: file, Props: map[string]any{"language": "go"}}
	}
	store := facts.NewStore()
	store.Add(
		layerIntent("svc", "handlers", 0, "app/handlers/**"),
		layerIntent("svc", "domain", 1, "app/domain/**"),
		goPkg("example.com/svc/app/handlers", "app/handlers"),
		goPkg("example.com/svc/app/domain", "app/domain"),
		facts.Fact{Kind: facts.KindDependency, Repo: "svc", Name: "example.com/svc/app/domain -> example.com/svc/app/handlers",
			File:      "app/domain/thing.go",
			Relations: []facts.Relation{{Kind: facts.RelImports, Target: "example.com/svc/app/handlers"}}},
	)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	var violation bool
	for _, in := range insights {
		if strings.Contains(in.Title, "classifies no modules") {
			t.Errorf("a declared layer matched no Go package: %s", in.Title)
		}
		if strings.Contains(strings.ToLower(in.Title), "violation") && in.Confidence == 1.0 {
			violation = true
		}
	}
	if !violation {
		t.Errorf("the domain package importing handlers was not reported: %+v", insights)
	}
}
