package engine

import (
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// SnapshotRecord is what the engine tells a recorder about a snapshot whose artifacts
// it has just written.
type SnapshotRecord struct {
	RepoPath string
	// Meta is the snapshot's metadata as written, output hashes included. The
	// published snapshot's own Meta lacks those until the republish.
	Meta     facts.SnapshotMeta
	Snapshot *facts.Snapshot
	// FactsPath is the facts.jsonl just written.
	FactsPath string
	// PreviousDir holds the snapshot the write rotated out, the one before this.
	PreviousDir string
	// Parents and Repos each cost a git invocation, so they are functions: a recorder
	// that has already recorded this snapshot never calls them.
	Parents func() []string
	Repos   func() []RepoState
}

// RepoState is where one repository of a multi-repo graph sat at snapshot time.
type RepoState struct {
	Label  string
	Commit string
	Dirty  bool
}

// SetSnapshotRecorder registers a function called at the end of every WriteArtifacts,
// after every artifact is on disk. It is how the architecture history is recorded
// without the engine knowing what a history is: the composition root wires one. A
// recorder must not fail the snapshot, so it returns nothing.
func (e *Engine) SetSnapshotRecorder(record func(SnapshotRecord)) {
	e.recordSnapshot = record
}

// gitParents returns the parent commits of HEAD, for the case where the topology has to
// be drawn without the repository in hand. It is a fallback and not the source of truth:
// enola observes a sparse subset of the commit graph, so the edges BETWEEN observed
// revisions are derived by ancestry at render time, not remembered here.
func gitParents(repoPath string) []string {
	out, err := runGit(repoPath, "rev-list", "--parents", "-n", "1", "HEAD")
	if err != nil {
		return nil
	}
	// "<commit> <parent>…" — the first field is HEAD itself.
	fields := strings.Fields(out)
	if len(fields) < 2 {
		return nil // a root commit has no parents
	}
	return fields[1:]
}

// repoStates records where each repository of a MULTI-repo graph sat at snapshot time. A
// multi-repo snapshot has no single commit, so a renderer needs the vector; single-repo
// graphs get nil, since the snapshot's own git info already says everything.
func repoStates(e *Engine, b *snapshotBundle) []RepoState {
	if len(b.repoPaths) < 2 {
		return nil
	}
	states := make([]RepoState, 0, len(b.repoPaths))
	for label, abs := range b.repoPaths {
		r := RepoState{Label: label}
		if gi := gitInfo(abs, e.cfg.Output.Dir); gi != nil {
			r.Commit, r.Dirty = gi.Commit, gi.Dirty
		}
		states = append(states, r)
	}
	// Sorted by label: repoPaths is a map, so an unsorted vector would reorder itself
	// between runs and make two byte-identical graphs produce two different log lines.
	sort.Slice(states, func(i, j int) bool { return states[i].Label < states[j].Label })
	return states
}
