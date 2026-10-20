package plan

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// scanFanIn is the fan-in blastRadius read before the index: a ReverseLookup
// for every member and via, which on a store with no graph scans every fact.
// It stays here as the reference the index is held to.
func scanFanIn(store *facts.Store, members map[string]bool) map[string]bool {
	in := map[string]bool{}
	for name := range members {
		for _, via := range blastVias {
			for _, f := range store.ReverseLookup(name, via) {
				if !members[f.Name] {
					in[f.Name] = true
				}
			}
		}
	}
	return in
}

// awkwardFacts holds the shapes where a reading of the relations other than
// the scan's would disagree with it: a fact relating to one name twice and by
// two vias, relations the blast radius does not follow, storage access, a
// dependency fact importing inside a module (the graph adds an edge from the
// module), a call through a Go module path (the graph retargets it), a member
// calling a member, and names shared by several facts. And the files a target
// can name them by: a fact with no repository under the plan's label, one
// under a leading slash, and one repository's file under another's path.
func awkwardFacts() []facts.Fact {
	return []facts.Fact{
		{Kind: facts.KindModule, Name: "internal/server", File: "internal/server", Props: map[string]any{"modulePath": "example.com/app"}},
		{Kind: facts.KindModule, Name: "internal/config", File: "internal/config"},
		{Kind: facts.KindDependency, Name: "internal/server -> internal/config/load", File: "internal/server/server.go",
			Relations: []facts.Relation{{Kind: facts.RelImports, Target: "internal/config/load"}, {Kind: facts.RelImports, Target: "internal/config"}}},
		{Kind: facts.KindSymbol, Name: "config.Load", File: "internal/config/load.go"},
		{Kind: facts.KindSymbol, Name: "server.Start", File: "internal/server/server.go",
			Relations: []facts.Relation{
				{Kind: facts.RelCalls, Target: "config.Load"},
				{Kind: facts.RelCalls, Target: "config.Load"},
				{Kind: facts.RelDependsOn, Target: "config.Load"},
				{Kind: facts.RelCalls, Target: "example.com/app/internal/config.Load"},
				{Kind: facts.RelInstantiates, Target: "users"},
				{Kind: facts.RelDeclares, Target: "server.helper"},
			}},
		{Kind: facts.KindSymbol, Name: "server.helper", File: "internal/server/server.go",
			Relations: []facts.Relation{{Kind: facts.RelCalls, Target: "server.Start"}}},
		{Kind: facts.KindStorage, Name: "users", File: "db/schema.rb"},
		{Kind: facts.KindSymbol, Name: "User", File: "app/models/user.rb",
			Relations: []facts.Relation{{Kind: facts.RelInjects, Target: "users"}, {Kind: facts.RelImplements, Target: "Model"}}},
		{Kind: facts.KindSymbol, Name: "User", File: "app/models/admin/user.rb",
			Relations: []facts.Relation{{Kind: facts.RelCalls, Target: "config.Load"}}},
		{Kind: facts.KindSymbol, Name: "Model", File: "app/models/model.rb"},
		{Kind: facts.KindRoute, Name: "GET /users", File: "config/routes.rb",
			Relations: []facts.Relation{{Kind: facts.RelCalls, Target: "User"}, {Kind: facts.RelNames, Target: "User"}}},
		{Kind: facts.KindSymbol, Name: "", File: "orphan.rb", Relations: []facts.Relation{{Kind: facts.RelCalls, Target: ""}}},
		{Kind: facts.KindSymbol, Name: "Legacy", File: "app/lib/legacy.rb", Relations: []facts.Relation{{Kind: facts.RelCalls, Target: "config.Load"}}},
		{Kind: facts.KindDependency, Name: "app/lib/legacy.rb -> json", File: "app/lib/legacy.rb", Relations: []facts.Relation{{Kind: facts.RelImports, Target: "json"}}},
		{Kind: facts.KindSymbol, Name: "Rooted", File: "/rooted/path.rb", Relations: []facts.Relation{{Kind: facts.RelCalls, Target: "Legacy"}}},
		{Kind: facts.KindSymbol, Name: "Tagged", File: "shop/app/lib/legacy.rb", Repo: "shop", Relations: []facts.Relation{{Kind: facts.RelCalls, Target: "Legacy"}}},
	}
}

func storeOf(all []facts.Fact) *facts.Store {
	store := facts.NewStore()
	store.Add(all...)
	return store
}

// fanInStores is every store the index is held to: the awkward shapes, and the
// golden facts the engine's own tests pin for each language.
func fanInStores(t *testing.T) map[string]*facts.Store {
	t.Helper()
	stores := map[string]*facts.Store{"awkward": storeOf(awkwardFacts())}
	golden, err := filepath.Glob(filepath.Join("..", "..", "internal", "engine", "testdata", "golden", "*.facts.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(golden) == 0 {
		t.Fatal("no golden facts under internal/engine/testdata/golden")
	}
	for _, path := range golden {
		read := facts.NewStore()
		if err := read.ReadJSONLFile(path); err != nil {
			t.Fatal(err)
		}
		stores[filepath.Base(path)] = storeOf(read.All())
	}
	return stores
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestFanInIsWhatTheScanFound(t *testing.T) {
	for name, store := range fanInStores(t) {
		index := IndexFanIn(store)
		asked := 0
		check := func(what string, members map[string]bool) {
			t.Helper()
			asked++
			want, got := sortedKeys(scanFanIn(store, members)), sortedKeys(index.dependents(members))
			if strings.Join(want, "\n") != strings.Join(got, "\n") {
				t.Errorf("%s, %s: the index found %q, the scan %q", name, what, got, want)
			}
		}

		names := map[string]bool{}
		files := map[string]bool{}
		for _, f := range store.All() {
			names[f.Name] = true
			if f.File != "" && isMemberKind(f.Kind) {
				files[f.File] = true
			}
			for _, rel := range f.Relations {
				names[rel.Target] = true
			}
		}
		for n := range names {
			check(fmt.Sprintf("name %q", n), map[string]bool{n: true})
		}
		labels := append(store.RepoLabels(), "")
		for file := range files {
			for _, label := range labels {
				members := memberNamesFor(store, label, file, TargetPath)
				check(fmt.Sprintf("file %q under %q", file, label), members)
				if trimmed := strings.TrimPrefix(file, label+"/"); label != "" && trimmed != file {
					check(fmt.Sprintf("file %q under %q", trimmed, label), memberNamesFor(store, label, trimmed, TargetPath))
				}
			}
		}
		if asked == 0 {
			t.Errorf("%s: nothing was asked", name)
		}
	}
}

// scanMembers and scanBlastRadius are the rest of what a plan read by scanning
// before it read through indexes: every fact of each member kind, and every
// dependency fact, tested against the target's file. Kept as the reference.
func scanMembers(store *facts.Store, label, target, kind string) map[string]bool {
	names := map[string]bool{}
	if kind == TargetSymbol {
		for _, f := range store.LookupByExactName(target) {
			if isMemberKind(f.Kind) {
				names[f.Name] = true
			}
		}
		return names
	}
	for _, mk := range memberKinds {
		for _, f := range store.ByKind(mk) {
			if fileMatchesTarget(f, label, target) {
				names[f.Name] = true
			}
		}
	}
	return names
}

func scanBlastRadius(store *facts.Store, label, target, kind string) *BlastRadius {
	members := scanMembers(store, label, target, kind)
	if len(members) == 0 {
		return nil
	}
	out := map[string]bool{}
	collectOut := func(f facts.Fact) {
		for _, rel := range f.Relations {
			if isBlastVia(rel.Kind) && !members[rel.Target] {
				out[rel.Target] = true
			}
		}
	}
	for name := range members {
		for _, f := range store.LookupByExactName(name) {
			if isMemberKind(f.Kind) {
				collectOut(f)
			}
		}
	}
	if kind == TargetPath {
		for _, f := range store.ByKind(facts.KindDependency) {
			if fileMatchesTarget(f, label, target) {
				collectOut(f)
			}
		}
	}
	in := scanFanIn(store, members)
	br := &BlastRadius{FanIn: len(in), FanOut: len(out), In: capSorted(in, BlastSampleCap), Out: capSorted(out, BlastSampleCap)}
	br.Truncated = len(br.In) < br.FanIn || len(br.Out) < br.FanOut
	return br
}

// Every blast radius a plan reports is the one the scans computed, for every
// file and every name in every store, under every label a fact carries, none,
// and one no fact carries.
func TestBlastRadiusIsWhatTheScanFound(t *testing.T) {
	for name, store := range fanInStores(t) {
		files, names := map[string]bool{}, map[string]bool{}
		labelSet := map[string]bool{"": true, "elsewhere": true}
		for _, repo := range store.RepoLabels() {
			labelSet[repo] = true
		}
		for _, f := range store.All() {
			if f.File != "" {
				files[f.File] = true
				if i := strings.Index(f.File, "/"); i >= 0 {
					files[f.File[i+1:]] = true
					labelSet[f.File[:i]] = true
				}
			}
			names[f.Name] = true
		}
		labels := sortedKeys(labelSet)
		measured := 0
		for _, label := range labels {
			req := Request{Paths: sortedKeys(files)}
			if label == "" {
				req.Symbols = sortedKeys(names)
			}
			report, err := Compute(context.Background(), req, Deps{RepoLabel: label, Store: store})
			if err != nil {
				t.Fatalf("%s under %q: %v", name, label, err)
			}
			for _, tr := range report.Targets {
				want := scanBlastRadius(store, label, tr.Target, tr.Kind)
				if tr.Measured != (want != nil) || !reflect.DeepEqual(tr.BlastRadius, want) {
					t.Errorf("%s under %q, %s %q: reported %+v, the scans found %+v", name, label, tr.Kind, tr.Target, tr.BlastRadius, want)
				}
				if tr.Measured {
					measured++
				}
			}
		}
		if measured == 0 {
			t.Errorf("%s: no target was measured", name)
		}
	}
}

// wideStore is a store the size where the scan cost a plan most of its time: a
// file declaring members, each called from many places, among many facts that
// call something else.
func wideStore(size, members, callers int) *facts.Store {
	var all []facts.Fact
	for m := 0; m < members; m++ {
		all = append(all, facts.Fact{Kind: facts.KindSymbol, Name: fmt.Sprintf("Wide#m%d", m), File: "app/models/wide.rb"})
	}
	for i := 0; i < size; i++ {
		target := fmt.Sprintf("Other%d", i%997)
		if i < callers {
			target = fmt.Sprintf("Wide#m%d", i%members)
		}
		all = append(all, facts.Fact{
			Kind: facts.KindSymbol, Name: fmt.Sprintf("Caller%d", i), File: fmt.Sprintf("app/c%d.rb", i%5000),
			Relations: []facts.Relation{{Kind: facts.RelCalls, Target: target}, {Kind: facts.RelDependsOn, Target: "Base"}},
		})
	}
	return storeOf(all)
}

func BenchmarkBlastRadiusFanIn(b *testing.B) {
	store := wideStore(200_000, 150, 3_000)
	members := memberNamesFor(store, "app", "app/models/wide.rb", TargetPath)
	b.Run("scan", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			scanFanIn(store, members)
		}
	})
	b.Run("index-built-each-time", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			IndexFanIn(store).dependents(members)
		}
	})
	index := IndexFanIn(store)
	b.Run("index-kept", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			index.dependents(members)
		}
	})
}
