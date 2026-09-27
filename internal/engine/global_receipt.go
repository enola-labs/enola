package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/version"
)

// globalReceiptDirName / globalReceiptFileName locate the graph-wide receipt under
// the user's home directory (~/.enola/receipt.json).
const (
	globalReceiptDirName  = ".enola"
	globalReceiptFileName = "receipt.json"

	// snapshotMetaFileName is a repo's own snapshot metadata, inside its output
	// directory. Read when assembling the graph receipt to recover the parsed
	// source size of repos this snapshot did not itself index.
	snapshotMetaFileName = "snapshot.meta.json"
)

// globalReceiptPath resolves ~/.enola/receipt.json. It returns an error when the
// home directory is unavailable (e.g. a sandboxed run with no $HOME) so callers
// can degrade gracefully instead of failing the snapshot.
func globalReceiptPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home dir: %w", err)
	}
	return filepath.Join(home, globalReceiptDirName, globalReceiptFileName), nil
}

// A user commonly runs several enola servers at once, one per agent terminal,
// each holding a different graph. ~/.enola/receipt.json can only ever describe
// one of them — whichever process generated last — so each workspace also gets
// its OWN receipt under ~/.enola/graphs/, keyed by the repo the server was
// launched for. That is the file a restart reads, so a server started in repo A
// restores A's graph instead of whatever a sibling terminal happened to load.
const graphsDirName = "graphs"

// workspaceKey derives a stable, human-readable filename stem for a workspace
// from its absolute repo path: the sanitized base name plus a short hash of the
// full path, so two repos sharing a base name do not collide. It mirrors the key
// scheme pkg/status uses for usage files; duplicated rather than shared so
// internal/engine keeps depending on nothing outside the engine.
func workspaceKey(absRepoPath string) string {
	sum := sha256.Sum256([]byte(absRepoPath))
	short := hex.EncodeToString(sum[:])[:8]

	base := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '-', r == '_', r == '.':
			return r
		default:
			return '_'
		}
	}, filepath.Base(absRepoPath))
	if base == "" {
		base = "repo"
	}
	return base + "-" + short
}

// canonicalRepoPath normalizes a repo path so the same workspace always maps to
// the same key regardless of how it was referenced — absolute, with symlinks
// resolved (e.g. macOS /var → /private/var).
func canonicalRepoPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}

// WorkspaceReceiptPath resolves ~/.enola/graphs/<key>.json for a workspace repo.
func WorkspaceReceiptPath(repoPath string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home dir: %w", err)
	}
	return filepath.Join(home, globalReceiptDirName, graphsDirName, workspaceKey(canonicalRepoPath(repoPath))+".json"), nil
}

// LoadWorkspaceReceipt reads the graph receipt for one workspace — the repo a
// server was launched for. It is the per-workspace counterpart to
// LoadGlobalReceipt and the file a restart should prefer, since the global one
// describes whichever process wrote last.
func LoadWorkspaceReceipt(repoPath string) (*facts.GraphReceipt, error) {
	path, err := WorkspaceReceiptPath(repoPath)
	if err != nil {
		return nil, err
	}
	return readGraphReceiptFile(path)
}

// readGraphReceiptFile reads and parses a graph receipt from an explicit path.
func readGraphReceiptFile(path string) (*facts.GraphReceipt, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading graph receipt: %w", err)
	}
	var gr facts.GraphReceipt
	if err := json.Unmarshal(data, &gr); err != nil {
		return nil, fmt.Errorf("parsing graph receipt %s: %w", path, err)
	}
	return &gr, nil
}

// LoadGlobalReceipt reads and parses ~/.enola/receipt.json — the graph-wide
// multi-repo registry describing which repositories currently compose the graph
// and where they live on disk. It is the counterpart to WriteGlobalReceipt and the
// source of truth a restart uses to reload the whole graph (not just one repo).
// Returns an error when the home dir is unavailable, the file is missing, or its
// contents cannot be parsed, so callers can fall back to a single-repo restore.
func LoadGlobalReceipt() (*facts.GraphReceipt, error) {
	path, err := globalReceiptPath()
	if err != nil {
		return nil, err
	}
	return readGraphReceiptFile(path)
}

// repoEntries returns one GraphRepoEntry per repository currently in the graph.
// In multi-repo (append) mode it iterates RepoPaths(); in single-repo mode it
// falls back to the sole primary repo from the snapshot meta. Git state is captured
// per repo via gitInfo (nil for non-git dirs) and fact counts come from the store's
// byRepo index. AddedAt/CommitChangedAt are left to the merge step; InGraphFor is
// derived at write time. Entries are sorted by Label for stable output.
func (e *Engine) repoEntries(b *snapshotBundle) []facts.GraphRepoEntry {
	// label -> absolute path for every repo in the graph, read from the passed
	// bundle so all counts come from one consistent published snapshot.
	repos := b.repoPaths
	if len(repos) == 0 {
		// Single-repo graph: repoPaths is nil until a repo is appended.
		if b.snapshot == nil || b.snapshot.Meta.RepoPath == "" {
			return nil
		}
		abs := b.snapshot.Meta.RepoPath
		repos = map[string]string{b.snapshot.Meta.Label(): abs}
	}

	entries := make([]facts.GraphRepoEntry, 0, len(repos))
	for label, abs := range repos {
		entries = append(entries, facts.GraphRepoEntry{
			Label:       label,
			Path:        abs,
			Git:         gitInfo(abs, e.cfg.Output.Dir),
			FactCount:   b.store.CountByRepo(label),
			SourceBytes: e.repoSourceBytes(b, abs),
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Label < entries[j].Label })
	return entries
}

// repoSourceBytes returns a repo's parsed-source size. The repo indexed by THIS
// snapshot is answered from memory; the rest are read from their own
// snapshot.meta.json, which is authoritative for them whichever session wrote it.
//
// Zero on any failure, which the merge step then treats as "no new reading" and
// carries the previous value forward — a repo whose metadata is momentarily
// unreadable must not have its corpus silently reset to nothing.
func (e *Engine) repoSourceBytes(b *snapshotBundle, absRepo string) int64 {
	if b.snapshot != nil && b.snapshot.Meta.RepoPath == absRepo {
		return b.snapshot.Meta.SourceBytes
	}
	data, err := os.ReadFile(filepath.Join(absRepo, e.cfg.Output.Dir, snapshotMetaFileName))
	if err != nil {
		return 0
	}
	var meta struct {
		SourceBytes int64 `json:"source_bytes"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return 0
	}
	return meta.SourceBytes
}

// crossRepoEdgeCount counts the consumer->provider edges in the cross-repo "graph
// of graphs". Those edges are materialized as depends_on relations on the synthetic
// KindService nodes (one per repo), so summing them reads the true cross-repo edge
// count directly — and cheaply (there are only as many service nodes as repos). It
// deliberately does NOT use ByKind(KindDependency): that kind also covers every
// ordinary import/dependency fact, which would over-count by orders of magnitude.
func crossRepoEdgeCount(store *facts.Store) int {
	n := 0
	for _, svc := range store.ByKind(facts.KindService) {
		for _, r := range svc.Relations {
			if r.Kind == facts.RelDependsOn {
				n++
			}
		}
	}
	return n
}

// assembleGraphReceipt builds a GraphReceipt describing the current graph state.
// Membership timestamps (AddedAt/CommitChangedAt) are set to their first-write
// defaults here; WriteGlobalReceipt merges forward from any prior receipt.
func (e *Engine) assembleGraphReceipt(b *snapshotBundle, now time.Time) facts.GraphReceipt {
	nowStr := now.UTC().Format(time.RFC3339)

	entries := e.repoEntries(b)
	for i := range entries {
		entries[i].AddedAt = nowStr
		entries[i].InGraphFor = "0s"
	}

	gr := facts.GraphReceipt{
		GeneratedAt:        nowStr,
		EnolaVersion:       version.Version,
		ServiceCount:       len(b.store.ByKind(facts.KindService)),
		CrossRepoEdgeCount: crossRepoEdgeCount(b.store),
		Coverage:           coverageSummary(b.store),
		Repos:              entries,
		Writer:             &facts.GraphWriter{PID: os.Getpid(), ClientPID: int(e.sessionClient.Load())},
	}
	if b.snapshot != nil {
		gr.SnapshotID = b.snapshot.Meta.SnapshotID
		gr.FactCount = b.snapshot.Meta.FactCount
		gr.InsightCount = b.snapshot.Meta.InsightCount
	}
	return gr
}

// WriteGlobalReceipt writes ~/.enola/receipt.json for the current graph. It reads
// any existing receipt to merge forward per-repo membership timestamps (so a repo's
// added_at is preserved across regenerations and a moved commit does not reset it),
// then atomically replaces the file. It never aborts a snapshot: a missing home dir
// is logged and skipped, and a corrupt prior receipt is treated as no prior state.
func (e *Engine) WriteGlobalReceipt() error {
	// Load the published bundle once; everything below reads from it, so the receipt
	// reflects a single consistent snapshot even if a generate republishes meanwhile.
	b := e.current.Load()
	if b.snapshot == nil {
		return fmt.Errorf("no snapshot generated")
	}

	path, err := globalReceiptPath()
	if err != nil {
		log.Printf("[engine] global receipt skipped: %v", err)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating global receipt dir: %w", err)
	}

	now := time.Now().UTC()
	gr := e.assembleGraphReceipt(b, now)

	// Merge forward membership timestamps from the prior receipt, if any. Repos
	// present in the prior receipt but absent now are simply omitted: the receipt
	// is rebuilt only from current entries, so departed repos drop out.
	prevByLabel := readPriorGraphReceipt(path)
	gr.Repos = mergeRepoEntries(gr.Repos, prevByLabel, now)

	data, err := json.MarshalIndent(gr, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling global receipt: %w", err)
	}
	if err := writeFileAtomic(path, data, 0o644); err != nil {
		return fmt.Errorf("writing global receipt: %w", err)
	}
	log.Printf("[engine] wrote %s (%d repos)", path, len(gr.Repos))

	// Also write this workspace's own receipt. The global file above is shared by
	// every enola process on the machine and describes whichever generated last;
	// this one is keyed by the repo this server was launched for, so a restart
	// restores THIS graph rather than a sibling terminal's. Non-fatal — the
	// global receipt is still a usable fallback.
	if err := e.writeWorkspaceReceipt(b, data); err != nil {
		log.Printf("[engine] warning: workspace receipt not written: %v", err)
	}
	// And this session's own, which the workspace receipt cannot be: every agent
	// session started in the same directory shares that one. Non-fatal as well.
	if err := e.writeSessionReceipt(b, data); err != nil {
		log.Printf("[engine] warning: session receipt not written: %v", err)
	}
	return nil
}

// SetSessionClient records the PID of the agent process this server serves. A
// server keeps it across an MCP restart (the agent process survives the restart),
// and two concurrent sessions never share it, so it tells a restarted server which
// graph it was serving. Call it before serving; a CLI run never does.
func (e *Engine) SetSessionClient(pid int) {
	e.sessionClient.Store(int64(pid))
}

// SessionClient returns the PID set by SetSessionClient, or zero.
func (e *Engine) SessionClient() int {
	return int(e.sessionClient.Load())
}

// sessionsDirName holds one receipt per agent session under ~/.enola/graphs/.
const sessionsDirName = "sessions"

// SessionReceiptPath resolves ~/.enola/graphs/sessions/<key>.<clientPID>.json, the
// receipt of the graph one agent session holds in one workspace.
func SessionReceiptPath(repoPath string, clientPID int) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home dir: %w", err)
	}
	name := fmt.Sprintf("%s.%d.json", workspaceKey(canonicalRepoPath(repoPath)), clientPID)
	return filepath.Join(home, globalReceiptDirName, graphsDirName, sessionsDirName, name), nil
}

// SessionReceipts lists the session receipts recorded for a workspace, keyed by the
// client PID each belongs to, so a caller that can tell a live process from a dead
// one can remove the receipts of sessions that have ended.
func SessionReceipts(repoPath string) (map[int]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolving home dir: %w", err)
	}
	prefix := workspaceKey(canonicalRepoPath(repoPath)) + "."
	dir := filepath.Join(home, globalReceiptDirName, graphsDirName, sessionsDirName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := map[int]string{}
	for _, ent := range entries {
		name := ent.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".json") {
			continue
		}
		var pid int
		if _, err := fmt.Sscanf(strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".json"), "%d", &pid); err != nil || pid <= 0 {
			continue
		}
		out[pid] = filepath.Join(dir, name)
	}
	return out, nil
}

// LoadSessionReceipt reads the graph receipt one agent session wrote for a workspace.
func LoadSessionReceipt(repoPath string, clientPID int) (*facts.GraphReceipt, error) {
	path, err := SessionReceiptPath(repoPath, clientPID)
	if err != nil {
		return nil, err
	}
	return readGraphReceiptFile(path)
}

// writeSessionReceipt persists the receipt bytes for this session, when this engine
// serves one.
func (e *Engine) writeSessionReceipt(b *snapshotBundle, data []byte) error {
	client := e.SessionClient()
	repo := e.workspaceRepo(b)
	if client <= 0 || repo == "" {
		return nil
	}
	path, err := SessionReceiptPath(repo, client)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating sessions dir: %w", err)
	}
	return writeFileAtomic(path, data, 0o644)
}

// ServesReposOf reports whether the receipt names exactly the repositories of the
// graph being served. loaded is false when nothing is served yet. A receipt for the
// same repos is a refresh of this graph (a CLI --generate, or a sibling session
// re-indexing the same code); one for different repos is another graph entirely.
func (e *Engine) ServesReposOf(gr *facts.GraphReceipt) (same, loaded bool) {
	_, served := e.loadedGraph()
	if len(served) == 0 {
		return false, false
	}
	named := make(map[string]bool, len(gr.Repos))
	for _, r := range gr.Repos {
		if r.Path != "" {
			named[canonicalRepoPath(r.Path)] = true
		}
	}
	if len(named) != len(served) {
		return false, true
	}
	for p := range named {
		if !served[p] {
			return false, true
		}
	}
	return true, true
}

// writeWorkspaceReceipt persists the same receipt bytes under
// ~/.enola/graphs/<workspace>.json. The workspace is the engine's configured
// repo — stable across appends, unlike the snapshot's most-recent repo — so the
// key does not move when a second repo is appended to the graph.
func (e *Engine) writeWorkspaceReceipt(b *snapshotBundle, data []byte) error {
	repo := e.workspaceRepo(b)
	if repo == "" {
		return nil
	}
	path, err := WorkspaceReceiptPath(repo)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating graphs dir: %w", err)
	}
	if err := writeFileAtomic(path, data, 0o644); err != nil {
		return fmt.Errorf("writing workspace receipt: %w", err)
	}
	log.Printf("[engine] wrote %s", path)
	return nil
}

// workspaceRepo returns the repo this engine's graph belongs to: the configured
// repo when set, otherwise the snapshot's primary repo.
func (e *Engine) workspaceRepo(b *snapshotBundle) string {
	if e.cfg != nil && e.cfg.Repo != "" {
		return canonicalRepoPath(e.cfg.Repo)
	}
	if b.snapshot != nil && b.snapshot.Meta.RepoPath != "" {
		return canonicalRepoPath(b.snapshot.Meta.RepoPath)
	}
	return ""
}

// GraphReceipt returns the receipt for the graph this engine holds RIGHT NOW,
// assembled in memory from the published snapshot bundle. It exists so a viewer
// (the dashboard) can describe its own process's graph instead of reading the
// shared ~/.enola/receipt.json, which any other running server may have
// overwritten with a different repo set.
//
// Membership timestamps are merged forward from this workspace's receipt on
// disk, so "added at" / "in graph for" survive a restart. Returns nil when no
// snapshot is loaded.
func (e *Engine) GraphReceipt() *facts.GraphReceipt {
	b := e.current.Load()
	return e.graphReceipt(b)
}

// DashboardState captures the store, snapshot, and graph receipt from one
// published bundle. A concurrent regeneration may publish another bundle after
// this returns, but these three values will continue to describe one generation.
func (e *Engine) DashboardState() (*facts.Store, *facts.Snapshot, *facts.GraphReceipt) {
	b := e.current.Load()
	return b.store, b.snapshot, e.graphReceipt(b)
}

func (e *Engine) graphReceipt(b *snapshotBundle) *facts.GraphReceipt {
	if b.snapshot == nil {
		return nil
	}
	now := time.Now().UTC()
	gr := e.assembleGraphReceipt(b, now)

	if path, err := WorkspaceReceiptPath(e.workspaceRepo(b)); err == nil {
		gr.Repos = mergeRepoEntries(gr.Repos, readPriorGraphReceipt(path), now)
	}
	return &gr
}

// mergeRepoEntries carries per-repo membership state forward from a prior receipt
// (keyed by label) onto the freshly-assembled current entries, and stamps each
// entry's derived InGraphFor. For a repo already present, AddedAt is preserved (a
// regeneration is not a re-entry) and a moved commit records CommitChangedAt=now
// WITHOUT resetting AddedAt; an unchanged commit carries the prior CommitChangedAt
// forward. A repo absent from prev keeps its default AddedAt=now. cur already
// excludes departed repos, so they drop out.
func mergeRepoEntries(cur []facts.GraphRepoEntry, prevByLabel map[string]facts.GraphRepoEntry, now time.Time) []facts.GraphRepoEntry {
	nowStr := now.UTC().Format(time.RFC3339)
	for i := range cur {
		c := &cur[i]
		if prev, ok := prevByLabel[c.Label]; ok {
			c.AddedAt = prev.AddedAt
			if prev.Git != nil && c.Git != nil && prev.Git.Commit != c.Git.Commit {
				c.CommitChangedAt = nowStr
			} else {
				c.CommitChangedAt = prev.CommitChangedAt
			}
			// A zero reading means the repo's metadata could not be read this
			// time, not that its corpus vanished. Carry the last known size
			// forward rather than writing the gap through — otherwise one
			// unreadable file silently un-prices every later query on that repo.
			if c.SourceBytes == 0 {
				c.SourceBytes = prev.SourceBytes
			}
		}
		c.InGraphFor = inGraphFor(c.AddedAt, now)
	}
	return cur
}

// readPriorGraphReceipt loads the existing global receipt keyed by repo label. A
// missing or corrupt file yields an empty map (the corrupt case is logged), so a
// hand-edited or truncated receipt self-heals rather than failing the write.
func readPriorGraphReceipt(path string) map[string]facts.GraphRepoEntry {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil // missing (or unreadable) => no prior state
	}
	var prev facts.GraphReceipt
	if err := json.Unmarshal(data, &prev); err != nil {
		log.Printf("[engine] warning: ignoring corrupt global receipt %s: %v", path, err)
		return nil
	}
	byLabel := make(map[string]facts.GraphRepoEntry, len(prev.Repos))
	for _, r := range prev.Repos {
		byLabel[r.Label] = r
	}
	return byLabel
}

// inGraphFor returns a human-readable duration since addedAt (RFC3339), rounded to
// the second. It falls back to "0s" when addedAt cannot be parsed.
func inGraphFor(addedAt string, now time.Time) string {
	t, err := time.Parse(time.RFC3339, addedAt)
	if err != nil {
		return "0s"
	}
	d := now.Sub(t)
	if d < 0 {
		d = 0
	}
	return d.Round(time.Second).String()
}

// writeFileAtomic writes data to a temp file in the destination directory and
// renames it over path, so a concurrent reader never sees a torn/partial receipt.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
