package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func memoRepo(t *testing.T, old time.Time) (string, []string) {
	t.Helper()
	repo := t.TempDir()
	var files []string
	for _, name := range []string{"a.go", "b.go", "c.go"} {
		path := filepath.Join(repo, name)
		if err := os.WriteFile(path, []byte("package x\n// "+name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
		files = append(files, name)
	}
	return repo, files
}

func TestDriftHashes_ReadsAFileOnlyAfterItChanged(t *testing.T) {
	requireMemo(t)
	old := time.Now().Add(-time.Hour)
	repo, files := memoRepo(t, old)
	e := settledEngine()

	first := e.driftHashes(context.Background(), repo, files)
	if got := e.drift.reads.Load(); got != 3 {
		t.Fatalf("first walk read %d files, want 3", got)
	}
	second := e.driftHashes(context.Background(), repo, files)
	if got := e.drift.reads.Load(); got != 3 {
		t.Fatalf("second walk read %d files in all, want it to read none again", got)
	}
	for name, hash := range first {
		if second[name] != hash {
			t.Errorf("%s: second walk hashed %s, first %s", name, second[name], hash)
		}
	}

	path := filepath.Join(repo, "b.go")
	if err := os.WriteFile(path, []byte("package x\n// edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, old.Add(time.Minute), old.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	third := e.driftHashes(context.Background(), repo, files)
	if got := e.drift.reads.Load(); got != 4 {
		t.Fatalf("third walk read %d files in all, want exactly the edited one more (4)", got)
	}
	if third["b.go"] == first["b.go"] {
		t.Error("an edited file kept its old hash")
	}
	if third["a.go"] != first["a.go"] || third["c.go"] != first["c.go"] {
		t.Error("an unedited file changed its hash")
	}
}

func TestDriftHashes_ASizeChangeIsSeenWithTheSameModificationTime(t *testing.T) {
	old := time.Now().Add(-time.Hour)
	repo, files := memoRepo(t, old)
	e := settledEngine()
	first := e.driftHashes(context.Background(), repo, files)

	path := filepath.Join(repo, "a.go")
	if err := os.WriteFile(path, []byte("package x\n// a longer line than before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	second := e.driftHashes(context.Background(), repo, files)
	if second["a.go"] == first["a.go"] {
		t.Error("a file that changed size kept its old hash")
	}
}

func TestDriftHashes_ARecentlyModifiedFileIsReadEveryTime(t *testing.T) {
	repo, files := memoRepo(t, time.Now())
	e := &Engine{}
	e.driftHashes(context.Background(), repo, files)
	e.driftHashes(context.Background(), repo, files)
	if got := e.drift.reads.Load(); got != 6 {
		t.Fatalf("two walks of files modified just now read %d files, want 6: a coarse clock cannot vouch for them", got)
	}
}

func TestDriftHashes_AMissingFileHasNoHash(t *testing.T) {
	repo, files := memoRepo(t, time.Now().Add(-time.Hour))
	e := settledEngine()
	e.driftHashes(context.Background(), repo, files)
	if err := os.Remove(filepath.Join(repo, "c.go")); err != nil {
		t.Fatal(err)
	}
	got := e.driftHashes(context.Background(), repo, files)
	if _, there := got["c.go"]; there {
		t.Error("a removed file kept a hash")
	}
}

func TestDriftHashes_AnEditThatRestoresSizeAndModificationTimeIsSeen(t *testing.T) {
	old := time.Now().Add(-time.Hour)
	repo, files := memoRepo(t, old)
	e := settledEngine()
	first := e.driftHashes(context.Background(), repo, files)

	path := filepath.Join(repo, "a.go")
	if err := os.WriteFile(path, []byte("package x\n// A.go\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	second := e.driftHashes(context.Background(), repo, files)
	if second["a.go"] == first["a.go"] {
		t.Error("a file edited to the same size and given back its modification time kept its old hash")
	}
}

func TestDriftHashes_AReplacedFileIsSeenWithTheSameSizeAndModificationTime(t *testing.T) {
	old := time.Now().Add(-time.Hour)
	repo, files := memoRepo(t, old)
	e := settledEngine()
	first := e.driftHashes(context.Background(), repo, files)

	path := filepath.Join(repo, "a.go")
	next := path + ".new"
	if err := os.WriteFile(next, []byte("package x\n// A.go\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(next, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, path); err != nil {
		t.Fatal(err)
	}
	second := e.driftHashes(context.Background(), repo, files)
	if second["a.go"] == first["a.go"] {
		t.Error("a file replaced by rename kept its old hash")
	}
}

func TestDriftHashes_AFileWithAnOldModificationTimeButANewChangeTimeIsReadEveryTime(t *testing.T) {
	requireMemo(t)
	repo, files := memoRepo(t, time.Now().Add(-time.Hour))
	e := &Engine{}
	e.driftHashes(context.Background(), repo, files)
	e.driftHashes(context.Background(), repo, files)
	if got := e.drift.reads.Load(); got != 6 {
		t.Fatalf("files changed just now read %d times in two walks, want 6: an old modification time does not vouch for a recent change time", got)
	}
}

func settledEngine() *Engine {
	e := &Engine{}
	e.drift.clock = func() time.Time { return time.Now().Add(time.Hour) }
	return e
}

func mustStat(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func requireMemo(t *testing.T) {
	t.Helper()
	if _, ok := fileStamp(mustStat(t, t.TempDir())); !ok {
		t.Skip("no change time on this platform: the memo is off and every walk hashes")
	}
}
