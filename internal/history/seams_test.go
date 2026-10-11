package history

// Edges of the history packages that nothing exercised: how a store that cannot
// be read says so, how a machine names itself to a store, and the one function
// that edits a recorded entry in place.

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
	pkghistory "github.com/enola-labs/enola/pkg/history"
)

// A directory that is not a store, and a store written by a newer build, are
// different refusals: one says "push creates it", the other says "upgrade".
func TestOpenShare_RefusesWhatItCannotRead(t *testing.T) {
	notAStore := t.TempDir()
	if _, err := pkghistory.OpenShare(notAStore); !errors.Is(err, pkghistory.ErrNoShare) {
		t.Errorf("a directory with no format marker = %v, want ErrNoShare", err)
	}

	newer := t.TempDir()
	if err := initShare(newer); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(newer, pkghistory.ShareFormatFileName)
	if err := os.WriteFile(marker, []byte("shared-history/99\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := pkghistory.OpenShare(newer)
	var unknown *pkghistory.UnknownShareFormatError
	if !errors.As(err, &unknown) {
		t.Fatalf("a store in a newer format = %v, want UnknownShareFormatError", err)
	}
	if unknown.Format != "shared-history/99" || unknown.Dir != newer {
		t.Errorf("the refusal names format %q in %q", unknown.Format, unknown.Dir)
	}
	for _, want := range []string{"shared-history/99", pkghistory.ShareFormatValue, "Upgrade enola"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must say %q; got: %v", want, err)
		}
	}

	// Pushing into it is refused as well, rather than writing an older format over it.
	root := t.TempDir()
	appendShareRevision(t, root, "aaaa0001", "c1", "2026-08-01T10:00:00Z", []string{factLine("Alpha", 1)})
	if _, err := Push(root, newer, PushOptions{Source: "machine-a"}); !errors.As(err, &unknown) {
		t.Errorf("push into a newer-format store = %v, want UnknownShareFormatError", err)
	}
}

// "Pruned" and "missing" are opposite answers to "where did this revision go",
// so their wording must not converge.
func TestShareErrorsSayWhetherARevisionWasRemovedOrLost(t *testing.T) {
	pruned := (&pkghistory.PrunedError{ID: "sha256:aaaa00011111", Source: "machine-a", At: "2026-08-01T10:00:00Z", Policy: "keep-last 1"}).Error()
	for _, want := range []string{"machine-a", "keep-last 1", "removed by retention, not missing"} {
		if !strings.Contains(pruned, want) {
			t.Errorf("pruned error must say %q; got: %s", want, pruned)
		}
	}
	gap := (&pkghistory.GapError{ID: "sha256:aaaa00011111", Source: "machine-a", Dir: "/store"}).Error()
	if !strings.Contains(gap, "machine-a") || strings.Contains(gap, "removed by retention") {
		t.Errorf("gap error must name the source and must not read as a prune; got: %s", gap)
	}
}

// A machine's source id is made once and then read back: a store's chains are
// keyed on it, so a machine that renamed itself between pushes would start a
// second chain and orphan the first.
func TestSourceID_IsCreatedOnceAndStable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	first, err := SourceID()
	if err != nil {
		t.Fatal(err)
	}
	if first == "" || sanitizeSource(first) != first {
		t.Errorf("source id %q must be non-empty and safe as a file name", first)
	}
	again, err := SourceID()
	if err != nil {
		t.Fatal(err)
	}
	if again != first {
		t.Errorf("source id changed between calls: %q then %q", first, again)
	}

	// A hand-written id is honoured, sanitized.
	if err := os.WriteFile(filepath.Join(home, ".enola", "source"), []byte("build box/7\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := SourceID(); err != nil || got != "build_box_7" {
		t.Errorf("hand-written source id = %q, %v; want build_box_7", got, err)
	}
}

func TestSanitizeSource(t *testing.T) {
	for in, want := range map[string]string{
		"host-1a2b":   "host-1a2b",
		"My.Host_01":  "My.Host_01",
		"a/b\\c d":    "a_b_c_d",
		"":            "machine",
		".":           "machine",
		"..":          "machine",
		"../../etc/x": ".._.._etc_x",
	} {
		if got := sanitizeSource(in); got != want {
			t.Errorf("sanitizeSource(%q) = %q, want %q", in, got, want)
		}
	}
}

// RewriteSummary edits exactly one entry, found by id AND sequence, and leaves
// every other entry and every revision's contents alone.
func TestRewriteSummary(t *testing.T) {
	root := t.TempDir()
	first := storeRevision(t, root, "aaaa0001", "c1", []string{factLine("Alpha", 1)}, Options{})
	second := storeRevision(t, root, "bbbb0002", "c2", []string{factLine("Alpha", 1), factLine("Beta", 2)}, Options{})

	summary := pkghistory.Summary{FactsAdded: 7}
	if err := RewriteSummary(root, second.ID, second.Seq, summary); err != nil {
		t.Fatalf("RewriteSummary: %v", err)
	}
	entries, err := pkghistory.Read(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("log holds %d entries after a rewrite, want 2", len(entries))
	}
	if !reflect.DeepEqual(entries[0], first) {
		t.Errorf("the other entry changed:\n got %+v\nwant %+v", entries[0], first)
	}
	if entries[1].Summary.FactsAdded != 7 {
		t.Errorf("summary = %+v, want the rewritten one", entries[1].Summary)
	}
	rewritten := entries[1]
	rewritten.Summary = second.Summary
	if !reflect.DeepEqual(rewritten, second) {
		t.Errorf("a rewrite changed more than the summary:\n got %+v\nwant %+v", rewritten, second)
	}
	lines, _, _, err := pkghistory.LoadLines(root, entries[1].Blob.Segment, entries[1].Blob.Member)
	if err != nil || len(lines) != 2 {
		t.Errorf("contents after a rewrite = %v, %v; want the two stored lines", lines, err)
	}

	// The right id at the wrong sequence is not that revision.
	if err := RewriteSummary(root, second.ID, second.Seq+10, summary); err == nil {
		t.Error("a sequence that does not match must be refused")
	}
	if err := RewriteSummary(root, "sha256:nope", 1, summary); err == nil {
		t.Error("an unknown id must be refused")
	}
	if err := RewriteSummary(t.TempDir(), second.ID, second.Seq, summary); err == nil {
		t.Error("a root with no history must be refused")
	}
}

// Findings are stored as a sorted multiset, so the stored form is a function of
// the set of findings and not of the order explainers happened to run in.
func TestInsightLinesAreSortedAndOrderIndependent(t *testing.T) {
	a := facts.Insight{Title: "A finding", Source: "cycles", Confidence: 1}
	b := facts.Insight{Title: "B finding", Source: "layers", Confidence: 0.5}

	forward, err := pkghistory.InsightLines([]facts.Insight{a, b, a})
	if err != nil {
		t.Fatal(err)
	}
	backward, err := pkghistory.InsightLines([]facts.Insight{a, a, b})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(forward, backward) {
		t.Errorf("order of input changed the stored lines:\n%v\n%v", forward, backward)
	}
	if len(forward) != 3 || forward[0] != forward[1] || !strings.Contains(forward[2], "B finding") {
		t.Errorf("lines = %v, want the repeated finding twice and then the other", forward)
	}
	if empty, err := pkghistory.InsightLines(nil); err != nil || len(empty) != 0 {
		t.Errorf("no findings = %v, %v; want no lines", empty, err)
	}
}
