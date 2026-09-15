package command

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/enola-labs/enola/internal/workspace"
	"github.com/enola-labs/enola/pkg/check"
)

// Cluster is `enola cluster init [--dry-run] [dir]`.
func (r *Runner) Cluster(args []string) {
	if len(args) == 0 || args[0] != "init" {
		fmt.Fprint(os.Stderr, "Usage: "+r.name()+" cluster init [--dry-run] [dir]\n\n"+
			"init writes a cluster config for a folder that holds several git repositories.\n"+
			"Run `"+r.name()+" cluster init --help`.\n")
		os.Exit(check.StatusUsageError.ExitCode())
	}
	r.ClusterInit(args[1:])
}

// ClusterInit writes dir/cluster.yaml listing the git repositories directly under dir,
// so a folder of repositories can be indexed as a cluster without writing the config by
// hand. It never overwrites and refuses a folder with fewer than two repositories.
func (r *Runner) ClusterInit(args []string) {
	fs := flag.NewFlagSet("cluster init", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dry := fs.Bool("dry-run", false, "print the config instead of writing it")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "Usage: "+r.name()+" cluster init [--dry-run] [dir]\n\n"+
			"Writes "+workspace.ClusterFileName+" in dir (default: the working directory), listing every\n"+
			"immediate subfolder that is a git repository. Pass the file to --generate to\n"+
			"index them as one linked graph. Never overwrites an existing config.\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		os.Exit(check.StatusUsageError.ExitCode())
	}
	dir := "."
	if fs.NArg() > 0 {
		dir = fs.Arg(0)
	}
	if !IsDirectory(dir) {
		fmt.Fprintf(os.Stderr, "cluster init: %s is not a directory\n", dir)
		os.Exit(check.StatusUsageError.ExitCode())
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cluster init:", err)
		os.Exit(check.StatusUsageError.ExitCode())
	}

	repos := workspace.ChildRepos(dir)
	if len(repos) < workspace.MinRepos {
		fmt.Fprintf(os.Stderr, "cluster init: %s has %d git repositories as immediate subfolders; a cluster needs at least %d\n",
			dir, len(repos), workspace.MinRepos)
		os.Exit(check.StatusUsageError.ExitCode())
	}

	if *dry {
		body, err := workspace.ClusterConfig(repos)
		if err != nil {
			fmt.Fprintln(os.Stderr, "cluster init:", err)
			os.Exit(1)
		}
		fmt.Print(body)
		return
	}
	path, err := workspace.WriteClusterConfig(dir, repos)
	if errors.Is(err, workspace.ErrClusterExists) {
		fmt.Fprintf(os.Stderr, "cluster init: %s exists; this command never overwrites a config\n", path)
		os.Exit(check.StatusUsageError.ExitCode())
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "cluster init:", err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s with %d repositories: %s\nnext: %s --generate %s\n",
		path, len(repos), workspace.Names(repos), r.name(), path)
}
