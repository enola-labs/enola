package engine

import (
	"path/filepath"

	"github.com/enola-labs/enola/internal/facts"
)

// sameRepository reports whether two checkouts at different paths are the same
// repository: two worktrees of one clone (they share a git common directory), or two
// clones of one remote. Either way the label from the remote is the same, and since
// graph nodes are name-keyed, holding both in one graph merges every symbol of the one
// into the other. Paths are compared canonically first, so a symlinked spelling of the
// loaded path counts as that path.
//
// Only a checkout ROOT is a checkout. Two directories inside one repository (a
// monorepo's apps indexed as a cluster) share its git directory and its remote, but
// they are labelled by their directory names and are different services, so they are
// not the same repository here. It is the same rule repoLabelFor applies.
func sameRepository(a, b string) bool {
	ca, cb := canonicalRepoPath(a), canonicalRepoPath(b)
	if ca == cb {
		return true
	}
	if !isGitTopLevel(ca) || !isGitTopLevel(cb) {
		return false
	}
	if da, db := gitCommonDir(ca), gitCommonDir(cb); da != "" && da == db {
		return true
	}
	ra, _ := runGit(ca, "remote", "get-url", "origin")
	rb, _ := runGit(cb, "remote", "get-url", "origin")
	na, nb := facts.NormalizeRemote(ra), facts.NormalizeRemote(rb)
	return na != "" && na == nb
}

// gitCommonDir is the repository directory a checkout's worktrees share, canonical, or
// "" outside git.
func gitCommonDir(dir string) string {
	d, err := runGit(dir, "rev-parse", "--git-common-dir")
	if err != nil || d == "" {
		return ""
	}
	if !filepath.IsAbs(d) {
		d = filepath.Join(dir, d)
	}
	return canonicalRepoPath(d)
}

// LoadedCheckoutOf returns the path of a repository in the loaded graph that is the
// same repository as repoPath checked out somewhere else, and how many repositories
// the graph holds. other is "" when there is none, which includes repoPath itself
// being loaded.
//
// A snapshot of that other checkout replaces the loaded one instead of joining it:
// an agent that moves from the main checkout into a worktree is switching checkouts,
// not building a two-repository graph out of one repository.
func (e *Engine) LoadedCheckoutOf(repoPath string) (other string, members int) {
	_, loaded := e.loadedGraph()
	target := canonicalRepoPath(repoPath)
	if loaded[target] {
		return "", len(loaded)
	}
	for p := range loaded {
		if sameRepository(p, target) {
			return p, len(loaded)
		}
	}
	return "", len(loaded)
}
