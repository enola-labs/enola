package command

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/enola-labs/enola/internal/engine"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/pkg/bootstrap"
)

// baseAtRef is the "before" for `check --base REF`: the repository as it was where the
// current branch left REF, snapshotted with the run's own configuration, written nowhere.
//
// It is the merge-base of HEAD and REF, not REF itself, because that is what a pull
// request is compared against: commits that landed on REF since the branch was cut are
// not the branch's doing. When REF is an ancestor of HEAD the two are the same.
//
// Every agent reviewing a branch had to build this by hand, and each build had a way to
// go wrong: a worktree touches the repository's .git, an export under another name is a
// different repository to the identity check, and replacing the export's contents
// deletes a baseline pinned in it. Here the export is named like the repository, its git
// metadata names the merge-base and the repository's remote, and it is removed after.
//
// It runs before the current snapshot is taken: comparability refuses a baseline
// generated after the snapshot it is compared against.
func (r *Runner) baseAtRef(ctx context.Context, cfgPath, repo, ref string) (*facts.Snapshot, string, error) {
	if _, err := gitOut(repo, "rev-parse", "--git-dir"); err != nil {
		return nil, "", fmt.Errorf("--base needs a git repository, and %s is not one", repo)
	}
	if _, err := gitOut(repo, "rev-parse", "--verify", "--quiet", ref+"^{commit}"); err != nil {
		return nil, "", fmt.Errorf("--base %s: no such commit or ref in %s", ref, repo)
	}
	mb, err := gitOut(repo, "merge-base", "HEAD", ref)
	if err != nil {
		return nil, "", fmt.Errorf("--base %s: HEAD and %s share no history", ref, ref)
	}

	tmp, err := os.MkdirTemp("", "enola-base-")
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	dir := filepath.Join(tmp, filepath.Base(repo))
	if err := exportCommit(repo, mb, dir); err != nil {
		return nil, "", err
	}

	eng, _, err := r.newEngine(bootstrap.Options{ConfigPath: cfgPath})
	if err != nil {
		return nil, "", err
	}
	eng.SetPersistCache(false)
	if _, err := eng.GenerateSnapshot(ctx, dir, false); err != nil {
		return nil, "", fmt.Errorf("snapshotting %s at %s: %w", repo, short(mb), err)
	}
	snap := eng.Snapshot()
	if snap == nil {
		return nil, "", fmt.Errorf("snapshotting %s at %s produced nothing", repo, short(mb))
	}
	base := &facts.Snapshot{Meta: eng.GraphMeta([]string{dir}), Facts: eng.Store().FactsRef(), Insights: snap.Insights}
	// The export has no .git of its own. It is this repository at this commit, so it
	// says so: the commit lets the changed-files list come from git, and the remote
	// lets the identity check compare remotes rather than directory names.
	git := &facts.GitInfo{Commit: mb}
	if state := engine.GitState(repo, ""); state != nil {
		git.Remote = state.Remote
	}
	base.Meta.Git = git
	return base, mb, nil
}

func gitOut(repo string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).Output()
	return strings.TrimSpace(string(out)), err
}

func short(sha string) string {
	if len(sha) > 9 {
		return sha[:9]
	}
	return sha
}

// exportCommit writes the tree of commit into dest, as `git archive` produces it.
func exportCommit(repo, commit, dest string) error {
	cmd := exec.Command("git", "-C", repo, "archive", "--format=tar", commit)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	extractErr := untar(out, dest)
	waitErr := cmd.Wait()
	if waitErr != nil {
		return fmt.Errorf("git archive %s: %v %s", short(commit), waitErr, strings.TrimSpace(stderr.String()))
	}
	return extractErr
}

// untar unpacks a git archive stream into dest. Entries that would land outside dest
// are refused: a tree can hold any path git accepts, and this writes to disk.
func untar(r io.Reader, dest string) error {
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	root := filepath.Clean(dest) + string(filepath.Separator)
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		path := filepath.Join(dest, filepath.FromSlash(h.Name))
		if !strings.HasPrefix(path+string(filepath.Separator), root) {
			return fmt.Errorf("archive entry %q leaves the export directory", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(h.Mode)&0o777|0o200)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(f, tr)
			closeErr := f.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		case tar.TypeSymlink:
			// Kept as links, as the checkout has them, but never followed out of dest:
			// one pointing outside is dropped rather than written.
			target := h.Linkname
			resolved := target
			if !filepath.IsAbs(resolved) {
				resolved = filepath.Join(filepath.Dir(path), resolved)
			}
			if !strings.HasPrefix(filepath.Clean(resolved)+string(filepath.Separator), root) {
				continue
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(target, path); err != nil {
				return err
			}
		}
	}
}
