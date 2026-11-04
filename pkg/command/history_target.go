package command

import (
	"path/filepath"

	"github.com/enola-labs/enola/internal/config"
	"github.com/enola-labs/enola/internal/workspace"
	"github.com/enola-labs/enola/pkg/history"
)

// historyScope is what a history command's positional argument names: one repository, or
// every member of a cluster. A cluster snapshot records into EACH member's own history, so
// a command that cleans or reports on a cluster has to visit all of them.
type historyScope struct {
	cfg *config.Config
	// repos are the absolute repository paths, in config order. Exactly one unless the
	// target is a cluster.
	repos []string
	// cluster names what made this a cluster (the cluster config, or the folder of
	// repositories); "" for a single repository.
	cluster string
}

// historyTargetOf resolves arg the way --generate would have indexed it, so a command
// reading history finds it where the snapshot recorded it:
//
//   - a config file: its repo, or every entry under repos:
//   - a directory (or "" for the working directory) with no mcp-arch.yaml of its own and
//     not itself a git repository: its cluster.yaml when it has one, otherwise the git
//     repositories directly under it, which --generate indexes as a cluster
//   - anything else: that one repository, as before
func (r *Runner) historyTargetOf(cmd, arg string) historyScope {
	dir := ""
	switch {
	case arg == "":
		if !fileExists("mcp-arch.yaml") {
			dir = "."
		}
	case isDirectory(arg) && !fileExists(filepath.Join(arg, "mcp-arch.yaml")):
		dir = arg
	}
	if dir != "" {
		if s, ok := r.folderScope(cmd, dir); ok {
			return s
		}
	}

	cfgPath := "mcp-arch.yaml"
	repoOverride := ""
	switch {
	case arg == "":
	case isDirectory(arg):
		abs, err := filepath.Abs(arg)
		if err != nil {
			r.cmdFatal(cmd, "resolving repo path %q: %v", arg, err)
		}
		repoOverride = abs
		if inner := filepath.Join(abs, "mcp-arch.yaml"); fileExists(inner) {
			cfgPath = inner
		}
	default:
		cfgPath = arg
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		// A missing config is the ordinary case (built-in defaults); anything else is
		// worth saying out loud, but is still not a reason to refuse to read a history.
		cfg = config.Default()
	}
	if repoOverride != "" {
		cfg.Repo = repoOverride
		cfg.Repos = nil
	}
	s := r.scopeOf(cmd, cfg)
	if len(cfg.Repos) > 0 {
		s.cluster = cfg.SourcePath
	}
	return s
}

// folderScope is dir as a cluster, when it is a folder of repositories rather than a
// repository.
func (r *Runner) folderScope(cmd, dir string) (historyScope, bool) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return historyScope{}, false
	}
	path, kids := workspace.Cluster(abs)
	if path != "" {
		cfg, err := config.Load(path)
		if err != nil {
			r.cmdFatal(cmd, "reading %s: %v", path, err)
		}
		if len(cfg.Repos) == 0 {
			r.cmdFatal(cmd, "%s has an empty repos list", path)
		}
		s := r.scopeOf(cmd, cfg)
		s.cluster = path
		return s, true
	}
	if len(kids) == 0 {
		return historyScope{}, false
	}
	// Settings other than the repositories come from the config in force, as they do
	// for --generate over the same folder.
	cfg, err := config.Load("mcp-arch.yaml")
	if err != nil {
		cfg = config.Default()
	}
	cfg.Repos = make([]string, len(kids))
	for i, k := range kids {
		cfg.Repos[i] = filepath.Join(abs, k)
	}
	s := r.scopeOf(cmd, cfg)
	s.cluster = abs
	return s, true
}

func (r *Runner) scopeOf(cmd string, cfg *config.Config) historyScope {
	if err := cfg.Normalize(); err != nil {
		r.cmdFatal(cmd, "invalid config: %v", err)
	}
	repos, err := cfg.RepoPaths()
	if err != nil || len(repos) == 0 {
		r.cmdFatal(cmd, "failed to resolve repo path: %v", err)
	}
	return historyScope{cfg: cfg, repos: repos}
}

// historyRoot is one history directory and the repositories that record into it. Members
// share one only when history.dir is an absolute path. cluster marks the history of the
// members together, recorded from snapshots of the whole cluster.
type historyRoot struct {
	root    string
	labels  []string
	cluster bool
}

// root is the history a reader shows for the scope: the cluster's own for a cluster,
// the repository's otherwise.
func (s historyScope) root() (string, error) {
	if s.cluster != "" {
		return history.ClusterRoot(s.repos, s.cfg.History.Dir)
	}
	return history.Root(s.repos[0], s.cfg.History.Dir)
}

// roots returns every history directory of the scope: for a cluster its own history
// first, then each member's, in repository order and without duplicates.
func (s historyScope) roots() ([]historyRoot, error) {
	var out []historyRoot
	if s.cluster != "" {
		root, err := history.ClusterRoot(s.repos, s.cfg.History.Dir)
		if err != nil {
			return nil, err
		}
		out = append(out, historyRoot{root: root, labels: memberLabels(s.repos), cluster: true})
	}
	at := map[string]int{}
	for _, repo := range s.repos {
		root, err := history.Root(repo, s.cfg.History.Dir)
		if err != nil {
			return nil, err
		}
		label := filepath.Base(repo)
		if i, ok := at[root]; ok {
			out[i].labels = append(out[i].labels, label)
			continue
		}
		at[root] = len(out)
		out = append(out, historyRoot{root: root, labels: []string{label}})
	}
	return out, nil
}

// historyRootOf resolves a history reader's argument to the path it reports on, the
// history directory to read, and the config in force. For a cluster the path is the
// folder holding the cluster config, or the folder of repositories.
func (r *Runner) historyRootOf(cmd, arg string) (string, string, *config.Config) {
	scope := r.historyTargetOf(cmd, arg)
	root, err := scope.root()
	if err != nil {
		r.cmdFatal(cmd, "cannot locate the history: %v", err)
	}
	if scope.cluster != "" {
		dir := scope.cluster
		if !isDirectory(dir) {
			dir = filepath.Dir(dir)
		}
		return dir, root, scope.cfg
	}
	return scope.repos[0], root, scope.cfg
}
