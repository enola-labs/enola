package history

import (
	"io"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/diff"
	"github.com/enola-labs/enola/internal/facts"
)

// A cluster rerun hands the recorder the same previous/ side once per repository
// dir; the delta is streamed from the previous facts file once and reused after.
func TestSummarizeOnce_ReusesTheDeltaForAnIdenticalPair(t *testing.T) {
	opens := 0
	previous := &previousSide{
		facts: diff.JSONLSource(func() (io.ReadCloser, error) {
			opens++
			return io.NopCloser(strings.NewReader("")), nil
		}),
		meta: facts.SnapshotMeta{OutputHashes: map[string]string{"facts.jsonl": "sha256:p", "insights.json": "sha256:q"}},
	}
	current := &facts.Snapshot{Meta: facts.SnapshotMeta{
		SnapshotID:   "snap",
		OutputHashes: map[string]string{"facts.jsonl": "sha256:a", "insights.json": "sha256:b"},
	}}

	e := NewRecorder(RecorderOptions{})
	first := e.summarizeOnce(current, previous)
	if opens == 0 {
		t.Fatal("first summary did not read the previous side")
	}
	after := opens
	second := e.summarizeOnce(current, previous)
	if opens != after {
		t.Fatalf("second summary re-read the previous side (%d opens, was %d)", opens, after)
	}
	if first.Headline() != second.Headline() {
		t.Fatalf("cached summary differs: %+v vs %+v", first, second)
	}

	unhashed := &previousSide{facts: previous.facts}
	e.summarizeOnce(current, unhashed)
	if opens == after {
		t.Fatal("a previous side without receipt hashes was served from the cache")
	}
}
