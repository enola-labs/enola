package engine_test

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/enola-labs/enola/internal/config"
	"github.com/enola-labs/enola/internal/engine"
	"github.com/enola-labs/enola/internal/explainers/cycles"
	"github.com/enola-labs/enola/internal/extractors/goextractor"
)

// The fact store shares structurally identical Props maps and Relations slices once
// frozen, which is sound only for a store nothing writes again. These tests pin WHERE
// that happens: at publication, on both paths that publish. Freezing earlier would put
// shared maps under the binders and annotators that write props in place; not freezing
// at all would silently drop the saving, which no behavioural test would notice.

func freezeTestRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	writeFile(t, filepath.Join(repo, "go.mod"), "module testmod\n\ngo 1.21\n")
	writeFile(t, filepath.Join(repo, "pkg", "a", "a.go"),
		"package a\n\nimport \"testmod/pkg/b\"\n\nfunc A() { b.B() }\n")
	writeFile(t, filepath.Join(repo, "pkg", "b", "b.go"),
		"package b\n\nimport \"testmod/pkg/a\"\n\nfunc B() { _ = a.A }\n")
	return repo
}

func freezeTestEngine(t *testing.T) *engine.Engine {
	t.Helper()
	eng, err := engine.New(config.Default())
	if err != nil {
		t.Fatal(err)
	}
	eng.RegisterExtractor(goextractor.New())
	eng.RegisterExplainer(cycles.New())
	return eng
}

// TestGenerateSnapshot_PublishesFrozenStore — the generation path.
func TestGenerateSnapshot_PublishesFrozenStore(t *testing.T) {
	eng := freezeTestEngine(t)
	repo := freezeTestRepo(t)

	if _, err := eng.GenerateSnapshot(context.Background(), repo, false); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if !eng.Store().Frozen() {
		t.Error("the published store is not frozen; the per-fact Props and Relations duplicates were never collapsed")
	}
}

// TestRestoreFromDir_PublishesFrozenStore — the restart path, and the one that matters
// most: a restored graph is held for hours without regenerating.
func TestRestoreFromDir_PublishesFrozenStore(t *testing.T) {
	eng := freezeTestEngine(t)
	repo := freezeTestRepo(t)

	if _, err := eng.GenerateSnapshot(context.Background(), repo, false); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if err := eng.WriteArtifacts(repo); err != nil {
		t.Fatalf("write artifacts: %v", err)
	}

	fresh, err := engine.New(config.Default())
	if err != nil {
		t.Fatal(err)
	}
	label := filepath.Base(repo)
	if err := fresh.RestoreFromDir(eng.OutputDir(repo), map[string]string{label: repo}, label); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if !fresh.Store().Frozen() {
		t.Error("the restored store is not frozen")
	}
}

// TestAppendMode_SharesNothingWithThePublishedBundle is the safety property the
// sharing rests on, asserted through the real engine.
//
// An append-mode regeneration carries the previously published bundle's facts forward
// and then lets binders rewrite them — unmatchedroutes deletes a prop, the
// annotators add them. Those facts came from a FROZEN store, where one Props map backs
// ~9 facts, so an aliased carry-forward would let one binder rewrite unrelated facts
// inside a bundle that concurrent MCP readers are still traversing.
//
// It asserts the STRUCTURAL invariant — that the new bundle shares no Props or
// Relations allocation with the old one — rather than watching for an observed
// mutation. Whether any given binder happens to fire on a given fixture is incidental;
// that the carry-forward is a real copy is not, and only the structural form of the
// assertion fails when the copy is removed.
func TestAppendMode_SharesNothingWithThePublishedBundle(t *testing.T) {
	eng := freezeTestEngine(t)
	repoA := freezeTestRepo(t)
	repoB := freezeTestRepo(t)

	if _, err := eng.GenerateSnapshot(context.Background(), repoA, false); err != nil {
		t.Fatalf("generate A: %v", err)
	}

	// Record the identity of every Props map and Relations array in the published
	// bundle, before anything can carry them forward.
	oldProps := map[uintptr]string{}
	oldRels := map[uintptr]string{}
	published := eng.Store().FactsRef()
	if len(published) == 0 {
		t.Fatal("precondition: repo A produced no facts")
	}
	propsSeen, relsSeen := 0, 0
	for _, f := range published {
		if len(f.Props) > 0 {
			oldProps[reflect.ValueOf(f.Props).Pointer()] = f.Name
			propsSeen++
		}
		if len(f.Relations) > 0 {
			oldRels[reflect.ValueOf(f.Relations).Pointer()] = f.Name
			relsSeen++
		}
	}
	if propsSeen == 0 || relsSeen == 0 {
		t.Fatalf("precondition: fixture must produce facts with props (%d) and relations (%d)", propsSeen, relsSeen)
	}

	if _, err := eng.GenerateSnapshot(context.Background(), repoB, true); err != nil {
		t.Fatalf("append B: %v", err)
	}

	for _, f := range eng.Store().FactsRef() {
		if len(f.Props) > 0 {
			if owner, shared := oldProps[reflect.ValueOf(f.Props).Pointer()]; shared {
				t.Fatalf("fact %q in the new bundle aliases the Props map of %q in the PUBLISHED bundle; "+
					"a binder writing to it would rewrite the old bundle and every fact sharing that map",
					f.Name, owner)
			}
		}
		if len(f.Relations) > 0 {
			if owner, shared := oldRels[reflect.ValueOf(f.Relations).Pointer()]; shared {
				t.Fatalf("fact %q in the new bundle aliases the Relations array of %q in the PUBLISHED bundle",
					f.Name, owner)
			}
		}
	}
}
