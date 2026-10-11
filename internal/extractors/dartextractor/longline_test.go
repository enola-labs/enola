package dartextractor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A line longer than the 1 MB the scanner used to allow ended the scan without a word,
// and every dependency after it was lost.
func TestPubspecAfterLongLine(t *testing.T) {
	file := filepath.Join(t.TempDir(), "pubspec.yaml")
	src := "name: app\n# " + strings.Repeat("x", 2*1024*1024) + "\ndependencies:\n  flutter:\n  http: ^1.0.0\n"
	if err := os.WriteFile(file, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	p := parsePubspec(file)
	if p == nil || p.Name != "app" || !p.IsFlutter || !p.Deps["http"] {
		t.Errorf("pubspec = %+v, want name app with flutter and http read from after the long line", p)
	}
}
