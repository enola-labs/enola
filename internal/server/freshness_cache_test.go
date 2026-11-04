package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/enola-labs/enola/internal/config"
	"github.com/enola-labs/enola/internal/engine"
	"github.com/enola-labs/enola/internal/facts"
)

func writeRestorable(t *testing.T, id string, at time.Time) string {
	t.Helper()
	dir := t.TempDir()
	line, _ := json.Marshal(facts.Fact{Kind: facts.KindModule, Name: "m", File: "m"})
	if err := os.WriteFile(filepath.Join(dir, "facts.jsonl"), append(line, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	meta, _ := json.Marshal(facts.SnapshotMeta{SnapshotID: id, GeneratedAt: at.UTC().Format(time.RFC3339)})
	if err := os.WriteFile(filepath.Join(dir, "snapshot.meta.json"), meta, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// The banner is cached for a few seconds because it shells out to git. The cache was not
// tied to the graph, so the calls right after generate_snapshot were told the graph they
// had just replaced was a day old.
func TestFreshnessBanner_NewSnapshotIsNotServedTheOldBanner(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	eng, err := engine.New(config.Default())
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{eng: eng}

	if err := eng.RestoreFromDir(writeRestorable(t, "old", time.Now().Add(-48*time.Hour)), nil, "r"); err != nil {
		t.Fatal(err)
	}
	if b := s.freshnessBanner(); !strings.Contains(b, "old") {
		t.Fatalf("a two-day-old graph must be flagged, got %q", b)
	}

	if err := eng.RestoreFromDir(writeRestorable(t, "new", time.Now()), nil, "r"); err != nil {
		t.Fatal(err)
	}
	if b := s.freshnessBanner(); b != "" {
		t.Errorf("a graph loaded a moment ago got the previous graph's banner: %q", b)
	}
}
