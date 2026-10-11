package history

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/filelock"
	pkghistory "github.com/enola-labs/enola/pkg/history"
)

func SourceID() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home dir: %w", err)
	}
	path := filepath.Join(home, ".enola", "source")
	if raw, err := os.ReadFile(path); err == nil {
		if s := sanitizeSource(strings.TrimSpace(string(raw))); s != "" {
			return s, nil
		}
	}
	host, _ := os.Hostname()
	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", fmt.Errorf("generating a source id: %w", err)
	}
	id := sanitizeSource(host) + "-" + hex.EncodeToString(suffix[:])
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(id+"\n"), 0o644); err != nil {
		return "", fmt.Errorf("recording the source id in %s: %w", path, err)
	}
	return id, nil
}

func sanitizeSource(s string) string {
	out := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '-', r == '_', r == '.':
			return r
		default:
			return '_'
		}
	}, s)
	if out == "" || out == "." || out == ".." {
		return "machine"
	}
	return out
}

func initShare(dir string) error {
	for _, sub := range []string{dir, filepath.Join(dir, pkghistory.ShareEntriesDir), filepath.Join(dir, pkghistory.ShareChainsDir)} {
		if err := os.MkdirAll(sub, 0o755); err != nil {
			return fmt.Errorf("creating %s: %w", sub, err)
		}
	}
	markerPath := filepath.Join(dir, pkghistory.ShareFormatFileName)
	if raw, err := os.ReadFile(markerPath); err == nil {
		if v := strings.TrimSpace(string(raw)); v != pkghistory.ShareFormatValue {
			return &pkghistory.UnknownShareFormatError{Dir: dir, Format: v}
		}
		return nil
	}
	// Written through a temp file and renamed into place, because a shared store is
	// by definition written by more than one machine and two of them may initialise
	// it at once. os.WriteFile truncates before it writes, so a concurrent reader
	// could observe the marker EMPTY — and an empty marker is not a missing one, it
	// is an unknown format, which this package refuses to read rather than misread.
	// The refusal was correct; the torn file it was reading was the bug.
	tmp, err := os.CreateTemp(dir, pkghistory.ShareFormatFileName+".tmp-*")
	if err != nil {
		return fmt.Errorf("creating %s: %w", markerPath, err)
	}
	// Cleanup is best-effort on every failure path: the temp file is named
	// <marker>.tmp-*, so a leftover is inert — it is not the marker, and nothing
	// reads it — and reporting the write error matters more than reporting a failure
	// to tidy up after it.
	cleanup := func() { _ = tmp.Close(); _ = os.Remove(tmp.Name()) }
	if _, err := tmp.WriteString(pkghistory.ShareFormatValue + "\n"); err != nil {
		cleanup()
		return fmt.Errorf("writing %s: %w", markerPath, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("writing %s: %w", markerPath, err)
	}
	if err := os.Rename(tmp.Name(), markerPath); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("writing %s: %w", markerPath, err)
	}
	return nil
}

type PushOptions struct {
	Source         string
	IncludeWorking bool
}

type PushReport struct {
	Store          string
	Source         string
	Pushed         int
	AlreadyThere   int
	Working        int
	HeaderOnly     []string
	Unavailable    []string
	EntriesWritten int
}

func Push(localRoot, storeDir string, opts PushOptions) (PushReport, error) {
	rep := PushReport{Store: storeDir}
	entries, err := pkghistory.Read(localRoot)
	if err != nil {
		return rep, err
	}
	entries = pkghistory.SortedByTime(entries)

	source := sanitizeSource(opts.Source)
	if opts.Source == "" {
		source, err = SourceID()
		if err != nil {
			return rep, err
		}
	}
	rep.Source = source

	if err := initShare(storeDir); err != nil {
		return rep, err
	}
	sh, err := pkghistory.OpenShare(storeDir)
	if err != nil {
		return rep, err
	}
	if err := checkPushAnchor(localRoot, storeDir, source, sh); err != nil {
		return rep, err
	}

	own := map[string]bool{}
	for _, r := range sh.Records {
		if r.Source == source && r.Kind == pkghistory.ShareKindRevision {
			own[r.ID] = true
		}
	}
	head := sh.Heads[source]

	chainPath := pkghistory.ShareChainPath(storeDir, source)
	lock, err := filelock.Acquire(chainPath)
	if err != nil {
		lock = nil
	}
	defer lock.Release()

	var chainLines []byte
	for _, e := range entries {
		if e.ID == "" {
			continue
		}
		if e.Working() && !opts.IncludeWorking {
			rep.Working++
			continue
		}
		if own[e.ID] {
			rep.AlreadyThere++
			continue
		}
		payload, err := payloadFor(localRoot, e)
		if err != nil {
			if errors.Is(err, pkghistory.ErrThinned) {
				rep.HeaderOnly = append(rep.HeaderOnly, e.Short())
			} else {
				rep.Unavailable = append(rep.Unavailable, fmt.Sprintf("%s (%v)", e.Short(), err))
			}
			continue
		}
		raw, err := payload.Encode()
		if err != nil {
			return rep, err
		}
		wrote, err := writeShareEntry(pkghistory.ShareEntryPath(storeDir, e.ID), raw)
		if err != nil {
			return rep, err
		}
		if wrote {
			rep.EntriesWritten++
		}

		sum := e.Summary
		rec := pkghistory.ShareRecord{
			Prev:    head,
			Kind:    pkghistory.ShareKindRevision,
			Repo:    e.Repo,
			ID:      e.ID,
			Payload: pkghistory.ShareDigest(raw),
			At:      e.At,
			Git:     e.Git,
			Epoch:   e.Epoch,
			Summary: &sum,
		}
		line, err := json.Marshal(rec)
		if err != nil {
			return rep, fmt.Errorf("encoding chain record for %s: %w", e.Short(), err)
		}
		chainLines = append(chainLines, line...)
		chainLines = append(chainLines, '\n')
		head = pkghistory.ShareRecordHash(line)
		own[e.ID] = true
		rep.Pushed++
	}

	if len(chainLines) > 0 {
		if err := appendChain(chainPath, chainLines); err != nil {
			return rep, err
		}
	}
	if err := writePushAnchor(localRoot, storeDir, source, head); err != nil {
		return rep, err
	}
	return rep, nil
}

func payloadFor(localRoot string, e pkghistory.Entry) (*pkghistory.SharePayload, error) {
	if e.Blob == nil {
		return nil, pkghistory.ErrThinned
	}
	factLines, insightLines, rev, err := pkghistory.LoadLines(localRoot, e.Blob.Segment, e.Blob.Member)
	if err != nil {
		return nil, err
	}
	if rev.Receipt.SnapshotID != "" && rev.Receipt.SnapshotID != e.ID {
		return nil, fmt.Errorf("stored contents belong to %s, not %s — the local segment numbering has been disturbed",
			pkghistory.ShortID(rev.Receipt.SnapshotID), e.Short())
	}
	rc := rev.Receipt
	return &pkghistory.SharePayload{
		ID:        e.ID,
		Repo:      e.Repo,
		Epoch:     e.Epoch,
		FactsHash: rev.FactsHash,
		Meta: pkghistory.ShareMeta{
			EnolaVersion:     rc.EnolaVersion,
			ExtractorVersion: rc.ExtractorVersion,
			ConfigHash:       rc.ConfigHash,
			IgnoreGlobHash:   rc.IgnoreGlobHash,
			Extractors:       rc.Extractors,
			Explainers:       rc.Explainers,
			Renderers:        rc.Renderers,
			ProvidersRan:     facts.RanProviders(rc.Providers),
		},
		FactLines:    factLines,
		InsightLines: insightLines,
	}, nil
}

func writeShareEntry(path string, raw []byte) (bool, error) {
	if _, err := os.Stat(path); err == nil {
		return false, nil
	}
	var gzipped bytes.Buffer
	zw := gzip.NewWriter(&gzipped)
	if _, err := zw.Write(raw); err != nil {
		_ = zw.Close()
		return false, fmt.Errorf("compressing shared entry: %w", err)
	}
	if err := zw.Close(); err != nil {
		return false, fmt.Errorf("compressing shared entry: %w", err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, fmt.Errorf("creating %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".tmp-entry-")
	if err != nil {
		return false, fmt.Errorf("staging shared entry in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(gzipped.Bytes()); err != nil {
		_ = tmp.Close()
		return false, fmt.Errorf("writing shared entry: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return false, fmt.Errorf("closing shared entry: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return false, fmt.Errorf("publishing shared entry: %w", err)
	}
	return true, nil
}

func appendChain(path string, lines []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("opening %s: %w", path, err)
	}
	if _, err := f.Write(lines); err != nil {
		_ = f.Close()
		return fmt.Errorf("appending to %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", path, err)
	}
	return nil
}

func anchorPath(localRoot, storeDir, source string) string {
	sum := sha256.Sum256([]byte(storeDir))
	return filepath.Join(localRoot, "share-state", hex.EncodeToString(sum[:])[:12]+"-"+source+".head")
}

func checkPushAnchor(localRoot, storeDir, source string, sh *pkghistory.Share) error {
	raw, err := os.ReadFile(anchorPath(localRoot, storeDir, source))
	if err != nil {
		return nil
	}
	anchor := strings.TrimSpace(string(raw))
	if anchor == "" {
		return nil
	}
	for _, r := range sh.Records {
		if r.Source == source && r.Hash == anchor {
			return nil
		}
	}
	return fmt.Errorf(
		"this machine last pushed chain record %s to %s, and the store's chain for %s no longer contains it — the chain was truncated or rewritten. Refusing to push onto it; run `history verify %s` and repair the store first",
		pkghistory.ShortID(anchor), storeDir, source, storeDir)
}

func writePushAnchor(localRoot, storeDir, source, head string) error {
	path := anchorPath(localRoot, storeDir, source)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	return os.WriteFile(path, []byte(head+"\n"), 0o644)
}

type PullOptions struct {
	Repo string
}

type PullReport struct {
	Store        string
	Repo         string
	Pulled       int
	AlreadyLocal int
	Pruned       []string
	Gaps         []string
}

func Pull(localRoot, storeDir string, opts PullOptions) (PullReport, error) {
	rep := PullReport{Store: storeDir}
	sh, err := pkghistory.OpenShare(storeDir)
	if err != nil {
		return rep, err
	}
	local, err := pkghistory.Read(localRoot)
	if err != nil && !isNoHistory(err) {
		return rep, err
	}

	repo, err := pullRepo(opts.Repo, local, sh)
	if err != nil {
		return rep, err
	}
	rep.Repo = repo

	have := map[string]bool{}
	for _, e := range local {
		have[e.ID] = true
	}

	for _, rec := range sh.RevisionsFor(repo) {
		if have[rec.ID] {
			rep.AlreadyLocal++
			continue
		}
		p, err := sh.LoadPayload(rec.ID)
		if err != nil {
			var pruned *pkghistory.PrunedError
			var gap *pkghistory.GapError
			switch {
			case errors.As(err, &pruned):
				rep.Pruned = append(rep.Pruned, pkghistory.ShortID(rec.ID))
				continue
			case errors.As(err, &gap):
				rep.Gaps = append(rep.Gaps, pkghistory.ShortID(rec.ID))
				continue
			}
			return rep, err
		}
		entry := sh.EntryFor(rec)
		recorded, err := Append(localRoot, entry, Options{
			Contents: &Contents{
				FactLines:    p.FactLines,
				InsightLines: p.InsightLines,
				Receipt:      receiptFromShare(p, rec),
			},
		})
		if err != nil {
			return rep, err
		}
		if recorded {
			have[rec.ID] = true
			rep.Pulled++
		}
	}
	return rep, nil
}

func pullRepo(requested string, local []pkghistory.Entry, sh *pkghistory.Share) (string, error) {
	repo, err := pkghistory.UnionRepo(local, sh, requested)
	if err != nil {
		return "", err
	}
	if repo == "" {
		return "", fmt.Errorf("the local history records no repository identity — pass --repo")
	}
	return repo, nil
}

func receiptFromShare(p *pkghistory.SharePayload, rec pkghistory.ShareRecord) facts.Receipt {
	return facts.Receipt{
		SnapshotID:       p.ID,
		EnolaVersion:     p.Meta.EnolaVersion,
		ExtractorVersion: p.Meta.ExtractorVersion,
		GeneratedAt:      rec.At,
		RepoPath:         p.Repo,
		Git:              rec.Git,
		Extractors:       p.Meta.Extractors,
		Explainers:       p.Meta.Explainers,
		Renderers:        p.Meta.Renderers,
		Providers:        providerRecordsFromRan(p.Meta.ProvidersRan),
		ConfigHash:       p.Meta.ConfigHash,
		IgnoreGlobHash:   p.Meta.IgnoreGlobHash,
		FactCount:        len(p.FactLines),
		InsightCount:     len(p.InsightLines),
	}
}

func providerRecordsFromRan(names []string) []facts.ProviderRecord {
	out := make([]facts.ProviderRecord, 0, len(names))
	for _, n := range names {
		out = append(out, facts.ProviderRecord{Name: n})
	}
	return out
}

type SharedGCOptions struct {
	KeepLast  int
	KeepSince time.Time
	Apply     bool
	Source    string
	Now       time.Time
}

func (o SharedGCOptions) Policy() string {
	var parts []string
	if o.KeepLast > 0 {
		parts = append(parts, fmt.Sprintf("keep-last %d", o.KeepLast))
	}
	if !o.KeepSince.IsZero() {
		parts = append(parts, "keep-since "+o.KeepSince.UTC().Format(time.RFC3339))
	}
	return strings.Join(parts, ", ")
}

func (o SharedGCOptions) now() time.Time {
	if o.Now.IsZero() {
		return time.Now()
	}
	return o.Now
}

type SharedGCRemoval struct {
	ID     string
	Repo   string
	At     string
	Source string
	Bytes  int64
}

type SharedGCReport struct {
	Store      string
	Policy     string
	Revisions  int
	Keep       int
	Remove     []SharedGCRemoval
	BytesFreed int64
	Applied    bool
}

func SharedGC(storeDir string, opts SharedGCOptions) (SharedGCReport, error) {
	rep := SharedGCReport{Store: storeDir, Policy: opts.Policy(), Applied: opts.Apply}
	if opts.KeepLast <= 0 && opts.KeepSince.IsZero() {
		return rep, errors.New("shared gc needs a retention policy: --keep-last or --keep-since")
	}
	sh, err := pkghistory.OpenShare(storeDir)
	if err != nil {
		return rep, err
	}

	source := sanitizeSource(opts.Source)
	if opts.Source == "" {
		source, err = SourceID()
		if err != nil {
			return rep, err
		}
	}

	removeByRepo := map[string][]SharedGCRemoval{}
	for _, repo := range sh.Repos() {
		recs := dedupByID(sh.RevisionsFor(repo))
		rep.Revisions += len(recs)
		keep := map[string]bool{}
		if opts.KeepLast > 0 {
			for i := len(recs) - opts.KeepLast; i < len(recs); i++ {
				if i >= 0 {
					keep[recs[i].ID] = true
				}
			}
		}
		if !opts.KeepSince.IsZero() {
			for _, rec := range recs {
				at, err := time.Parse(time.RFC3339, rec.At)
				if err != nil || !at.Before(opts.KeepSince) {
					keep[rec.ID] = true
				}
			}
		}
		for _, rec := range recs {
			if keep[rec.ID] {
				rep.Keep++
				continue
			}
			path := pkghistory.ShareEntryPath(storeDir, rec.ID)
			info, err := os.Stat(path)
			if err != nil {
				rep.Keep++
				continue
			}
			removal := SharedGCRemoval{ID: rec.ID, Repo: repo, At: rec.At, Source: rec.Source, Bytes: info.Size()}
			removeByRepo[repo] = append(removeByRepo[repo], removal)
			rep.Remove = append(rep.Remove, removal)
			rep.BytesFreed += info.Size()
		}
	}
	if !opts.Apply || len(rep.Remove) == 0 {
		return rep, nil
	}

	chainPath := pkghistory.ShareChainPath(storeDir, source)
	lock, err := filelock.Acquire(chainPath)
	if err != nil {
		lock = nil
	}
	defer lock.Release()

	head := sh.Heads[source]
	var chainLines []byte
	repos := make([]string, 0, len(removeByRepo))
	for repo := range removeByRepo {
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	for _, repo := range repos {
		ids := make([]string, 0, len(removeByRepo[repo]))
		for _, removal := range removeByRepo[repo] {
			ids = append(ids, removal.ID)
		}
		sort.Strings(ids)
		rec := pkghistory.ShareRecord{
			Prev:   head,
			Kind:   pkghistory.ShareKindPrune,
			Repo:   repo,
			At:     opts.now().UTC().Format(time.RFC3339),
			Pruned: ids,
			Policy: rep.Policy,
		}
		line, err := json.Marshal(rec)
		if err != nil {
			return rep, fmt.Errorf("encoding prune record: %w", err)
		}
		chainLines = append(chainLines, line...)
		chainLines = append(chainLines, '\n')
		head = pkghistory.ShareRecordHash(line)
	}
	if err := appendChain(chainPath, chainLines); err != nil {
		return rep, err
	}
	for _, removal := range rep.Remove {
		if err := os.Remove(pkghistory.ShareEntryPath(storeDir, removal.ID)); err != nil && !os.IsNotExist(err) {
			return rep, fmt.Errorf("removing pruned entry %s: %w", pkghistory.ShortID(removal.ID), err)
		}
	}
	return rep, nil
}

func dedupByID(recs []pkghistory.ShareRecord) []pkghistory.ShareRecord {
	seen := map[string]bool{}
	out := make([]pkghistory.ShareRecord, 0, len(recs))
	for _, r := range recs {
		if seen[r.ID] {
			continue
		}
		seen[r.ID] = true
		out = append(out, r)
	}
	return out
}
