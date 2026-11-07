// Package detectnames supplies the file names an extractor's detection answers
// from, for the one caller that has no engine walk to borrow.
//
// Detection used to be a bounded re-walk inside every extractor, and every bound
// was a cliff: dotnet/runtime carries 3,270 C/C++ sources with none inside the
// three levels the C++ detector scanned, so the language was absent from the graph
// entirely. plugin.FileListDetector removes the re-walk by answering from the names
// the engine already collected. Detect remains on the plugin.Extractor interface,
// though, and out-of-engine callers still reach for it.
//
// So each migrated extractor keeps exactly ONE decision — its DetectFiles predicate
// over names — and its Detect becomes Walk plus that predicate. The two answers
// cannot drift apart, because there is only one of them.
package detectnames

import (
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/enola-labs/enola/internal/workspace"
)

// pruned names the directories no detection may descend into, whatever it is
// looking for. They are the directories that hold OTHER PEOPLE'S code: a repository
// does not become a C++ project by vendoring a C library, and node_modules would
// make every repository that has one a TypeScript project.
//
// It is deliberately shorter than the engine's ignore globs. Those decide what gets
// INDEXED and are the user's to configure; this decides what a language is spelled
// by, and a detector that needs more (build output, generated sources) filters the
// names itself — so that it filters them identically whether they arrived from here
// or from the engine's walk.
var pruned = map[string]bool{
	"node_modules": true,
	"vendor":       true,
	"testdata":     true,
}

// Walk returns every file under repoPath as a repo-relative, forward-slash path,
// pruning `pruned` and every dot-directory. It is best-effort: an unreadable
// subtree contributes nothing rather than failing the walk, because detection
// answering "no" is always better than a snapshot failing to start.
func Walk(repoPath string) []string {
	var names []string
	_ = filepath.WalkDir(repoPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// A directory that cannot be read is skipped whole; a file that cannot be
			// stat'd is simply not a name.
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, rerr := filepath.Rel(repoPath, path)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			if pruned[d.Name()] || strings.HasPrefix(d.Name(), ".") || workspace.IsLinkedWorktree(path) {
				return filepath.SkipDir
			}
			return nil
		}
		names = append(names, rel)
		return nil
	})
	return names
}

// HasSegment reports whether a repo-relative path contains dir as a whole path
// segment. Detectors use it to drop the names they must not detect on — build
// output, generated sources — by the same rule regardless of which walk produced
// the name. Substring matching would be wrong: "src/binder/x.cs" is not in "bin".
func HasSegment(rel, dir string) bool {
	for _, seg := range strings.Split(rel, "/") {
		if seg == dir {
			return true
		}
	}
	return false
}

// HasAnySegment reports whether rel contains any of dirs as a whole path segment.
func HasAnySegment(rel string, dirs ...string) bool {
	for _, d := range dirs {
		if HasSegment(rel, d) {
			return true
		}
	}
	return false
}

// Base is filepath.Base for an already-slashed repo-relative name.
func Base(rel string) string {
	if i := strings.LastIndex(rel, "/"); i >= 0 {
		return rel[i+1:]
	}
	return rel
}
