package rubyextractor

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// tree-sitter-ruby has no rule for a symbol naming one of the special
// globals spelt with punctuation (`:$"`, `:$'`), so the array holding it leaves
// an error node. Recovery keeps the method after it here, but where an error
// node falls decides what a file loses, so the extractor cannot vouch for a file
// holding one; it says which files it read only in part, which is what the
// receipt's parse errors are for.
const specialGlobalSymbol = "module LoadPathHelper\n" +
	"  def globals\n" +
	"    [:$\"]\n" +
	"  end\n" +
	"\n" +
	"  def after_the_error\n" +
	"  end\n" +
	"end\n"

func TestExtract_NamesTheFilesTheParserLeftAnErrorIn(t *testing.T) {
	repo := t.TempDir()
	files := map[string]string{
		"app/helpers/load_path_helper.rb": specialGlobalSymbol,
		"app/models/board.rb":             "class Board\n  def title; end\nend\n",
	}
	var rel []string
	for name, body := range files {
		path := filepath.Join(repo, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		rel = append(rel, name)
	}
	out, err := New().Extract(context.Background(), repo, rel)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range out {
		if f.Kind != facts.KindExtraction || f.Name != "ruby:parse" {
			continue
		}
		if got := f.PropAny(facts.PropParsedWithErrors); !reflect.DeepEqual(got, []string{"app/helpers/load_path_helper.rb"}) {
			t.Fatalf("%s = %v, want the one file with an error node", facts.PropParsedWithErrors, got)
		}
		coverage := f.PropAny("edge_coverage").([]map[string]any)[0]
		if coverage["resolved"] != 1 || coverage["unresolved"] != 1 {
			t.Fatalf("edge_coverage = %v, want one file read whole and one in part", coverage)
		}
		return
	}
	t.Fatal("no ruby:parse extraction fact: a Ruby repository says how many of its files it read whole")
}
