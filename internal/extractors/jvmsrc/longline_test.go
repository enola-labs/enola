package jvmsrc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A line longer than bufio's 64 KB default ended the scan without a word, so a package
// declaration below one was never seen.
func TestReadPackageAfterLongLine(t *testing.T) {
	file := filepath.Join(t.TempDir(), "A.java")
	src := "// " + strings.Repeat("x", 200*1024) + "\npackage com.example;\n"
	if err := os.WriteFile(file, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if pkg, ok := readPackage(file); !ok || pkg != "com.example" {
		t.Errorf("package = %q, %v; want com.example", pkg, ok)
	}
}
