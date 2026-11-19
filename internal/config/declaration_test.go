package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A repository declaration handed over as a config used to load as an empty config, so
// every command reading it answered about defaults while the caller believed it had read
// their rules. It is refused, naming what the file is and what to pass instead.
func TestLoadRefusesADeclaration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "draft.yaml")
	if err := os.WriteFile(path, []byte("layers:\n  - {name: web, paths: [\"web/**\"]}\nrules: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("a declaration loaded as a config")
	}
	for _, want := range []string{"layers:", "rules:", "enola-intent.yaml", "--declaration"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}

// A config that declares intent where it belongs, under intent:, still loads.
func TestLoadAcceptsIntentUnderItsKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp-arch.yaml")
	cfg := "repos:\n  - a\nintent:\n  a:\n    layers:\n      - {name: web, paths: [\"web/**\"]}\n"
	if err := os.WriteFile(path, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("a config with intent under its key was refused: %v", err)
	}
}
