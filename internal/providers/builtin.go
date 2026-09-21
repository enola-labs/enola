package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/providers/rubydex"
)

// builtIns are the providers the binary carries itself, keyed by the name a
// `providers:` entry with no command uses. Each runs in-process and answers
// with facts that pass the same validation as JSONL from an external tool,
// a census, and the version of the engine it read through.
var builtIns = map[string]func(ctx context.Context, repoPath string, ignored func(file string) bool) ([]facts.Fact, string, facts.ProviderCensus, string){
	"rubydex": runRubydex,
}

// BuiltInNames lists the providers a config may name without a command.
func BuiltInNames() []string {
	names := make([]string, 0, len(builtIns))
	for name := range builtIns {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func runBuiltIn(ctx context.Context, p Provider, in Input) ([]facts.Fact, facts.ProviderRecord) {
	repoPath := in.RepoPath
	record := facts.ProviderRecord{Name: p.Name}
	skip := func(format string, args ...any) ([]facts.Fact, facts.ProviderRecord) {
		record.Skipped = true
		record.Reason = fmt.Sprintf(format, args...)
		return nil, record
	}
	run, ok := builtIns[p.Name]
	if !ok {
		return skip("no command and no built-in provider named %q (built-ins: %v)", p.Name, BuiltInNames())
	}
	if p.Name == "rubydex" && in.Cache != nil {
		if accepted, record, served := serveRubydexIndex(p, in, record); served {
			return accepted, record
		}
	}
	accepted, version, census, refusal := run(ctx, repoPath, in.Ignored)
	if refusal != "" {
		return skip("%s", refusal)
	}
	record.Version = version
	if p.ExpectedVersion != "" && version != p.ExpectedVersion {
		return skip("version mismatch: this enola carries %s, expected %s", version, p.ExpectedVersion)
	}
	for i, f := range accepted {
		if err := validateFact(f); err != nil {
			return skip("invalid output: fact %d: %v", i, err)
		}
	}
	record.Census = &census
	stamp(accepted, p.Name, version)
	sortFacts(accepted)
	if p.Name == "rubydex" && in.Cache != nil {
		recordRubydexIndex(p, in, accepted, census, &record)
	}
	return accepted, record
}

// The Rubydex index is one entry per run, never patched: a reference in one
// file resolves against declarations in another, so the facts are a function
// of the whole Ruby file set and the locked bundle together. The key is the
// engine library's version, the digest of the sorted Ruby paths with their
// content digests, and the lockfile's digest; the census the index produced
// travels beside the facts in a second entry under a stable key, so a hit can
// report it as recorded and a miss can say which part of the key moved.
const rubydexIndexMetaKey = "index-meta\x00rubydex"

type rubydexIndexMeta struct {
	Version     string               `json:"version"`
	FilesDigest string               `json:"files_digest"`
	LockDigest  string               `json:"lock_digest"`
	Census      facts.ProviderCensus `json:"census"`
}

func rubydexIndexParts(in Input) (filesDigest, lockDigest string) {
	var ruby []string
	for _, file := range in.Files {
		if rubydex.Indexable(file) {
			ruby = append(ruby, file)
		}
	}
	sort.Strings(ruby)
	parts := make([]string, 0, 2*len(ruby))
	for _, file := range ruby {
		parts = append(parts, file, in.Hashes[file])
	}
	filesDigest = digestOf(parts...)
	if data, err := os.ReadFile(filepath.Join(in.RepoPath, "Gemfile.lock")); err == nil {
		lockDigest = digestOf(string(data))
	}
	return filesDigest, lockDigest
}

func rubydexIndexKey(version, filesDigest, lockDigest string) string {
	return "index\x00rubydex\x00" + version + "\x00" + filesDigest + "\x00" + lockDigest
}

// serveRubydexIndex answers from the cache when the recorded index matches the
// tree and the bundle. The meta entry is read without carrying it forward:
// either the hit carries it forward below, or the miss replaces it.
func serveRubydexIndex(p Provider, in Input, record facts.ProviderRecord) ([]facts.Fact, facts.ProviderRecord, bool) {
	if p.ExpectedVersion != "" && rubydex.Version != p.ExpectedVersion {
		return nil, record, false
	}
	filesDigest, lockDigest := rubydexIndexParts(in)
	meta, hasMeta := readRubydexMeta(in.Cache)
	if !hasMeta || meta.Version != rubydex.Version || meta.FilesDigest != filesDigest || meta.LockDigest != lockDigest {
		return nil, record, false
	}
	accepted, ok := in.Cache.Get(rubydexIndexKey(rubydex.Version, filesDigest, lockDigest))
	if !ok {
		return nil, record, false
	}
	in.Cache.Get(rubydexIndexMetaKey)
	record.Version = rubydex.Version
	census := meta.Census
	record.Census = &census
	record.Reuse = &facts.ProviderReuse{Reused: len(accepted), Cache: "hit"}
	return accepted, record, true
}

func recordRubydexIndex(p Provider, in Input, accepted []facts.Fact, census facts.ProviderCensus, record *facts.ProviderRecord) {
	filesDigest, lockDigest := rubydexIndexParts(in)
	miss := "cold"
	if meta, ok := readRubydexMeta(in.Cache); ok {
		switch {
		case meta.Version != rubydex.Version:
			miss = "version"
		case meta.LockDigest != lockDigest:
			miss = "lockfile"
		default:
			miss = "files"
		}
	}
	record.Reuse = &facts.ProviderReuse{Computed: len(accepted), Cache: "miss", Miss: miss}
	in.Cache.Put(rubydexIndexKey(rubydex.Version, filesDigest, lockDigest), accepted)
	meta := rubydexIndexMeta{Version: rubydex.Version, FilesDigest: filesDigest, LockDigest: lockDigest, Census: census}
	encoded, err := json.Marshal(meta)
	if err != nil {
		return
	}
	in.Cache.Put(rubydexIndexMetaKey, []facts.Fact{{Kind: "provider-index", Name: "rubydex", Props: map[string]any{"meta": string(encoded)}}})
}

func readRubydexMeta(cache Cache) (rubydexIndexMeta, bool) {
	entry, ok := cache.Peek(rubydexIndexMetaKey)
	if !ok || len(entry) != 1 {
		return rubydexIndexMeta{}, false
	}
	encoded, _ := entry[0].PropAny("meta").(string)
	var meta rubydexIndexMeta
	if err := json.Unmarshal([]byte(strings.TrimSpace(encoded)), &meta); err != nil {
		return rubydexIndexMeta{}, false
	}
	return meta, true
}

func runRubydex(ctx context.Context, repoPath string, ignored func(file string) bool) ([]facts.Fact, string, facts.ProviderCensus, string) {
	path, installed := rubydex.Installed()
	if !installed {
		return nil, "", facts.ProviderCensus{}, fmt.Sprintf("the Rubydex library is not installed at %s; run `%s`", path, rubydex.FetchHint)
	}
	lib, err := rubydex.Open(path)
	if err != nil {
		return nil, "", facts.ProviderCensus{}, err.Error()
	}
	result := rubydex.Collect(ctx, lib, repoPath, ignored)
	if result.Refusal != "" {
		return nil, "", facts.ProviderCensus{}, result.Refusal
	}
	return result.Facts, rubydex.Version, result.Census, ""
}
