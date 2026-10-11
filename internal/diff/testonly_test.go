package diff

import (
	"io"

	"github.com/enola-labs/enola/internal/facts"
)

// Entry points that only this package's tests call. They are kept out of the
// production files so the binary carries only what it runs.

// CountAgainstJSONL computes the delta between a baseline snapshot stored as JSONL and
// the current facts, without materialising the baseline.
//
// # Why hashes
//
// Every string that identifies a fact — its key, its props — is reduced to a 64-bit
// FNV-1a hash, because holding the strings is the thing being avoided: factKey alone
// averages ~60 bytes and a kernel-sized snapshot has 1.9M of them per side.
//
// A collision would miscount by one. For 1.9M keys in a 64-bit space the expected
// number of collisions is n²/2^65 ≈ 1e-7, and the blast radius is one integer in a log
// line, not a fact in a graph. Compute — which diff_snapshot uses, and which must NAME
// the facts rather than count them — keeps the exact strings throughout.
//
// # Two passes
//
// Pass 1 reduces every fact; pass 2 revisits only the facts whose key is shared, to
// recover the exact intraGroupOrder that decides how group members pair up. A source
// backed by a file is therefore read twice. That is deliberate: the alternative is
// keeping an ordering string for every fact, which is the memory this exists to avoid,
// and pass 2 is skipped altogether when no key is shared.
func CountAgainstJSONL(open func() (io.ReadCloser, error), current []facts.Fact) (*Counts, error) {
	return count(JSONLSource(open), SliceSource(current))
}

// CountSnapshots is the in-memory form, for callers that already hold both sides (and
// for the differential test that grades this against Compute).
func CountSnapshots(baseline, current []facts.Fact) *Counts {
	c, err := count(SliceSource(baseline), SliceSource(current))
	if err != nil {
		// A slice source cannot fail; only a reader can.
		return &Counts{AddedByKind: map[string]int{}, RemovedByKind: map[string]int{}}
	}
	return c
}
