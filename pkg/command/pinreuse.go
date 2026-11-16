package command

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/enola-labs/enola/internal/diff"
	"github.com/enola-labs/enola/internal/engine"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/pkg/bootstrap"
	"github.com/enola-labs/enola/pkg/check"
)

// snapshotIsCurrent reports whether the graph's on-disk snapshot describes every
// member's working tree under this build and config, so a pin can freeze what is
// already there. It reads metas, never the facts: the decision is a few hashes per
// member, not a reload of the union. A cluster's members are read from its own
// directory (members.json over the cluster's meta), one repository from its own
// output dir. Any doubt, including a member with no snapshot or a meta without file
// hashes, answers no. The provider set is not compared here: CurrentMeta describes a
// run that has not happened, so it cannot say which providers will run, and an equal
// config hash already proves the same providers are configured.
func snapshotIsCurrent(eng *bootstrap.Engine, repoPaths []string) (string, string) {
	newest := ""
	union := ""
	graphDir := eng.GraphDir(repoPaths)
	for _, repoPath := range repoPaths {
		name := filepath.Base(repoPath)
		var meta *facts.SnapshotMeta
		var err error
		if len(repoPaths) > 1 {
			meta, err = engine.ReadMemberMeta(graphDir, repoPath)
		} else {
			meta, err = readSnapshotMeta(graphDir)
		}
		if err != nil {
			return "", name + " holds no snapshot"
		}
		current := eng.CurrentMeta(repoPath)
		if current == nil {
			return "", name + " could not be described"
		}
		if meta.ConfigHash != current.ConfigHash {
			return "", name + "'s snapshot was written under another config"
		}
		for _, kind := range check.BlockingKinds(diff.CompareMeta(*meta, *current)) {
			if kind == diff.WarnProviderSet {
				continue
			}
			return "", name + "'s snapshot is not comparable with this build: " + string(kind)
		}
		drift, err := eng.DriftFromMeta(repoPath, *meta)
		if err != nil {
			return "", name + ": " + err.Error()
		}
		if drift.Unknown {
			return "", name + "'s snapshot carries no file hashes"
		}
		if drift.Count() > 0 {
			return "", name + " moved since its snapshot: " + drift.Summary(3)
		}
		if union == "" {
			union = meta.SnapshotID
		} else if meta.SnapshotID != union {
			return "", name + " holds a different union than the first member (a fan-out that did not finish)"
		}
		if meta.GeneratedAt > newest {
			newest = meta.GeneratedAt
		}
	}
	if newest == "" {
		return "", "no members"
	}
	return newest, ""
}

func readSnapshotMeta(dir string) (*facts.SnapshotMeta, error) {
	if _, err := os.Stat(filepath.Join(dir, "facts.jsonl")); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(dir, "snapshot.meta.json"))
	if err != nil {
		return nil, err
	}
	var meta facts.SnapshotMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil, err
	}
	return &meta, nil
}
