package bootstrap

// Entry points that only this package's tests call. They are kept out of the
// production files so the binary carries only what it runs.

// PeakHeap reports the highest HeapAlloc seen so far, for a caller that wants the
// figure without ending the watch. Zero when not instrumented.
func (w *MemWatch) PeakHeap() uint64 {
	if w == nil {
		return 0
	}
	return w.peak.Load()
}
