package history

// The union is what blame, show and diff read when a shared store is in play:
// local revisions and the store's, deduplicated and put on one timeline. These
// tests pin how that timeline is ordered, which side a revision's contents are
// read from, and how the repository is chosen when nobody names one.

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	pkghistory "github.com/enola-labs/enola/pkg/history"
)

func unionIDs(revs []pkghistory.UnionRevision) []string {
	out := make([]string, 0, len(revs))
	for _, u := range revs {
		out = append(out, strings.TrimPrefix(u.Entry.ID, "sha256:"))
	}
	return out
}

func entry(id, at string) pkghistory.Entry {
	return pkghistory.Entry{ID: "sha256:" + id, Repo: "github.com/org/repo", At: at}
}

func TestUnion_LocalOriginSaysWhereAPulledRevisionCameFrom(t *testing.T) {
	own := entry("aaaa", "2026-08-01T10:00:00Z")
	pulled := entry("bbbb", "2026-08-01T11:00:00Z")
	pulled.Origin = "machine-b"

	revs := pkghistory.LocalUnion([]pkghistory.Entry{own, pulled})
	if len(revs) != 2 || !revs[0].Local || !revs[1].Local {
		t.Fatalf("LocalUnion = %+v, want two local revisions", revs)
	}
	if got := revs[0].Origins; !reflect.DeepEqual(got, []string{"local"}) {
		t.Errorf("own revision origins = %v, want [local]", got)
	}
	if got := revs[1].Origins; !reflect.DeepEqual(got, []string{"local (pulled from machine-b)"}) {
		t.Errorf("pulled revision origins = %v, want [local (pulled from machine-b)]", got)
	}
	if got := pkghistory.UnionEntries(revs); !reflect.DeepEqual(got, []pkghistory.Entry{own, pulled}) {
		t.Errorf("UnionEntries = %+v, want the two entries back in order", got)
	}
}

// Timestamps are compared as instants. A backfill stamps each commit with its
// author's UTC offset, so the text of an earlier instant can sort after a later one.
func TestUnion_OrdersByInstantNotByText(t *testing.T) {
	later := entry("aaaa", "2026-08-01T20:05:30+02:00")   // 18:05 UTC
	earlier := entry("bbbb", "2026-08-01T20:27:26+03:00") // 17:27 UTC

	revs := pkghistory.BuildUnion([]pkghistory.Entry{later, earlier}, nil, "")
	if got, want := unionIDs(revs), []string{"bbbb", "aaaa"}; !reflect.DeepEqual(got, want) {
		t.Errorf("order = %v, want %v (by instant)", got, want)
	}
}

// A timestamp that does not parse is still a revision: it is placed where its
// text says rather than dropped.
func TestUnion_UnparseableTimestampFallsBackToText(t *testing.T) {
	revs := pkghistory.BuildUnion([]pkghistory.Entry{
		entry("cccc", "2026-08-02T10:00:00Z"),
		entry("aaaa", "not-a-time"),
		entry("bbbb", "2026-08-01T10:00:00Z"),
	}, nil, "")
	if got, want := unionIDs(revs), []string{"bbbb", "cccc", "aaaa"}; !reflect.DeepEqual(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

// Revisions are stamped to the second, and an agent loop lands several in one. The
// order they were recorded in is the only evidence of which came first, so a tie is
// broken by it and never by id. The ids here are chosen to sort the wrong way round.
func TestUnion_SameSecondTieKeepsRecordedOrder(t *testing.T) {
	first := entry("ffff", "2026-08-01T10:00:00Z")
	first.Seq = 1
	second := entry("aaaa", "2026-08-01T10:00:00Z")
	second.Seq = 2

	for name, recorded := range map[string][]pkghistory.Entry{
		"in order":     {first, second},
		"out of order": {second, first},
	} {
		revs := pkghistory.BuildUnion(recorded, nil, "")
		if got, want := unionIDs(revs), []string{"ffff", "aaaa"}; !reflect.DeepEqual(got, want) {
			t.Errorf("%s: union order on a tie = %v, want %v (by sequence)", name, got, want)
		}
	}
}

// The bug the tie-break caused: two snapshots within one second, the second adding
// a fact. Read in id order, the later snapshot came first and blame reported the
// fact as added and then removed.
func TestBlame_TwoSnapshotsInOneSecondAreReadInTheOrderTheyWereTaken(t *testing.T) {
	root := t.TempDir()
	at := "2026-08-01T10:00:00Z"
	appendShareRevision(t, root, "ffff0001", "c1", at, []string{factLine("Alpha", 1)})
	appendShareRevision(t, root, "aaaa0002", "c2", at, []string{factLine("Alpha", 1), factLine("Beta", 2)})
	local, err := pkghistory.Read(root)
	if err != nil {
		t.Fatal(err)
	}

	b, err := pkghistory.BlameUnion(root, pkghistory.BuildUnion(local, nil, "github.com/org/repo"), nil, "Beta", pkghistory.BlameOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Events) != 1 || len(b.Events[0].Added) != 1 || len(b.Events[0].Removed) != 0 {
		t.Fatalf("blame events = %+v, want one event that adds Beta and removes nothing", b.Events)
	}
	if b.Events[0].Entry.ID != "sha256:aaaa0002" || !b.Present() {
		t.Errorf("Beta was introduced by %s and present=%v; want the second snapshot, still present", b.Events[0].Entry.ID, b.Present())
	}
}

// The same holds for a machine that has only the store: one source's revisions in
// one second follow the order that source pushed them in.
func TestUnion_SameSecondTieInAStoreFollowsTheChain(t *testing.T) {
	root, store := t.TempDir(), t.TempDir()
	at := "2026-08-01T10:00:00Z"
	appendShareRevision(t, root, "ffff0001", "c1", at, []string{factLine("Alpha", 1)})
	appendShareRevision(t, root, "aaaa0002", "c2", at, []string{factLine("Alpha", 1), factLine("Beta", 2)})
	appendShareRevision(t, root, "cccc0003", "c3", at, []string{factLine("Alpha", 1)})
	mustPush(t, root, store, "machine-a")
	sh, err := pkghistory.OpenShare(store)
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"ffff0001", "aaaa0002", "cccc0003"}
	var listed []string
	for _, rec := range sh.RevisionsFor("github.com/org/repo") {
		listed = append(listed, strings.TrimPrefix(rec.ID, "sha256:"))
	}
	if !reflect.DeepEqual(listed, want) {
		t.Errorf("RevisionsFor on a tie = %v, want %v (chain order)", listed, want)
	}
	revs := pkghistory.BuildUnion(nil, sh, "github.com/org/repo")
	if got := unionIDs(revs); !reflect.DeepEqual(got, want) {
		t.Errorf("store-only union on a tie = %v, want %v (chain order)", got, want)
	}

	// Retention counts from the newest: with a tie it must keep the one pushed last.
	rep, err := SharedGC(store, SharedGCOptions{KeepLast: 1, Source: "machine-a"})
	if err != nil {
		t.Fatal(err)
	}
	var removed []string
	for _, r := range rep.Remove {
		removed = append(removed, strings.TrimPrefix(r.ID, "sha256:"))
	}
	if !reflect.DeepEqual(removed, []string{"ffff0001", "aaaa0002"}) {
		t.Errorf("keep-last 1 on a tie would remove %v, want the two pushed first", removed)
	}
}

// A tie between revisions from different sources has no recorded order to recover,
// so it only has to be the same every time: by source, own revisions first.
func TestUnion_SameSecondTieAcrossSourcesIsStable(t *testing.T) {
	at := "2026-08-01T10:00:00Z"
	own := entry("zzzz", at)
	own.Seq = 9
	pulled := entry("mmmm", at)
	pulled.Seq = 1
	pulled.Origin = "machine-b"

	revs := pkghistory.BuildUnion([]pkghistory.Entry{pulled, own}, nil, "")
	if got, want := unionIDs(revs), []string{"zzzz", "mmmm"}; !reflect.DeepEqual(got, want) {
		t.Errorf("own and pulled on a tie = %v, want %v (own first)", got, want)
	}
	again := pkghistory.BuildUnion([]pkghistory.Entry{own, pulled}, nil, "")
	if !reflect.DeepEqual(unionIDs(again), unionIDs(revs)) {
		t.Errorf("the order depends on the order of the input: %v then %v", unionIDs(revs), unionIDs(again))
	}
}

func TestUnion_DropsRepeatedLocalIDsAndKeepsEntriesWithoutOne(t *testing.T) {
	revs := pkghistory.BuildUnion([]pkghistory.Entry{
		entry("aaaa", "2026-08-01T10:00:00Z"),
		entry("aaaa", "2026-08-01T12:00:00Z"),
		{At: "2026-08-01T11:00:00Z"},
		{At: "2026-08-01T13:00:00Z"},
	}, nil, "")
	if len(revs) != 3 {
		t.Fatalf("union holds %d revisions, want 3: one per id, and each entry without an id", len(revs))
	}
	if revs[0].Entry.At != "2026-08-01T10:00:00Z" {
		t.Errorf("the first recording of a repeated id is the one kept; got %s", revs[0].Entry.At)
	}
}

// A revision both sides hold is one revision with two origins, and it keeps the
// local entry (which can be read without the store) while gaining the store's
// record (which can be read when the local contents were thinned).
func TestUnion_MergesTheStoreIntoLocalRevisions(t *testing.T) {
	rootLocal, rootRemote, store := t.TempDir(), t.TempDir(), t.TempDir()
	appendShareRevision(t, rootLocal, "aaaa0001", "c1", "2026-08-01T10:00:00Z", []string{factLine("Alpha", 1)})
	mustPush(t, rootLocal, store, "machine-a")
	appendShareRevision(t, rootRemote, "bbbb0002", "c2", "2026-08-01T09:00:00Z", []string{factLine("Beta", 2)})
	mustPush(t, rootRemote, store, "machine-b")

	local, err := pkghistory.Read(rootLocal)
	if err != nil {
		t.Fatal(err)
	}
	sh, err := pkghistory.OpenShare(store)
	if err != nil {
		t.Fatal(err)
	}

	revs := pkghistory.BuildUnion(local, sh, "github.com/org/repo")
	if got, want := unionIDs(revs), []string{"bbbb0002", "aaaa0001"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v: the store's earlier revision sorts before the local one", got, want)
	}
	remote, both := revs[0], revs[1]
	if remote.Local || remote.Record == nil || remote.Entry.Origin != "machine-b" {
		t.Errorf("store-only revision = local %v, record %v, origin %q; want a store record from machine-b",
			remote.Local, remote.Record != nil, remote.Entry.Origin)
	}
	if !both.Local || both.Record == nil || both.Entry.Blob == nil {
		t.Errorf("shared revision = local %v, record %v, blob %v; want the local entry with the store record attached",
			both.Local, both.Record != nil, both.Entry.Blob != nil)
	}

	// Another repository's revisions stay out.
	if other := pkghistory.BuildUnion(nil, sh, "github.com/org/elsewhere"); len(other) != 0 {
		t.Errorf("union for a repo the store does not hold = %v, want none", unionIDs(other))
	}
}

func TestUnionRevision_LinesReadsLocalFirstThenTheStore(t *testing.T) {
	root, store := t.TempDir(), t.TempDir()
	lines := []string{factLine("Alpha", 1)}
	appendShareRevision(t, root, "aaaa0001", "c1", "2026-08-01T10:00:00Z", lines)
	mustPush(t, root, store, "machine-a")
	local, err := pkghistory.Read(root)
	if err != nil {
		t.Fatal(err)
	}
	sh, err := pkghistory.OpenShare(store)
	if err != nil {
		t.Fatal(err)
	}
	rev := pkghistory.BuildUnion(local, sh, "github.com/org/repo")[0]

	// Local contents are read without the store.
	got, insights, err := rev.Lines(root, nil)
	if err != nil || !reflect.DeepEqual(got, lines) || len(insights) != 1 {
		t.Errorf("local read = %v, %v, %v; want the stored lines and one finding", got, insights, err)
	}

	// A revision whose local contents are gone is read from the store.
	headerOnly := rev
	headerOnly.Entry.Blob = nil
	got, _, err = headerOnly.Lines(root, sh)
	if err != nil || !reflect.DeepEqual(got, lines) {
		t.Errorf("store read = %v, %v; want the stored lines", got, err)
	}

	// With neither, the answer is "thinned", which blame counts as skipped.
	if _, _, err := headerOnly.Lines(root, nil); !errors.Is(err, pkghistory.ErrThinned) {
		t.Errorf("no contents on either side = %v, want ErrThinned", err)
	}
	storeless := headerOnly
	storeless.Record = nil
	if _, _, err := storeless.Lines(root, sh); !errors.Is(err, pkghistory.ErrThinned) {
		t.Errorf("no store record = %v, want ErrThinned", err)
	}
}

func TestUnionRepo(t *testing.T) {
	one, two, empty := t.TempDir(), t.TempDir(), t.TempDir()

	rootA := t.TempDir()
	appendShareRevision(t, rootA, "aaaa0001", "c1", "2026-08-01T10:00:00Z", []string{factLine("Alpha", 1)})
	mustPush(t, rootA, one, "machine-a")
	mustPush(t, rootA, two, "machine-a")

	rootB := t.TempDir()
	other := pkghistory.Entry{ID: "sha256:bbbb0002", Repo: "github.com/org/other", At: "2026-08-01T11:00:00Z", Epoch: "epoch1", Git: gitAt("c2")}
	if _, err := Append(rootB, other, Options{Contents: contents(other.ID, []string{factLine("Beta", 2)}, nil)}); err != nil {
		t.Fatal(err)
	}
	mustPush(t, rootB, two, "machine-b")
	if err := initShare(empty); err != nil {
		t.Fatal(err)
	}

	open := func(dir string) *pkghistory.Share {
		t.Helper()
		sh, err := pkghistory.OpenShare(dir)
		if err != nil {
			t.Fatal(err)
		}
		return sh
	}
	local := []pkghistory.Entry{{Repo: "github.com/org/old"}, {Repo: "github.com/org/current"}, {}}

	cases := []struct {
		name      string
		local     []pkghistory.Entry
		share     *pkghistory.Share
		requested string
		want      string
		wantErr   string
	}{
		{name: "a requested repo wins", local: local, share: open(two), requested: "github.com/org/asked", want: "github.com/org/asked"},
		{name: "otherwise the newest local entry that names one", local: local, share: open(two), want: "github.com/org/current"},
		{name: "no local history and no store", want: ""},
		{name: "a store holding one repo names it", share: open(one), want: "github.com/org/repo"},
		{name: "an empty store cannot answer", share: open(empty), wantErr: "holds no revisions"},
		{name: "a store holding several cannot choose", share: open(two), wantErr: "cannot tell which repository is meant"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pkghistory.UnionRepo(tc.local, tc.share, tc.requested)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Errorf("UnionRepo = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}
