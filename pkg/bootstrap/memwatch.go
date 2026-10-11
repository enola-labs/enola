package bootstrap

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"runtime/pprof"
	"sync/atomic"
	"time"
)

// MemWatch samples the heap for the lifetime of a run and reports the PEAK, which
// is the number that decides whether enola fits on a machine.
//
// It exists because the resident cost of a loaded graph and the cost of producing
// it are different by a factor of five to eight (measured: a 1.89M-fact kernel
// snapshot settles at 854 MiB and peaks at 6,524 MiB), and every instrument that
// was reachable before this one reported the wrong half:
//
//   - the engine's own end-of-snapshot log line runs after FreeOSMemory, so it
//     reports the survivor, never the peak;
//   - `ps rss` and macOS footprint report neither, because Darwin's
//     MADV_FREE_REUSABLE keeps freed pages resident — a kernel snapshot has shown
//     12.9 GB RSS against a 1.2 GB live heap;
//   - runtime.MemStats has no peak field, only instantaneous values.
//
// So the peak has to be sampled. The sampler ticks rather than hooking the GC
// because the peak regularly falls BETWEEN collections: two thirds of it is
// allocation churn the collector has not swept yet, which is exactly what a
// GC-triggered hook would miss.
//
// It is a development instrument, wired to hidden flags. Nothing in the product
// path reads it, and it writes to stderr rather than into any artifact — memory
// figures are not reproducible, and receipt.json is.
type MemWatch struct {
	profilePath string // "" writes no profiles, only the summary line
	stop        chan struct{}
	done        chan struct{}
	peak        atomic.Uint64
	start       time.Time
}

// Sampling rate. ReadMemStats stops the world, so this trades resolution against
// perturbing the thing it measures.
//
// The rate is adaptive because one interval cannot serve both ends of the corpus.
// At 150ms a two-minute kernel snapshot pays about 800 pauses — under 0.1% of its
// wall clock — while still resolving the seconds-long spikes that set its peak. But
// eShop finishes in 374ms, which is TWO samples, and a peak seen twice is not a
// peak, it is a coincidence: the first measured run reported 7 MiB for a repo whose
// real peak nobody knows. So the first two seconds are sampled at 10ms, which costs
// at most 200 extra reads and is invisible on any run long enough to care about.
const (
	memWatchFastInterval = 10 * time.Millisecond
	memWatchInterval     = 150 * time.Millisecond
	memWatchFastWindow   = 2 * time.Second
)

// StartMemWatch begins sampling. profilePath may be empty, in which case only the
// summary line is produced; when set, a heap profile is written to it at every new
// high-water mark (so the last one written describes the peak) and a second one to
// profilePath+".final" describing the steady state.
//
// Writing the profile on each new high-water mark rather than once at the end is
// what makes the composition of the PEAK readable. Note that the profile's totals
// will be smaller than the HeapAlloc it was captured at, because inuse_space counts
// swept live objects and the peak is mostly unswept garbage; that difference is a
// finding, not an error.
func StartMemWatch(profilePath string) *MemWatch {
	w := &MemWatch{
		profilePath: profilePath,
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
		start:       time.Now(),
	}
	go w.run()
	return w
}

func (w *MemWatch) run() {
	defer close(w.done)
	// A timer rather than a ticker, so the interval can widen after the fast window
	// without tearing down and rebuilding the sampler.
	t := time.NewTimer(memWatchFastInterval)
	defer t.Stop()
	var ms runtime.MemStats
	for {
		select {
		case <-w.stop:
			return
		case <-t.C:
			runtime.ReadMemStats(&ms)
			if ms.HeapAlloc > w.peak.Load() {
				w.peak.Store(ms.HeapAlloc)
				w.writeProfile(w.profilePath)
			}
			next := memWatchInterval
			if time.Since(w.start) < memWatchFastWindow {
				next = memWatchFastInterval
			}
			t.Reset(next)
		}
	}
}

// writeProfile dumps a heap profile to path, or does nothing when profiles were not
// requested. A write failure is silent on purpose: this runs inside the sampling
// loop, and a full disk must not turn a working snapshot into a wall of log noise.
func (w *MemWatch) writeProfile(path string) {
	if path == "" {
		return
	}
	f, err := os.Create(path)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_ = pprof.Lookup("heap").WriteTo(f, 0)
}

// Report stops the sampler and writes one machine-readable summary line to out.
//
// factCount is what the run produced, so the scale-free figures (bytes and
// allocations per fact) can be derived; pass 0 when the run produced no snapshot.
//
// The steady-state reading is taken after an explicit GC so it describes what is
// RETAINED — for a long-running MCP server, the cost of holding the graph — rather
// than whatever the collector had not got to yet.
// A nil *MemWatch is the "not instrumented" case and every method tolerates it, so
// the caller keeps one unconditional Report call per exit path instead of guarding
// each one — and cannot get a nil-check wrong on the path it forgot to test.
func (w *MemWatch) Report(out io.Writer, factCount int) {
	if w == nil {
		return
	}
	close(w.stop)
	<-w.done
	elapsed := time.Since(w.start)

	runtime.GC()
	// Guard on the FIELD, not on the concatenation: with no profile requested the
	// path is "", and ""+".final" is a perfectly valid relative filename that
	// writeProfile will happily create. --memstats used to drop a stray `.final`
	// heap profile into whatever directory it was run from.
	if w.profilePath != "" {
		w.writeProfile(w.profilePath + ".final")
	}

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	// One line, key=value, stable field order: bench-sweep.sh parses it, and a
	// human reads it. The MiB values are truncated rather than rounded so a
	// ratchet never fails on a rounding boundary.
	//
	// Write errors are dropped rather than reported. This is a diagnostic line on
	// its way to a terminal or a benchmark's stderr capture, and the only thing a
	// caller could do about a failed write is try to report it the same way.
	_, _ = fmt.Fprintf(out, "[memwatch] peakHeapMiB=%d steadyHeapMiB=%d sysMiB=%d totalAllocMiB=%d mallocs=%d facts=%d wallMs=%d\n",
		w.peak.Load()>>20, ms.HeapAlloc>>20, ms.Sys>>20, ms.TotalAlloc>>20,
		ms.Mallocs, factCount, elapsed.Milliseconds())

	if w.profilePath != "" {
		_, _ = fmt.Fprintf(out, "[memwatch] wrote %s (peak) and %s.final (steady state)\n",
			w.profilePath, w.profilePath)
	}
}
