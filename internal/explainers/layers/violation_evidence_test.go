package layers

import (
	"context"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A layer violation cites four entities: the import edge, its raw target, and
// the two modules. Only the first of them lives in the file the violation was
// found in, and only it may carry that file.
//
// It carried none, which cost twice. A reader got a name with no location. And a
// consumer could not tell WHICH import edge was meant: one module importing the
// same target from several files produces several dependency facts under one
// name — 459 such names in one measured repository — so the citation resolved to
// a set rather than to a fact, and was dropped.
func TestLayerViolation_CitesTheImportEdgeWithItsFile(t *testing.T) {
	const importFile = "app/domain/thing.go"
	store := facts.NewStore()
	store.Add(
		layerIntent("svc", "handlers", 0, "app/handlers/**"),
		layerIntent("svc", "domain", 1, "app/domain/**"),
		facts.Fact{Kind: facts.KindModule, Repo: "svc", Name: "app/handlers"},
		facts.Fact{Kind: facts.KindModule, Repo: "svc", Name: "app/domain"},
		facts.Fact{Kind: facts.KindDependency, Repo: "svc", Name: "domain-imports-handlers",
			File:      importFile,
			Relations: []facts.Relation{{Kind: facts.RelImports, Target: "app/handlers"}}},
	)

	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}

	var violation *facts.Insight
	for i := range insights {
		if strings.Contains(strings.ToLower(insights[i].Title), "violation") {
			violation = &insights[i]
		}
	}
	if violation == nil {
		t.Fatal("no layer violation reported: the fixture stopped exercising this")
	}

	byName := map[string]facts.Evidence{}
	for _, e := range violation.Evidence {
		if e.Fact != "" {
			byName[e.Fact] = e
		}
	}

	edge, ok := byName["domain-imports-handlers"]
	if !ok {
		t.Fatal("the violation does not cite the import edge at all")
	}
	if edge.File != importFile {
		t.Errorf("the import edge is cited with file %q, want %q", edge.File, importFile)
	}

	// The other three are declared elsewhere. Claiming they live in the importing
	// file would point a reader at the wrong place, and would make the citation
	// resolve to the wrong fact or to none.
	for _, name := range []string{"app/handlers", "app/domain"} {
		if e, ok := byName[name]; ok && e.File == importFile {
			t.Errorf("%q is cited with the importing file %q; it is declared elsewhere", name, importFile)
		}
	}
}
