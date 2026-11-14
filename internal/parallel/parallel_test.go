package parallel

import (
	"context"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// MapFiles must return results aligned to input order regardless of how the
// concurrent workers are scheduled — this ordering is what makes enola's
// per-file output deterministic.
func TestMapFiles_OrderPreserved(t *testing.T) {
	const n = 500
	items := make([]string, n)
	for i := range items {
		items[i] = strconv.Itoa(i)
	}

	got := MapFiles(context.Background(), items, func(s string) string {
		return s + "!"
	})

	if len(got) != n {
		t.Fatalf("len(got) = %d, want %d", len(got), n)
	}
	for i := range got {
		if want := items[i] + "!"; got[i] != want {
			t.Fatalf("got[%d] = %q, want %q (ordering not preserved)", i, got[i], want)
		}
	}
}

func TestMapFiles_Empty(t *testing.T) {
	got := MapFiles(context.Background(), nil, func(s string) int { return 1 })
	if len(got) != 0 {
		t.Errorf("expected empty result, got %v", got)
	}
}

func TestMapFiles_AppliesToEveryItem(t *testing.T) {
	items := []string{"a", "b", "c", "d"}
	var calls atomic.Int32
	MapFiles(context.Background(), items, func(s string) string {
		calls.Add(1)
		return s
	})
	if calls.Load() != int32(len(items)) {
		t.Errorf("fn called %d times, want %d", calls.Load(), len(items))
	}
}

// A cancelled context must not deadlock and must leave not-yet-started slots at
// the zero value. Run under -race to catch any data race on the results slice.
func TestMapFiles_Cancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before starting

	items := make([]string, 100)
	for i := range items {
		items[i] = strconv.Itoa(i)
	}

	got := MapFiles(ctx, items, func(s string) string { return "processed-" + s })
	if len(got) != len(items) {
		t.Fatalf("len(got) = %d, want %d", len(got), len(items))
	}
	// With the context already cancelled, every slot should hold the zero value.
	for i, v := range got {
		if v != "" {
			t.Fatalf("slot %d = %q, expected zero value after cancellation", i, v)
		}
	}
}

func TestMapFiles_Deterministic(t *testing.T) {
	items := make([]string, 200)
	for i := range items {
		items[i] = strconv.Itoa(i * 7)
	}
	fn := func(s string) string { return s + "-x" }

	a := MapFiles(context.Background(), items, fn)
	b := MapFiles(context.Background(), items, fn)
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("non-deterministic at %d: %q vs %q", i, a[i], b[i])
		}
	}
}

// Calls made at once share one pool: however many run, about GOMAXPROCS items are in
// flight in all, plus one per caller, which always works through its own items.
func TestMapFiles_CallsShareOnePool(t *testing.T) {
	procs := runtime.GOMAXPROCS(0)
	const calls = 4
	var inFlight, peak atomic.Int64
	items := make([]string, 200)
	var wg sync.WaitGroup
	for range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			MapFiles(context.Background(), items, func(string) int {
				n := inFlight.Add(1)
				for {
					p := peak.Load()
					if n <= p || peak.CompareAndSwap(p, n) {
						break
					}
				}
				time.Sleep(200 * time.Microsecond)
				inFlight.Add(-1)
				return 0
			})
		}()
	}
	wg.Wait()
	if limit := int64(procs + calls); peak.Load() > limit {
		t.Fatalf("%d items in flight at once, want at most %d (GOMAXPROCS %d + %d callers)", peak.Load(), limit, procs, calls)
	}
}

// A call made from inside another, with every token of the pool held, finishes: it
// runs on its caller rather than waiting for the pool.
func TestMapFiles_NestedCallsDoNotWaitForThePool(t *testing.T) {
	outer := make([]string, 4*runtime.GOMAXPROCS(0))
	inner := make([]string, 50)
	done := make(chan struct{})
	go func() {
		defer close(done)
		got := MapFiles(context.Background(), outer, func(string) int {
			sum := 0
			for _, v := range MapFiles(context.Background(), inner, func(string) int { return 1 }) {
				sum += v
			}
			return sum
		})
		for i, v := range got {
			if v != len(inner) {
				t.Errorf("outer item %d: %d inner results, want %d", i, v, len(inner))
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("nested MapFiles did not finish")
	}
}
