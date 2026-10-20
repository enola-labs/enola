//go:build darwin || freebsd || netbsd

package engine

import (
	"os"
	"syscall"
)

func fileStamp(info os.FileInfo) (stamp, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return stamp{}, false
	}
	return stamp{ctimeNs: int64(st.Ctimespec.Sec)*1e9 + int64(st.Ctimespec.Nsec), ino: uint64(st.Ino), dev: uint64(st.Dev)}, true
}
