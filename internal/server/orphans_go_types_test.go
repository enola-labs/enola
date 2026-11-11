package server_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A Go type is referenced by its import-path name, from a field, a map key, a
// struct literal or a field in another package. Each of those keeps it alive; a
// struct nothing mentions is still reported.
func TestFindOrphans_GoTypesUsedByOtherSymbolsAreLive(t *testing.T) {
	dir := t.TempDir()
	for rel, body := range map[string]string{
		"go.mod": "module example.com/library\n\ngo 1.22\n",
		"internal/shelf/cache.go": `package shelf

type key struct{ id int }

type Settings struct{ Size int }

type forgotten struct{ note string }

type Cache struct{ m map[key]string }

func (c *Cache) Get(id int) string { return c.m[key{id: id}] }
`,
		"internal/config/config.go": `package config

import "example.com/library/internal/shelf"

type Config struct{ Shelf shelf.Settings }
`,
		"main.go": `package main

import (
	"example.com/library/internal/config"
	"example.com/library/internal/shelf"
)

func main() {
	c := &shelf.Cache{}
	_ = c.Get(config.Config{}.Shelf.Size)
}
`,
	} {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s := startInMemory(t)
	if res := s.call(t, "generate_snapshot", map[string]any{"repo_path": dir}); res.IsError {
		t.Fatal(text(res))
	}
	out := text(s.call(t, "find_orphans", map[string]any{"output_mode": "compact", "mode": "both"}))
	for _, live := range []string{"shelf.key ", "shelf.Settings "} {
		if strings.Contains(out, live) {
			t.Errorf("%sis used by another symbol but reported as an orphan:\n%s", live, out)
		}
	}
	if !strings.Contains(out, "shelf.forgotten ") {
		t.Errorf("shelf.forgotten is used by nothing but not reported:\n%s", out)
	}
}
