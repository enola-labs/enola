//go:build windows

package engine

import "os"

// fileIdentity is not read on Windows, where FileInfo carries no link count; every
// file then counts as freed in full.
func fileIdentity(os.FileInfo) ([2]uint64, uint64, bool) {
	return [2]uint64{}, 0, false
}
