package engine

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A path the extractor looked for on disk keys its facts: while nothing changes they
// are reused, and once the path appears they are not.
func TestExtractorCache_ProbedPathKeysTheFacts(t *testing.T) {
	repo := t.TempDir()
	path := filepath.Join(t.TempDir(), "extractor_cache.json")
	ff := []facts.Fact{{Kind: facts.KindDependency, Name: "vendor/x", Props: map[string]any{"unresolved_reason": "missing-target"}}}

	c := loadExtractorCache(path, true)
	c.putProbed("ts", repo, []string{"vendor/x.ts"}, ff)
	c.putProbed("go", repo, nil, []facts.Fact{{Kind: facts.KindSymbol, Name: "G"}})
	if err := c.save(); err != nil {
		t.Fatal(err)
	}

	warm := loadExtractorCache(path, true)
	if _, hit := warm.getProbed("ts", repo); !hit {
		t.Fatal("unchanged probes: cached facts not reused")
	}
	if _, hit := warm.getProbed("go", repo); !hit {
		t.Fatal("an extractor that probed nothing: cached facts not reused")
	}
	if err := warm.save(); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(repo, "vendor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "vendor", "x.ts"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	after := loadExtractorCache(path, true)
	defer after.discard()
	if _, hit := after.getProbed("ts", repo); hit {
		t.Error("the probed path now exists, yet the facts extracted without it were served")
	}
	if _, hit := after.getProbed("go", repo); !hit {
		t.Error("a probe of another extractor invalidated this one")
	}
}
