package engine_test

// The goldens pin what each fixture's fact graph is; this test pins that the graph only
// moves when cacheVersion does. cacheVersion is the graph's behaviour stamp: an
// unedited tree whose graph was written under the same one is not regenerated, and the
// append guard keeps a prior union written under it. A change that moves facts.jsonl
// without a bump (an extractor, a binder, a linker) therefore leaves graphs on disk
// that no longer describe what this build would write, and nothing says so. See the
// cacheVersion item in docs/EXTENDING.md.
//
// testdata/golden_versions.json records, under the cacheVersion it was taken at, a hash
// of each fixture's input (its tree, its config, its sub-repo order) and of its facts
// golden. With the same version and the same input, the facts must hash the same.
// Insights goldens are left out: they pin what explainers conclude, explainers run over
// the stored facts and are no part of facts.jsonl, and a facts change that moves a
// finding already shows in the facts hash.
//
// `go test ./internal/engine -run TestGolden -update` maintains the record: after a
// bump it re-records everything, and under the same version it records only fixtures
// that are new or whose input changed. It never re-records a facts change on an
// unchanged input under an unchanged version, so -update cannot paper over a missing
// bump.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/engine"
)

const goldenVersionsPath = "testdata/golden_versions.json"

type goldenVersionEntry struct {
	Input string `json:"input"`
	Facts string `json:"facts"`
}

type goldenVersions struct {
	CacheVersion string                        `json:"cache_version"`
	Goldens      map[string]goldenVersionEntry `json:"goldens"`
}

func TestGoldenFactsNeedCacheVersionBump(t *testing.T) {
	current := map[string]goldenVersionEntry{}
	for _, f := range fixtures {
		facts, err := os.ReadFile(filepath.Join("testdata", "golden", f.name+".facts.jsonl"))
		if err != nil {
			t.Fatalf("read golden for %s: %v", f.name, err)
		}
		current[f.name] = goldenVersionEntry{Input: fixtureInputHash(t, f), Facts: sha(facts)}
	}
	version := engine.ExtractorVersion()

	var rec goldenVersions
	if raw, err := os.ReadFile(goldenVersionsPath); err == nil {
		if err := json.Unmarshal(raw, &rec); err != nil {
			t.Fatalf("parse %s: %v", goldenVersionsPath, err)
		}
	} else if !*update {
		t.Fatalf("read %s (run `go test ./internal/engine -run TestGolden -update` to create it): %v", goldenVersionsPath, err)
	}

	if *update {
		rec = rerecord(rec, current, version)
		raw, err := json.MarshalIndent(rec, "", "  ")
		if err != nil {
			t.Fatalf("marshal %s: %v", goldenVersionsPath, err)
		}
		if err := os.WriteFile(goldenVersionsPath, append(raw, '\n'), 0o644); err != nil {
			t.Fatalf("write %s: %v", goldenVersionsPath, err)
		}
	}

	const rerun = "run `go test ./internal/engine -run TestGolden -update` to re-record"
	if rec.CacheVersion != version {
		t.Fatalf("%s was recorded at cacheVersion %s and this build is %s: %s", goldenVersionsPath, rec.CacheVersion, version, rerun)
	}
	names := make([]string, 0, len(current))
	for n := range current {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		got, was := current[n], rec.Goldens[n]
		switch {
		case was == (goldenVersionEntry{}):
			t.Errorf("golden %s is not recorded in %s: %s", n, goldenVersionsPath, rerun)
		case was.Input != got.Input:
			t.Errorf("fixture %s changed since %s recorded it: %s", n, goldenVersionsPath, rerun)
		case was.Facts != got.Facts:
			t.Errorf("golden facts of %s changed while cacheVersion stayed %s and the fixture did not: "+
				"bump cacheVersion in internal/engine/cache.go with a versionCoverage entry in internal/cachecov "+
				"(docs/EXTENDING.md, the cacheVersion item), then %s", n, version, rerun)
		}
	}
	for n := range rec.Goldens {
		if _, ok := current[n]; !ok {
			t.Errorf("%s records %s, which is no longer a fixture: %s", goldenVersionsPath, n, rerun)
		}
	}
}

// rerecord returns the record -update writes: everything at a new version, and at the
// same version only the fixtures that are new or whose input changed.
func rerecord(rec goldenVersions, current map[string]goldenVersionEntry, version string) goldenVersions {
	if rec.CacheVersion != version || rec.Goldens == nil {
		return goldenVersions{CacheVersion: version, Goldens: current}
	}
	out := goldenVersions{CacheVersion: version, Goldens: map[string]goldenVersionEntry{}}
	for n, got := range current {
		was, ok := rec.Goldens[n]
		if !ok || was.Input != got.Input {
			was = got
		}
		out.Goldens[n] = was
	}
	return out
}

// fixtureInputHash hashes what a fixture's graph is computed from: every file of its
// tree, the config it is built with, and the order its sub-repos are snapshotted in.
func fixtureInputHash(t *testing.T, f fixture) string {
	t.Helper()
	h := sha256.New()
	h.Write([]byte("subrepos\x00" + strings.Join(f.subRepos, "\x00") + "\x00"))
	if f.config != "" {
		cfg, err := os.ReadFile(filepath.Join("testdata", "configs", f.config))
		if err != nil {
			t.Fatalf("read config of %s: %v", f.name, err)
		}
		h.Write([]byte("config\x00" + string(cfg) + "\x00"))
	}
	root := f.sourceDir()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Output, not input: a test that snapshots a fixture in place (rather than
			// a copy) leaves its .enola there, ignored by git.
			if d.Name() == ".enola" {
				return filepath.SkipDir
			}
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		h.Write([]byte("file\x00" + filepath.ToSlash(rel) + "\x00"))
		h.Write([]byte(sha(data) + "\x00"))
		return nil
	})
	if err != nil {
		t.Fatalf("hash fixture %s: %v", f.name, err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
