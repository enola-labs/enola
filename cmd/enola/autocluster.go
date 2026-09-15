package main

import (
	"fmt"
	"io"
	"path/filepath"

	"github.com/enola-labs/enola/internal/clientspec"
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

// clusterConfig returns the config that indexes dir's repositories as a cluster and the
// cluster config file it rests on, saying on out what it did. A cluster config already in
// dir is the user's and is used as it is. Otherwise one listing repos is written and the
// config in force keeps every other setting. A folder that cannot be written to is still
// indexed as a cluster, for this run, and the returned path is "".
func clusterConfig(out io.Writer, binName string, cfg *config.Config, dir string, repos []string) (*config.Config, string, error) {
	_, _ = fmt.Fprintf(out, "\nDetected %d git repositories in %s: %s\n", len(repos), dir, workspace.Names(repos))
	_, _ = fmt.Fprintln(out, "Indexing them as a cluster, so each one becomes a service and the calls between them are linked.")

	path, existing, writeErr := workspace.EnsureClusterConfig(dir, repos)
	run := withRepos(cfg, dir, repos)
	switch {
	case existing:
		loaded, err := config.Load(path)
		if err != nil {
			return nil, "", err
		}
		_, _ = fmt.Fprintf(out, "  Using the cluster config already there: %s\n", path)
		if cfg.SourcePath != "" {
			_, _ = fmt.Fprintf(out, "  Settings from %s are not applied; that cluster config governs this run.\n", cfg.SourcePath)
		}
		if notice := clientspec.UnsupportedNotice(loaded.Clients); notice != "" {
			_, _ = fmt.Fprint(out, "warning: "+notice)
		}
		run = loaded
	case writeErr != nil:
		path = ""
		_, _ = fmt.Fprintf(out, "  Could not write a cluster config (%v); indexing them as a cluster for this run only.\n", writeErr)
	default:
		_, _ = fmt.Fprintf(out, "  Wrote %s; pass it to --generate to index the same cluster directly.\n", path)
		if cfg.SourcePath != "" {
			_, _ = fmt.Fprintf(out, "  Settings from %s apply to this run; %s lists only the repositories.\n", cfg.SourcePath, filepath.Base(path))
		}
	}
	_, _ = fmt.Fprintf(out, "  To index %s as one repository instead: %s --generate --no-cluster %s\n\n", dir, binName, dir)
	return run, path, nil
}

// withRepos is cfg indexing dir's repos as a cluster, every other setting kept. Paths
// are absolute, so they resolve the same whichever file cfg was read from.
func withRepos(cfg *config.Config, dir string, repos []string) *config.Config {
	run := *cfg
	run.Repos = make([]string, len(repos))
	for i, r := range repos {
		run.Repos[i] = filepath.Join(dir, r)
	}
	return &run
}
