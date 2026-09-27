//go:build !darwin && !linux && !windows

package status

import "errors"

// ParentPID is not implemented on this platform; AgentPID then reports an unknown session.
func ParentPID(int) (int, error) {
	return 0, errors.New("parent process lookup not supported on this platform")
}
