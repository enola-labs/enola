package tsextractor

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func fileNames(prefix, ext string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s/f%d%s", prefix, i, ext)
	}
	return out
}

func TestJavaScriptLeads(t *testing.T) {
	cases := []struct {
		name  string
		files []string
		want  bool
	}{
		{"a browser game: scripts, a page and a stylesheet", []string{"index.html", "css/style.css", "js/engine.js", "js/ui.js", "test/sim.js", "README.md"}, true},
		{"a Node program with ES modules", []string{"package.json", "src/main.mjs", "src/lib.cjs"}, true},
		{"no JavaScript at all", []string{"README.md", "docs/guide.md"}, false},
		{"a Go service with a build script", append(fileNames("internal", ".go", 40), "scripts/build.js"), false},
		{"a tie is not a lead", []string{"tool.py", "tool.js"}, false},
		{"JavaScript beside fewer scripts of another language", append(fileNames("src", ".js", 12), "scripts/release.py", "scripts/seed.py"), true},
		{"a C library split across .c and .h", append(append(fileNames("src", ".c", 3), fileNames("include", ".h", 3)...), fileNames("demo", ".js", 4)...), false},
		{"a web app whose JavaScript is vendored libraries", append(append(fileNames("src/main/scala", ".scala", 20), fileNames("src/main/webapp/assets/vendors/ace", ".js", 300)...), "src/main/webapp/assets/common/js/app.js"), false},
		{"only installed, built or minified JavaScript", []string{"node_modules/x/index.js", "dist/bundle.js", "public/jquery.min.js"}, false},
		{"a repository in a language no extractor reads", append(fileNames("lib", ".ex", 30), "assets/js/app.js"), false},
	}
	for _, tc := range cases {
		if got := javaScriptLeads(tc.files); got != tc.want {
			t.Errorf("%s: javaScriptLeads = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A directory of plain JavaScript, with no tsconfig, no framework dependency and no
// package.json, was detected by nothing and snapshotted as its README alone.
func TestDetectPlainJavaScriptProject(t *testing.T) {
	repo := t.TempDir()
	for rel, src := range map[string]string{
		"index.html":   "<script src=js/engine.js></script>",
		"js/engine.js": "function roll() { return 4 }\n",
		"js/ui.js":     "function draw() {}\n",
		"README.md":    "# A game\n",
	} {
		path := filepath.Join(repo, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if ok, err := (&TSExtractor{}).Detect(repo); err != nil || !ok {
		t.Errorf("Detect = %v, %v; want the JavaScript extractor to claim a plain JavaScript directory", ok, err)
	}
}
