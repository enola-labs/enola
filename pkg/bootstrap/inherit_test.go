package bootstrap_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/flows"
)

// The same program in every language with classes (testdata/inherit): a Base whose
// run() calls self.step() and whose common() calls self.helper.work(), a Concrete
// overriding step() and calling common(), an Other overriding step() too, and a main
// running c.run() and c.handle() on a Concrete. The benchmark harness grades the same
// fixtures (enola-benchmarks, bench-inherit.py); this keeps them true in CI.

// inheritNames spells each fixture's symbols the way its extractor names them.
var inheritNames = map[string]map[string]string{
	"python": {"main": "app/main.main", "Base.run": "app/lib.Base.run", "Base.common": "app/lib.Base.common", "Concrete.step": "app/main.Concrete.step", "Concrete.handle": "app/main.Concrete.handle", "Other.step": "app/other.Other.step"},
	"ts":     {"main": "src.main", "Base.run": "src.Base.run", "Base.common": "src.Base.common", "Concrete.step": "src.Concrete.step", "Concrete.handle": "src.Concrete.handle", "Other.step": "src.Other.step"},
	"java":   {"main": "src/main/java/app.Concrete.main", "Base.run": "src/main/java/app.Base.run", "Base.common": "src/main/java/app.Base.common", "Concrete.step": "src/main/java/app.Concrete.step", "Concrete.handle": "src/main/java/app.Concrete.handle", "Other.step": "src/main/java/app.Other.step"},
	"kotlin": {"main": "src/main/kotlin/app.main", "Base.run": "src/main/kotlin/app.Base.run", "Base.common": "src/main/kotlin/app.Base.common", "Concrete.step": "src/main/kotlin/app.Concrete.step", "Concrete.handle": "src/main/kotlin/app.Concrete.handle", "Other.step": "src/main/kotlin/app.Other.step"},
	"csharp": {"main": "App.Program.Main", "Base.run": "App.Base.Run", "Base.common": "App.Base.Common", "Concrete.step": "App.Concrete.Step", "Concrete.handle": "App.Concrete.Handle", "Other.step": "App.Other.Step"},
	"php":    {"main": "main", "Base.run": `App\Base::run`, "Base.common": `App\Base::common`, "Concrete.step": `App\Concrete::step`, "Concrete.handle": `App\Concrete::handle`, "Other.step": `App\Other::step`},
	"ruby":   {"main": "..main", "Base.run": "Base#run", "Base.common": "Base#common", "Concrete.step": "Concrete#step", "Concrete.handle": "Concrete#handle", "Other.step": "Other#step"},
	"scala":  {"main": "src/main/scala/app.Main.main", "Base.run": "src/main/scala/app.Base.run", "Base.common": "src/main/scala/app.Base.common", "Concrete.step": "src/main/scala/app.Concrete.step", "Concrete.handle": "src/main/scala/app.Concrete.handle", "Other.step": "src/main/scala/app.Other.step"},
	"dart":   {"main": "bin.main", "Base.run": "lib.Base.run", "Base.common": "lib.Base.common", "Concrete.step": "bin.Concrete.step", "Concrete.handle": "bin.Concrete.handle", "Other.step": "lib.Other.step"},
	"swift":  {"main": "Sources/App.AppMain.main", "Base.run": "Sources/App.Base.run", "Base.common": "Sources/App.Base.common", "Concrete.step": "Sources/App.Concrete.step", "Concrete.handle": "Sources/App.Concrete.handle", "Other.step": "Sources/App.Other.step"},
	"cpp":    {"main": "src.main", "Base.run": "src.Base::run", "Base.common": "src.Base::common", "Concrete.step": "src.Concrete::step", "Concrete.handle": "src.Concrete::handle", "Other.step": "src.Other::step"},
	"rust":   {"main": "src.main", "Base.run": "src.Base.run", "Base.common": "src.Base.common", "Concrete.step": "src.Concrete.step", "Concrete.handle": "src.Concrete.handle", "Other.step": "src.Other.step"},
}

// Calls through inheritance resolve in every language: c.run() on a Concrete reaches
// the Base.run it inherits (I1), a self-call to an inherited member reaches it (I2),
// main's flow runs Concrete's override of step from inside Base.run (O), and not
// Other's (OX). Dart tracks no receiver types, so the subtype c.run() went through is
// unknown there and an abstract step runs every override: OX is not asserted for it.
func TestInheritedCallsResolvePerLanguage(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for lang, names := range inheritNames {
		t.Run(lang, func(t *testing.T) {
			eng := newEngine(t)
			repo := copyFixture(t, filepath.Join("testdata", "inherit", lang))
			if _, err := eng.GenerateSnapshot(context.Background(), repo, false); err != nil {
				t.Fatalf("GenerateSnapshot: %v", err)
			}
			calls := func(from string) []string {
				var out []string
				for _, f := range eng.Store().LookupByExactName(from) {
					for _, r := range f.Relations {
						if r.Kind == facts.RelCalls {
							out = append(out, r.Target)
						}
					}
				}
				return out
			}
			if got := calls(names["main"]); !slices.Contains(got, names["Base.run"]) {
				t.Errorf("I1: main calls %v, want %s", got, names["Base.run"])
			}
			if got := calls(names["Concrete.handle"]); !slices.Contains(got, names["Base.common"]) {
				t.Errorf("I2: Concrete.handle calls %v, want %s", got, names["Base.common"])
			}
			a := flows.Analyze(eng.Store(), flows.Options{})
			reaches := func(symbol string) bool {
				ids, _ := a.Through(symbol)
				for _, i := range ids {
					if a.Flows[i].Entry.Name == names["main"] {
						return true
					}
				}
				return false
			}
			if !reaches(names["Concrete.step"]) {
				t.Errorf("O: main's flow does not reach %s", names["Concrete.step"])
			}
			if lang != "dart" && reaches(names["Other.step"]) {
				t.Errorf("OX: main's flow reaches %s", names["Other.step"])
			}
		})
	}
}

// copyFixture copies a testdata tree into a temporary repository, so a snapshot
// writes its .enola there and not into the source tree.
func copyFixture(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}
