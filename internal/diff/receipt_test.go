package diff

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func TestCompareMeta(t *testing.T) {
	base := facts.SnapshotMeta{
		RepoPath: "/repo", EnolaVersion: "1.0", IgnoreGlobHash: "h",
		Extractors: []string{"go", "typescript"},
	}

	t.Run("equivalent inputs produce no warnings", func(t *testing.T) {
		cur := base
		if c := CompareMeta(base, cur); !c.Comparable {
			t.Errorf("expected comparable, got warnings: %v", c.Warnings)
		}
	})

	t.Run("comparable verdict is an explicit JSON field", func(t *testing.T) {
		// The gap this fixes: a full-JSON consumer must read comparable:true
		// directly, not infer it from an absent warnings list.
		out, err := json.Marshal(CompareMeta(base, base))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(out), `"comparable":true`) {
			t.Errorf(`clean verdict must marshal an explicit "comparable":true; got %s`, out)
		}
	})

	t.Run("extractor removed in current is flagged as REMOVED", func(t *testing.T) {
		cur := base
		cur.Extractors = []string{"go"} // typescript dropped from the current run
		msg := strings.Join(CompareMeta(base, cur).Warnings, " ")
		if !strings.Contains(msg, "typescript") {
			t.Fatalf("warning should name the missing extractor, got: %s", msg)
		}
		// typescript was in the baseline, not current → its facts appear as REMOVED.
		if !strings.Contains(msg, "baseline had extractor(s) not in the current run: typescript") ||
			!strings.Contains(msg, "REMOVED") {
			t.Errorf("expected a 'baseline had … REMOVED' direction, got: %s", msg)
		}
	})

	t.Run("extractor added in current is flagged as ADDED", func(t *testing.T) {
		cur := base
		cur.Extractors = []string{"go", "typescript", "openapi"} // openapi gained
		msg := strings.Join(CompareMeta(base, cur).Warnings, " ")
		// openapi is in current, not baseline → its facts appear as ADDED.
		if !strings.Contains(msg, "current run added extractor(s) not in the baseline: openapi") ||
			!strings.Contains(msg, "ADDED") {
			t.Errorf("expected a 'current added … ADDED' direction, got: %s", msg)
		}
	})

	t.Run("different enola version is flagged", func(t *testing.T) {
		cur := base
		cur.EnolaVersion = "2.0"
		if CompareMeta(base, cur).Comparable {
			t.Error("expected a warning for differing enola versions")
		}
	})

	t.Run("empty baseline meta is a soft not-verifiable note, not a hard mismatch", func(t *testing.T) {
		// An auto-loaded baseline carries only RepoPath.
		autoLoaded := facts.SnapshotMeta{RepoPath: "/repo"}
		c := CompareMeta(autoLoaded, base)
		if c.Comparable {
			t.Error("expected a not-verifiable note for an empty baseline")
		}
		if !strings.Contains(strings.Join(c.Warnings, " "), "predates snapshot receipts") {
			t.Errorf("expected the soft 'predates receipts' note, got: %v", c.Warnings)
		}
	})
}

func TestCompareReceipts(t *testing.T) {
	base := facts.SnapshotMeta{
		EnolaVersion: "1.0", SnapshotID: "aaa", Extractors: []string{"go"},
		FilesSeen: 100, FilesParsed: 90, ParseErrors: 0,
	}

	t.Run("rising parse errors surface as a quality regression", func(t *testing.T) {
		cur := base
		cur.SnapshotID = "bbb"
		cur.ParseErrors = 3
		rc := CompareReceipts(base, cur)
		if len(rc.QualityRegressions) == 0 {
			t.Fatal("expected a quality regression for rising parse errors")
		}
		if !strings.Contains(strings.Join(rc.QualityRegressions, " "), "parse errors rose") {
			t.Errorf("unexpected regressions: %v", rc.QualityRegressions)
		}
	})

	t.Run("dropping parsed/seen ratio is a regression", func(t *testing.T) {
		cur := base
		cur.SnapshotID = "bbb"
		cur.FilesParsed = 50 // 50% vs 90%
		rc := CompareReceipts(base, cur)
		if !strings.Contains(strings.Join(rc.QualityRegressions, " "), "parsed/seen ratio dropped") {
			t.Errorf("expected a ratio-drop regression, got: %v", rc.QualityRegressions)
		}
	})

	t.Run("identical snapshot ids short-circuit", func(t *testing.T) {
		rc := CompareReceipts(base, base)
		if !rc.Identical {
			t.Error("expected Identical=true for matching snapshot_id")
		}
	})

	// A newly-ignored directory prunes a whole subtree. It reports as a dirs_skipped
	// delta and NOT as a quality regression: pruning vendor/ is usually the operator
	// doing the right thing, and files_seen falling is the signal that costs something.
	t.Run("a pruned directory surfaces as a dirs_skipped delta", func(t *testing.T) {
		cur := base
		cur.SnapshotID = "bbb"
		cur.DirsSkipped = 2

		rc := CompareReceipts(base, cur)

		var found *MetricDelta
		for i := range rc.Deltas {
			if rc.Deltas[i].Name == "dirs_skipped" {
				found = &rc.Deltas[i]
			}
		}
		if found == nil {
			t.Fatalf("no dirs_skipped delta; got %v", rc.Deltas)
		}
		if found.Before != 0 || found.After != 2 || found.Delta != 2 {
			t.Errorf("dirs_skipped delta = %+v, want before=0 after=2 delta=2", *found)
		}
		if len(rc.QualityRegressions) != 0 {
			t.Errorf("pruning a directory is not a regression, got: %v", rc.QualityRegressions)
		}
	})
}

// A union's members are labels on its facts, not fields of its meta, so the meta
// of a two-repository union and of its last member alone are field-for-field
// equal. compare_receipts answered "equivalent inputs" for that pair while
// diff_snapshot, over the same two snapshots, refused it.
func TestCompareSnapshotReceipts_ADroppedUnionMemberIsNotEquivalentInputs(t *testing.T) {
	meta := facts.SnapshotMeta{EnolaVersion: "1.0", Extractors: []string{"go"}, FilesSeen: 10, FilesParsed: 10}
	kept := facts.Fact{Kind: facts.KindSymbol, Name: "Kept", File: "web/kept.ts", Repo: "web"}
	gone := facts.Fact{Kind: facts.KindSymbol, Name: "Gone", File: "api/gone.rb", Repo: "api"}

	base := &facts.Snapshot{Meta: meta, Facts: []facts.Fact{kept, gone}}
	base.Meta.SnapshotID = "aaa"
	cur := &facts.Snapshot{Meta: meta, Facts: []facts.Fact{kept}}
	cur.Meta.SnapshotID = "bbb"

	if c := CompareMeta(base.Meta, cur.Meta); !c.Comparable {
		t.Fatalf("the metas alone must compare clean, or this test proves nothing: %+v", c)
	}

	rc := CompareSnapshotReceipts(base, cur)
	if !rc.Comparability.HasKind(WarnUnionMembership) {
		t.Fatalf("comparability = %+v, want union_membership", rc.Comparability)
	}
	if rc.Comparability.Comparable {
		t.Error("Comparable = true for a union that lost a member")
	}
	if out := rc.Render(); !strings.Contains(out, "Not safely comparable") || strings.Contains(out, "equivalent inputs") {
		t.Errorf("render must refuse the pair, got:\n%s", out)
	}
	if want := Compute(base, cur).Comparability; !reflect.DeepEqual(rc.Comparability, want) {
		t.Errorf("compare_receipts and diff_snapshot disagree about one pair:\n receipt %+v\n diff    %+v", rc.Comparability, want)
	}

	same := CompareSnapshotReceipts(base, &facts.Snapshot{Meta: cur.Meta, Facts: []facts.Fact{kept, gone}})
	if !same.Comparability.Comparable {
		t.Errorf("an unchanged member set must stay comparable: %+v", same.Comparability)
	}
}
