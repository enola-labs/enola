package history

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"testing"
)

// encodeBuffered is Encode as it was before the facts section could be streamed: every
// line copied with its sign, sorted, and written into one buffer. Kept as the oracle the
// streaming encoder must match byte for byte, since a stored revision's bytes are what
// gzip and every older reader see.
func encodeBuffered(r Revision) []byte {
	receiptJSON, _ := json.Marshal(r.Receipt)
	var b bytes.Buffer
	b.WriteString(revisionMagic + "\n")
	if r.Parent != "" {
		fmt.Fprintf(&b, "parent %s\n", r.Parent)
	}
	fmt.Fprintf(&b, "facts %s\n", r.FactsHash)
	fmt.Fprintf(&b, "receipt %d\n", len(receiptJSON))
	b.Write(receiptJSON)
	b.WriteByte('\n')
	for _, s := range []struct {
		name string
		p    Patch
	}{{"facts", r.Facts}, {"insights", r.Insights}} {
		b.WriteString(s.name + "\n")
		for _, line := range interleave(s.p) {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	return b.Bytes()
}

// randomLines draws n lines from a small alphabet, so duplicates and shared prefixes are
// common: the cases where an ordering or a multiset count goes wrong.
func randomLines(rng *rand.Rand, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf(`{"name":"S%d","line":%d}`, rng.Intn(40), rng.Intn(4))
	}
	return out
}

func TestRevision_EncodeMatchesTheBufferedEncoder(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 300; i++ {
		prev, cur := randomLines(rng, rng.Intn(60)), randomLines(rng, rng.Intn(60))
		rev := Revision{
			FactsHash: HashLines(cur),
			Facts:     ComputePatch(prev, cur),
			Insights:  ComputePatch(cur[:len(cur)/2], prev[:len(prev)/2]),
		}
		if i%2 == 0 {
			rev.Parent = HashLines(prev)
		}
		var got bytes.Buffer
		if err := rev.Encode(&got); err != nil {
			t.Fatal(err)
		}
		if want := encodeBuffered(rev); !bytes.Equal(got.Bytes(), want) {
			t.Fatalf("case %d: streaming encoder differs from the buffered one\n got: %q\nwant: %q", i, got.Bytes(), want)
		}
	}
}

// A patch built by hand need not be sorted. The encoder must still write stored order,
// or the file decodes to a patch whose halves are out of order.
func TestRevision_EncodeSortsAPatchBuiltOutOfOrder(t *testing.T) {
	rev := Revision{
		FactsHash: "sha256:x",
		Facts:     Patch{Del: lines("c", "a"), Add: lines("d", "b")},
	}
	var got bytes.Buffer
	if err := rev.Encode(&got); err != nil {
		t.Fatal(err)
	}
	if want := encodeBuffered(rev); !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("got %q, want %q", got.Bytes(), want)
	}
}

// EncodeFacts with the patch's own lines is Encode: one encoder, whichever way the facts
// section arrives.
func TestRevision_EncodeFactsWritesWhatItIsGiven(t *testing.T) {
	rev := Revision{FactsHash: "sha256:x", Insights: Patch{Add: lines("i")}}
	var got bytes.Buffer
	err := rev.EncodeFacts(&got, func(sw *SectionWriter) error {
		if err := sw.LineString('-', "a"); err != nil {
			return err
		}
		return sw.Line('+', []byte("b"))
	})
	if err != nil {
		t.Fatal(err)
	}
	rev.Facts = Patch{Del: lines("a"), Add: lines("b")}
	if want := encodeBuffered(rev); !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("got %q, want %q", got.Bytes(), want)
	}
}
