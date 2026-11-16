package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/enola-labs/enola/internal/config"
)

// ClusterCopy is a cluster's graph that an older enola wrote into a member's own output
// directory, where it stands in for the member's own snapshot: at the top level, in
// previous/, or as a pinned baseline/.
type ClusterCopy struct {
	Member      string // the member repository, absolute
	Dir         string // the directory holding the copy
	Where       string // "snapshot", "previous" or "baseline"
	GeneratedAt string
	Files       []string // the files the copy is made of, absolute
}

// copyRenderers are the artifacts the renderers wrote beside a snapshot, removed with a
// copy at the top level of an output dir. A copy's receipt names what it wrote in
// output_hashes; these cover a receipt that predates a renderer.
var copyRenderers = []string{"llm_context.md", "docs.jsonl", "flows.json"}

// FindClusterCopies lists the cluster graphs held in the output directories of repos:
// snapshots whose receipt counts more than one service, which a graph of one
// repository never has. Only the receipt is read, so finding none costs a few small
// reads per repository.
func FindClusterCopies(cfg *config.Config, repos []string) []ClusterCopy {
	var out []ClusterCopy
	for _, repo := range repos {
		abs, err := filepath.Abs(repo)
		if err != nil {
			continue
		}
		top := cfg.OutputDirFor(abs)
		for _, w := range []struct{ where, dir string }{
			{"snapshot", top},
			{"previous", filepath.Join(top, PreviousSubdir)},
			{"baseline", filepath.Join(top, BaselineSubdir)},
		} {
			rcpt, ok := readCopyReceipt(w.dir)
			if !ok {
				continue
			}
			c := ClusterCopy{Member: abs, Dir: w.dir, Where: w.where, GeneratedAt: rcpt.GeneratedAt}
			if w.where == "snapshot" {
				c.Files = topLevelCopyFiles(w.dir, rcpt.OutputHashes)
			} else {
				c.Files = filesUnder(w.dir)
			}
			out = append(out, c)
		}
	}
	return out
}

type copyReceipt struct {
	GeneratedAt  string            `json:"generated_at"`
	OutputHashes map[string]string `json:"output_hashes"`
	Quality      struct {
		Coverage *struct {
			ServicesTotal int `json:"services_total"`
		} `json:"coverage"`
	} `json:"quality"`
}

// readCopyReceipt reads dir's receipt and reports whether it describes a cluster: a
// graph with more than one service node, which only repositories linked together
// have. A coverage rollup alone is not that: one repository's receipt carries one
// too, with no services, when an extractor reported what it could not resolve, and
// taking it for a cluster's would delete the repository's own snapshot.
func readCopyReceipt(dir string) (copyReceipt, bool) {
	var r copyReceipt
	data, err := os.ReadFile(filepath.Join(dir, "receipt.json"))
	if err != nil || json.Unmarshal(data, &r) != nil {
		return r, false
	}
	return r, r.Quality.Coverage != nil && r.Quality.Coverage.ServicesTotal > 1
}

// topLevelCopyFiles are the files of a copy at the top of an output dir: the snapshot's
// own and every artifact its receipt hashed. The extractor cache and the hook's state
// beside them are the member's, and stay.
func topLevelCopyFiles(dir string, hashed map[string]string) []string {
	names := map[string]bool{}
	for _, n := range snapshotArtifactFiles {
		names[n] = true
	}
	for _, n := range copyRenderers {
		names[n] = true
	}
	for n := range hashed {
		if filepath.Base(n) == n {
			names[n] = true
		}
	}
	var out []string
	for n := range names {
		p := filepath.Join(dir, n)
		if _, err := os.Lstat(p); err == nil {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

func filesUnder(dir string) []string {
	var out []string
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			out = append(out, p)
		}
		return nil
	})
	return out
}

// CopyBytes are a set of copies' sizes: apparent counts every file as its size, as a
// backup or `du` on one directory does; Freed counts only the files whose every link is
// among them, the bytes their removal actually returns.
type CopyBytes struct {
	Apparent, Freed int64
}

// MeasureClusterCopies sizes copies.
func MeasureClusterCopies(copies []ClusterCopy) CopyBytes {
	type node struct {
		size          int64
		links, inside uint64
	}
	nodes := map[[2]uint64]*node{}
	var b CopyBytes
	for _, c := range copies {
		for _, f := range c.Files {
			info, err := os.Lstat(f)
			if err != nil {
				continue
			}
			b.Apparent += info.Size()
			key, links, ok := fileIdentity(info)
			if !ok {
				b.Freed += info.Size()
				continue
			}
			n := nodes[key]
			if n == nil {
				n = &node{size: info.Size(), links: links}
				nodes[key] = n
			}
			n.inside++
		}
	}
	for _, n := range nodes {
		if n.inside >= n.links {
			b.Freed += n.size
		}
	}
	return b
}

// PruneClusterCopies removes copies from the members' output dirs. When home, a cluster's
// own directory, has no baseline, the newest baseline among the copies is moved there
// first: it was the cluster's pin, and the cluster's commands now read it there. Every
// other copy is removed. It returns the copy it moved, if any.
func PruneClusterCopies(copies []ClusterCopy, home string) (*ClusterCopy, error) {
	var moved *ClusterCopy
	if home != "" {
		if _, err := os.Stat(filepath.Join(home, BaselineSubdir, "facts.jsonl")); errors.Is(err, os.ErrNotExist) {
			for i := range copies {
				c := &copies[i]
				if c.Where == "baseline" && (moved == nil || c.GeneratedAt > moved.GeneratedAt) {
					moved = c
				}
			}
		}
	}
	if moved != nil {
		if err := os.MkdirAll(home, 0o755); err != nil {
			return nil, err
		}
		if err := copyArtifacts(moved.Dir, filepath.Join(home, BaselineSubdir)); err != nil {
			return nil, fmt.Errorf("moving the cluster's baseline from %s: %w", moved.Dir, err)
		}
		if err := copyPinMark(moved.Dir, filepath.Join(home, BaselineSubdir)); err != nil {
			return nil, err
		}
	}
	for _, c := range copies {
		if err := removeCopy(c); err != nil {
			return moved, err
		}
	}
	return moved, nil
}

// copyPinMark carries pin.json, which copyArtifacts leaves behind, so the moved pin
// still says which session made it.
func copyPinMark(src, dst string) error {
	data, err := os.ReadFile(filepath.Join(src, PinMarkFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dst, PinMarkFile), data, 0o644)
}

func removeCopy(c ClusterCopy) error {
	if c.Where == "snapshot" {
		defer lockDir(c.Dir)()
		for _, f := range c.Files {
			if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		return nil
	}
	defer lockDir(filepath.Dir(c.Dir))()
	return os.RemoveAll(c.Dir)
}
