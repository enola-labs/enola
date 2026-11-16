package engine

import (
	"context"
	"log"
	"runtime"
	"slices"
	"sync"
	"time"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/pkg/plugin"
)

// analysisJobs is how many explainers run at once, from the config and the cores
// this process may use.
func (e *Engine) analysisJobs() int {
	return e.cfg.AnalysisJobsFor(runtime.GOMAXPROCS(0))
}

// runAnnotators lets enabled explainers write derived values back onto the facts
// before any of them run Explain.
//
// Two orderings matter and neither is incidental. It runs AFTER the graph is built, so
// whole-graph derivations (afferent/efferent coupling) are computable at all; and it runs
// BEFORE every Explain rather than interleaved per explainer, so one explainer's insights
// can never depend on whether another happened to be registered ahead of it — which would
// make the snapshot depend on registration order.
//
// With more than one analysis job, the read-only passes of the annotators that split
// (plugin.SplitAnnotator) run concurrently first; the writes are then applied in
// registration order, and an annotator that does not split runs whole at its place in
// that order, so it sees exactly the writes it would have seen one after another.
//
// An annotator failure is logged and skipped, exactly as an explainer failure is: a
// missing derived prop costs a diff some detail, and refusing to produce a snapshot over
// it would cost the caller everything else in the graph.
func (e *Engine) runAnnotators(ctx context.Context) {
	var anns []plugin.Explainer
	for _, exp := range e.explainers.All() {
		if _, ok := exp.(plugin.Annotator); ok && e.cfg.IsExplainerEnabled(exp.Name()) {
			anns = append(anns, exp)
		}
	}
	applies := make([]func(), len(anns))
	errs := make([]error, len(anns))
	if jobs := e.analysisJobs(); jobs > 1 {
		sem := make(chan struct{}, jobs)
		var wg sync.WaitGroup
		for i, exp := range anns {
			split, ok := exp.(plugin.SplitAnnotator)
			if !ok {
				continue
			}
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				t0 := time.Now()
				applies[i], errs[i] = split.ComputeAnnotations(ctx, e.store)
				log.Printf("[engine] annotator %s computed in %s", exp.Name(), time.Since(t0).Round(time.Millisecond))
			}()
		}
		wg.Wait()
		for i, exp := range anns {
			if _, ok := exp.(plugin.SplitAnnotator); !ok {
				continue
			}
			if errs[i] != nil {
				log.Printf("[engine] annotator %s error: %v", exp.Name(), errs[i])
			}
			if applies[i] == nil {
				applies[i] = func() {}
			}
		}
	}
	for i, exp := range anns {
		if apply := applies[i]; apply != nil {
			log.Printf("[engine] applying annotator: %s", exp.Name())
			apply()
			continue
		}
		log.Printf("[engine] running annotator: %s", exp.Name())
		if err := exp.(plugin.Annotator).Annotate(ctx, e.store); err != nil {
			log.Printf("[engine] annotator %s error: %v", exp.Name(), err)
		}
	}
}

// slowExplainers start first when explainers run concurrently, slowest first, so
// the longest one does not begin last and become the whole wait. Measured on a
// 100-repository cluster; a name missing here only starts later, which changes
// nothing but the time.
var slowExplainers = []string{"performance", "god-class", "hotspots", "dead-methods", "dead-code", "cycles", "dependency-depth", "complexity-outliers"}

// runExplainers runs all enabled explainers.
func (e *Engine) runExplainers(ctx context.Context, allNames []string) ([]facts.Insight, []string, error) {
	e.runAnnotators(ctx)
	// The annotators are the last writers of facts: explainers, renderers and the
	// snapshot only read. Sharing the identical Props and Relations now rather than at
	// publication takes the duplicates out of the heap while the explainers, the
	// largest transient allocators of a generate, run over it.
	e.store.Freeze()

	var exps []plugin.Explainer
	for _, exp := range e.explainers.All() {
		if e.cfg.IsExplainerEnabled(exp.Name()) {
			exps = append(exps, exp)
		}
	}
	type result struct {
		insights []facts.Insight
		err      error
	}
	results := make([]result, len(exps))
	run := func(i int) {
		exp := exps[i]
		log.Printf("[engine] running explainer: %s", exp.Name())
		t0 := time.Now()
		// An explainer whose evidence includes files no extractor parses gets the
		// walked names too; see plugin.WalkAware.
		if wa, ok := exp.(plugin.WalkAware); ok {
			results[i].insights, results[i].err = wa.ExplainFiles(ctx, e.store, allNames)
		} else {
			results[i].insights, results[i].err = exp.Explain(ctx, e.store)
		}
		log.Printf("[engine] explainer %s ran in %s", exp.Name(), time.Since(t0).Round(time.Millisecond))
	}

	if jobs := min(e.analysisJobs(), len(exps)); jobs > 1 {
		// Every explainer only reads the store, frozen above; results land in their
		// own slot and are assembled below in registration order, so the output is
		// the same as one after another.
		order := make([]int, len(exps))
		for i := range order {
			order[i] = i
		}
		rank := func(i int) int {
			if r := slices.Index(slowExplainers, exps[i].Name()); r >= 0 {
				return r
			}
			return len(slowExplainers)
		}
		slices.SortStableFunc(order, func(a, b int) int { return rank(a) - rank(b) })
		next := make(chan int, len(order))
		for _, i := range order {
			next <- i
		}
		close(next)
		var wg sync.WaitGroup
		for range jobs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range next {
					run(i)
				}
			}()
		}
		wg.Wait()
	} else {
		for i := range exps {
			run(i)
		}
	}

	var allInsights []facts.Insight
	var usedNames []string
	for i, exp := range exps {
		insights, err := results[i].insights, results[i].err
		if err != nil {
			log.Printf("[engine] explainer %s error: %v", exp.Name(), err)
			continue
		}
		// Tag each insight with its producing explainer so clients can fetch and
		// filter findings by source (e.g. query_insights(explainer="unused-routes"))
		// without every explainer having to set the field itself.
		for j := range insights {
			insights[j].Source = exp.Name()
		}
		allInsights = append(allInsights, insights...)
		usedNames = append(usedNames, exp.Name())
		log.Printf("[engine] explainer %s: produced %d insights", exp.Name(), len(insights))
	}
	return allInsights, usedNames, nil
}
