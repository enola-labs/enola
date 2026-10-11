package status

// Entry points that only this package's tests call. They are kept out of the
// production files so the binary carries only what it runs.

// OnToolCall records a completed call by name only, crediting it as a successful
// query. It is retained for callers that cannot describe the outcome; the server
// uses Record, which prices the call properly.
func (t *Tracker) OnToolCall(toolName, repo string) {
	t.Record(ToolCall{Tool: toolName, Repo: repo, OK: true})
}

// GetStatus returns the current status snapshot for a single repo. Caller need
// not hold the lock.
func (t *Tracker) GetStatus(repo string) StatusInfo {
	repo = canonicalRepoPath(repo)
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.infoLocked(t.stateLocked(repo))
}
