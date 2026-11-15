package engine

// A cluster extracts its members side by side and takes them into the union one
// after another, in the order the config lists them.
//
// What a member's extraction produces depends on the union in two ways only: the
// label its facts carry and whether their paths are prefixed. Both are settled in
// order by beginRunAs and prepareStore, against the members taken before it, and a
// collision between labels, another checkout of a loaded repository or a stale prior
// state can change them. So each member is extracted ahead under the place it takes
// when nothing changes it, into a store of its own, and when its turn comes the place
// it actually takes is compared with that one. The same place adopts the extraction;
// a different one discards it and extracts the member again in turn, as a cluster
// always did. Either way the union is what extracting one member after another makes.

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/enola-labs/enola/internal/facts"
)

// prefetched is one member extracted ahead of its turn.
type prefetched struct {
	id repoIdentity
	// run is the member walked and extracted into store, under the label and append
	// mode it predicted.
	run   *snapshotRun
	store *facts.Store
	err   error
}

// prefetchRepo extracts a member into a store of its own, under its own label and
// the given append mode. It touches nothing of e but the config and the extractors,
// both read-only, so several run at once.
func (e *Engine) prefetchRepo(ctx context.Context, repoPath string, appendMode, deferred bool) *prefetched {
	start := time.Now()
	id, err := e.identify(repoPath)
	if err != nil {
		return &prefetched{err: err}
	}
	resolved, err := e.resolveIntent(id.absRepo, id.label)
	if err != nil {
		return &prefetched{id: id, err: err}
	}
	x := &Engine{cfg: e.cfg, extractors: e.extractors, persistCache: e.persistCache, store: facts.NewStore()}
	r := &snapshotRun{
		ctx: ctx, start: start,
		absRepo: id.absRepo, srcRoot: sourceRoot(id.absRepo), git: id.git, repoLabel: id.label, resolved: resolved,
		appendMode: appendMode, deferred: deferred, work: x.store,
	}
	if err := x.walk(r); err != nil {
		return &prefetched{id: id, err: err}
	}
	if err := x.extract(r); err != nil {
		return &prefetched{id: id, err: err}
	}
	// Detached here rather than in the union, so this linear pass runs beside the
	// other members too. The last member is linked, not carried, and keeps its own.
	if deferred {
		x.store.DetachFrom(0)
		r.detached = true
	}
	return &prefetched{id: id, run: r, store: x.store}
}

// adoptPrefetched takes pf's facts into the union when r, the member's turn, placed
// it where pf was extracted, and reports whether it did. A failed prefetch is never
// adopted: the turn extracts again and meets the failure itself, or not, when the
// failure came from the predicted place.
func (e *Engine) adoptPrefetched(r *snapshotRun, pf *prefetched) bool {
	if pf == nil || pf.err != nil {
		return false
	}
	p := pf.run
	if p.absRepo != r.absRepo || p.repoLabel != r.repoLabel || p.appendMode != r.appendMode {
		log.Printf("[engine] %s took the label %q (append %v) where it was extracted as %q (append %v); extracting it again",
			r.absRepo, r.repoLabel, r.appendMode, p.repoLabel, p.appendMode)
		return false
	}
	r.preCount = e.store.Count()
	e.store.Add(pf.store.FactsRef()...)
	e.store.CarryDocTexts(pf.store, func(facts.FactKey) bool { return true })
	r.files, r.testFiles, r.allNames, r.skips, r.hashes = p.files, p.testFiles, p.allNames, p.skips, p.hashes
	r.usedExtractors, r.shadowed, r.parseErrs, r.provRecords = p.usedExtractors, p.shadowed, p.parseErrs, p.provRecords
	r.progress, r.t, r.detached = p.progress, p.t, p.detached
	return true
}

// clusterPrefetch extracts a cluster's members ahead of their turns, at most jobs at
// once, the largest first (extractionWeight). A finished extraction waits for its
// turn without holding a job: what waits is facts the union would hold by then, so
// it is bounded by the union, and the extraction memory by the jobs.
type clusterPrefetch struct {
	cancel context.CancelFunc
	ctx    context.Context
	wg     sync.WaitGroup
	// slots[i] receives member i's extraction; nil for a member not extracted ahead.
	slots []chan *prefetched
}

// startPrefetch starts extracting the members of a cluster ahead. With one job, or
// one member, nothing is extracted ahead and every turn extracts its own.
func (e *Engine) startPrefetch(ctx context.Context, repoPaths []string, appendFirst bool) *clusterPrefetch {
	p := &clusterPrefetch{}
	jobs := e.cfg.ClusterJobsFor(len(repoPaths))
	if jobs < 2 {
		return p
	}
	p.ctx, p.cancel = context.WithCancel(ctx)
	p.slots = make([]chan *prefetched, len(repoPaths))
	// A repository listed twice is extracted ahead once: two extractions of one
	// checkout would share its extractor cache file. The later turn extracts its own.
	seen := map[string]bool{}
	var order []int
	weight := make([]int64, len(repoPaths))
	for i, repo := range repoPaths {
		key := repo
		if abs, err := filepath.Abs(repo); err == nil {
			key = abs
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		p.slots[i] = make(chan *prefetched, 1)
		order = append(order, i)
		weight[i] = extractionWeight(key)
	}
	// The largest first: one started late is the run's tail, whichever turn it has.
	sort.SliceStable(order, func(a, b int) bool { return weight[order[a]] > weight[order[b]] })
	log.Printf("[engine] extracting %d repositories, %d at a time", len(repoPaths), jobs)
	sem := make(chan struct{}, jobs)
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for _, i := range order {
			select {
			case sem <- struct{}{}:
			case <-p.ctx.Done():
				return
			}
			p.wg.Add(1)
			go func() {
				defer p.wg.Done()
				pf := e.prefetchRepo(p.ctx, repoPaths[i], appendFirst || i > 0, i < len(repoPaths)-1)
				<-sem
				p.slots[i] <- pf
			}()
		}
	}()
	return p
}

// extractionWeight guesses how long a repository takes to extract, to start the
// longest first: the size of the git index of the checkout holding it, which grows
// with the files it tracks. Zero when there is none. It decides only when a member is
// extracted, never what is extracted.
func extractionWeight(abs string) int64 {
	for dir := abs; ; {
		dotGit := filepath.Join(dir, ".git")
		if st, err := os.Stat(dotGit); err == nil {
			index := filepath.Join(dotGit, "index")
			if !st.IsDir() {
				// A worktree: .git names the directory holding its own index.
				b, err := os.ReadFile(dotGit)
				if err != nil {
					return 0
				}
				gitdir, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir: ")
				if !ok {
					return 0
				}
				if !filepath.IsAbs(gitdir) {
					gitdir = filepath.Join(dir, gitdir)
				}
				index = filepath.Join(gitdir, "index")
			}
			if st, err := os.Stat(index); err == nil {
				return st.Size()
			}
			return 0
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return 0
		}
		dir = parent
	}
}

// take waits for member i's extraction, or nil when it was not extracted ahead or the
// run was cancelled before it started.
func (p *clusterPrefetch) take(i int) *prefetched {
	if p.slots == nil || p.slots[i] == nil {
		return nil
	}
	select {
	case pf := <-p.slots[i]:
		return pf
	case <-p.ctx.Done():
		return nil
	}
}

// stop cancels the extractions still running and waits for them to return.
func (p *clusterPrefetch) stop() {
	if p.cancel == nil {
		return
	}
	p.cancel()
	p.wg.Wait()
}
