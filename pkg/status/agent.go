package status

import "os"

// AgentPID returns the PID of the agent session this process runs under, or zero when
// it cannot tell.
//
// An MCP server knows its agent directly: it is the server's parent, recorded in the
// instance registry as ClientPID. A hook or a CLI command the agent runs does not; it
// sits below a shell or two. So it walks up its own ancestors and answers with the
// first one a live server names as its agent. A process with no enola server in its
// ancestry (a human's terminal, an agent without the MCP server) gets zero, and callers
// treat zero as "unknown session" rather than as a session of its own.
func AgentPID() int {
	clients := map[int]bool{}
	for _, inst := range LiveInstances() {
		if inst.ClientPID > 0 {
			clients[inst.ClientPID] = true
		}
	}
	if len(clients) == 0 {
		return 0
	}
	// Deep enough for any harness's shell nesting; bounded so a cycle in a
	// misreported process table cannot spin.
	pid := os.Getppid()
	for depth := 0; depth < 32 && pid > 1; depth++ {
		if clients[pid] {
			return pid
		}
		next, err := ParentPID(pid)
		if err != nil || next == pid {
			return 0
		}
		pid = next
	}
	return 0
}
