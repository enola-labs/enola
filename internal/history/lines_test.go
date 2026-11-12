package history

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	pkghistory "github.com/enola-labs/enola/pkg/history"
)

// The merge is ComputePatch without the maps: the same lines in each half, multiplicities
// included, in stored order.
func TestMergeLines_IsComputePatch(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	draw := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("L%02d", rng.Intn(25))
		}
		sort.Strings(out)
		return out
	}
	for i := 0; i < 500; i++ {
		prev, cur := draw(rng.Intn(40)), draw(rng.Intn(40))
		var got pkghistory.Patch
		var order []string
		err := mergeLines(prev, sliceLines(cur),
			func(l string) error { got.Del = append(got.Del, l); order = append(order, "-"+l); return nil },
			func(l []byte) error {
				got.Add = append(got.Add, string(l))
				order = append(order, "+"+string(l))
				return nil
			},
			nil)
		if err != nil {
			t.Fatal(err)
		}
		want := pkghistory.ComputePatch(prev, cur)
		if fmt.Sprint(got.Del) != fmt.Sprint(want.Del) || fmt.Sprint(got.Add) != fmt.Sprint(want.Add) {
			t.Fatalf("prev %v cur %v: merged %+v, ComputePatch %+v", prev, cur, got, want)
		}
		if !sort.SliceIsSorted(order, func(a, b int) bool { return order[a][1:] < order[b][1:] }) {
			t.Fatalf("not in stored order: %v", order)
		}
	}
}

func writeLines(t *testing.T, path string, lines []string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// bigFacts is n realistic fact lines, sorted as facts.jsonl is, with duplicates.
func bigFacts(n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, factLine(fmt.Sprintf("pkg%03d.Symbol%05d", i%300, i/2), 10+i%500))
	}
	sort.Strings(out)
	return out
}

// appendFromPath records one revision whose facts are read from path.
func appendFromPath(t *testing.T, root, path string, i int) pkghistory.Entry {
	t.Helper()
	id := fmt.Sprintf("sha256:cccc%04d", i)
	c := contents(id, nil, []string{`{"title":"finding","confidence":1}`})
	c.FactsPath = path
	e := pkghistory.Entry{ID: id, Repo: "github.com/org/repo", At: "2026-08-03T10:00:00Z", Epoch: "epoch1", Git: gitAt(fmt.Sprintf("c%d", i))}
	if _, err := Append(root, e, Options{WorkingKeep: -1, Contents: c}); err != nil {
		t.Fatal(err)
	}
	entries, err := pkghistory.Read(root)
	if err != nil {
		t.Fatal(err)
	}
	return entries[len(entries)-1]
}

// A revision streamed from facts.jsonl is stored byte for byte as the buffered writer
// stored it (lines read in, ComputePatch, encode, gzip in one write), for a base and for
// a delta. Byte identity rather than a round trip, because a round trip cannot see a
// change in what older readers and the gzip stream are handed.
func TestBlobs_StreamedRevisionIsStoredAsTheBufferedOneWas(t *testing.T) {
	first := bigFacts(20000)
	second := append([]string(nil), first[:19800]...)
	second = append(second, first[14990:15010]...) // duplicates of lines already present
	for i := 0; i < 100; i++ {
		second = append(second, factLine(fmt.Sprintf("new.Symbol%04d", i), i))
	}
	sort.Strings(second)

	root := t.TempDir()
	path := filepath.Join(t.TempDir(), "facts.jsonl")
	insights := []string{`{"title":"finding","confidence":1}`}
	var parent []string
	for i, lines := range [][]string{first, second} {
		writeLines(t, path, lines)
		e := appendFromPath(t, root, path, i)
		if e.Blob.Member != i+1 {
			t.Fatalf("revision %d is member %d of its segment; the test needs a base and then a delta", i, e.Blob.Member)
		}

		want := pkghistory.Revision{
			FactsHash: pkghistory.HashLines(lines),
			Receipt:   contents(e.ID, nil, nil).Receipt,
			Facts:     pkghistory.ComputePatch(parent, lines),
			Insights:  pkghistory.ComputePatch(nil, insights),
		}
		if parent != nil {
			want.Parent = pkghistory.HashLines(parent)
			want.Insights = pkghistory.ComputePatch(insights, insights)
		}
		var raw, gz bytes.Buffer
		if err := want.Encode(&raw); err != nil {
			t.Fatal(err)
		}
		zw := gzip.NewWriter(&gz)
		_, _ = zw.Write(raw.Bytes())
		_ = zw.Close()

		got, err := os.ReadFile(pkghistory.RevisionPath(root, e.Blob.Segment, e.Blob.Member))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, gz.Bytes()) {
			t.Fatalf("revision %d (member %d): %d bytes stored, the buffered writer stores %d", i, e.Blob.Member, len(got), gz.Len())
		}
		parent = lines
	}
}

// A facts.jsonl out of order was not written by facts.Store.WriteJSONL. It is still
// recorded, by reading it in and sorting it, and it reconstructs to the sorted lines.
func TestBlobs_AnUnsortedFactsFileIsReadInAndSorted(t *testing.T) {
	lines := []string{factLine("C", 3), factLine("A", 1), factLine("B", 2)}
	root := t.TempDir()
	path := filepath.Join(t.TempDir(), "facts.jsonl")
	writeLines(t, path, lines)
	e := appendFromPath(t, root, path, 0)
	got, _, _, err := pkghistory.LoadLines(root, e.Blob.Segment, e.Blob.Member)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(lines)
	if fmt.Sprint(got) != fmt.Sprint(lines) {
		t.Fatalf("reconstructed %v, want %v", got, lines)
	}
}

// Recording a base revision from facts.jsonl allocates a fixed amount, not the file:
// it used to hold the lines, a signed copy of each, the encoding and the gzip of it,
// about four times facts.jsonl alive at once beside the store it was written from.
func TestBlobs_ABaseRevisionDoesNotHoldTheFile(t *testing.T) {
	lines := bigFacts(160000)
	path := filepath.Join(t.TempDir(), "facts.jsonl")
	writeLines(t, path, lines)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	lines = nil

	root := t.TempDir()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	appendFromPath(t, root, path, 0)
	runtime.ReadMemStats(&after)

	allocated := after.TotalAlloc - before.TotalAlloc
	if allocated > uint64(info.Size())/2 {
		t.Fatalf("recording a %d-byte facts.jsonl allocated %d bytes; it should not scale with the file", info.Size(), allocated)
	}
	t.Logf("facts.jsonl %d bytes, allocated %d", info.Size(), allocated)
}
