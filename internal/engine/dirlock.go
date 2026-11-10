package engine

import "sync"

// dirLocks serializes writes into one output directory across every engine in the
// process. An agent's server has one engine, so this never contends there; a served
// instance has one per session, and two sessions indexing the same repository would
// otherwise rotate previous/, write facts.jsonl and pin baseline/ in the same
// directory at once, leaving a mix of both runs. Last writer wins, whole.
//
// Package-level because what it protects is: the filesystem is shared by every
// engine, whatever owns them.
var dirLocks sync.Map // abs output dir -> *sync.Mutex

func lockDir(dir string) func() {
	m, _ := dirLocks.LoadOrStore(dir, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}
