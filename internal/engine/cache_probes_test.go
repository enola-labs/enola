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
	c.putProbed("ts", repo, []string{"vendor/x.ts"}, nil, ff)
	c.putProbed("go", repo, nil, nil, []facts.Fact{{Kind: facts.KindSymbol, Name: "G"}})
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

// A file the extractor read outside its files keys its facts by content: an edit or
// a deletion is a miss, an unchanged file is a hit.
func TestExtractorCache_ReadPathContentKeysTheFacts(t *testing.T) {
	repo := t.TempDir()
	path := filepath.Join(t.TempDir(), "extractor_cache.json")
	theme := filepath.Join(repo, "vendor", "theme.ts")
	if err := os.MkdirAll(filepath.Dir(theme), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(theme, []byte(`export const paper = "#fff"`), 0o644); err != nil {
		t.Fatal(err)
	}
	ff := []facts.Fact{{Kind: facts.KindSymbol, Name: "tailwind/colors.paper"}}

	c := loadExtractorCache(path, true)
	c.putProbed("ts", repo, nil, []string{"vendor/theme.ts"}, ff)
	if err := c.save(); err != nil {
		t.Fatal(err)
	}

	warm := loadExtractorCache(path, true)
	if _, hit := warm.getProbed("ts", repo); !hit {
		t.Fatal("unchanged read file: cached facts not reused")
	}
	if err := warm.save(); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(theme, []byte(`export const paper = "#123"`), 0o644); err != nil {
		t.Fatal(err)
	}
	edited := loadExtractorCache(path, true)
	if _, hit := edited.getProbed("ts", repo); hit {
		t.Error("the read file was edited, yet the facts extracted from its old content were served")
	}
	edited.discard()

	if err := os.Remove(theme); err != nil {
		t.Fatal(err)
	}
	deleted := loadExtractorCache(path, true)
	defer deleted.discard()
	if _, hit := deleted.getProbed("ts", repo); hit {
		t.Error("the read file was deleted, yet the facts extracted from it were served")
	}
}
