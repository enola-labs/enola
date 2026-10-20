//go:build !linux && !openbsd && !dragonfly && !darwin && !freebsd && !netbsd

package engine

import "os"

// Without a change time the memo cannot tell an edit that restored the size and
// modification time from no edit, so it answers that it cannot tell.
func fileStamp(os.FileInfo) (stamp, bool) { return stamp{}, false }
