package history

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

const (
	ShareFormatFileName = "format"
	ShareFormatValue    = "shared-history/1"
	ShareEntriesDir     = "entries"
	ShareChainsDir      = "chains"

	ShareKindRevision = "revision"
	ShareKindPrune    = "prune"
)

var ErrNoShare = errors.New("no shared history store")

type UnknownShareFormatError struct {
	Dir    string
	Format string
}

func (e *UnknownShareFormatError) Error() string {
	return fmt.Sprintf(
		"the store at %s declares format %q and this build reads %q — refusing to read rather than misread. Upgrade enola to read it",
		e.Dir, e.Format, ShareFormatValue)
}

type PrunedError struct {
	ID     string
	Source string
	At     string
	Policy string
}

func (e *PrunedError) Error() string {
	return fmt.Sprintf("revision %s was pruned from the shared store by %s on %s (policy %s) — removed by retention, not missing",
		ShortID(e.ID), e.Source, e.At, e.Policy)
}

type GapError struct {
	ID     string
	Source string
	Dir    string
}

func (e *GapError) Error() string {
	return fmt.Sprintf(
		"revision %s (recorded by %s) has no stored payload in %s and no prune record covers it — a gap: the store is missing data it claims to hold",
		ShortID(e.ID), e.Source, e.Dir)
}

type TamperError struct {
	ID     string
	Detail string
}

func (e *TamperError) Error() string {
	return fmt.Sprintf("revision %s: %s", ShortID(e.ID), e.Detail)
}

type ShareMeta struct {
	EnolaVersion     string   `json:"enola_version,omitempty"`
	ExtractorVersion string   `json:"extractor_version,omitempty"`
	ConfigHash       string   `json:"config_hash,omitempty"`
	IgnoreGlobHash   string   `json:"ignore_glob_hash,omitempty"`
	Extractors       []string `json:"extractors,omitempty"`
	Explainers       []string `json:"explainers,omitempty"`
	Renderers        []string `json:"renderers,omitempty"`
	ProvidersRan     []string `json:"providers_ran,omitempty"`
}

type SharePayload struct {
	ID           string    `json:"id"`
	Repo         string    `json:"repo"`
	Epoch        string    `json:"epoch"`
	FactsHash    string    `json:"facts_hash"`
	Meta         ShareMeta `json:"meta"`
	FactLines    []string  `json:"fact_lines"`
	InsightLines []string  `json:"insight_lines,omitempty"`
}

func (p SharePayload) Encode() ([]byte, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("encoding shared payload for %s: %w", ShortID(p.ID), err)
	}
	return raw, nil
}

func ShareDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func ShareEntryPath(dir, id string) string {
	return filepath.Join(dir, ShareEntriesDir, "sha256-"+strings.TrimPrefix(id, "sha256:")+".json.gz")
}

func ShareChainPath(dir, source string) string {
	return filepath.Join(dir, ShareChainsDir, source+".jsonl")
}

func ReadSharePayload(path string) (*SharePayload, []byte, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, err
		}
		return nil, nil, fmt.Errorf("opening %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, nil, fmt.Errorf("reading %s: %w", path, err)
	}
	defer func() { _ = gz.Close() }()

	raw, err := io.ReadAll(gz)
	if err != nil {
		return nil, nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var p SharePayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, nil, fmt.Errorf("decoding %s: %w", path, err)
	}
	return &p, raw, nil
}

type ShareRecord struct {
	Prev    string         `json:"prev"`
	Kind    string         `json:"kind"`
	Repo    string         `json:"repo,omitempty"`
	ID      string         `json:"id,omitempty"`
	Payload string         `json:"payload,omitempty"`
	At      string         `json:"at,omitempty"`
	Git     *facts.GitInfo `json:"git,omitempty"`
	Epoch   string         `json:"epoch,omitempty"`
	Summary *Summary       `json:"summary,omitempty"`
	Pruned  []string       `json:"pruned,omitempty"`
	Policy  string         `json:"policy,omitempty"`

	Source string `json:"-"`
	Hash   string `json:"-"`
	Line   int    `json:"-"`
}

func ShareRecordHash(line []byte) string {
	sum := sha256.Sum256(line)
	return "sha256:" + hex.EncodeToString(sum[:])
}

type ChainBreak struct {
	Source string
	Line   int
	Detail string
}

type Share struct {
	Dir     string
	Sources []string
	Records []ShareRecord
	Heads   map[string]string
	Breaks  []ChainBreak
}

func OpenShare(dir string) (*Share, error) {
	sh, err := openShare(dir, false)
	if err != nil {
		return nil, err
	}
	return sh, nil
}

func openShare(dir string, lenient bool) (*Share, error) {
	if err := checkShareFormat(dir); err != nil {
		return nil, err
	}
	sh := &Share{Dir: dir, Heads: map[string]string{}}

	chainsDir := filepath.Join(dir, ShareChainsDir)
	dirents, err := os.ReadDir(chainsDir)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("listing %s: %w", chainsDir, err)
	}
	for _, d := range dirents {
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
			continue
		}
		source := strings.TrimSuffix(d.Name(), ".jsonl")
		records, head, breaks, err := readChain(filepath.Join(chainsDir, d.Name()), source, lenient)
		if err != nil {
			return nil, err
		}
		sh.Sources = append(sh.Sources, source)
		sh.Records = append(sh.Records, records...)
		sh.Heads[source] = head
		sh.Breaks = append(sh.Breaks, breaks...)
	}
	sort.Strings(sh.Sources)
	return sh, nil
}

func checkShareFormat(dir string) error {
	raw, err := os.ReadFile(filepath.Join(dir, ShareFormatFileName))
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%w at %s (no %s marker — `history push` creates one)", ErrNoShare, dir, ShareFormatFileName)
		}
		return fmt.Errorf("reading the store format marker in %s: %w", dir, err)
	}
	if v := strings.TrimSpace(string(raw)); v != ShareFormatValue {
		return &UnknownShareFormatError{Dir: dir, Format: v}
	}
	return nil
}

func readChain(path, source string, lenient bool) ([]ShareRecord, string, []ChainBreak, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", nil, fmt.Errorf("reading %s: %w", path, err)
	}
	complete := len(data) > 0 && data[len(data)-1] == '\n'

	var records []ShareRecord
	var breaks []ChainBreak
	head := ""
	line := 0
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line++
		raw := bytes.TrimRight(sc.Bytes(), "\r")
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		var rec ShareRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			if !complete && line == countLines(data) {
				break
			}
			detail := fmt.Sprintf("malformed record: %v", err)
			if lenient {
				breaks = append(breaks, ChainBreak{Source: source, Line: line, Detail: detail})
				continue
			}
			return nil, "", nil, fmt.Errorf("%s:%d: %s", path, line, detail)
		}
		if rec.Prev != head {
			detail := fmt.Sprintf("chain broken: record claims predecessor %s, the chain ends at %s — a record was edited, removed or reordered",
				shortOrNone(rec.Prev), shortOrNone(head))
			if !lenient {
				return nil, "", nil, fmt.Errorf("%s:%d: %s", path, line, detail)
			}
			breaks = append(breaks, ChainBreak{Source: source, Line: line, Detail: detail})
		}
		rec.Source = source
		rec.Hash = ShareRecordHash(raw)
		rec.Line = line
		head = rec.Hash
		records = append(records, rec)
	}
	if err := sc.Err(); err != nil {
		return nil, "", nil, fmt.Errorf("reading %s: %w", path, err)
	}
	return records, head, breaks, nil
}

func countLines(data []byte) int {
	n := bytes.Count(data, []byte{'\n'})
	if len(data) > 0 && data[len(data)-1] != '\n' {
		n++
	}
	return n
}

func shortOrNone(hash string) string {
	if hash == "" {
		return "(chain start)"
	}
	return ShortID(hash)
}

func (s *Share) Revisions() []ShareRecord {
	var out []ShareRecord
	for _, r := range s.Records {
		if r.Kind == ShareKindRevision {
			out = append(out, r)
		}
	}
	return out
}

func (s *Share) RevisionsFor(repo string) []ShareRecord {
	var out []ShareRecord
	for _, r := range s.Revisions() {
		if repo == "" || r.Repo == repo {
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return recordBefore(out[i], out[j]) })
	return out
}

// recordBefore orders two store records by time, and records sharing a second by
// their source and then by their line in that source's chain, which is the order
// they were pushed in. See unionBefore for why the id is only the last resort.
func recordBefore(a, b ShareRecord) bool {
	if c := compareInstant(a.At, b.At); c != 0 {
		return c < 0
	}
	if a.Source != b.Source {
		return a.Source < b.Source
	}
	if a.Line != b.Line {
		return a.Line < b.Line
	}
	return a.ID < b.ID
}

func (s *Share) Repos() []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range s.Revisions() {
		if r.Repo != "" && !seen[r.Repo] {
			seen[r.Repo] = true
			out = append(out, r.Repo)
		}
	}
	sort.Strings(out)
	return out
}

func (s *Share) RecordFor(id string) (ShareRecord, bool) {
	for _, r := range s.Revisions() {
		if r.ID == id {
			return r, true
		}
	}
	return ShareRecord{}, false
}

func (s *Share) PruneFor(id string) (ShareRecord, bool) {
	for _, r := range s.Records {
		if r.Kind != ShareKindPrune {
			continue
		}
		for _, pruned := range r.Pruned {
			if pruned == id {
				return r, true
			}
		}
	}
	return ShareRecord{}, false
}

func (s *Share) LoadPayload(id string) (*SharePayload, error) {
	rec, ok := s.RecordFor(id)
	if !ok {
		return nil, fmt.Errorf("no revision %s in the shared store at %s", ShortID(id), s.Dir)
	}
	path := ShareEntryPath(s.Dir, id)
	p, raw, err := ReadSharePayload(path)
	if err != nil {
		if os.IsNotExist(err) {
			if prune, pruned := s.PruneFor(id); pruned {
				return nil, &PrunedError{ID: id, Source: prune.Source, At: prune.At, Policy: prune.Policy}
			}
			return nil, &GapError{ID: id, Source: rec.Source, Dir: s.Dir}
		}
		return nil, err
	}
	if got := ShareDigest(raw); got != rec.Payload {
		return nil, &TamperError{ID: id, Detail: fmt.Sprintf(
			"the stored payload does not match the digest its chain record carries (got %s, want %s) — the file was altered after it was recorded",
			ShortID(got), ShortID(rec.Payload))}
	}
	if p.ID != id {
		return nil, &TamperError{ID: id, Detail: fmt.Sprintf(
			"the payload at %s belongs to revision %s, not %s", path, ShortID(p.ID), ShortID(id))}
	}
	if got := HashLines(p.FactLines); got != p.FactsHash {
		return nil, &TamperError{ID: id, Detail: fmt.Sprintf(
			"the payload's fact lines do not match its recorded facts hash (got %s, want %s) — the stored contents are damaged",
			ShortID(got), ShortID(p.FactsHash))}
	}
	return p, nil
}

func (s *Share) EntryFor(rec ShareRecord) Entry {
	e := Entry{
		ID:     rec.ID,
		Repo:   rec.Repo,
		At:     rec.At,
		Epoch:  rec.Epoch,
		Git:    rec.Git,
		Origin: rec.Source,
	}
	if rec.Summary != nil {
		e.Summary = *rec.Summary
	}
	return e
}

func (s *Share) LoadSnapshot(rec ShareRecord) (*facts.Snapshot, error) {
	p, err := s.LoadPayload(rec.ID)
	if err != nil {
		return nil, err
	}
	parsedFacts, err := decodeFactLines(p.FactLines)
	if err != nil {
		return nil, err
	}
	insights, err := decodeInsightLines(p.InsightLines)
	if err != nil {
		return nil, err
	}
	return &facts.Snapshot{
		Meta:     shareMetaToSnapshotMeta(p, rec),
		Facts:    parsedFacts,
		Insights: insights,
	}, nil
}

func shareMetaToSnapshotMeta(p *SharePayload, rec ShareRecord) facts.SnapshotMeta {
	providers := make([]facts.ProviderRecord, 0, len(p.Meta.ProvidersRan))
	for _, name := range p.Meta.ProvidersRan {
		providers = append(providers, facts.ProviderRecord{Name: name})
	}
	return facts.SnapshotMeta{
		RepoPath:         p.Repo,
		GeneratedAt:      rec.At,
		Extractors:       p.Meta.Extractors,
		Explainers:       p.Meta.Explainers,
		Renderers:        p.Meta.Renderers,
		FactCount:        len(p.FactLines),
		InsightCount:     len(p.InsightLines),
		Providers:        providers,
		EnolaVersion:     p.Meta.EnolaVersion,
		ExtractorVersion: p.Meta.ExtractorVersion,
		SnapshotID:       p.ID,
		Git:              rec.Git,
		ConfigHash:       p.Meta.ConfigHash,
		IgnoreGlobHash:   p.Meta.IgnoreGlobHash,
	}
}

type ShareProblem struct {
	Kind   string
	Source string
	Line   int
	ID     string
	Detail string
}

type ShareVerifyReport struct {
	Dir       string
	Sources   []string
	Revisions int
	Verified  int
	Pruned    int
	Problems  []ShareProblem
}

func (r ShareVerifyReport) Clean() bool { return len(r.Problems) == 0 }

func VerifyShare(dir string) (ShareVerifyReport, error) {
	rep := ShareVerifyReport{Dir: dir}
	sh, err := openShare(dir, true)
	if err != nil {
		return rep, err
	}
	rep.Sources = sh.Sources
	for _, b := range sh.Breaks {
		rep.Problems = append(rep.Problems, ShareProblem{
			Kind: "chain-break", Source: b.Source, Line: b.Line, Detail: b.Detail,
		})
	}

	byID := map[string][]ShareRecord{}
	var order []string
	for _, rec := range sh.Revisions() {
		if _, seen := byID[rec.ID]; !seen {
			order = append(order, rec.ID)
		}
		byID[rec.ID] = append(byID[rec.ID], rec)
	}
	rep.Revisions = len(order)

	for _, id := range order {
		recs := byID[id]
		if divergent := divergentPayloads(recs); divergent != "" {
			rep.Problems = append(rep.Problems, ShareProblem{
				Kind: "divergent", ID: id,
				Detail: fmt.Sprintf("revision %s is recorded with different payload digests by different sources (%s) — the same snapshot cannot have two contents", ShortID(id), divergent),
			})
			continue
		}
		if _, err := sh.LoadPayload(id); err != nil {
			var pruned *PrunedError
			var gap *GapError
			var tampered *TamperError
			kind := "damaged"
			switch {
			case errors.As(err, &pruned):
				rep.Pruned++
				continue
			case errors.As(err, &gap):
				kind = "gap"
			case errors.As(err, &tampered):
				kind = "tampered"
			}
			rep.Problems = append(rep.Problems, ShareProblem{
				Kind: kind, Source: recs[0].Source, ID: id, Detail: err.Error(),
			})
			continue
		}
		rep.Verified++
	}
	return rep, nil
}

func divergentPayloads(recs []ShareRecord) string {
	var parts []string
	seen := map[string]bool{}
	for _, r := range recs {
		if !seen[r.Payload] {
			seen[r.Payload] = true
			parts = append(parts, fmt.Sprintf("%s by %s", ShortID(r.Payload), r.Source))
		}
	}
	if len(seen) < 2 {
		return ""
	}
	return strings.Join(parts, ", ")
}
