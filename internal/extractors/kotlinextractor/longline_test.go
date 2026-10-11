package kotlinextractor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A line longer than bufio's 64 KB default ended the scan without a word, so a package
// declaration below one was never seen and the file had no source root.
func TestSourceRootAfterLongLine(t *testing.T) {
	repo := t.TempDir()
	rel := filepath.Join("src", "main", "kotlin", "com", "example", "A.kt")
	if err := os.MkdirAll(filepath.Dir(filepath.Join(repo, rel)), 0o755); err != nil {
		t.Fatal(err)
	}
	src := "// " + strings.Repeat("x", 200*1024) + "\npackage com.example\n"
	if err := os.WriteFile(filepath.Join(repo, rel), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	root, ok := kotlinFileSourceRoot(repo, filepath.ToSlash(rel))
	if !ok || !strings.HasSuffix(strings.TrimSuffix(root, "/"), "src/main/kotlin") {
		t.Errorf("source root = %q, %v; want the directory above com/example", root, ok)
	}
}
