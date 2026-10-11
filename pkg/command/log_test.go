package command

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/config"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/pkg/history"
)

// `enola log /some/other/repo`, run from a directory that has its own mcp-arch.yaml,
// loads that config — the same cwd fallback `check` uses on purpose. The advice printed
// for an empty history must not then tell the user to enable recording in a config
// belonging to a repository they were not asking about: following it would start
// recording a different repo's history, and the reason would be invisible.
func TestConfigToEdit_NamesTheRepoBeingReportedOn(t *testing.T) {
	repo := t.TempDir()
	elsewhere := filepath.Join(t.TempDir(), "mcp-arch.yaml")

	cfg := config.Default()
	cfg.SourcePath = elsewhere
	if got := configToEdit(repo, cfg); got != filepath.Join(repo, "mcp-arch.yaml") {
		t.Errorf("a config outside the repo must not be the one recommended, got %s", got)
	}

	inside := filepath.Join(repo, "mcp-arch.yaml")
	cfg.SourcePath = inside
	if got := configToEdit(repo, cfg); got != inside {
		t.Errorf("the repo's own config must be recommended as loaded, got %s", got)
	}
}

func onelineEntry(id, at, commit, ref, epoch string, dirty bool, s history.Summary) history.Entry {
	return history.Entry{
		ID:      "sha256:" + id,
		At:      at,
		Epoch:   epoch,
		Git:     &facts.GitInfo{Commit: commit, Ref: ref, Dirty: dirty},
		Summary: s,
	}
}

func TestRenderOneline(t *testing.T) {
	out := renderOneline([]history.Entry{
		onelineEntry("aaa1111", "2026-08-01T10:00:00Z", "c0ffee1111ab", "main", "ep1", false,
			history.Summary{Initial: true, FactCount: 100, InsightCount: 4}),
		onelineEntry("bbb2222", "2026-08-02T10:00:00Z", "deadbeef22cd", "main", "ep1", false,
			history.Summary{FactsAdded: 12, EdgesAdded: 2, FindingsNew: 1}),
	}, false)

	if !strings.Contains(out, "aaa1111") || !strings.Contains(out, "bbb2222") {
		t.Fatalf("both revisions must appear:\n%s", out)
	}
	if !strings.Contains(out, "c0ffee1111ab") {
		t.Errorf("the commit must be shown:\n%s", out)
	}
	// Oldest first: a history read forward is a story; read backward it is a changelog,
	// and "how did it get like this?" is the forward question.
	if strings.Index(out, "aaa1111") > strings.Index(out, "bbb2222") {
		t.Errorf("want oldest first:\n%s", out)
	}
	if strings.Contains(out, "epoch changed") {
		t.Errorf("nothing about enola changed here, so no seam should be drawn:\n%s", out)
	}
}

// A delta computed across an epoch change describes a rebuild, not somebody's work. A
// reader who is not told that will read it as a change to the code — so the seam is drawn
// on its own line rather than left to be inferred from a version column nobody scans.
func TestRenderOneline_MarksAnEpochSeam(t *testing.T) {
	out := renderOneline([]history.Entry{
		onelineEntry("aaa1111", "2026-08-01T10:00:00Z", "c1", "main", "ep1", false, history.Summary{FactsAdded: 1}),
		onelineEntry("bbb2222", "2026-08-02T10:00:00Z", "c2", "main", "ep2", false,
			history.Summary{FactsAdded: 900, FactsRemoved: 850, Incomparable: true}),
	}, false)

	if !strings.Contains(out, "epoch changed") {
		t.Fatalf("want an epoch seam between the two revisions:\n%s", out)
	}
	if !strings.Contains(out, "rebuild noise") {
		t.Errorf("the seam must say what it means, not just that it happened:\n%s", out)
	}
	if !strings.Contains(out, "incomparable") {
		t.Errorf("the affected revision must be marked:\n%s", out)
	}
	// The seam belongs ABOVE the revision that opens the new epoch.
	if strings.Index(out, "epoch changed") > strings.Index(out, "bbb2222") {
		t.Errorf("the seam must precede the revision it explains:\n%s", out)
	}
}

// A shape that was NOT derived from the repository must say so. The reader cannot tell by
// looking, and the two support different conclusions: a git-derived graph is evidence about
// what descends from what, a time-ordered one is only evidence about what happened next.
func TestTopologyNote_DisclosesEveryFallback(t *testing.T) {
	if got := topologyNote(history.SourceGit); got != "" {
		t.Errorf("a git-derived shape needs no caveat, got %q", got)
	}
	for _, src := range []history.TopologySource{history.SourceRecordedParents, history.SourceTime} {
		if got := topologyNote(src); got == "" {
			t.Errorf("source %q must be disclosed", src)
		}
	}
	if !strings.Contains(topologyNote(history.SourceTime), "timeline, not a history") {
		t.Error("the time fallback must say what it is, not merely that it was used")
	}
}

// The graph shows ancestry; the summaries show the delta from the previous SNAPSHOT. Those
// are the same thing until somebody switches branches, at which point a row can report the
// other branch's work as removed — true of the pair enola compared, and not what the row's
// position in the graph implies. Saying so is cheaper and more honest than recomputing,
// which would make one revision show different numbers in different views.
func TestObservationOrderNote(t *testing.T) {
	linear := history.Topology{Rows: []history.Row{
		{Entry: history.Entry{ID: "sha256:a"}},
		{Entry: history.Entry{ID: "sha256:b"}, Parents: []int{0}},
		{Entry: history.Entry{ID: "sha256:c"}, Parents: []int{1}},
	}}
	if got := observationOrderNote(linear, "enola"); got != "" {
		t.Errorf("a linear history needs no caveat, got %q", got)
	}

	// b and c both descend from a: c does not follow the row printed above it.
	branched := history.Topology{Rows: []history.Row{
		{Entry: history.Entry{ID: "sha256:a"}},
		{Entry: history.Entry{ID: "sha256:b"}, Parents: []int{0}},
		{Entry: history.Entry{ID: "sha256:c"}, Parents: []int{0}},
	}}
	got := observationOrderNote(branched, "enola")
	if got == "" {
		t.Fatal("a branch switch must be disclosed")
	}
	if !strings.Contains(got, "enola diff") {
		t.Errorf("the caveat must point at the command that answers the ancestry question, got %q", got)
	}
}

// A multi-repo history has one commit PER REPOSITORY and none overall, so a drawn shape can
// only follow one of them. Saying so is the difference between a partial picture and a
// misleading one.
func TestMultiRepoNote(t *testing.T) {
	single := []history.Entry{{ID: "sha256:a"}}
	if got := multiRepoNote(single); got != "" {
		t.Errorf("a single-repo history needs no note, got %q", got)
	}

	multi := []history.Entry{{ID: "sha256:a", Repos: []history.RepoRef{
		{Label: "api", Commit: "aaaaaaaaaaaa"},
		{Label: "web", Commit: "bbbbbbbbbbbb"},
	}}}
	got := multiRepoNote(multi)
	if !strings.Contains(got, "2 repositories") {
		t.Errorf("want the repository count, got %q", got)
	}
}

// …and each repository's position goes on the row, since the graph cannot carry it.
func TestDecorations_CarriesTheMultiRepoVector(t *testing.T) {
	e := onelineEntry("a", "", "c0ffee1111ab", "main", "ep", false, history.Summary{})
	e.Repos = []history.RepoRef{{Label: "web", Commit: "deadbeef2222"}}
	got := decorations(e)
	if !strings.Contains(got, "web@deadbeef2222") {
		t.Errorf("want the other repository's position on the row, got %q", got)
	}
}

// --stat answers "what KIND of thing changed here". Net per kind, because the headline
// already gave the totals and a pair of numbers per kind buries the answer in arithmetic.
func TestStatLines(t *testing.T) {
	e := onelineEntry("a", "", "c1", "main", "ep", false, history.Summary{
		ByKind: map[string]int{"symbol": 12, "storage": -1},
	})
	got := statLines(e, "  ")
	if !strings.Contains(got, "symbol       +12") {
		t.Errorf("want a signed net count per kind, got:\n%s", got)
	}
	if !strings.Contains(got, "storage      -1") {
		t.Errorf("a net removal must read as one, got:\n%s", got)
	}
	if statLines(onelineEntry("a", "", "c1", "main", "ep", false, history.Summary{}), "  ") != "" {
		t.Error("a revision with no per-kind delta must contribute no lines")
	}
}

// --since takes either a remembered date or "the last few days", because both are things
// people mean by it.
func TestParseSince(t *testing.T) {
	if _, err := parseSince("2026-08-01T00:00:00Z"); err != nil {
		t.Errorf("an RFC3339 instant must parse: %v", err)
	}
	if _, err := parseSince("72h"); err != nil {
		t.Errorf("a duration must parse: %v", err)
	}
	if _, err := parseSince("last tuesday"); err == nil {
		t.Error("an unparseable value must be reported, not silently ignored")
	}
}

// An unreadable timestamp must not silently shorten the history: dropping a revision for a
// reason unrelated to what was asked is worse than showing one revision too many.
func TestTakenAfter_KeepsWhatItCannotJudge(t *testing.T) {
	cutoff, err := parseSince("2026-08-02T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	entries := []history.Entry{
		{ID: "sha256:old", At: "2026-08-01T00:00:00Z"},
		{ID: "sha256:new", At: "2026-08-03T00:00:00Z"},
		{ID: "sha256:junk", At: "not a timestamp"},
	}
	got := takenAfter(entries, cutoff)
	if len(got) != 2 {
		t.Fatalf("want the new and the unreadable, got %d", len(got))
	}
	if got[0].ID != "sha256:new" || got[1].ID != "sha256:junk" {
		t.Errorf("got %v", got)
	}
}

func TestOnlyCommitted(t *testing.T) {
	entries := []history.Entry{
		onelineEntry("aaa1111", "2026-08-01T10:00:00Z", "c1", "main", "ep1", false, history.Summary{}),
		onelineEntry("bbb2222", "2026-08-01T10:05:00Z", "c1", "main", "ep1", true, history.Summary{}),
		{ID: "sha256:ccc3333", At: "2026-08-01T10:06:00Z"}, // no git at all
	}
	got := onlyCommitted(entries)
	if len(got) != 1 || got[0].ID != "sha256:aaa1111" {
		t.Fatalf("want only the committed revision, got %+v", got)
	}
}

func TestDecorations_DirtyAndClean(t *testing.T) {
	clean := decorations(onelineEntry("a", "", "c0ffee1111ab", "main", "ep", false, history.Summary{}))
	if strings.Contains(clean, "dirty") {
		t.Errorf("a clean revision must not be marked dirty: %q", clean)
	}
	dirty := decorations(onelineEntry("a", "", "c0ffee1111ab", "main", "ep", true, history.Summary{}))
	if !strings.Contains(dirty, "dirty") {
		t.Errorf("a working revision must say so: %q", dirty)
	}
}
