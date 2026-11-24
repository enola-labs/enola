package engine_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/config"
	"github.com/enola-labs/enola/internal/engine"
	"github.com/enola-labs/enola/internal/extractors/cppextractor"
	"github.com/enola-labs/enola/internal/facts"
)

// TestTestAwareExtractorSeesTestsAndKeysTheCacheOnThem pins the engine side of
// plugin.TestAwareExtractor. The C and C++ extractor gets the ignored test files in
// its production extraction and returns their test refs with it, and because that
// output is cached, editing only a test file must miss the cache: a key over the
// production files alone would serve the old test refs.
func TestTestAwareExtractorSeesTestsAndKeysTheCacheOnThem(t *testing.T) {
	repo := t.TempDir()
	writeFile(t, filepath.Join(repo, "src", "util.h"), "int checksum(int x);\nint other(int x);\n")
	writeFile(t, filepath.Join(repo, "src", "util.c"), "#include \"util.h\"\nint checksum(int x) { return x; }\nint other(int x) { return x; }\n")
	writeFile(t, filepath.Join(repo, "test", "util_test.c"), "#include \"../src/util.h\"\nvoid run(void) { checksum(1); }\n")

	refs := func() map[string]bool {
		t.Helper()
		eng, err := engine.New(config.Default())
		if err != nil {
			t.Fatal(err)
		}
		eng.RegisterExtractor(cppextractor.New())
		if _, err := eng.GenerateSnapshot(context.Background(), repo, false); err != nil {
			t.Fatal(err)
		}
		if err := eng.WriteArtifacts(repo); err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, f := range eng.Store().FactsRef() {
			if f.Kind == facts.KindSymbol && f.File == "test/util_test.c" {
				t.Errorf("test file indexed as code: %s", f.Name)
			}
			if f.Kind == facts.KindTestRef && f.File == "test/util_test.c" {
				for _, r := range f.Relations {
					out[r.Target] = true
				}
			}
		}
		return out
	}

	if got := refs(); !got["src.checksum"] {
		t.Fatalf("first snapshot: test refs %v, want src.checksum", got)
	}
	writeFile(t, filepath.Join(repo, "test", "util_test.c"), "#include \"../src/util.h\"\nvoid run(void) { other(1); }\n")
	if got := refs(); !got["src.other"] || got["src.checksum"] {
		t.Fatalf("after editing only the test: test refs %v, want src.other alone", got)
	}
}
