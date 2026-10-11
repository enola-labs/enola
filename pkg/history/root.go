package history

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Root returns the directory holding a repository's architecture history.
//
// The default is OUTSIDE the repository — ~/.enola/graphs/<key>/history — for three
// reasons, each of which is a case repo-local storage gets wrong:
//
//   - enola can be pointed at a repository it has no business writing to. Reading the
//     architecture of a checkout you do not own is a normal thing to do, and `enola log`
//     on it must not create files in it.
//   - History outlives `.enola`. Clearing the output directory is routine (it is
//     derivable, that is the whole point) and must not silently delete the one thing
//     that is not.
//   - It matches where the graph receipt already lives, so everything enola keeps per
//     workspace keeps sitting together.
//
// override is config `history.dir`, for the case where a team WANTS the history inside
// the repository — to commit it, or to publish it from CI as an artifact. A relative
// override resolves against the repository, an absolute one is taken as given.
//
// A repo-local override lands under a path the walker must ignore. `.enola/history` is
// covered by the derived `<output.dir>/**` glob; a location outside the output directory
// needs its own ignore entry, or enola indexes its own history as source.
func Root(repoPath, override string) (string, error) {
	if override != "" {
		if filepath.IsAbs(override) {
			return filepath.Clean(override), nil
		}
		abs, err := filepath.Abs(repoPath)
		if err != nil {
			return "", fmt.Errorf("resolving repo path %q: %w", repoPath, err)
		}
		return filepath.Join(abs, filepath.FromSlash(override)), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home dir: %w", err)
	}
	return filepath.Join(home, ".enola", "graphs", workspaceKey(canonicalRepoPath(repoPath)), "history"), nil
}

// workspaceKey derives a stable, human-readable directory name for a workspace from its
// absolute path: the sanitized base name plus a short hash of the full path, so two
// repositories sharing a base name do not collide.
//
// This mirrors the scheme internal/engine uses for ~/.enola/graphs/<key>.json (and
// pkg/status for its usage files) so a workspace's history sits beside its receipt under
// one recognizable name. It is deliberately a separate implementation rather than a
// shared one: this package must import nothing but internal/facts and the standard library
// (see the package doc), and the engine's copy exists so the engine depends on nothing
// outside itself. The two agreeing is cosmetic — they name different files, and a
// divergence would be untidy rather than wrong.
func workspaceKey(absRepoPath string) string {
	sum := sha256.Sum256([]byte(absRepoPath))
	short := hex.EncodeToString(sum[:])[:8]

	base := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '-', r == '_', r == '.':
			return r
		default:
			return '_'
		}
	}, filepath.Base(absRepoPath))
	if base == "" {
		base = "repo"
	}
	return base + "-" + short
}

// canonicalRepoPath normalizes a repo path so the same workspace always maps to the same
// key however it was referenced — absolute, with symlinks resolved (macOS /var →
// /private/var, which is otherwise enough to give one repository two histories).
func canonicalRepoPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}
