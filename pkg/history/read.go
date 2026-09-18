package history

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ErrNoHistory is returned when a root holds no log at all. It is a normal state — a
// repository that has never been snapshotted with history enabled — so callers report it
// as "nothing recorded yet" rather than as a failure.
var ErrNoHistory = errors.New("no architecture history recorded")

// FormatFileName marks a history root's store format, beside the log. Histories written
// before the marker existed carry none and are read as FormatVersion 1 — the only format
// that ever existed then — so the marker's absence is compatibility, never an error.
const FormatFileName = "format"

// FormatVersion is the store format this build reads and writes: 1, the JSONL log plus
// numbered blob segments. Any future change to that layout bumps this, and the marker is
// what lets an old build REFUSE a new store instead of misreading it.
const FormatVersion = 1

// UnknownFormatError names a history store whose format marker this build does not know.
// A typed error rather than a string, so a caller can tell "refuse to read" from
// "corrupt" — the store is presumed healthy, just newer than the reader.
type UnknownFormatError struct {
	Root    string
	Version string
}

func (e *UnknownFormatError) Error() string {
	return fmt.Sprintf(
		"the history at %s declares store format %q and this build reads format %d — refusing to read rather than misread. "+
			"A newer enola wrote this store; upgrade enola to read it, or delete the history directory (it is derived data, losing only convenience)",
		e.Root, e.Version, FormatVersion)
}

// checkFormat verdicts a root's marker before anything reads its log.
func checkFormat(root string) error {
	raw, err := os.ReadFile(filepath.Join(root, FormatFileName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading the history format marker in %s: %w", root, err)
	}
	if v := strings.TrimSpace(string(raw)); v != strconv.Itoa(FormatVersion) {
		return &UnknownFormatError{Root: root, Version: v}
	}
	return nil
}

// Read returns every entry in a history root, oldest first.
//
// A TRAILING malformed line is tolerated and dropped: the log is appended to on every
// snapshot, so a crash or a full disk mid-write leaves a partial final line, and refusing
// to read the other 900 entries because of it would make the failure permanent for no
// reason. A malformed line ANYWHERE ELSE is an error naming its number — that is
// corruption rather than an interrupted write, and silently skipping it would hide data
// loss behind a log that still renders.
func Read(root string) ([]Entry, error) {
	if err := checkFormat(root); err != nil {
		return nil, err
	}
	path := filepath.Join(root, LogFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNoHistory
		}
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	return Parse(data, path)
}

// Parse decodes a log's bytes. name is used only in error messages. Exported so a
// consumer holding the log from somewhere other than a local file — a fetched artifact,
// a merge of two logs — decodes it through exactly the same rules.
func Parse(data []byte, name string) ([]Entry, error) {
	var entries []Entry
	sc := bufio.NewScanner(bytes.NewReader(data))
	// A single entry is a few hundred bytes, but Repos/Parents/Refs on a large
	// multi-repo graph can push a line past bufio's 64 KiB default, which would
	// otherwise surface as a truncated-line error on exactly the biggest graphs.
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	// Whether the file's last byte is a newline decides what a bad final line MEANS: with
	// the newline the writer finished, so a bad line is corruption; without it the write
	// was interrupted, which is the tolerated case.
	complete := len(data) > 0 && data[len(data)-1] == '\n'

	line := 0
	var pending []Entry // entries after the last successfully parsed one
	for sc.Scan() {
		line++
		raw := bytes.TrimSpace(sc.Bytes())
		if len(raw) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(raw, &e); err != nil {
			// Defer the verdict: this is only tolerable if it turns out to be the last
			// line of an incomplete file.
			pending = append(pending, Entry{Seq: line})
			if complete {
				return nil, fmt.Errorf("%s:%d: malformed entry: %w", name, line, err)
			}
			continue
		}
		if len(pending) > 0 {
			// Something unparseable sat BEFORE this good line, so it was not a
			// truncated tail.
			return nil, fmt.Errorf("%s:%d: malformed entry (followed by valid entries, so this is corruption, not an interrupted write)", name, pending[0].Seq)
		}
		entries = append(entries, e)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", name, err)
	}
	return entries, nil
}

// SortedByTime returns the entries in the order they describe rather than the order they
// were written, oldest first.
//
// Those are the same thing until a BACKFILL, which appends revisions for old commits after
// revisions for new ones — so a log read as written would show last month's architecture
// below this morning's. Every surface that presents a TIMELINE (log, blame, the MCP tools)
// sorts; nothing that WRITES does.
//
// That split is deliberate and load-bearing. Blobs chain in append order, so the writer
// asks "what did I store last" and must get the answer in append order: sorting inside Read
// would make a backfilled revision chain off the newest graph instead of its own
// predecessor, turning every patch into a near-complete rewrite and every revision into its
// own segment.
//
// Compared as INSTANTS, not as strings. RFC3339 only sorts lexically when every timestamp
// shares one UTC offset, and a history that mixes live and backfilled revisions never does:
// the recorder stamps UTC ("…Z"), while a backfill stamps each commit's own committer date
// with its author's offset. Seen on a Rust repository carrying +03:00 and +02:00, where the
// lexically-first revision (20:05:30+02:00 = 18:05 UTC) is forty minutes LATER than the real
// first (20:27:26+03:00 = 17:27 UTC) — enough to put the wrong revision at the start of the
// timeline, and from there to mislabel which one began the history.
//
// An unparseable timestamp keeps its string for comparison rather than being dropped or
// sorted to one end: it is still a revision, and guessing where it belongs is worse than
// placing it where its text says.
//
// Stable, so revisions sharing an instant keep the order they were recorded in — a second's
// resolution is coarse enough that an agent loop lands several in one.
func SortedByTime(entries []Entry) []Entry {
	out := append([]Entry(nil), entries...)
	sort.SliceStable(out, func(i, j int) bool {
		ti, oki := time.Parse(time.RFC3339, out[i].At)
		tj, okj := time.Parse(time.RFC3339, out[j].At)
		if oki == nil && okj == nil {
			return ti.Before(tj)
		}
		return out[i].At < out[j].At
	})
	return out
}

// Last returns the most recently appended entry, or false when there is none. The last
// LINE, not the greatest Seq or the latest At: it is what a writer compares against to
// decide whether a new snapshot is worth recording.
func Last(entries []Entry) (Entry, bool) {
	if len(entries) == 0 {
		return Entry{}, false
	}
	return entries[len(entries)-1], true
}

// Resolve maps a revision selector to an entry.
//
// Accepted forms, in the order they are tried:
//
//	""            the newest revision
//	latest        the newest revision
//	HEAD          the newest revision
//	HEAD~N        N revisions back from the newest
//	@N            the revision with Seq == N
//	<id prefix>   a snapshot ID prefix, with or without "sha256:" (≥4 chars)
//	<commit>      a git commit, or a prefix of one (≥4 chars); the NEWEST revision
//	              taken at that commit, since one commit can have many
//	<ref name>    a name in Refs ("baseline")
//
// An ambiguous prefix is an error naming the candidates rather than a silent pick.
func Resolve(entries []Entry, sel string) (Entry, error) {
	if len(entries) == 0 {
		return Entry{}, ErrNoHistory
	}
	s := strings.TrimSpace(sel)
	switch {
	case s == "" || strings.EqualFold(s, "latest") || s == "HEAD":
		return entries[len(entries)-1], nil
	case strings.HasPrefix(s, "HEAD~"):
		n, err := strconv.Atoi(s[len("HEAD~"):])
		if err != nil || n < 0 {
			return Entry{}, fmt.Errorf("bad revision %q: expected HEAD~<number>", sel)
		}
		i := len(entries) - 1 - n
		if i < 0 {
			return Entry{}, fmt.Errorf("revision %q is before the start of the history (%d revisions recorded)", sel, len(entries))
		}
		return entries[i], nil
	case strings.HasPrefix(s, "@"):
		n, err := strconv.Atoi(s[1:])
		if err != nil {
			return Entry{}, fmt.Errorf("bad revision %q: expected @<seq>", sel)
		}
		for _, e := range entries {
			if e.Seq == n {
				return e, nil
			}
		}
		return Entry{}, fmt.Errorf("no revision with seq %d", n)
	}

	// Named refs are exact and few, so they win over any prefix interpretation.
	for i := len(entries) - 1; i >= 0; i-- {
		for _, r := range entries[i].Refs {
			if r == s {
				return entries[i], nil
			}
		}
	}

	if len(s) < 4 {
		return Entry{}, fmt.Errorf("revision %q is too short to identify anything — use at least 4 characters", sel)
	}
	needle := strings.ToLower(strings.TrimPrefix(s, "sha256:"))

	var byID []Entry
	for _, e := range entries {
		if strings.HasPrefix(strings.TrimPrefix(e.ID, "sha256:"), needle) {
			byID = append(byID, e)
		}
	}
	if n := len(byID); n == 1 {
		return byID[0], nil
	} else if n > 1 {
		return Entry{}, ambiguous(sel, byID)
	}

	// A commit can hold several revisions (a working revision per edit round, plus the
	// committed one), so this resolves to the newest rather than reporting ambiguity —
	// "the architecture at commit X" means the last thing observed there.
	for i := len(entries) - 1; i >= 0; i-- {
		if c := entries[i].Commit(); c != "" && strings.HasPrefix(c, needle) {
			return entries[i], nil
		}
	}
	return Entry{}, fmt.Errorf("no revision matches %q", sel)
}

func ambiguous(sel string, matches []Entry) error {
	var ids []string
	for _, e := range matches {
		if len(ids) == 5 {
			ids = append(ids, "…")
			break
		}
		ids = append(ids, e.Short())
	}
	return fmt.Errorf("revision %q is ambiguous — matches %s", sel, strings.Join(ids, ", "))
}
