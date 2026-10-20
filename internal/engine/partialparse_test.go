package engine_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/pkg/bootstrap"
)

// A file the Ruby reader read only in part is a parse error in the receipt,
// named by file, on a cold run and on a warm one that reuses the extractor's
// cached facts alike.
func TestGenerate_ARubyFileReadInPartIsAParseError(t *testing.T) {
	repo := t.TempDir()
	for name, body := range map[string]string{
		"Gemfile":                         "source \"https://rubygems.org\"\n",
		"app/helpers/load_path_helper.rb": "module LoadPathHelper\n  def globals\n    [:$\"]\n  end\nend\n",
		"app/models/board.rb":             "class Board\n  def title; end\nend\n",
	} {
		path := filepath.Join(repo, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, run := range []string{"cold", "warm"} {
		eng, _, err := bootstrap.NewEngine(bootstrap.Options{ConfigPath: filepath.Join(t.TempDir(), "no-such-config.yaml")})
		if err != nil {
			t.Fatal(err)
		}
		snapshot, err := eng.GenerateSnapshot(context.Background(), repo, false)
		if err != nil {
			t.Fatal(err)
		}
		meta := snapshot.Meta
		if meta.ParseErrors != 1 || len(meta.ParseErrorSample) != 1 ||
			meta.ParseErrorSample[0].File != "app/helpers/load_path_helper.rb" || meta.ParseErrorSample[0].Extractor != "ruby" {
			t.Fatalf("%s run: parse_errors %d, sample %+v, want the one file read in part", run, meta.ParseErrors, meta.ParseErrorSample)
		}
	}
}
