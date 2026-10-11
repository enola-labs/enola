package scalaextractor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A line longer than the 1 MB the scanner used to allow ended the scan without a word,
// and every route after it was lost.
func TestPlayRoutesAfterLongLine(t *testing.T) {
	file := filepath.Join(t.TempDir(), "routes")
	src := "GET /a controllers.A.first\n# " + strings.Repeat("x", 2*1024*1024) + "\nGET /b controllers.A.second\n"
	if err := os.WriteFile(file, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	parsed := parsePlayRoutesFile(file, "conf/routes")
	if parsed == nil || len(parsed.routes) != 2 {
		t.Fatalf("routes = %+v, want the one before the long line and the one after", parsed)
	}
	if last := parsed.routes[1]; last.path != "/b" || last.line != 3 {
		t.Errorf("last route = %+v, want /b on line 3", last)
	}
}
