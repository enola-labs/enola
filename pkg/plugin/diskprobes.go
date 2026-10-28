package plugin

import (
	"context"
	"sort"
	"sync"
)

// DiskProbes records the paths an extractor looked for on disk outside the files it
// was given: a resolved import into an ignored folder, a generated file. The cache
// key is computed from the given files before extraction runs, so such a read is
// invisible to it, and a target created or deleted later would be served stale. The
// engine stores the paths beside the cached facts and folds whether each exists into
// the key it looks them up under.
//
// Paths are repo-relative and slash-separated. Safe for concurrent use: an extractor
// probes from its parallel workers.
type DiskProbes struct {
	mu    sync.Mutex
	paths map[string]bool
}

type diskProbesKey struct{}

// WithDiskProbes returns a context that records disk probes into the returned set.
func WithDiskProbes(ctx context.Context) (context.Context, *DiskProbes) {
	p := &DiskProbes{paths: map[string]bool{}}
	return context.WithValue(ctx, diskProbesKey{}, p), p
}

// DiskProber returns the function an extractor calls with each path it looks for on
// disk, a no-op when the caller records none.
func DiskProber(ctx context.Context) func(relPath string) {
	p, _ := ctx.Value(diskProbesKey{}).(*DiskProbes)
	if p == nil {
		return func(string) {}
	}
	return func(relPath string) {
		p.mu.Lock()
		p.paths[relPath] = true
		p.mu.Unlock()
	}
}

// Paths returns the recorded paths, sorted.
func (p *DiskProbes) Paths() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.paths))
	for path := range p.paths {
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}
