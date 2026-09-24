package engine

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/enola-labs/enola/internal/pathglob"
)

// pyPackageExemptGlobs are the default ignore globs that name build OUTPUT by a
// directory name Python also uses for real packages. `**/build/**` exists for
// Gradle's data/build/kspCaches and a JS sub-app's build/, but a service can just
// as well have `server/features/build/__init__.py`: one open-source FastAPI app
// lost 71 source files and 62 routes that way. A directory that directly holds
// a .py file is a Python package, not an artifact, so these globs do not apply to
// it. The test is a .py file rather than __init__.py because a namespace package
// (PEP 420) has none, and that FastAPI app's build/ is one. Build output is
// unaffected: setuptools' build/ holds only lib/, bdist.*/ and temp.*/ at its top,
// and Gradle, JS bundlers and Sphinx never write .py files into theirs.
var pyPackageExemptGlobs = map[string]bool{"**/build/**": true, "**/tmp/**": true, "**/dist/**": true}

// pyPackageExemptNames are the directory names those globs match.
var pyPackageExemptNames = map[string]bool{"build": true, "tmp": true, "dist": true}

// pyPackageExemption answers, per walked path, whether an ignore match is lifted
// because every directory on the path named like one of pyPackageExemptGlobs
// holds Python source AND no other ignore glob matches. Directory checks are cached:
// the walk asks about every file under a kept package.
type pyPackageExemption struct {
	root  string
	rest  *pathglob.Set // the ignore list without the exemptable globs
	isPkg map[string]bool
}

func newPyPackageExemption(root string, ignore []string) *pyPackageExemption {
	var rest []string
	for _, g := range ignore {
		if !pyPackageExemptGlobs[g] {
			rest = append(rest, g)
		}
	}
	return &pyPackageExemption{root: root, rest: pathglob.Compile(rest), isPkg: map[string]bool{}}
}

// exempt reports whether relPath's ignore match on pattern is lifted.
func (p *pyPackageExemption) exempt(relPath, pattern string, isDir bool) bool {
	if !pyPackageExemptGlobs[pattern] {
		return false
	}
	segs := strings.Split(relPath, "/")
	dirs := len(segs)
	if !isDir {
		dirs-- // the last segment is the file itself
	}
	for i := 0; i < dirs; i++ {
		if !pyPackageExemptNames[segs[i]] {
			continue
		}
		if !p.pkg(strings.Join(segs[:i+1], "/")) {
			return false // a plain build/ on the path: output, however deep the package below it
		}
	}
	return !p.rest.MatchAny(relPath)
}

func (p *pyPackageExemption) pkg(dir string) bool {
	if v, ok := p.isPkg[dir]; ok {
		return v
	}
	entries, _ := os.ReadDir(filepath.Join(p.root, filepath.FromSlash(dir)))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".py") {
			p.isPkg[dir] = true
			return true
		}
	}
	p.isPkg[dir] = false
	return false
}
