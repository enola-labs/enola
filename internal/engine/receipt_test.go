package engine_test

// Receipt determinism: the snapshot receipt exists to PROVE what the graph was
// deterministic over, so the receipt itself must be deterministic. The snapshot
// ID (a content fingerprint) and the receipt minus its wall-clock fields
// (generated_at, duration) must be byte-identical across two independent runs on
// the same inputs — otherwise the fingerprint cannot key equivalence.

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/pkg/bootstrap"
)

func TestReceiptDeterminism(t *testing.T) {
	for _, f := range fixtures {
		f := f
		t.Run(f.name, func(t *testing.T) {
			first := receiptForFixture(t, f)
			second := receiptForFixture(t, f)

			if first.SnapshotID == "" {
				t.Fatalf("empty snapshot_id for %s", f.name)
			}
			if first.SnapshotID != second.SnapshotID {
				t.Errorf("non-deterministic snapshot_id for %s: %q != %q", f.name, first.SnapshotID, second.SnapshotID)
			}
			// Every receipt hash carries the "sha256:" prefix (issue #60's notation).
			if !strings.HasPrefix(first.SnapshotID, "sha256:") {
				t.Errorf("snapshot_id missing sha256: prefix for %s: %q", f.name, first.SnapshotID)
			}
			if first.ConfigHash == "" || !strings.HasPrefix(first.ConfigHash, "sha256:") {
				t.Errorf("config_hash missing or unprefixed for %s: %q", f.name, first.ConfigHash)
			}
			// The artifact hashes are the only part of the receipt that can catch a
			// nondeterministic renderer, and they are recorded by WriteArtifacts. When
			// this test did not call it the map was empty, so this loop inspected
			// nothing and the comparison below could not see a moving artifact at all
			// — the defect the test exists to catch was invisible to it.
			if len(first.OutputHashes) == 0 {
				t.Fatalf("no output hashes recorded for %s: the receipt comparison is blind", f.name)
			}
			for _, want := range []string{"facts.jsonl", "insights.json", "llm_context.md"} {
				if _, ok := first.OutputHashes[want]; !ok {
					t.Errorf("no output hash for %s in %s: %v", want, f.name, first.OutputHashes)
				}
			}
			for name, h := range first.OutputHashes {
				if !strings.HasPrefix(h, "sha256:") {
					t.Errorf("output hash for %s missing sha256: prefix in %s: %q", name, f.name, h)
				}
			}

			// llm_context.md's footer carries "Generated at <t> in <d>", so its hash
			// cannot repeat by construction while that footer stands (finding 0001,
			// part 2 — deliberately held). Its determinism is asserted directly on the
			// renderer instead, by TestRender_RepeatedRendersAreIdentical.
			delete(first.OutputHashes, "llm_context.md")
			delete(second.OutputHashes, "llm_context.md")

			// Zero out the wall-clock fields, then the receipts must be identical.
			first.GeneratedAt, second.GeneratedAt = "", ""
			first.Duration, second.Duration = "", ""
			fb, _ := json.Marshal(first)
			sb, _ := json.Marshal(second)
			if string(fb) != string(sb) {
				t.Errorf("non-deterministic receipt for %s:\nfirst:  %s\nsecond: %s", f.name, fb, sb)
			}

			// FilesParsed must never exceed FilesSeen — a sanity check on the tally.
			if first.Quality.FilesParsed > first.Quality.FilesSeen {
				t.Errorf("files_parsed (%d) > files_seen (%d) for %s",
					first.Quality.FilesParsed, first.Quality.FilesSeen, f.name)
			}
		})
	}
}

// receiptForFixture runs the full pipeline over a fresh copy of the fixture and
// returns the resulting receipt. It mirrors snapshotFixture but keeps the meta.
func receiptForFixture(t *testing.T, f fixture) facts.Receipt {
	t.Helper()

	root := copyTree(t, f.sourceDir(), t.TempDir())
	eng, _, err := bootstrap.NewEngine(bootstrap.Options{
		ConfigPath: f.configPath(t),
	})
	if err != nil {
		t.Fatalf("bootstrap.NewEngine: %v", err)
	}

	for i, sub := range f.subRepos {
		repoPath := root
		if sub != "." {
			repoPath = filepath.Join(root, sub)
		}
		if _, err := eng.GenerateSnapshot(context.Background(), repoPath, i > 0); err != nil {
			t.Fatalf("GenerateSnapshot(%s): %v", sub, err)
		}
	}

	// The receipt's output_hashes are recorded by WriteArtifacts, not by
	// GenerateSnapshot: without this call the map is empty and every loop below
	// that iterates it inspects nothing.
	if err := eng.WriteArtifacts(root); err != nil {
		t.Fatalf("WriteArtifacts: %v", err)
	}

	r := eng.Snapshot().Meta.Receipt()
	// The repo path is an absolute temp dir that differs per run; exclude it from
	// the determinism comparison (it is not part of the snapshot_id either).
	r.RepoPath = ""
	// Git state depends on the ambient checkout, not the fixture; normalize it.
	r.Git = nil
	return r
}
