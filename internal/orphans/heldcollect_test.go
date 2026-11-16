package orphans

import (
	"context"
	"reflect"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// Explain after Annotate reuses Annotate's collect pass once, and the reuse is
// invisible: the insights match a fresh Explainer's, and a different store, or the
// same store after it grew, is collected again.
func TestExplain_ReusesAnnotateCollectOnce(t *testing.T) {
	ctx := context.Background()
	store := facts.NewStore()
	store.Add(sym("pkg.DeadFunc", "function"), sym("pkg.DeadStruct", "struct"))

	want, err := (&Explainer{}).Explain(ctx, store)
	if err != nil {
		t.Fatal(err)
	}

	e := &Explainer{}
	if err := e.Annotate(ctx, store); err != nil {
		t.Fatal(err)
	}
	if e.held == nil {
		t.Fatal("Annotate kept nothing for Explain")
	}
	got, err := e.Explain(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Explain after Annotate differs from a fresh Explain:\n got %+v\nwant %+v", got, want)
	}
	if e.held != nil {
		t.Error("the held collect pass was not released after one use")
	}

	if err := e.Annotate(ctx, store); err != nil {
		t.Fatal(err)
	}
	store.Add(sym("pkg.AlsoDead", "function"))
	grown, err := e.Explain(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := (&Explainer{}).Explain(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(grown, fresh) {
		t.Error("Explain served a collect pass held for a smaller store")
	}

	if err := e.Annotate(ctx, store); err != nil {
		t.Fatal(err)
	}
	other := facts.NewStore()
	other.Add(sym("pkg.Other", "function"))
	gotOther, err := e.Explain(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	wantOther, err := (&Explainer{}).Explain(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotOther, wantOther) {
		t.Error("Explain served a collect pass held for another store")
	}
}
