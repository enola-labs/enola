//go:build windows

package status

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ParentPID returns the parent of process pid, from a toolhelp process snapshot.
func ParentPID(pid int) (int, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return 0, err
	}
	defer func() { _ = windows.CloseHandle(snap) }()
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	for err = windows.Process32First(snap, &entry); err == nil; err = windows.Process32Next(snap, &entry) {
		if int(entry.ProcessID) == pid {
			return int(entry.ParentProcessID), nil
		}
	}
	return 0, fmt.Errorf("process %d not found", pid)
}
