package swiftextractor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A line longer than the 1 MB the scanner used to allow ended the scan without a word,
// and every type reference after it was lost.
func TestTypeReferencesAfterLongLine(t *testing.T) {
	file := filepath.Join(t.TempDir(), "Assets.swift")
	src := "let a: Alpha\nlet blob = \"" + strings.Repeat("x", 2*1024*1024) + "\"\nlet b: Beta\n"
	if err := os.WriteFile(file, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(extractTypeReferences(file), ","); got != "Alpha,Beta" {
		t.Errorf("type references = %q, want Alpha,Beta", got)
	}
}
