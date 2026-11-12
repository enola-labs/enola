//go:build !windows

package command

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/enola-labs/enola/pkg/bootstrap"
)

// probeMemOnSignal makes SIGUSR1 write a memory mark named "probe" while ctx lasts,
// so a benchmark can read what a served instance holds at a moment of its choosing:
// after the traffic of a reload has drained, or after idle sessions timed out. Only
// under --memstats; otherwise the signal is ignored, as Go ignores it by default.
func probeMemOnSignal(ctx context.Context) {
	if !bootstrap.MemWatchActive() {
		return
	}
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGUSR1)
	go func() {
		defer signal.Stop(ch)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ch:
				bootstrap.MarkMemPhase("probe", 0)
			}
		}
	}()
}
