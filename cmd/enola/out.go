package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/enola-labs/enola/internal/config"
)

// takeOut removes `--out DIR` or `--out=DIR` from args and returns DIR.
func takeOut(args []string) ([]string, string, error) {
	var rest []string
	out := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--out":
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
				return nil, "", fmt.Errorf("--out needs a directory: --out DIR")
			}
			out = args[i+1]
			i++
		case strings.HasPrefix(a, "--out="):
			out = strings.TrimPrefix(a, "--out=")
			if out == "" {
				return nil, "", fmt.Errorf("--out needs a directory: --out DIR")
			}
		default:
			rest = append(rest, a)
		}
	}
	return rest, out, nil
}

// applyOut makes dir the cluster's output directory for this run. It is refused for a
// single repository, whose graph is its own and stays in its own output dir, and
// inside any member, whose next snapshot would index the cluster's artifacts as source.
func applyOut(cfg *config.Config, dir string) error {
	repos, err := cfg.RepoPaths()
	if err != nil {
		return err
	}
	if len(repos) < 2 {
		return fmt.Errorf("names a cluster's directory, and this run indexes one repository, which writes to its own %s", cfg.Output.Dir)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	for _, r := range repos {
		if within(abs, r) {
			return fmt.Errorf("%s is inside %s, one of the repositories indexed; put it outside every member", abs, r)
		}
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return err
	}
	probe, err := os.CreateTemp(abs, ".enola-write-")
	if err != nil {
		return fmt.Errorf("%s is not writable: %w", abs, err)
	}
	_ = probe.Close()
	_ = os.Remove(probe.Name())
	cfg.Output.ClusterDir = abs
	return nil
}

// within reports whether path is dir or below it.
func within(path, dir string) bool {
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
