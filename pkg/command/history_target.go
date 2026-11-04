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
	if err != nil || workspace.IsRepo(abs) {
		return historyScope{}, false
	}
	if path := filepath.Join(abs, workspace.ClusterFileName); fileExists(path) {
		cfg, err := config.Load(path)
		if err != nil {
			r.cmdFatal(cmd, "reading %s: %v", path, err)
		}
		if len(cfg.Repos) > 0 {
			s := r.scopeOf(cmd, cfg)
			s.cluster = path
			return s, true
		}
	}
	kids := workspace.Clusterable(abs, "")
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
// share one only when history.dir is an absolute path.
type historyRoot struct {
	root   string
	labels []string
}

// roots returns the distinct history directories of the scope, in repository order.
func (s historyScope) roots() ([]historyRoot, error) {
	var out []historyRoot
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
