package crossrepo_test

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/linkers/crossrepo"
	deploymentsignal "github.com/enola-labs/enola/internal/linkers/crossrepo/signals/deployment"
	graphqlsignal "github.com/enola-labs/enola/internal/linkers/crossrepo/signals/graphqlsig"
	httpsignal "github.com/enola-labs/enola/internal/linkers/crossrepo/signals/http"
	importsignal "github.com/enola-labs/enola/internal/linkers/crossrepo/signals/imports"
	kafkasignal "github.com/enola-labs/enola/internal/linkers/crossrepo/signals/kafka"
	sharedcodesignal "github.com/enola-labs/enola/internal/linkers/crossrepo/signals/sharedcode"
	"github.com/enola-labs/enola/internal/linkers/vocab"
	"github.com/enola-labs/enola/pkg/plugin"
)

// The engine hands ComputeLinks the store's own facts, not a copy, so neither the
// linker nor any signal may write one: a Props entry, a relation or a field. Run over
// every committed multi-repository graph, the facts must serialize to the same bytes
// after linking as before.
func TestComputeLinks_LeavesItsInputAsItWas(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "..", "engine", "testdata", "golden", "*multirepo.facts.jsonl"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no multi-repository goldens found: %v", err)
	}
	v := vocab.Default()
	signals := []plugin.CrossRepoSignal{
		httpsignal.New(v), importsignal.New(), kafkasignal.New(v),
		graphqlsignal.New(), deploymentsignal.New(), sharedcodesignal.New(v),
	}
	linked := 0
	for _, path := range paths {
		store := facts.NewStore()
		if err := store.ReadJSONLFile(path); err != nil {
			t.Fatal(err)
		}
		var before bytes.Buffer
		if err := store.WriteJSONL(&before); err != nil {
			t.Fatal(err)
		}
		links := crossrepo.ComputeLinks(store.FactsRef(), nil, signals, v)
		linked += len(links)
		var after bytes.Buffer
		if err := store.WriteJSONL(&after); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before.Bytes(), after.Bytes()) {
			t.Errorf("%s: linking changed the facts it read", filepath.Base(path))
		}
	}
	if linked == 0 {
		t.Fatal("no fixture produced a link, so nothing was exercised")
	}
}
