package sqlextractor

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestExtractDDLTables(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "001.sql"), []byte("CREATE TABLE IF NOT EXISTS traces (id UUID);\nALTER TABLE traces ADD COLUMN name text;\nCREATE TABLE `events` (id int);"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := New().Extract(context.Background(), dir, []string{"001.sql"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d storage facts, want 2: %#v", len(got), got)
	}
	if got[0].Props["table"] != "events" || got[1].Props["table"] != "traces" {
		t.Fatalf("tables = %v, %v", got[0].Props["table"], got[1].Props["table"])
	}
}
