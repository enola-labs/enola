package engine_test

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// goldenFact is the part of a golden fact line this invariant reads.
type goldenFact struct {
	Kind  string         `json:"kind"`
	Name  string         `json:"name"`
	Repo  string         `json:"repo"`
	Props map[string]any `json:"props"`
}

// TestGoldens_EveryUnresolvedCallCarriesAReason holds the two HTTP passes to one
// account, over every committed golden.
//
// The linker counts a client call unresolved in its service's edge_coverage; a
// separate pass stamps unmatched_reason on the call sites it could not resolve. The
// counter and the stamps are different code, and where they disagree a reader sees an
// unresolved count with no call site to explain it, or a reason on a call that
// resolved. So: the calls carrying a reason other than attributed_by_intent must number
// exactly `unresolved`, and those attributed by intent exactly `declared`.
func TestGoldens_EveryUnresolvedCallCarriesAReason(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("testdata", "golden", "*.facts.jsonl"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no goldens found: %v", err)
	}
	checked := 0
	for _, path := range paths {
		name := strings.TrimSuffix(filepath.Base(path), ".facts.jsonl")
		t.Run(name, func(t *testing.T) {
			all := readGoldenFacts(t, path)

			type account struct{ unresolved, declared int }
			coverage := map[string]*account{}
			for _, f := range all {
				if f.Kind != facts.KindService {
					continue
				}
				for _, entry := range coverageEntries(f) {
					if entry["edge_type"] == "http_client" {
						coverage[f.Repo] = &account{unresolved: asInt(entry["unresolved"]), declared: asInt(entry["declared"])}
					}
				}
			}

			stamped := map[string]*account{}
			for _, f := range all {
				if f.Kind != facts.KindRoute || f.Repo == "" || f.Props["role"] != facts.RoleClient ||
					f.Props[facts.PropRouteType] == facts.RouteTypeGraphQL {
					continue
				}
				reason, _ := f.Props["unmatched_reason"].(string)
				if reason == "" {
					continue
				}
				if stamped[f.Repo] == nil {
					stamped[f.Repo] = &account{}
				}
				if reason == "attributed_by_intent" {
					stamped[f.Repo].declared++
				} else {
					stamped[f.Repo].unresolved++
				}
			}

			repos := map[string]bool{}
			for repo := range coverage {
				repos[repo] = true
			}
			for repo := range stamped {
				repos[repo] = true
			}
			names := make([]string, 0, len(repos))
			for repo := range repos {
				names = append(names, repo)
			}
			sort.Strings(names)
			for _, repo := range names {
				want, got := coverage[repo], stamped[repo]
				if want == nil {
					want = &account{}
				}
				if got == nil {
					got = &account{}
				}
				checked++
				if want.unresolved != got.unresolved {
					t.Errorf("repo %s: edge_coverage unresolved=%d, but %d client call(s) carry a reason explaining it",
						repo, want.unresolved, got.unresolved)
				}
				if want.declared != got.declared {
					t.Errorf("repo %s: edge_coverage declared=%d, but %d client call(s) carry attributed_by_intent",
						repo, want.declared, got.declared)
				}
			}
		})
	}
	if checked == 0 {
		t.Fatal("no golden carries HTTP coverage: the invariant checked nothing")
	}
}

func readGoldenFacts(t *testing.T, path string) []goldenFact {
	t.Helper()
	fh, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fh.Close() }()
	var out []goldenFact
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 4<<20), 4<<20)
	for sc.Scan() {
		var f goldenFact
		if err := json.Unmarshal(sc.Bytes(), &f); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		out = append(out, f)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func coverageEntries(f goldenFact) []map[string]any {
	raw, _ := f.Props["edge_coverage"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func asInt(v any) int {
	if n, ok := v.(float64); ok {
		return int(n)
	}
	return 0
}
