package orphans

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// TestCollectAgainstBundledSnapshot is an integration smoke test that runs the
// store adapter against the repo's bundled .enola/facts.jsonl  It is skipped when the snapshot is absent so the unit
// suite stays hermetic.
func TestCollectAgainstBundledSnapshot(t *testing.T) {
	factsPath := filepath.Join("..", "..", ".enola", "facts.jsonl")
	if _, err := os.Stat(factsPath); err != nil {
		t.Skipf("bundled snapshot not found at %s; skipping", factsPath)
	}

	store := facts.NewStore()
	if err := store.ReadJSONLFile(factsPath); err != nil {
		t.Fatalf("loading facts: %v", err)
	}

	syms, refSources := collect(store)
	if len(syms) == 0 {
		t.Fatal("expected symbols from a real snapshot, got none")
	}

	orphans := classify(syms, refSources, options{Mode: "both", Visibility: "all"})

	// Sanity: a real codebase is not mostly dead code. The conservative matcher
	// should leave the large majority of symbols classed as used.
	if len(orphans) >= len(syms) {
		t.Fatalf("orphans (%d) >= total symbols (%d): matcher likely broken", len(orphans), len(syms))
	}
	t.Logf("snapshot: %d symbols, %d orphans (%.1f%%)",
		len(syms), len(orphans), 100*float64(len(orphans))/float64(len(syms)))

	// Invariants: every reported orphan is genuinely unreferenced by another
	// symbol, and every isolated orphan truly has no outgoing usage edge.
	byName := make(map[string]symInput, len(syms))
	for _, s := range syms {
		byName[s.Name] = s
	}
	for _, o := range orphans {
		sym := byName[o.Name]
		if referencedByOther(sym, refSources) {
			t.Errorf("orphan %q is actually referenced by another symbol", o.Name)
		}
		if o.Class == classIsolated && sym.UsageOut {
			t.Errorf("isolated orphan %q has outgoing usage edges", o.Name)
		}
		if o.Class == classUnreferenced && !sym.UsageOut {
			t.Errorf("unreferenced orphan %q has no outgoing usage edges", o.Name)
		}
	}

	// The 'isolated' subset must be a subset of 'both'.
	iso := classify(syms, refSources, options{Mode: classIsolated, Visibility: "all"})
	if len(iso) > len(orphans) {
		t.Errorf("isolated set (%d) larger than both (%d)", len(iso), len(orphans))
	}

	// Route-handler rescue: a method wired to a route via props["handler"] must
	// not be reported as an orphan (handlers are registered as method values, not
	// called, so without this rescue they would all be false positives). This can
	// only be asserted when the bundled snapshot actually contains handler-bearing
	// routes — which depends on what was indexed. A self-snapshot of these Go MCP
	// repos yields only handler-less OpenAPI spec routes (operationId, no handler),
	// so gate the assertion on the data being present; the rescue itself is covered
	// hermetically by TestCollectFoldsRouteHandler.
	handlerSegs := make(map[string]struct{})
	for _, f := range store.ByKind(facts.KindRoute) {
		if h, _ := f.Props[propHandler].(string); h != "" {
			handlerSegs[lastSeg(h)] = struct{}{}
		}
	}
	if len(handlerSegs) == 0 {
		t.Log("bundled snapshot has no handler-bearing routes; skipping route-handler rescue assertion")
	} else {
		for _, o := range orphans {
			if _, isHandler := handlerSegs[bareName(o.Name)]; isHandler {
				t.Errorf("route-handler %q reported as orphan; rescue not applied", o.Name)
			}
		}
	}

	// confidence=high should yield only functions (the reliably-tracked kind).
	high := classify(syms, refSources, options{Mode: "both", Visibility: "all", Confidence: confHigh})
	for _, o := range high {
		if o.Kind != facts.SymbolFunc {
			t.Errorf("confidence=high returned non-function %q (%s)", o.Name, o.Kind)
		}
	}
}

// TestCollectFoldsModuleUsageEdges verifies the rescue for C/C++ file-scope
// registration macros: the extractor records module_init(foo)/EXPORT_SYMBOL(foo)
// references as call edges on the directory MODULE fact (not a symbol), and
// collect() must fold those in so the registered entry point is not reported as an
// orphan. Without the fold, `drivers/char.chr_dev_init` looks dead.
func TestCollectFoldsModuleUsageEdges(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		facts.Fact{
			Kind: facts.KindSymbol, Name: "drivers/char.chr_dev_init",
			File: "drivers/char/mem.c", Repo: "linux",
			Props:     map[string]any{"symbol_kind": facts.SymbolFunc, "static": true},
			Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: "drivers/char"}},
		},
		facts.Fact{
			Kind: facts.KindSymbol, Name: "drivers/char.truly_dead",
			File: "drivers/char/mem.c", Repo: "linux",
			Props:     map[string]any{"symbol_kind": facts.SymbolFunc, "static": true},
			Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: "drivers/char"}},
		},
		facts.Fact{
			Kind: facts.KindModule, Name: "drivers/char", File: "drivers/char",
			Props:     map[string]any{"language": "c"},
			Relations: []facts.Relation{{Kind: facts.RelCalls, Target: "drivers/char.chr_dev_init"}},
		},
	)

	syms, refSources := collect(store)
	got := orphansByName(classify(syms, refSources, options{Mode: "both", Visibility: "all"}))

	if _, isOrphan := got["drivers/char.chr_dev_init"]; isOrphan {
		t.Errorf("chr_dev_init is referenced via a module-fact call edge; must not be an orphan")
	}
	if _, isOrphan := got["drivers/char.truly_dead"]; !isOrphan {
		t.Errorf("truly_dead has no references; should still be reported as an orphan")
	}
}
