package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// The provider seam runs its providers concurrently, and each one it hands the
// cache writes through the same spool: a per-file provider puts one entry per
// file it computed while the built-in Rubydex provider puts its whole index and
// reads its meta entry. Two goroutines on one buffered writer interleave their
// bytes, and on forem a generate with prism per-file beside the built-in
// panicked in put 2 runs of 6. This test is that shape without the providers:
// two writers and a reader through providerCache at once. Under -race it fails
// on the unlocked spool; without it, the saved file stops parsing or loses
// entries often enough to fail too.
func TestProviderCache_ConcurrentWritersKeepTheSpoolWhole(t *testing.T) {
	path := filepath.Join(t.TempDir(), "extractor_cache.json")
	seed := loadExtractorCache(path, true)
	seed.put(providerCache{c: seed}.key("index-meta\x00rubydex"), []facts.Fact{{Kind: "provider-index", Name: "rubydex"}})
	if err := seed.save(); err != nil {
		t.Fatal(err)
	}

	c := loadExtractorCache(path, true)
	view := providerCache{c: c}
	const perWriter = 400
	var wg sync.WaitGroup
	for writer := 0; writer < 2; writer++ {
		wg.Add(1)
		go func(writer int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				name := fmt.Sprintf("w%d-%03d", writer, i)
				view.Put("file\x00"+name, []facts.Fact{{Kind: facts.KindDependency, Name: name, File: name + ".rb"}})
			}
		}(writer)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		view.Peek("index-meta\x00rubydex")
		view.Get("index-meta\x00rubydex")
	}()
	wg.Wait()
	if err := c.save(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var parsed cacheFile
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("two providers writing at once left a cache that does not parse: %v", err)
	}
	if want := 2*perWriter + 1; len(parsed.Entries) != want {
		t.Fatalf("the cache holds %d entries, want %d: concurrent writes lost or merged entries", len(parsed.Entries), want)
	}
	for writer := 0; writer < 2; writer++ {
		for i := 0; i < perWriter; i++ {
			name := fmt.Sprintf("w%d-%03d", writer, i)
			raw, ok := parsed.Entries[view.key("file\x00"+name)]
			if !ok {
				t.Fatalf("entry %s is missing", name)
			}
			var ff []facts.Fact
			if err := json.Unmarshal(raw, &ff); err != nil || len(ff) != 1 || ff[0].Name != name {
				t.Fatalf("entry %s holds %s, want its own one fact", name, raw)
			}
		}
	}
}
