package engine

import (
	"path/filepath"
	"sort"

	"github.com/enola-labs/enola/internal/facts"
)

func withMemberProvenance(union, turn facts.SnapshotMeta) facts.SnapshotMeta {
	m := union
	m.RepoPath = turn.RepoPath
	m.RepoLabel = turn.RepoLabel
	m.Git = turn.Git
	m.GeneratedAt = turn.GeneratedAt
	m.Duration = turn.Duration
	m.Extractors = turn.Extractors
	m.Providers = turn.Providers
	m.FileHashes = turn.FileHashes
	m.FilesSeen = turn.FilesSeen
	m.FilesParsed = turn.FilesParsed
	m.SourceBytes = turn.SourceBytes
	m.FilesSkipped = turn.FilesSkipped
	m.DirsSkipped = turn.DirsSkipped
	m.SkippedSample = turn.SkippedSample
	m.ShadowedExtractors = turn.ShadowedExtractors
	m.ParseErrors = turn.ParseErrors
	m.ParseErrorSample = turn.ParseErrorSample
	m.Census = turn.Census
	return m
}

func memberMetas(prev *snapshotBundle, appendMode bool, absRepo string, turn facts.SnapshotMeta) map[string]facts.SnapshotMeta {
	members := map[string]facts.SnapshotMeta{}
	if appendMode && prev != nil {
		for path, meta := range prev.members {
			members[path] = meta
		}
	}
	members[absRepo] = turn
	return members
}

// MetaFor is the meta the published union writes into repoPath's own dir: the
// union's fields with the member's provenance overlaid, when that member is known
// to the bundle, and the published meta otherwise. A baseline pinned from a member
// dir carries this shape, so anything comparing the current state against it has
// to ask for the same member rather than the union's last turn.
func (e *Engine) MetaFor(repoPath string) facts.SnapshotMeta {
	b := e.current.Load()
	if b.snapshot == nil {
		return facts.SnapshotMeta{}
	}
	abs, err := filepath.Abs(repoPath)
	if err != nil {
		return b.snapshot.Meta
	}
	if turn, ok := b.members[abs]; ok {
		return withMemberProvenance(b.snapshot.Meta, turn)
	}
	return b.snapshot.Meta
}

// Receipt is the current snapshot's receipt, with a member entry per repository when
// the snapshot holds several: their provenance is each its own, and the top-level
// fields describe only the repository indexed last.
func (e *Engine) Receipt() facts.Receipt {
	b := e.current.Load()
	if b == nil || b.snapshot == nil {
		return facts.Receipt{}
	}
	r := b.snapshot.Meta.Receipt()
	if len(b.members) < 2 {
		return r
	}
	for path, m := range b.members {
		label := m.RepoLabel
		if label == "" {
			label = filepath.Base(path)
		}
		r.Members = append(r.Members, facts.MemberReceipt{
			Repo: label, RepoPath: path, Git: m.Git, Extractors: m.Extractors,
			FilesSeen: m.FilesSeen, FilesParsed: m.FilesParsed, FilesSkipped: m.FilesSkipped, DirsSkipped: m.DirsSkipped, ParseErrors: m.ParseErrors,
		})
	}
	// The union's extraction quality is its members' together; the samples were the
	// last member's alone, and are each member's own receipt's to give.
	q := &r.Quality
	q.FilesSeen, q.FilesParsed, q.FilesSkipped, q.DirsSkipped, q.ParseErrors = 0, 0, 0, 0, 0
	q.SkippedSample, q.ParseErrorSample = nil, nil
	for _, m := range r.Members {
		q.FilesSeen += m.FilesSeen
		q.FilesParsed += m.FilesParsed
		q.FilesSkipped += m.FilesSkipped
		q.DirsSkipped += m.DirsSkipped
		q.ParseErrors += m.ParseErrors
	}
	sort.Slice(r.Members, func(i, j int) bool { return r.Members[i].Repo < r.Members[j].Repo })
	return r
}
