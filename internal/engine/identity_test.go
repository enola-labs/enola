package engine_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A declaration keeps its id indexed alone, at any place in a cluster and when indexed again,
// including in a repository holding a directory of its own name, whose path alone looks already
// prefixed. Identical source in two repositories is still two declarations.
func TestIdentity_SameAloneAndAtAnyPlaceInACluster(t *testing.T) {
	root := t.TempDir()
	write := func(path, src string) {
		t.Helper()
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("alpha/alpha/code.go", "package alpha\n\nfunc AlphaFn() {}\n")
	write("beta/beta/code.go", "package beta\n\nfunc BetaFn() {}\n")
	for _, r := range []string{"alpha", "beta"} {
		write(r+"/shared/same.go", "package shared\n\nfunc Same() {}\n")
	}
	alpha, beta := filepath.Join(root, "alpha"), filepath.Join(root, "beta")

	symbolIn := func(t *testing.T, s *facts.Store, repo, suffix string) facts.Fact {
		t.Helper()
		for _, f := range s.All() {
			if f.Kind == facts.KindSymbol && strings.HasSuffix(f.Name, suffix) && (repo == "" || f.Repo == repo) {
				return f
			}
		}
		t.Fatalf("no symbol %s in %q", suffix, repo)
		return facts.Fact{}
	}
	index := func(t *testing.T, repos ...string) *facts.Store {
		t.Helper()
		eng := freezeTestEngine(t)
		for i, r := range repos {
			if _, err := eng.GenerateSnapshot(context.Background(), r, i > 0); err != nil {
				t.Fatal(err)
			}
		}
		return eng.Store()
	}

	symbol := func(t *testing.T, s *facts.Store, suffix string) facts.Fact {
		t.Helper()
		return symbolIn(t, s, "", suffix)
	}

	soloA, soloB := index(t, alpha), index(t, beta)
	aloneA, aloneB := symbol(t, soloA, "AlphaFn"), symbol(t, soloB, "BetaFn")
	sameA, sameB := symbol(t, soloA, "Same"), symbol(t, soloB, "Same")
	if aloneB.File != "beta/code.go" || aloneB.RepoPrefixed {
		t.Fatalf("alone: %+v", aloneB)
	}
	if sameA.Identity() == sameB.Identity() {
		t.Errorf("identical source in alpha and beta shares the id %s", sameA.Identity())
	}
	if again := symbol(t, index(t, beta), "BetaFn"); again.Identity() != aloneB.Identity() {
		t.Errorf("indexed again: %s, was %s", again.Identity(), aloneB.Identity())
	}
	for name, repos := range map[string][]string{"alpha first": {alpha, beta}, "beta first": {beta, alpha}} {
		t.Run(name, func(t *testing.T) {
			s := index(t, repos...)
			a, b := symbol(t, s, "AlphaFn"), symbol(t, s, "BetaFn")
			if a.Identity() != aloneA.Identity() || b.Identity() != aloneB.Identity() {
				t.Errorf("ids changed: alpha %s, was %s; beta %s, was %s", a.Identity(), aloneA.Identity(), b.Identity(), aloneB.Identity())
			}
			if a.File != "alpha/alpha/code.go" || b.File != "beta/beta/code.go" {
				t.Errorf("display paths %s, %s", a.File, b.File)
			}
			if symbolIn(t, s, "alpha", "Same").Identity() != sameA.Identity() || symbolIn(t, s, "beta", "Same").Identity() != sameB.Identity() {
				t.Error("the identical declarations in alpha and beta changed ids in the cluster")
			}

			// The file has to carry it too: a union read back from disk has the same ids.
			var buf bytes.Buffer
			if err := s.WriteJSONL(&buf); err != nil {
				t.Fatal(err)
			}
			found := false
			for _, line := range bytes.Split(buf.Bytes(), []byte("\n")) {
				var w struct {
					Name         string `json:"name"`
					ID           string `json:"id"`
					RepoPrefixed bool   `json:"repo_prefixed"`
				}
				if len(line) == 0 {
					continue
				}
				if err := json.Unmarshal(line, &w); err != nil {
					t.Fatal(err)
				}
				if strings.HasSuffix(w.Name, "BetaFn") {
					found = true
					if w.ID != aloneB.Identity() || !w.RepoPrefixed {
						t.Errorf("written %+v", w)
					}
				}
			}
			if !found {
				t.Error("BetaFn was not written")
			}
			back := facts.NewStore()
			if err := back.ReadJSONL(bytes.NewReader(buf.Bytes())); err != nil {
				t.Fatal(err)
			}
			if got := symbol(t, back, "BetaFn"); got.Identity() != aloneB.Identity() {
				t.Errorf("read back: %s", got.Identity())
			}
		})
	}
}
