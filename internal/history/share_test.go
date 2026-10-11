package history

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/enola-labs/enola/internal/facts"
	pkghistory "github.com/enola-labs/enola/pkg/history"
)

func appendShareRevision(t *testing.T, root, id, commit, at string, factLines []string) pkghistory.Entry {
	t.Helper()
	e := pkghistory.Entry{
		ID:    "sha256:" + id,
		Repo:  "github.com/org/repo",
		At:    at,
		Epoch: "epoch1",
		Git:   &facts.GitInfo{Commit: commit, Ref: "main"},
	}
	recorded, err := Append(root, e, Options{
		Contents: contents(e.ID, factLines, []string{`{"title":"finding","confidence":1}`}),
	})
	if err != nil {
		t.Fatalf("append %s: %v", id, err)
	}
	if !recorded {
		t.Fatalf("revision %s was not recorded", id)
	}
	entries, err := pkghistory.Read(root)
	if err != nil {
		t.Fatal(err)
	}
	return entries[len(entries)-1]
}

func mustPush(t *testing.T, root, store, source string) PushReport {
	t.Helper()
	rep, err := Push(root, store, PushOptions{Source: source})
	if err != nil {
		t.Fatalf("push from %s as %s: %v", root, source, err)
	}
	return rep
}

func TestShare_IdenticalSnapshotsProduceIdenticalEntries(t *testing.T) {
	lines := []string{factLine("A", 10), factLine("B", 20)}
	rootA, rootB := t.TempDir(), t.TempDir()
	appendShareRevision(t, rootA, "aaaa0001", "c1", "2026-08-01T10:00:00Z", lines)
	appendShareRevision(t, rootB, "aaaa0001", "c1", "2026-08-02T11:30:00Z", lines)

	storeA, storeB := t.TempDir(), t.TempDir()
	mustPush(t, rootA, storeA, "machine-a")
	mustPush(t, rootB, storeB, "machine-b")

	path := pkghistory.ShareEntryPath(storeA, "sha256:aaaa0001")
	rawA, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rawB, err := os.ReadFile(pkghistory.ShareEntryPath(storeB, "sha256:aaaa0001"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rawA, rawB) {
		t.Error("the same snapshot pushed from two machines produced different entry bytes")
	}

	shared := t.TempDir()
	repA := mustPush(t, rootA, shared, "machine-a")
	repB := mustPush(t, rootB, shared, "machine-b")
	if repA.EntriesWritten != 1 || repB.EntriesWritten != 0 {
		t.Errorf("entry files written = %d then %d, want 1 then 0 (content-addressed: the second writer finds the file)",
			repA.EntriesWritten, repB.EntriesWritten)
	}
	files, err := os.ReadDir(filepath.Join(shared, pkghistory.ShareEntriesDir))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Errorf("shared store holds %d entry files for one snapshot, want 1", len(files))
	}
	rep, err := pkghistory.VerifyShare(shared)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Clean() || rep.Verified != 1 || len(rep.Sources) != 2 {
		t.Errorf("verify after two-machine push: verified=%d sources=%v problems=%v", rep.Verified, rep.Sources, rep.Problems)
	}
}

func TestShare_PushIsIdempotent(t *testing.T) {
	root, store := t.TempDir(), t.TempDir()
	appendShareRevision(t, root, "aaaa0001", "c1", "2026-08-01T10:00:00Z", []string{factLine("A", 1)})
	appendShareRevision(t, root, "aaaa0002", "c2", "2026-08-01T11:00:00Z", []string{factLine("A", 1), factLine("B", 2)})

	first := mustPush(t, root, store, "machine-a")
	if first.Pushed != 2 {
		t.Fatalf("first push recorded %d revisions, want 2", first.Pushed)
	}
	chainPath := pkghistory.ShareChainPath(store, "machine-a")
	before, err := os.ReadFile(chainPath)
	if err != nil {
		t.Fatal(err)
	}

	second := mustPush(t, root, store, "machine-a")
	if second.Pushed != 0 || second.AlreadyThere != 2 {
		t.Errorf("second push: pushed=%d alreadyThere=%d, want 0 and 2", second.Pushed, second.AlreadyThere)
	}
	after, err := os.ReadFile(chainPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("an idempotent re-push changed the chain")
	}
}

func TestShare_PullImportsWithProvenanceAndIsIdempotent(t *testing.T) {
	rootA, store, rootB := t.TempDir(), t.TempDir(), t.TempDir()
	appendShareRevision(t, rootA, "aaaa0001", "c1", "2026-08-01T10:00:00Z", []string{factLine("Alpha", 1)})
	appendShareRevision(t, rootA, "aaaa0002", "c2", "2026-08-01T11:00:00Z", []string{factLine("Alpha", 1), factLine("Beta", 2)})
	appendShareRevision(t, rootA, "aaaa0003", "c3", "2026-08-01T12:00:00Z", []string{factLine("Beta", 2)})
	mustPush(t, rootA, store, "machine-a")

	rep, err := Pull(rootB, store, PullOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Pulled != 3 || rep.Repo != "github.com/org/repo" {
		t.Fatalf("pull: pulled=%d repo=%q, want 3 and github.com/org/repo", rep.Pulled, rep.Repo)
	}

	entries, err := pkghistory.Read(rootB)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("local log holds %d entries after pull, want 3", len(entries))
	}
	for _, e := range entries {
		if e.Origin != "machine-a" {
			t.Errorf("pulled entry %s carries origin %q, want machine-a", e.Short(), e.Origin)
		}
		if e.Blob == nil {
			t.Errorf("pulled entry %s has no stored contents", e.Short())
		}
	}

	b, err := pkghistory.BlameLines(rootB, pkghistory.SortedByTime(entries), "Beta", pkghistory.BlameOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if intro, ok := b.Introduced(); !ok || intro.ID != "sha256:aaaa0002" {
		t.Errorf("blame over pulled history: introduced=%v ok=%v, want aaaa0002", intro.ID, ok)
	}

	again, err := Pull(rootB, store, PullOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if again.Pulled != 0 || again.AlreadyLocal != 3 {
		t.Errorf("second pull: pulled=%d alreadyLocal=%d, want 0 and 3", again.Pulled, again.AlreadyLocal)
	}
}

func TestShare_VerifyNamesGapsTamperAndChainBreaks(t *testing.T) {
	root, store := t.TempDir(), t.TempDir()
	appendShareRevision(t, root, "aaaa0001", "c1", "2026-08-01T10:00:00Z", []string{factLine("A", 1)})
	appendShareRevision(t, root, "aaaa0002", "c2", "2026-08-01T11:00:00Z", []string{factLine("B", 2)})
	mustPush(t, root, store, "machine-a")

	if err := os.Remove(pkghistory.ShareEntryPath(store, "sha256:aaaa0001")); err != nil {
		t.Fatal(err)
	}
	rep, err := pkghistory.VerifyShare(store)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Problems) != 1 || rep.Problems[0].Kind != "gap" || rep.Problems[0].ID != "sha256:aaaa0001" {
		t.Fatalf("after deleting an entry file, verify reports %+v, want one gap naming aaaa0001", rep.Problems)
	}

	tamperSharePayload(t, store, "sha256:aaaa0002")
	rep, err = pkghistory.VerifyShare(store)
	if err != nil {
		t.Fatal(err)
	}
	if !hasProblem(rep, "tampered", "sha256:aaaa0002") {
		t.Errorf("after altering an entry file, verify reports %+v, want a tampered problem naming aaaa0002", rep.Problems)
	}

	chainPath := pkghistory.ShareChainPath(store, "machine-a")
	raw, err := os.ReadFile(chainPath)
	if err != nil {
		t.Fatal(err)
	}
	edited := bytes.Replace(raw, []byte(`"ref":"main"`), []byte(`"ref":"mine"`), 1)
	if bytes.Equal(edited, raw) {
		t.Fatal("test fixture: nothing edited in the chain")
	}
	if err := os.WriteFile(chainPath, edited, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := pkghistory.OpenShare(store); err == nil {
		t.Error("OpenShare read an edited chain without refusing")
	}
	rep, err = pkghistory.VerifyShare(store)
	if err != nil {
		t.Fatal(err)
	}
	if !hasProblemKind(rep, "chain-break") {
		t.Errorf("after editing a chain record, verify reports %+v, want a chain-break", rep.Problems)
	}
}

func TestShare_PushRefusesATruncatedOwnChain(t *testing.T) {
	root, store := t.TempDir(), t.TempDir()
	appendShareRevision(t, root, "aaaa0001", "c1", "2026-08-01T10:00:00Z", []string{factLine("A", 1)})
	appendShareRevision(t, root, "aaaa0002", "c2", "2026-08-01T11:00:00Z", []string{factLine("B", 2)})
	mustPush(t, root, store, "machine-a")

	chainPath := pkghistory.ShareChainPath(store, "machine-a")
	raw, err := os.ReadFile(chainPath)
	if err != nil {
		t.Fatal(err)
	}
	firstLine := raw[:bytes.IndexByte(raw, '\n')+1]
	if err := os.WriteFile(chainPath, firstLine, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = Push(root, store, PushOptions{Source: "machine-a"})
	if err == nil || !strings.Contains(err.Error(), "no longer contains") {
		t.Errorf("push onto a truncated chain: err=%v, want a refusal naming the missing record", err)
	}
}

func TestShare_ConcurrentWritersCannotCorruptTheStore(t *testing.T) {
	store := t.TempDir()
	sharedLines := []string{factLine("Shared", 1)}

	roots := map[string]string{"machine-a": t.TempDir(), "machine-b": t.TempDir()}
	appendShareRevision(t, roots["machine-a"], "aaaa0001", "c1", "2026-08-01T10:00:00Z", sharedLines)
	appendShareRevision(t, roots["machine-a"], "aaaa0002", "c2", "2026-08-01T11:00:00Z", []string{factLine("OnlyA", 2)})
	appendShareRevision(t, roots["machine-b"], "aaaa0001", "c1", "2026-08-01T10:30:00Z", sharedLines)
	appendShareRevision(t, roots["machine-b"], "bbbb0003", "c3", "2026-08-01T12:00:00Z", []string{factLine("OnlyB", 3)})

	var wg sync.WaitGroup
	errs := make(chan error, len(roots))
	for source, root := range roots {
		wg.Add(1)
		go func(source, root string) {
			defer wg.Done()
			if _, err := Push(root, store, PushOptions{Source: source}); err != nil {
				errs <- err
			}
		}(source, root)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	rep, err := pkghistory.VerifyShare(store)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Clean() {
		t.Fatalf("store after concurrent pushes is not clean: %+v", rep.Problems)
	}
	if rep.Revisions != 3 || rep.Verified != 3 {
		t.Errorf("store holds %d revisions (%d verified), want 3 verified — the shared snapshot dedups by digest", rep.Revisions, rep.Verified)
	}
}

func TestShare_BlameUnionNamesTheStoreEachRevisionCameFrom(t *testing.T) {
	rootLocal, rootRemote, store := t.TempDir(), t.TempDir(), t.TempDir()
	appendShareRevision(t, rootLocal, "aaaa0001", "c1", "2026-08-01T10:00:00Z", []string{factLine("Alpha", 1)})
	mustPush(t, rootLocal, store, "machine-a")
	appendShareRevision(t, rootRemote, "bbbb0002", "c2", "2026-08-01T11:00:00Z", []string{factLine("Alpha", 1), factLine("Beta", 2)})
	mustPush(t, rootRemote, store, "machine-b")

	local, err := pkghistory.Read(rootLocal)
	if err != nil {
		t.Fatal(err)
	}
	sh, err := pkghistory.OpenShare(store)
	if err != nil {
		t.Fatal(err)
	}
	revs := pkghistory.BuildUnion(pkghistory.SortedByTime(local), sh, "github.com/org/repo")
	if len(revs) != 2 {
		t.Fatalf("union holds %d revisions, want 2", len(revs))
	}
	if got := revs[0].Origins; len(got) != 2 || got[0] != "local" || got[1] != "store:machine-a" {
		t.Errorf("revision known to both sides carries origins %v, want [local store:machine-a]", got)
	}
	if got := revs[1].Origins; len(got) != 1 || got[0] != "store:machine-b" {
		t.Errorf("store-only revision carries origins %v, want [store:machine-b]", got)
	}

	b, err := pkghistory.BlameUnion(rootLocal, revs, sh, "Beta", pkghistory.BlameOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Events) != 1 {
		t.Fatalf("blame found %d events, want 1", len(b.Events))
	}
	ev := b.Events[0]
	if ev.Entry.ID != "sha256:bbbb0002" || len(ev.Origins) != 1 || ev.Origins[0] != "store:machine-b" {
		t.Errorf("blame event: id=%s origins=%v, want bbbb0002 from store:machine-b", ev.Entry.ID, ev.Origins)
	}
	if ev.Entry.Origin != "machine-b" {
		t.Errorf("store revision names origin %q, want machine-b", ev.Entry.Origin)
	}
}

func TestShare_DiffReconstructsFromTheStore(t *testing.T) {
	root, store := t.TempDir(), t.TempDir()
	appendShareRevision(t, root, "aaaa0001", "c1", "2026-08-01T10:00:00Z", []string{factLine("A", 1)})
	mustPush(t, root, store, "machine-a")

	sh, err := pkghistory.OpenShare(store)
	if err != nil {
		t.Fatal(err)
	}
	rec, ok := sh.RecordFor("sha256:aaaa0001")
	if !ok {
		t.Fatal("record missing")
	}
	snap, err := sh.LoadSnapshot(rec)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Facts) != 1 || snap.Facts[0].Name != "A" {
		t.Errorf("reconstructed snapshot holds %d facts, want the pushed fact A", len(snap.Facts))
	}
	if snap.Meta.SnapshotID != "sha256:aaaa0001" || snap.Meta.EnolaVersion != "test" {
		t.Errorf("reconstructed meta lost its provenance: id=%s version=%s", snap.Meta.SnapshotID, snap.Meta.EnolaVersion)
	}
	if snap.Meta.Git == nil || snap.Meta.Git.Commit != "c1" {
		t.Errorf("reconstructed meta lost its git state: %+v", snap.Meta.Git)
	}
}

func TestShare_GCPrintsFirstAndRecordsThePrune(t *testing.T) {
	root, store, fresh := t.TempDir(), t.TempDir(), t.TempDir()
	appendShareRevision(t, root, "aaaa0001", "c1", "2026-08-01T10:00:00Z", []string{factLine("A", 1)})
	appendShareRevision(t, root, "aaaa0002", "c2", "2026-08-01T11:00:00Z", []string{factLine("B", 2)})
	appendShareRevision(t, root, "aaaa0003", "c3", "2026-08-01T12:00:00Z", []string{factLine("C", 3)})
	mustPush(t, root, store, "machine-a")

	if _, err := SharedGC(store, SharedGCOptions{Source: "machine-a"}); err == nil {
		t.Error("shared gc without a policy did not refuse")
	}

	dry, err := SharedGC(store, SharedGCOptions{KeepLast: 1, Source: "machine-a"})
	if err != nil {
		t.Fatal(err)
	}
	if dry.Applied || len(dry.Remove) != 2 {
		t.Fatalf("dry run: applied=%v remove=%d, want a printed removal of 2", dry.Applied, len(dry.Remove))
	}
	for _, id := range []string{"sha256:aaaa0001", "sha256:aaaa0002"} {
		if _, err := os.Stat(pkghistory.ShareEntryPath(store, id)); err != nil {
			t.Errorf("dry run deleted %s", id)
		}
	}

	applied, err := SharedGC(store, SharedGCOptions{KeepLast: 1, Apply: true, Source: "machine-a",
		Now: time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	if !applied.Applied || len(applied.Remove) != 2 {
		t.Fatalf("apply: applied=%v remove=%d, want 2 removed", applied.Applied, len(applied.Remove))
	}

	rep, err := pkghistory.VerifyShare(store)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Clean() || rep.Pruned != 2 || rep.Verified != 1 {
		t.Errorf("verify after prune: pruned=%d verified=%d problems=%v — a recorded prune is never a gap",
			rep.Pruned, rep.Verified, rep.Problems)
	}

	pull, err := Pull(fresh, store, PullOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if pull.Pulled != 1 || len(pull.Pruned) != 2 {
		t.Errorf("pull after prune: pulled=%d pruned=%v, want 1 pulled and 2 named pruned", pull.Pulled, pull.Pruned)
	}

	sh, err := pkghistory.OpenShare(store)
	if err != nil {
		t.Fatal(err)
	}
	b, err := pkghistory.BlameUnion(fresh, pkghistory.BuildUnion(nil, sh, "github.com/org/repo"), sh, "C", pkghistory.BlameOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if b.Pruned != 2 || b.Scanned != 1 {
		t.Errorf("blame after prune: pruned=%d scanned=%d, want pruned revisions reported as pruned, not searched", b.Pruned, b.Scanned)
	}

	if _, err := sh.LoadPayload("sha256:aaaa0001"); err != nil {
		var pruned *pkghistory.PrunedError
		if !errors.As(err, &pruned) || pruned.Policy != "keep-last 1" {
			t.Errorf("loading a pruned revision: err=%v, want a PrunedError carrying the policy", err)
		}
	} else {
		t.Error("loading a pruned revision succeeded")
	}
}

func tamperSharePayload(t *testing.T, store, id string) {
	t.Helper()
	path := pkghistory.ShareEntryPath(store, id)
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(gz)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	edited := bytes.Replace(raw, []byte(`internal/x/x.go`), []byte(`internal/x/y.go`), 1)
	if bytes.Equal(edited, raw) {
		t.Fatal("test fixture: nothing edited in the payload")
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(edited); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func hasProblem(rep pkghistory.ShareVerifyReport, kind, id string) bool {
	for _, p := range rep.Problems {
		if p.Kind == kind && p.ID == id {
			return true
		}
	}
	return false
}

func hasProblemKind(rep pkghistory.ShareVerifyReport, kind string) bool {
	for _, p := range rep.Problems {
		if p.Kind == kind {
			return true
		}
	}
	return false
}
