package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/enola-labs/enola/internal/config"
)

// A file's size and time are recorded with its hash, so a later run can take an
// untouched file as unchanged without reading it. One modified in the last moments
// before it was read gets no time: a change later in the same second would keep the
// recorded second, and must not be mistaken for the content hashed.
func TestHashFilesWithStat_RecentFileHasNoTime(t *testing.T) {
	dir := t.TempDir()
	old, fresh := filepath.Join(dir, "old.go"), filepath.Join(dir, "fresh.go")
	for _, p := range []string{old, fresh} {
		if err := os.WriteFile(p, []byte("package x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	e, err := New(config.Default())
	if err != nil {
		t.Fatal(err)
	}
	hashes, stats := e.hashFilesWithStat(context.Background(), dir, []string{"old.go", "fresh.go"})
	if hashes["old.go"] == "" || hashes["fresh.go"] == "" {
		t.Fatalf("files not hashed: %v", hashes)
	}
	if st := stats["old.go"]; st.mod != past.UTC().Format(time.RFC3339) || st.size != int64(len("package x\n")) {
		t.Errorf("old.go recorded %+v", st)
	}
	if st := stats["fresh.go"]; st.mod != "" {
		t.Errorf("a file modified just now was recorded with a time: %+v", st)
	}
}
