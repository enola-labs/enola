package providers

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// memoryCache is the seam's view of the engine cache, held in a map: Get and
// Peek read, Put stores, nothing is carried forward or dropped, which is the
// part of the engine's contract these tests do not exercise.
type memoryCache struct{ entries map[string][]facts.Fact }

func newMemoryCache() *memoryCache { return &memoryCache{entries: map[string][]facts.Fact{}} }

func (m *memoryCache) Get(key string) ([]facts.Fact, bool) {
	ff, ok := m.entries[key]
	return ff, ok
}

func (m *memoryCache) Peek(key string) ([]facts.Fact, bool) { return m.Get(key) }

func (m *memoryCache) Put(key string, ff []facts.Fact) { m.entries[key] = ff }

// writePerFileProvider writes a provider that reads the listing the seam hands
// it and emits one symbol fact per listed file, plus, when extra is set, a fact
// about a file it was never handed. Every run appends a line to marker, so a
// test can tell whether the provider ran at all.
func writePerFileProvider(t *testing.T, marker, extra string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "per-file-provider")
	script := "#!/bin/sh\n" +
		"for a in \"$@\"; do if [ \"$a\" = \"--version\" ]; then echo 1.0.0; exit 0; fi; done\n" +
		"echo ran >> " + marker + "\n" +
		"listing=\"\"\n" +
		"while [ $# -gt 0 ]; do if [ \"$1\" = \"--files\" ]; then listing=\"$2\"; fi; shift; done\n" +
		"[ -n \"$listing\" ] || { echo 'no listing' >&2; exit 1; }\n" +
		"while read -r f; do\n" +
		"  printf '{\"kind\":\"symbol\",\"name\":\"sym:%s\",\"file\":\"%s\",\"props\":{\"resolution_level\":\"name-only\"}}\\n' \"$f\" \"$f\"\n" +
		"done < \"$listing\"\n"
	if extra != "" {
		script += "printf '{\"kind\":\"symbol\",\"name\":\"sym:%s\",\"file\":\"%s\",\"props\":{\"resolution_level\":\"name-only\"}}\\n' " + extra + " " + extra + "\n"
	}
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func perFileInput(script string, cache Cache, files []string, hashes map[string]string) Input {
	return Input{
		Providers: []Provider{{Name: "fake", Command: []string{script}, Files: FilesPerFile, Extensions: []string{".rb"}}},
		RepoPath:  os.TempDir(),
		Files:     files,
		Hashes:    hashes,
		Cache:     cache,
	}
}

func runsRecorded(t *testing.T, marker string) int {
	t.Helper()
	data, err := os.ReadFile(marker)
	if err != nil {
		return 0
	}
	return strings.Count(string(data), "ran")
}

func TestRunWith_PerFileReusesUnchangedFiles(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "runs")
	script := writePerFileProvider(t, marker, "")
	cache := newMemoryCache()
	files := []string{"app/a.rb", "app/b.rb", "app/c.js"}
	hashes := map[string]string{"app/a.rb": "aaa", "app/b.rb": "bbb", "app/c.js": "ccc"}

	first, records := RunWith(context.Background(), perFileInput(script, cache, files, hashes))
	if len(first) != 2 {
		t.Fatalf("cold run facts = %d, want one per Ruby file: %+v", len(first), first)
	}
	if r := records[0].Reuse; r == nil || r.Reused != 0 || r.Computed != 2 || r.FilesComputed != 2 {
		t.Fatalf("cold run reuse = %+v, want computed 2 over 2 files", records[0].Reuse)
	}

	second, records := RunWith(context.Background(), perFileInput(script, cache, files, hashes))
	if runsRecorded(t, marker) != 1 {
		t.Fatalf("warm run invoked the provider; a fully reused provider must not run")
	}
	if r := records[0].Reuse; r == nil || r.Reused != 2 || r.Computed != 0 || r.FilesReused != 2 {
		t.Fatalf("warm run reuse = %+v, want reused 2 over 2 files", records[0].Reuse)
	}
	if len(second) != len(first) || second[0].Name != first[0].Name || second[1].Name != first[1].Name {
		t.Fatalf("warm facts %+v differ from cold %+v", second, first)
	}

	hashes["app/b.rb"] = "bbb2"
	third, records := RunWith(context.Background(), perFileInput(script, cache, files, hashes))
	if runsRecorded(t, marker) != 2 {
		t.Fatalf("an edited file must run the provider once more")
	}
	if r := records[0].Reuse; r == nil || r.Reused != 1 || r.Computed != 1 || r.FilesComputed != 1 {
		t.Fatalf("edited run reuse = %+v, want one reused and one computed", records[0].Reuse)
	}
	if len(third) != 2 {
		t.Fatalf("edited run facts = %+v", third)
	}
}

func TestRunWith_PerFileDropsFactsOutsideTheListing(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "runs")
	script := writePerFileProvider(t, marker, "lib/elsewhere.rb")
	cache := newMemoryCache()
	files := []string{"app/a.rb", "lib/elsewhere.rb"}
	hashes := map[string]string{"app/a.rb": "aaa", "lib/elsewhere.rb": "eee"}

	first, records := RunWith(context.Background(), perFileInput(script, cache, files, hashes))
	if r := records[0].Reuse; r == nil || r.OutsideScope != 0 {
		t.Fatalf("cold run over every file has nothing outside scope: %+v", records[0].Reuse)
	}
	if len(first) != 3 {
		t.Fatalf("cold run facts = %d, want both files plus the duplicate the provider adds: %+v", len(first), first)
	}

	hashes["app/a.rb"] = "aaa2"
	second, records := RunWith(context.Background(), perFileInput(script, cache, files, hashes))
	if r := records[0].Reuse; r == nil || r.OutsideScope != 1 {
		t.Fatalf("a fact about a file the provider was not handed must be dropped and counted, got %+v", records[0].Reuse)
	}
	for _, f := range second {
		if f.File == "lib/elsewhere.rb" && strings.Contains(f.Name, "elsewhere") && f.Props[PropProvider] == nil {
			t.Fatalf("unstamped fact leaked: %+v", f)
		}
	}
	if len(second) != 3 {
		t.Fatalf("warm run facts = %d: the reused elsewhere entry plus the recomputed a.rb, never the out-of-scope duplicate: %+v", len(second), second)
	}
}
