package dartextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func TestDartBundledSDKSignatures(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{
		"lib/repo.dart": ` class Repo {void load() {}}
 void first(List<Repo> xs) {xs.first.load();}
 void mapped(List<Repo> xs) {xs.map((x) => x).toList();}
 Future<void> awaited(Future<Repo> f) async {final r = await f; r.load();}
 void keyed(Map<String, Repo> m) {m.values.first.load();}
 void text(String s) {s.trim().toLowerCase();}
 class List2 {void add(int x) {}}
 void shadow(List2 l) {l.add(1);}
 `})
	for name, want := range map[string]string{
		"lib.first":   "sdk/lib/core.Iterable.first",
		"lib.awaited": "lib.Repo.load",
		"lib.mapped":  "sdk/lib/core.Iterable.map",
		"lib.text":    "sdk/lib/core.String.toLowerCase",
		"lib.shadow":  "lib.List2.add",
	} {
		if f := findFact(all, facts.KindSymbol, name); !hasReviewCall(f, want) {
			t.Errorf("%s: want call to %s: %+v", name, want, f)
		}
	}
	for _, f := range all {
		if f.File == "sdk/lib/core/core.dart" {
			t.Fatalf("bundled SDK fact leaked into the output: %s", f.Name)
		}
	}
}
