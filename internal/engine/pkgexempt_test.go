package engine

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPyPackageExemption(t *testing.T) {
	root := t.TempDir()
	for _, f := range []string{
		"server/features/build/__init__.py",
		"server/features/build/api.py",
		"server/features/build/node_modules/x/index.js",
		"server/features/build/assets/logo.png",
		"web/build/static/app.js",
		"build/lib/pkg/__init__.py",
		"app/tmp/__init__.py",
		"svc/dist/handlers.py",
	} {
		p := filepath.Join(root, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ignore := []string{"**/node_modules/**", "**/build/**", "**/tmp/**", "**/dist/**"}
	ex := newPyPackageExemption(root, ignore)

	cases := []struct {
		path    string
		pattern string
		isDir   bool
		want    bool
	}{
		{"server/features/build", "**/build/**", true, true},
		{"server/features/build/api.py", "**/build/**", false, true},
		{"server/features/build/assets", "**/build/**", true, true},
		// another ignore glob still applies inside an exempt package
		{"server/features/build/node_modules", "**/build/**", true, false},
		// a build/ that is not a package is output
		{"web/build", "**/build/**", true, false},
		{"web/build/static/app.js", "**/build/**", false, false},
		// setuptools output: build/ itself is not a package, whatever is under it
		{"build/lib/pkg/__init__.py", "**/build/**", false, false},
		{"app/tmp", "**/tmp/**", true, true},
		// a namespace package (no __init__.py) is still source
		{"svc/dist", "**/dist/**", true, true},
		// a glob outside the exemptable set is never lifted
		{"server/features/build/node_modules/x/index.js", "**/node_modules/**", false, false},
	}
	for _, c := range cases {
		if got := ex.exempt(c.path, c.pattern, c.isDir); got != c.want {
			t.Errorf("exempt(%q, %q) = %v, want %v", c.path, c.pattern, got, c.want)
		}
	}
}
