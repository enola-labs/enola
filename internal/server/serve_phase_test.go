package server_test

import (
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/enola-labs/enola/pkg/bootstrap"
)

// phases records what a served instance reports to OnPhase, for the memory instrument.
type phases struct {
	mu    sync.Mutex
	names []string
}

func (p *phases) record(name string, _ int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.names = append(p.names, name)
}

func (p *phases) seen() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.names)
}

func (p *phases) waitFor(t *testing.T, want []string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !slices.Equal(p.seen(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("phases %v, want %v", p.seen(), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A session's generate is a build; a second session adopting that graph builds
// nothing, and must not be reported as if it had.
func TestServePhases_BuildIsReportedOnceAndAdoptionNot(t *testing.T) {
	var ph phases
	s := startServed(t, bootstrap.ServeOptions{OnPhase: ph.record})
	a, err := s.connect(t, testToken)
	if err != nil {
		t.Fatal(err)
	}
	a.generate(t, s.repo)
	b, err := s.connect(t, testToken)
	if err != nil {
		t.Fatal(err)
	}
	b.generate(t, s.repo)
	ph.waitFor(t, []string{"build"})
}

// Over the budget the graph is unloaded, then restored at the session's next call:
// one unload and one restore, and the restored graph is not reported as built.
func TestServePhases_UnloadThenRestore(t *testing.T) {
	var ph phases
	s := startServed(t, bootstrap.ServeOptions{MemoryBudget: 1, OnPhase: ph.record})
	a, err := s.connect(t, testToken)
	if err != nil {
		t.Fatal(err)
	}
	a.generate(t, s.repo)
	ph.waitFor(t, []string{"build", "unload"})
	a.call(t, "query_facts", map[string]any{"kind": "symbol", "output_mode": "summary"})
	// The restored graph is over the budget too, so it is unloaded again after the call.
	ph.waitFor(t, []string{"build", "unload", "restore", "unload"})
}

// A fixed instance reports its startup load, then a reload when the operator
// indexes again.
func TestServePhases_FixedLoadThenReload(t *testing.T) {
	var ph phases
	s, cfg := startFixedWith(t, true, ph.record)
	ph.waitFor(t, []string{"load"})
	addSymbol(t, s.repo, "Again")
	indexAs(t, cfg, s.repo)
	ph.waitFor(t, []string{"load", "reload"})
}
