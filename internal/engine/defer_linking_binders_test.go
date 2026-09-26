package engine_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/enola-labs/enola/pkg/bootstrap"
)

// TestDeferLinking_BindersEndInTheSameUnion is the union invariant with every plugin
// registered. TestDeferLinking_ClusterEndsInTheSameUnion holds it for the Go extractor
// alone, which runs no binders, so it could not see a binder that adds its output again
// on every pass: appending repositories one at a time duplicated the module edges the
// module-edges binder derives, one copy per append, while the deferred walk a cluster
// and repo_paths take derived them once. Every fact is compared, props included.
func TestDeferLinking_BindersEndInTheSameUnion(t *testing.T) {
	base := filepath.Join("testdata", "repos")
	repos := []string{
		filepath.Join(base, "ruby_sample"),
		filepath.Join(base, "ts_ember_sample"),
		filepath.Join(base, "swift_sample"),
		filepath.Join(base, "go_sample"),
	}

	walk := func(deferred bool) []string {
		eng, _, err := bootstrap.NewEngine(bootstrap.Options{
			ConfigPath: filepath.Join(t.TempDir(), "no-such-config.yaml"),
		})
		if err != nil {
			t.Fatalf("NewEngine: %v", err)
		}
		eng.SetPersistCache(false)
		for i, repo := range repos {
			eng.SetDeferLinking(deferred && i < len(repos)-1)
			if _, err := eng.GenerateSnapshot(context.Background(), repo, i > 0); err != nil {
				t.Fatalf("generate %s: %v", repo, err)
			}
		}
		out := make([]string, 0, eng.Store().Count())
		for _, f := range eng.Store().All() {
			b, err := json.Marshal(f)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, string(b))
		}
		sort.Strings(out)
		return out
	}

	eager, lazy := walk(false), walk(true)
	// The fixtures must exercise the binders that once duplicated their output, or
	// this passes by measuring nothing.
	for _, want := range []string{`"name":"module-edge: `, `"kind":"extraction","name":"ember:templates"`} {
		found := false
		for _, f := range lazy {
			if strings.Contains(f, want) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("no fact matching %s: the fixtures no longer exercise that binder", want)
		}
	}
	if len(eager) == len(lazy) {
		same := true
		for i := range eager {
			if eager[i] != lazy[i] {
				same = false
				break
			}
		}
		if same {
			return
		}
	}
	count := func(xs []string) map[string]int {
		m := map[string]int{}
		for _, x := range xs {
			m[x]++
		}
		return m
	}
	e, l := count(eager), count(lazy)
	shown := 0
	for k, n := range e {
		if l[k] != n && shown < 5 {
			t.Errorf("appending one at a time left %d of this fact, the deferred walk %d:\n%s", n, l[k], k)
			shown++
		}
	}
	for k, n := range l {
		if _, ok := e[k]; !ok && shown < 5 {
			t.Errorf("only the deferred walk has this fact (%d):\n%s", n, k)
			shown++
		}
	}
	t.Fatalf("facts differ: %d appending one at a time, %d deferred", len(eager), len(lazy))
}
