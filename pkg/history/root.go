package history

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
	return keyName(filepath.Base(absRepoPath)) + "-" + hex.EncodeToString(sum[:])[:8]
}

// keyName sanitizes a folder name for use in a directory key.
func keyName(name string) string {
	base := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '-', r == '_', r == '.':
			return r
		default:
			return '_'
		}
	}, name)
	if base == "" {
		base = "repo"
	}
	return base
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

// ClusterDirName holds cluster histories under a configured history dir.
const ClusterDirName = "clusters"

// ClusterRoot returns the directory holding the architecture history of a multi-repo
// graph: the revisions recorded from snapshots of all of members together.
//
// A cluster revision used to be appended to every member's own history, which stored the
// same union N times and mixed it into each member's single-repository timeline, where
// the step between a member alone and the whole cluster read as tens of thousands of
// facts appearing and vanishing. A cluster has one history of its own instead.
//
// It is keyed by the member set, not by the cluster config: the same repositories are
// the same graph whether they were indexed from a cluster.yaml, a folder of
// repositories, or a list passed over MCP, and only the member set is known in all
// three. The readable part of the name is the members' common parent folder.
//
// With history.dir set, it lives under that member-independent part of the first
// member's root, in clusters/<key>; segments are only ever read from seg/, so a
// subdirectory of a history root is never mistaken for one.
func ClusterRoot(members []string, override string) (string, error) {
	if len(members) == 0 {
		return "", fmt.Errorf("a cluster history needs its members")
	}
	canon := make([]string, len(members))
	for i, m := range members {
		canon[i] = canonicalRepoPath(m)
	}
	sort.Strings(canon)
	key := clusterKey(canon)
	if override != "" {
		root, err := Root(canon[0], override)
		if err != nil {
			return "", err
		}
		return filepath.Join(root, ClusterDirName, key), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home dir: %w", err)
	}
	return filepath.Join(home, ".enola", "graphs", key, "history"), nil
}

// clusterKey names a sorted member set: the common parent folder's sanitized name and a
// short hash of every member path.
func clusterKey(sorted []string) string {
	sum := sha256.Sum256([]byte(strings.Join(sorted, "\n")))
	short := hex.EncodeToString(sum[:])[:8]
	parent := commonDir(sorted)
	name := "cluster"
	if base := filepath.Base(parent); base != string(filepath.Separator) && base != "." {
		name = base
	}
	return keyName(name) + "-cluster-" + short
}

// commonDir is the deepest folder containing every path.
func commonDir(paths []string) string {
	dir := filepath.Dir(paths[0])
	for _, p := range paths[1:] {
		for dir != filepath.Dir(dir) && !strings.HasPrefix(p+string(filepath.Separator), dir+string(filepath.Separator)) {
			dir = filepath.Dir(dir)
		}
	}
	return dir
}
