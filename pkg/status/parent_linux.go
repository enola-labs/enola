//go:build linux

package status

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// ParentPID returns the parent of process pid, read from /proc/<pid>/stat.
func ParentPID(pid int) (int, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	// The command name is parenthesized and may itself contain spaces or ")", so the
	// fields are counted from the LAST ")": state, then the parent PID.
	s := string(data)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, fmt.Errorf("unexpected /proc/%d/stat format", pid)
	}
	fields := strings.Fields(s[i+1:])
	if len(fields) < 2 {
		return 0, fmt.Errorf("unexpected /proc/%d/stat format", pid)
	}
	return strconv.Atoi(fields[1])
}
