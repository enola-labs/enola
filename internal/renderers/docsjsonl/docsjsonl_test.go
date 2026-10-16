package docsjsonl

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/pkg/plugin"
)

func render(t *testing.T, store *facts.Store) (facts.Artifact, []Line) {
	t.Helper()
	var r plugin.Renderer = New()
	sr, ok := r.(plugin.StoreRenderer)
	if !ok {
		t.Fatal("the docs renderer must read the store")
	}
	artifacts, err := sr.RenderStore(context.Background(), &facts.Snapshot{}, store)
	if err != nil || len(artifacts) != 1 {
		t.Fatalf("artifacts = %v, err = %v", artifacts, err)
	}
	var lines []Line
	sc := bufio.NewScanner(bytes.NewReader(artifacts[0].Content))
	for sc.Scan() {
		var l Line
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatalf("each line must be one JSON object: %v\n%s", err, sc.Bytes())
		}
		lines = append(lines, l)
	}
	return artifacts[0], lines
}

// Every documented fact is one line, joined to facts.jsonl by id, with the whole
// comment when the store holds one and the doc when that is all of it. An
// undocumented fact is not listed, and a repeated fact is one line.
func TestRenderStore_WritesEachDocumentedFact(t *testing.T) {
	sym := func(name, doc string) facts.Fact {
		f := facts.Fact{Kind: facts.KindSymbol, Name: name, File: "shop/order.go", Line: 3}
		if doc != "" {
			f.SetProp(facts.PropDoc, doc)
		}
		return f
	}
	place := sym("shop.Place", "Place takes an order.")
	place.SetProp(facts.PropDocText, "Place takes an order.\n\nIt charges the card first.")
	store := facts.NewStore()
	store.Add(place, sym("shop.Cancel", "Cancel drops an order."), sym("shop.bare", ""), sym("shop.Cancel", "Cancel drops an order."),
		facts.Fact{Kind: facts.KindModule, Name: "shop", File: "shop", Props: map[string]any{facts.PropDoc: "Package shop sells."}})
	store.SetRepoRange(0, "shop-repo")
	store.TakeDocTexts(0)

	artifact, lines := render(t, store)
	if artifact.Name != ArtifactName {
		t.Errorf("artifact name = %q", artifact.Name)
	}
	byName := map[string]Line{}
	for _, l := range lines {
		byName[l.Name] = l
	}
	if len(lines) != 3 {
		t.Fatalf("lines = %v, want one each for shop.Place, shop.Cancel and shop", lines)
	}
	if got := byName["shop.Place"].Doc; got != "Place takes an order.\n\nIt charges the card first." {
		t.Errorf("Place doc = %q, want the whole comment", got)
	}
	if got := byName["shop.Cancel"].Doc; got != "Cancel drops an order." {
		t.Errorf("Cancel doc = %q", got)
	}
	if l := byName["shop"]; l.Kind != facts.KindModule || l.Doc != "Package shop sells." {
		t.Errorf("module line = %+v", l)
	}
	if l := byName["shop.Place"]; l.ID != facts.FactID("shop-repo", facts.KindSymbol, "shop.Place", "shop/order.go") || l.Repo != "shop-repo" || l.Line != 3 {
		t.Errorf("Place line = %+v", l)
	}
	for i := 1; i < len(lines); i++ {
		if lines[i-1].ID > lines[i].ID {
			t.Errorf("lines are not sorted by id: %s before %s", lines[i-1].ID, lines[i].ID)
		}
	}
}

// A snapshot with nothing documented still gets the file, empty.
func TestRenderStore_NothingDocumentedIsAnEmptyFile(t *testing.T) {
	store := facts.NewStore()
	store.Add(facts.Fact{Kind: facts.KindSymbol, Name: "a.F", File: "a/a.go"})
	artifact, lines := render(t, store)
	if len(lines) != 0 || len(artifact.Content) != 0 {
		t.Errorf("content = %q", artifact.Content)
	}
}
