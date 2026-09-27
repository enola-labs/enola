package status

import (
	"os"
	"testing"
	"time"
)

func TestParentPIDOfThisProcess(t *testing.T) {
	got, err := ParentPID(os.Getpid())
	if err != nil {
		t.Skipf("parent lookup unsupported here: %v", err)
	}
	if got != os.Getppid() {
		t.Fatalf("ParentPID(self) = %d, want %d", got, os.Getppid())
	}
}

// A hook runs a shell or two below its agent. AgentPID must climb past the processes
// between it and the agent to the one a live server names as its client, and must not
// invent a session where no server names any ancestor.
func TestAgentPIDClimbsToTheRegisteredAgent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())

	if got := AgentPID(); got != 0 {
		t.Fatalf("with no server registered, AgentPID = %d, want 0", got)
	}

	grandparent, err := ParentPID(os.Getppid())
	if err != nil || grandparent <= 1 {
		t.Skipf("no usable grandparent (%d, %v)", grandparent, err)
	}
	now := time.Now()
	if err := writeInstance(Instance{PID: os.Getpid(), StartTime: now, Heartbeat: now, ClientPID: grandparent}); err != nil {
		t.Fatal(err)
	}
	if got := AgentPID(); got != grandparent {
		t.Fatalf("AgentPID = %d, want the registered agent %d two levels up", got, grandparent)
	}
}
