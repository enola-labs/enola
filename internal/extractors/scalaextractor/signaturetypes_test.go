package scalaextractor

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func scalaRepo(t *testing.T, files map[string]string) []facts.Fact {
	t.Helper()
	dir := t.TempDir()
	var rel []string
	for name, src := range files {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		rel = append(rel, name)
	}
	slices.Sort(rel)
	ff, err := New().Extract(context.Background(), dir, rel)
	if err != nil {
		t.Fatal(err)
	}
	return ff
}

// A type a signature or a field names is used by the declaration: a config case
// class a function takes, the type of a field, imported or declared in the same
// package. A type the repository does not declare (String, Option) adds nothing.
func TestSignatureTypes_AreUsedByTheirDeclaration(t *testing.T) {
	ff := scalaRepo(t, map[string]string{
		"src/main/scala/shop/config/package.scala": `package shop.config

case class StoreConf(path: String)
case class AppConf(store: Option[StoreConf])
`,
		"src/main/scala/shop/Loader.scala": `package shop

import shop.config.StoreConf

object Loader {
  def load(conf: StoreConf): Map[String, Int] = Map.empty
}
`,
	})
	names := func(suffix string) []string {
		var out []string
		for _, f := range ff {
			if f.Kind == facts.KindSymbol && strings.HasSuffix(f.Name, suffix) {
				for _, r := range f.Relations {
					if r.Kind == facts.RelNames {
						out = append(out, r.Target)
					}
				}
			}
		}
		return out
	}
	if got := names("Loader.load"); !slices.ContainsFunc(got, func(s string) bool { return strings.HasSuffix(s, ".StoreConf") }) {
		t.Errorf("load names %v, want the imported StoreConf", got)
	}
	if got := names(".AppConf"); !slices.ContainsFunc(got, func(s string) bool { return strings.HasSuffix(s, ".StoreConf") }) {
		t.Errorf("AppConf names %v, want StoreConf from its own package", got)
	}
	for _, f := range ff {
		for _, r := range f.Relations {
			if r.Kind == facts.RelNames && !strings.Contains(r.Target, ".") {
				t.Errorf("%s names an unresolved %q", f.Name, r.Target)
			}
		}
	}
}
