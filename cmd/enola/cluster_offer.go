package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/enola-labs/enola/internal/config"
	"github.com/enola-labs/enola/internal/workspace"
)

// foldedRepos returns the directory a single-repository run would index and the git
// repositories directly under it, when there are enough to be worth saying. A config
// with repos: already names a cluster, so it returns nothing.
func foldedRepos(cfg *config.Config) (string, []string) {
	if len(cfg.Repos) > 0 {
		return "", nil
	}
	dir, err := filepath.Abs(cfg.Repo)
	if err != nil {
		return "", nil
	}
	return dir, workspace.Folded(dir, cfg.SourcePath)
}

// offerCluster asks whether to index dir's repositories as a cluster and returns the
// config to run with, or "" to go on as one repository. Enter accepts. Anything else,
// end of input included, declines, so nothing is written without an answer.
func offerCluster(in io.Reader, out io.Writer, dir string, repos []string) (string, error) {
	_, _ = fmt.Fprintf(out, "\n%s holds %d git repositories: %s\n", dir, len(repos), workspace.Names(repos))
	_, _ = fmt.Fprintln(out, "Indexed as one repository they get no service nodes and no edges between them.")

	existing := filepath.Join(dir, workspace.ClusterFileName)
	if _, err := os.Stat(existing); err == nil {
		_, _ = fmt.Fprintf(out, "Use the cluster config already there, %s? [Y/n] ", existing)
		if !confirm(in) {
			_, _ = fmt.Fprintf(out, "Indexing %s as one repository.\n\n", dir)
			return "", nil
		}
		return existing, nil
	}

	_, _ = fmt.Fprintf(out, "Write %s and index them as a cluster? [Y/n] ", existing)
	if !confirm(in) {
		_, _ = fmt.Fprintf(out, "Indexing %s as one repository.\n\n", dir)
		return "", nil
	}
	path, err := workspace.WriteClusterConfig(dir, repos)
	if err != nil {
		return "", err
	}
	_, _ = fmt.Fprintf(out, "Wrote %s\n\n", path)
	return path, nil
}

func confirm(in io.Reader) bool {
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && line == "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "", "y", "yes":
		return true
	}
	return false
}
