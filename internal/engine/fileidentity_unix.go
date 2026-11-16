//go:build !windows

package engine

import (
	"os"
	"syscall"
)

// fileIdentity is info's device and inode, and how many links it has, so the bytes a
// removal returns can be told from the bytes it only unlinks.
func fileIdentity(info os.FileInfo) ([2]uint64, uint64, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return [2]uint64{}, 0, false
	}
	return [2]uint64{uint64(st.Dev), uint64(st.Ino)}, uint64(st.Nlink), true //nolint:unconvert // Dev and Nlink widths differ by platform
}
