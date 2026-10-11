package engine

import (
	"path/filepath"

	"github.com/enola-labs/enola/internal/pathglob"
)

// Entry points that only this package's tests call. They are kept out of the
// production files so the binary carries only what it runs.

// matchAnyGlob and matchGlob are thin aliases onto the shared matcher, which lives
// in internal/facts alongside the other path predicates (IsTestPath). The performance
// analyzer's ENOLA_PERF_EXCLUDE globs documented `**` support that path.Match cannot
// give them; rather than grow a second glob implementation, it uses this one.
func matchAnyGlob(relPath string, patterns []string) bool {
	return pathglob.MatchAny(relPath, patterns)
}

func matchGlob(relPath string, patterns []string) (string, bool) {
	return pathglob.Match(relPath, patterns)
}

// isIgnored checks whether a path matches any ignore pattern. isDir is unused: the
// patterns discriminate on shape, not on file type, and a directory that matches is
// pruned by the caller.
func (e *Engine) isIgnored(relPath string, isDir bool) bool {
	return matchAnyGlob(filepath.ToSlash(relPath), e.cfg.Ignore)
}
