// Package parallel provides small concurrency helpers used by the extractors to
// parse files in parallel while keeping output deterministic.
package parallel

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
)

// helpers is the process's pool of extra workers, one token per CPU. Every MapFiles
// call shares it, so a cluster extracting several repositories at once keeps about
// GOMAXPROCS files in flight in all, rather than GOMAXPROCS per repository. Sized
// once, at the first call.
var helpers struct {
	once   sync.Once
	tokens chan struct{}
}

func helperTokens() chan struct{} {
	helpers.once.Do(func() { helpers.tokens = make(chan struct{}, runtime.GOMAXPROCS(0)) })
	return helpers.tokens
}

// MapFiles applies fn to each item concurrently and returns the results in the SAME
// order as items (results[i] == fn(items[i])).
//
// The calling goroutine works through the items itself, and up to GOMAXPROCS-1
// helpers join it, each holding a token of the process-wide pool while it works. A
// helper that gets no token before the caller has claimed every item does not start,
// so a call never waits on the pool: one made from inside another, or while other
// calls hold every token, runs on its caller alone.
//
// Output order is independent of completion order, so callers that concatenate
// the per-item results get byte-for-byte deterministic output regardless of how
// the work was scheduled — which is essential for enola's reproducibility
// guarantee. If ctx is cancelled mid-run, items not yet started are skipped and
// their slots hold the zero value of T.
func MapFiles[T any](ctx context.Context, items []string, fn func(item string) T) []T {
	results := make([]T, len(items))
	if len(items) == 0 {
		return results
	}

	var next atomic.Int64
	work := func() {
		for {
			i := int(next.Add(1) - 1)
			if i >= len(items) {
				return
			}
			// Honor cancellation by skipping the items not yet started.
			if ctx.Err() != nil {
				continue
			}
			results[i] = fn(items[i])
		}
	}

	extra := min(runtime.GOMAXPROCS(0), len(items)) - 1
	tokens := helperTokens()
	claimed := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(extra)
	for w := 0; w < extra; w++ {
		go func() {
			defer wg.Done()
			select {
			case tokens <- struct{}{}:
			case <-claimed:
				return
			}
			defer func() { <-tokens }()
			work()
		}()
	}
	work()
	close(claimed)
	wg.Wait()

	return results
}
