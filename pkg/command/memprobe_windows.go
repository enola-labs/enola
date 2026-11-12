//go:build windows

package command

import "context"

// probeMemOnSignal does nothing on Windows, which has no SIGUSR1.
func probeMemOnSignal(context.Context) {}
