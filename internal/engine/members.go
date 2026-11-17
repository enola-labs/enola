package engine

import (
	"encoding/json"
	"fmt"
	"os"
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

// gitAt is repository abs's git state as the turn that indexed it into b read it,
// and a fresh read for a repository b holds no turn for (a graph restored from
// disk). The history vector and the graph receipt ask for every member after a
// cluster build, and reading git again there costs four subprocesses per member for
// an answer the run already has. It is also the more accurate answer: the facts
// describe the tree as it was when the turn read it, not as it is now.
func (b *snapshotBundle) gitAt(abs, outputDir string) *facts.GitInfo {
	turn, ok := b.members[abs]
	if !ok {
		return gitInfo(abs, outputDir)
	}
	if turn.Git == nil {
		return nil
	}
	g := *turn.Git
	return &g
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
	return clusterReceipt(b.snapshot.Meta, b.members)
}

// clusterReceipt is meta's receipt, with a member entry per repository when members
// holds several.
func clusterReceipt(meta facts.SnapshotMeta, members map[string]facts.SnapshotMeta) facts.Receipt {
	r := meta.Receipt()
	if len(members) < 2 {
		return r
	}
	for path, m := range members {
		label := m.RepoLabel
		if label == "" {
			label = filepath.Base(path)
		}
		r.Members = append(r.Members, facts.MemberReceipt{
			Repo: label, RepoPath: path, Git: m.Git, Extractors: m.Extractors,
			FilesSeen: m.FilesSeen, FilesParsed: m.FilesParsed, FilesSkipped: m.FilesSkipped, DirsSkipped: m.DirsSkipped, ParseErrors: m.ParseErrors,
		})
	}
	// The top-level provenance was one member's, the anchor's (see writeBundle): its
	// path, its git state, the time indexing it took, the extractors it needed and
	// its walk census. Each member's entry carries those; the cluster's own are the
	// extractors any member needed, and no path, git state, duration or census.
	r.RepoPath, r.Git, r.Duration = "", nil, ""
	extractors := map[string]bool{}
	for _, m := range r.Members {
		for _, x := range m.Extractors {
			extractors[x] = true
		}
	}
	r.Extractors = make([]string, 0, len(extractors))
	for x := range extractors {
		r.Extractors = append(r.Extractors, x)
	}
	sort.Strings(r.Extractors)
	// The union's extraction quality is its members' together; the samples were the
	// last member's alone, and are each member's own receipt's to give.
	q := &r.Quality
	q.Census = nil
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

// Members is every repository the published graph holds, by absolute path, sorted.
// Several make a cluster, whose artifacts are in GraphDir(Members()).
func (e *Engine) Members() []string {
	b := e.current.Load()
	if b == nil {
		return nil
	}
	return b.memberPaths()
}

// memberPaths is every repository b holds, by absolute path, sorted. Both maps are
// read: members is keyed by path, so two repositories that share a label are still
// two, and repoPaths is all a graph restored from disk has.
func (b *snapshotBundle) memberPaths() []string {
	seen := map[string]bool{}
	for _, abs := range b.repoPaths {
		seen[abs] = true
	}
	for abs := range b.members {
		seen[abs] = true
	}
	out := make([]string, 0, len(seen))
	for abs := range seen {
		out = append(out, abs)
	}
	sort.Strings(out)
	return out
}

// MembersMetaFile is the file in a cluster's directory holding each member's own
// provenance: what each member's snapshot.meta.json said when members held a copy of
// the union. Drift, restore labels and freshness read a member's entry here.
const MembersMetaFile = "members.json"

// writeMembersMeta writes b's member metas, sorted by path, into dir. The union
// fields are the cluster's snapshot.meta.json's; each entry carries only the
// member's own provenance.
func writeMembersMeta(dir string, b *snapshotBundle) error {
	paths := make([]string, 0, len(b.members))
	for p := range b.members {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	out := make([]facts.SnapshotMeta, 0, len(paths))
	for _, p := range paths {
		out = append(out, withMemberProvenance(facts.SnapshotMeta{}, b.members[p]))
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling %s: %w", MembersMetaFile, err)
	}
	if err := writeFileAtomic(filepath.Join(dir, MembersMetaFile), data, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", MembersMetaFile, err)
	}
	return nil
}

// ReadMembersMeta reads a cluster directory's members.json, keyed by member path.
func ReadMembersMeta(dir string) (map[string]facts.SnapshotMeta, error) {
	data, err := os.ReadFile(filepath.Join(dir, MembersMetaFile))
	if err != nil {
		return nil, err
	}
	var list []facts.SnapshotMeta
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("reading %s: %w", MembersMetaFile, err)
	}
	out := make(map[string]facts.SnapshotMeta, len(list))
	for _, m := range list {
		out[m.RepoPath] = m
	}
	return out, nil
}

// ReadMemberMeta is repoPath's meta in the cluster directory dir: the cluster's
// snapshot.meta.json with the member's own provenance from members.json overlaid. It
// is what that member's own snapshot.meta.json held when a cluster wrote its union
// into every member.
func ReadMemberMeta(dir, repoPath string) (*facts.SnapshotMeta, error) {
	if _, err := os.Stat(filepath.Join(dir, "facts.jsonl")); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(dir, snapshotMetaFileName))
	if err != nil {
		return nil, err
	}
	var union facts.SnapshotMeta
	if err := json.Unmarshal(raw, &union); err != nil {
		return nil, err
	}
	members, err := ReadMembersMeta(dir)
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(repoPath)
	if err != nil {
		return nil, err
	}
	turn, ok := members[abs]
	if !ok {
		return nil, fmt.Errorf("%s is not a member of the graph in %s", repoPath, dir)
	}
	m := withMemberProvenance(union, turn)
	return &m, nil
}
