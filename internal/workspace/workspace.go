// Package workspace recognises a directory that holds several git repositories side by
// side.
//
// Given such a directory as a repository, enola indexes everything under it as ONE
// repository. The facts are real, but there are no service nodes and no edges between
// the repositories, so every cross-repo question comes back empty, and nothing in the
// output said why. The CLI and the MCP server use this package to say so, and
// `cluster init` uses it to write the cluster config that indexes them as a cluster.
package workspace

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// ClusterFileName is the config `cluster init` writes, beside the repositories it lists.
const ClusterFileName = "cluster.yaml"

// MinRepos is how many child repositories make a directory a folder of repositories.
// A single repository in a subfolder is an ordinary layout (a vendored checkout, a docs
// site) and says nothing about what the user meant.
const MinRepos = 2

// DocsURL is where cluster configs are documented.
const DocsURL = "https://github.com/enola-labs/enola/blob/main/docs/CLUSTERS.md"

// ErrClusterExists is returned by WriteClusterConfig when the directory already has one.
var ErrClusterExists = errors.New("cluster config already exists")

// maxNamed bounds how many repository names a message lists before summarising the rest.
const maxNamed = 8

// ChildRepos returns the names of dir's immediate subdirectories that are git
// repositories, sorted. A `.git` file counts as well as a directory, so submodules and
// worktrees are found. Hidden directories and node_modules are skipped. Only the first
// level is read: a repository nested deeper belongs to whatever contains it.
func ChildRepos(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var repos []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || name == "node_modules" {
			continue
		}
		child := filepath.Join(dir, name)
		// Stat rather than the entry's own type, so a symlinked checkout counts.
		if info, err := os.Stat(child); err != nil || !info.IsDir() {
			continue
		}
		if _, err := os.Lstat(filepath.Join(child, ".git")); err == nil {
			repos = append(repos, name)
		}
	}
	// os.ReadDir returns entries sorted by name, so repos already is.
	return repos
}

// Folded returns the child repositories a single-repository run over dir would index as
// one, or nil when there is nothing to say. configSource is the config file the run
// loaded, "" for built-in defaults. A config inside dir itself means someone already
// decided how dir is indexed, so that stays quiet.
func Folded(dir, configSource string) []string {
	if configSource != "" && filepath.Dir(configSource) == filepath.Clean(dir) {
		return nil
	}
	repos := ChildRepos(dir)
	if len(repos) < MinRepos {
		return nil
	}
	return repos
}

// IsRepo reports whether dir is itself a git repository.
func IsRepo(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, ".git"))
	return err == nil
}

// IsFolderOfRepos reports whether dir is a folder of repositories rather than a
// repository: not a git repository itself, and holding at least MinRepos that are. The
// session hooks ask this, because they snapshot their directory as ONE repository
// whatever config sits in it. Over a folder of every repository a user has, that was
// hundreds of thousands of files per session start and again per graded turn.
func IsFolderOfRepos(dir string) bool {
	return !IsRepo(dir) && len(ChildRepos(dir)) >= MinRepos
}

// Clusterable returns the child repositories enola indexes as a cluster when dir is given
// as one repository, or nil. It is Folded, except for a dir that is itself a git
// repository: nested checkouts inside a repository are part of it, and indexing only
// the children would drop the repository's own code.
func Clusterable(dir, configSource string) []string {
	if IsRepo(dir) {
		return nil
	}
	return Folded(dir, configSource)
}

// EnsureClusterConfig returns dir's cluster config, writing one that lists repos when
// there is none. existing reports that the file was already there, in which case it is
// the user's and is read as it is. A write failure returns the error with an empty path,
// and the caller indexes the cluster without a file.
func EnsureClusterConfig(dir string, repos []string) (path string, existing bool, err error) {
	path, err = WriteClusterConfig(dir, repos)
	if errors.Is(err, ErrClusterExists) {
		return path, true, nil
	}
	if err != nil {
		return "", false, err
	}
	return path, false, nil
}

// Names lists repository names for a message, summarising past the first few.
func Names(repos []string) string {
	if len(repos) <= maxNamed {
		return strings.Join(repos, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(repos[:maxNamed], ", "), len(repos)-maxNamed)
}

// Remedy is the command that indexes dir as a cluster: the existing cluster config when
// dir has one, otherwise `cluster init`, which writes it.
func Remedy(binName, dir string) string {
	cluster := filepath.Join(dir, ClusterFileName)
	if _, err := os.Stat(cluster); err == nil {
		return binName + " --generate " + shellArg(cluster)
	}
	return binName + " cluster init " + shellArg(dir)
}

// FoldWarning tells a terminal reader that dir is being indexed as one repository, what
// that costs, and how to index it as a cluster instead.
func FoldWarning(binName, dir string, repos []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\nwarning: %s holds %d git repositories: %s\n", dir, len(repos), Names(repos))
	b.WriteString("  They are indexed as ONE repository: no service nodes and no edges between them.\n")
	cluster := filepath.Join(dir, ClusterFileName)
	if _, err := os.Stat(cluster); err == nil {
		fmt.Fprintf(&b, "  A cluster config is already there. To use it:\n    %s\n", Remedy(binName, dir))
	} else {
		fmt.Fprintf(&b, "  To index them as a cluster:\n    %s\n    %s --generate %s\n",
			Remedy(binName, dir), binName, shellArg(cluster))
	}
	fmt.Fprintf(&b, "  See %s\n\n", DocsURL)
	return b.String()
}

// ClusterConfig renders the cluster config listing repos. Entries are relative to the
// file, which is how config.RepoPaths resolves them, so the file means the same thing
// wherever enola runs from.
func ClusterConfig(repos []string) (string, error) {
	var buf bytes.Buffer
	buf.WriteString("# Written by enola for this folder of repositories. Each entry is a git repository, relative to\n" +
		"# this file. Pass this file to --generate to index them as one linked graph.\n")
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(struct {
		Repos []string `yaml:"repos"`
	}{repos}); err != nil {
		return "", err
	}
	if err := enc.Close(); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// WriteClusterConfig writes dir's cluster config listing repos and returns its path. It
// never overwrites: an existing file returns ErrClusterExists, with the path.
func WriteClusterConfig(dir string, repos []string) (string, error) {
	path := filepath.Join(dir, ClusterFileName)
	body, err := ClusterConfig(repos)
	if err != nil {
		return "", err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, fs.ErrExist) {
		return path, fmt.Errorf("%s: %w", path, ErrClusterExists)
	}
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(body); err != nil {
		_ = f.Close()
		return "", err
	}
	return path, f.Close()
}

// shellArg quotes a path only when a shell would split or expand it, so the common case
// stays copyable as printed.
func shellArg(p string) string {
	if strings.ContainsAny(p, " \t'\"$`\\") {
		return strconv.Quote(p)
	}
	return p
}
