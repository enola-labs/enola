package command

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/enola-labs/enola/internal/diff"
	"github.com/enola-labs/enola/internal/engine"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/pkg/bootstrap"
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
	if gone := membersDropped(graphDir, repoPaths); gone != "" {
		return "", gone
	}
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
		if kinds := metaBlockingKinds(*meta, *current); len(kinds) > 0 {
			return "", name + "'s snapshot is not comparable with this build: " + string(kinds[0])
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

// membersDropped names the members the snapshot in dir measured that repoPaths does
// not, as the gate would decline them (union_membership), or "" when there are none.
// A configured cluster directory outlives its member list, so a config that dropped a
// member finds the union that measured it there, while every member it still names
// matches its tree. The per-member checks cannot see a member they were never asked
// about; members.json can.
func membersDropped(dir string, repoPaths []string) string {
	c := membershipComparability(dir, repoPaths)
	if c.Comparable {
		return ""
	}
	return fmt.Sprintf("the snapshot in %s is not comparable with this config: %s (%s)",
		dir, diff.WarnUnionMembership, strings.Join(c.Warnings, "; "))
}

// membershipComparability is the union arm for the snapshot in dir against
// repoPaths, read from its members.json. A directory without one is a single
// repository's and compares clean.
func membershipComparability(dir string, repoPaths []string) diff.Comparability {
	members, err := engine.ReadMembersMeta(dir)
	if err != nil {
		return diff.CompareMembership(nil, nil)
	}
	label := func(path string) string {
		if m, ok := members[path]; ok && m.RepoLabel != "" {
			return m.RepoLabel
		}
		return filepath.Base(path)
	}
	var base, cur []string
	for path := range members {
		base = append(base, label(path))
	}
	for _, p := range repoPaths {
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		cur = append(cur, label(p))
	}
	return diff.CompareMembership(base, cur)
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
