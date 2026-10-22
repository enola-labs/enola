package impact

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/resolve"
)

func TestCanonicalTarget_FileRefUsesExtensionlessDependencyNode(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		facts.Fact{Kind: facts.KindFileRef, Name: "ui/src/api/api.ts", File: "ui/src/api/api.ts"},
		facts.Fact{Kind: facts.KindDependency, Name: "ui/src/page -> ui/src/api/api", Relations: []facts.Relation{
			{Kind: facts.RelImports, Target: "ui/src/api/api"},
		}},
	)
	got, res := canonicalTarget(store, "ui/src/api/api.ts")
	if got != "ui/src/api/api" {
		t.Fatalf("canonical target = %q", got)
	}
	if res == nil || res.Query != "ui/src/api/api.ts" || res.Matched != got {
		t.Fatalf("normalization was not disclosed: %+v", res)
	}
}

func TestCanonicalTarget_GenuineFileRefStaysPut(t *testing.T) {
	store := facts.NewStore()
	store.Add(facts.Fact{Kind: facts.KindFileRef, Name: "ui/src/setup.ts", File: "ui/src/setup.ts"})
	got, res := canonicalTarget(store, "ui/src/setup.ts")
	if got != "ui/src/setup.ts" || res != nil {
		t.Fatalf("got %q, %+v", got, res)
	}
}

func fileStore() *facts.Store {
	s := facts.NewStore()
	s.Add(
		facts.Fact{Kind: facts.KindSymbol, Name: "pkg/a.Alpha", Repo: "shop", File: "pkg/a/a.go", Line: 3},
		// Beta calls Alpha from the same file: part of the file, not a dependent of it.
		facts.Fact{Kind: facts.KindSymbol, Name: "pkg/a.Beta", Repo: "shop", File: "pkg/a/a.go", Line: 9,
			Relations: []facts.Relation{{Kind: facts.RelCalls, Target: "pkg/a.Alpha"}}},
		facts.Fact{Kind: facts.KindSymbol, Name: "pkg/b.UsesAlpha", Repo: "shop", File: "pkg/b/b.go", Line: 4,
			Relations: []facts.Relation{{Kind: facts.RelCalls, Target: "pkg/a.Alpha"}}},
		facts.Fact{Kind: facts.KindSymbol, Name: "pkg/c.UsesBeta", Repo: "shop", File: "pkg/c/c.go", Line: 4,
			Relations: []facts.Relation{{Kind: facts.RelCalls, Target: "pkg/a.Beta"}}},
		facts.Fact{Kind: facts.KindDependency, Name: "pkg/d -> fmt", Repo: "shop", File: "pkg/d/d.go",
			Relations: []facts.Relation{{Kind: facts.RelImports, Target: "fmt"}}},
	)
	s.BuildGraph()
	return s
}

// A source file is not a node. Asked about one, the answer is the impact of the
// symbols it declares, taken together, and it says so.
func TestAnalyze_FilePathIsTheSymbolsItDeclares(t *testing.T) {
	const root = "/work/shop"
	r := resolve.Resolver{Store: fileStore(), RepoPath: root}
	for _, target := range []string{"pkg/a/a.go", root + "/pkg/a/a.go"} {
		report, err := Analyze(r, Request{Target: target})
		if err != nil {
			t.Fatalf("Analyze(%q): %v", target, err)
		}
		if !report.Resolved() || report.Target != "pkg/a/a.go" || report.Resolution.Matched != "pkg/a/a.go" {
			t.Errorf("Analyze(%q): target %q, resolution %+v; want the file", target, report.Target, report.Resolution)
		}
		if got := strings.Join(report.Seeds, ","); got != "pkg/a.Alpha,pkg/a.Beta" {
			t.Errorf("Analyze(%q): seeds = %s", target, got)
		}
		var deps []string
		for _, n := range report.ByDepth[1] {
			deps = append(deps, n.Name)
		}
		if got := strings.Join(deps, ","); got != "pkg/b.UsesAlpha,pkg/c.UsesBeta" || report.TotalDependents != 2 {
			t.Errorf("Analyze(%q): dependents %s, total %d; want the two callers outside the file", target, got, report.TotalDependents)
		}
	}
}

// A file with no symbol has nothing to walk back from, and a path to no file is
// still an unknown name: neither becomes an empty answer.
func TestAnalyze_FilePathWithoutSymbolsStaysAnError(t *testing.T) {
	r := resolve.Resolver{Store: fileStore()}
	for _, target := range []string{"pkg/d/d.go", "pkg/a/missing.go"} {
		if report, err := Analyze(r, Request{Target: target}); err == nil {
			t.Errorf("Analyze(%q) = %+v; want an error", target, report)
		}
	}
}

// namespaceStore is compactly nested Ruby: no file declares Shop on its own, so the
// only node named Shop is a generated stub.
func namespaceStore() *facts.Store {
	sym := func(name, file, kind string, props map[string]any, calls ...string) facts.Fact {
		p := map[string]any{"symbol_kind": kind, "language": "ruby"}
		for k, v := range props {
			p[k] = v
		}
		f := facts.Fact{Kind: facts.KindSymbol, Name: name, Repo: "mono", File: file, Line: 1, Props: p}
		for _, c := range calls {
			f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelCalls, Target: c})
		}
		return f
	}
	generated := map[string]any{"generated": true}
	s := facts.NewStore()
	s.Add(
		sym("Shop", "mono/config/stubs/modules.rb", facts.SymbolInterface, generated),
		sym("Shop::Orders", "mono/config/stubs/modules.rb", facts.SymbolInterface, generated),
		sym("Shop::Orders::Facade", "mono/packs/shop/app/public/shop/orders/facade.rb", facts.SymbolInterface, nil),
		sym("Shop::Orders::Facade.place", "mono/packs/shop/app/public/shop/orders/facade.rb", facts.SymbolFunc, nil, "Shop::Orders::Place.for"),
		sym("Shop::Orders::Place", "mono/packs/shop/app/domain/shop/orders/place.rb", facts.SymbolClass, nil),
		sym("Shop::Orders::Place.for", "mono/packs/shop/app/domain/shop/orders/place.rb", facts.SymbolFunc, nil),
		// Callers outside the namespace: one through the facade, one straight to a class.
		sym("Billing::Charge#run", "mono/packs/billing/app/domain/billing/charge.rb", facts.SymbolMethod, nil, "Shop::Orders::Facade.place"),
		sym("Admin::OrdersController#index", "mono/packs/admin/app/controllers/admin/orders_controller.rb", facts.SymbolMethod, nil, "Shop::Orders::Place.for"),
		// Shopping is not nested under Shop: a prefix of the name, not of the namespace.
		sym("Shopping::Cart", "mono/packs/cart/app/domain/shopping/cart.rb", facts.SymbolClass, nil),
	)
	s.BuildGraph()
	return s
}

func dependentNames(report Report) []string {
	var names []string
	for d, nodes := range report.ByDepth {
		if d == 0 {
			continue
		}
		for _, n := range nodes {
			names = append(names, n.Name)
		}
	}
	slices.Sort(names)
	return names
}

// A namespace declared only compactly resolves to its stub, and its impact is its
// nested constants' impact: the stub alone has no dependents.
func TestAnalyze_NamespaceRollsUpNestedConstants(t *testing.T) {
	r := resolve.Resolver{Store: namespaceStore()}
	for _, tc := range []struct {
		target    string
		wantSeeds string
		wantDeps  string
	}{
		{"Shop", "Shop::Orders::Facade,Shop::Orders::Facade.place,Shop::Orders::Place,Shop::Orders::Place.for",
			"Admin::OrdersController#index,Billing::Charge#run"},
		{"Shop::Orders", "Shop::Orders::Facade,Shop::Orders::Facade.place,Shop::Orders::Place,Shop::Orders::Place.for",
			"Admin::OrdersController#index,Billing::Charge#run"},
	} {
		report, err := Analyze(r, Request{Target: tc.target, MaxDepth: 1})
		if err != nil {
			t.Fatalf("Analyze(%q): %v", tc.target, err)
		}
		if report.SeedKind != SeedsNamespace {
			t.Errorf("Analyze(%q): seed kind %q, want namespace", tc.target, report.SeedKind)
		}
		if got := strings.Join(report.Seeds, ","); got != tc.wantSeeds {
			t.Errorf("Analyze(%q): seeds = %s, want %s", tc.target, got, tc.wantSeeds)
		}
		if got := strings.Join(dependentNames(report), ","); got != tc.wantDeps {
			t.Errorf("Analyze(%q): dependents = %s, want %s", tc.target, got, tc.wantDeps)
		}
	}
}

// A class is not a namespace even when constants nest under it, and a module with
// nothing nested under it is just itself.
func TestAnalyze_NamespaceRollupOnlyForNamespaces(t *testing.T) {
	r := resolve.Resolver{Store: namespaceStore()}
	for _, target := range []string{"Shop::Orders::Place", "Shop::Orders::Facade", "Shopping::Cart"} {
		report, err := Analyze(r, Request{Target: target, MaxDepth: 1})
		if err != nil {
			t.Fatalf("Analyze(%q): %v", target, err)
		}
		if report.SeedKind != "" || len(report.Seeds) != 0 {
			t.Errorf("Analyze(%q): seeds %v (%s); want the node itself", target, report.Seeds, report.SeedKind)
		}
	}
}

// packStore is two packages with manifests, each a root whose code sits in
// subdirectories, plus a package holding code directly.
func packStore() *facts.Store {
	sym := func(name, file string, calls ...string) facts.Fact {
		f := facts.Fact{Kind: facts.KindSymbol, Name: name, Repo: "mono", File: file, Line: 1,
			Props: map[string]any{"symbol_kind": facts.SymbolClass, "language": "ruby"}}
		for _, c := range calls {
			f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelCalls, Target: c})
		}
		return f
	}
	manifest := func(dir string, deps ...string) facts.Fact {
		f := facts.Fact{Kind: facts.KindModule, Name: dir, Repo: "mono", File: "mono/" + dir + "/package.yml",
			Props: map[string]any{"packwerk": true}}
		for _, d := range deps {
			f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelDependsOn, Target: d})
		}
		return f
	}
	s := facts.NewStore()
	s.Add(
		manifest("packs/orders"),
		manifest("packs/billing", "packs/orders"),
		manifest("packs/admin"),
		facts.Fact{Kind: facts.KindModule, Name: "packs/flat", Repo: "mono"},
		// Inside packs/orders: a top-level constant outside the pack's own namespace,
		// a caller inside the pack, and the pack's own spec.
		sym("Orders::Facade", "mono/packs/orders/app/public/orders/facade.rb"),
		sym("Receipts::Mailer", "mono/packs/orders/app/domain/receipts/mailer.rb"),
		sym("Orders::Internal", "mono/packs/orders/app/domain/orders/internal.rb", "Orders::Facade"),
		facts.Fact{Kind: facts.KindTestRef, Name: "packs/orders/spec/orders/facade_spec.rb", Repo: "mono",
			File:      "mono/packs/orders/spec/orders/facade_spec.rb",
			Relations: []facts.Relation{{Kind: facts.RelCalls, Target: "Orders::Facade"}}},
		// Outside it.
		sym("Billing::Invoice", "mono/packs/billing/app/domain/billing/invoice.rb", "Orders::Facade"),
		sym("Admin::Report", "mono/packs/admin/app/domain/admin/report.rb", "Receipts::Mailer"),
		sym("Admin::Export", "mono/packs/admin/app/domain/admin/export.rb", "Receipts::Mailer"),
		// The package root's README is prose: it does not make the root a package
		// with code of its own.
		facts.Fact{Kind: facts.KindSymbol, Name: "packs/orders/README.md", Repo: "mono", File: "mono/packs/orders/README.md",
			Props:     map[string]any{"symbol_kind": facts.SymbolDocument, "language": "markdown"},
			Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: "packs/orders"}, {Kind: facts.RelNames, Target: "Orders::Facade"}}},
		// A guide elsewhere linking into the package: prose, not a dependent.
		facts.Fact{Kind: facts.KindSymbol, Name: "docs/guide.md#orders", Repo: "mono", File: "mono/docs/guide.md",
			Props:     map[string]any{"symbol_kind": facts.SymbolSection, "language": "markdown"},
			Relations: []facts.Relation{{Kind: facts.RelNames, Target: "Orders::Facade"}, {Kind: facts.RelNames, Target: "packs/orders"}}},
		// A mixin from another package, projected onto an includer in each package:
		// the member is the mixin's node, so the other includer is not a dependent.
		facts.Fact{Kind: facts.KindSymbol, Name: "Audit::Trackable", Repo: "mono", File: "mono/packs/audit/app/public/audit/trackable.rb",
			Props: map[string]any{"symbol_kind": facts.SymbolInterface, "language": "ruby"}},
		facts.Fact{Kind: facts.KindSymbol, Name: "Audit::Trackable.track", Repo: "mono", File: "mono/packs/audit/app/public/audit/trackable.rb",
			Props: map[string]any{"symbol_kind": facts.SymbolFunc, "language": "ruby"}},
		facts.Fact{Kind: facts.KindSymbol, Name: "Orders::Ledger", Repo: "mono", File: "mono/packs/orders/app/domain/orders/ledger.rb",
			Props:     map[string]any{"symbol_kind": facts.SymbolClass, "language": "ruby", facts.PropMixinMembers: map[string]any{"Audit::Trackable.track": "include"}},
			Relations: []facts.Relation{{Kind: facts.RelHasMethod, Target: "Audit::Trackable.track"}}},
		facts.Fact{Kind: facts.KindSymbol, Name: "Admin::Ledger", Repo: "mono", File: "mono/packs/admin/app/domain/admin/ledger.rb",
			Props:     map[string]any{"symbol_kind": facts.SymbolClass, "language": "ruby", facts.PropMixinMembers: map[string]any{"Audit::Trackable.track": "include"}},
			Relations: []facts.Relation{{Kind: facts.RelHasMethod, Target: "Audit::Trackable.track"}}},
		// A package with code directly in it already is a node of its own.
		sym("Flat::Thing", "mono/packs/flat/thing.rb"),
		sym("Billing::UsesFlat", "mono/packs/billing/app/domain/billing/uses_flat.rb", "Flat::Thing"),
	)
	s.BuildGraph()
	return s
}

// A package root is the code beneath it: its dependents are the callers of every
// symbol declared there, its own code and tests are not, and the manifest edge
// still counts. Dependents are grouped by owning package.
func TestAnalyze_DirectoryModuleRollsUpSymbolsBeneathIt(t *testing.T) {
	r := resolve.Resolver{Store: packStore(), RepoPaths: map[string]string{"mono": "/work/mono"}}
	report, err := Analyze(r, Request{Target: "packs/orders", MaxDepth: 1})
	if err != nil {
		t.Fatal(err)
	}
	if report.SeedKind != SeedsDirectory {
		t.Fatalf("seed kind %q, want directory", report.SeedKind)
	}
	if got := strings.Join(report.Seeds, ","); got != "Orders::Facade,Orders::Internal,Orders::Ledger,Receipts::Mailer" {
		t.Errorf("seeds = %s; tests beneath the directory are not seeds", got)
	}
	if got := strings.Join(dependentNames(report), ","); got != "Admin::Export,Admin::Report,Billing::Invoice,packs/billing" {
		t.Errorf("dependents = %s; want the outside callers and the declaring manifest only", got)
	}
	var packages []string
	for _, p := range report.Packages {
		packages = append(packages, p.Package+"="+strconv.Itoa(p.Dependents))
	}
	if got := strings.Join(packages, ","); got != "mono/packs/admin=2,mono/packs/billing=2" {
		t.Errorf("packages = %s", got)
	}
}

func TestAnalyze_DirectoryModuleWithOwnCodeIsItself(t *testing.T) {
	r := resolve.Resolver{Store: packStore(), RepoPaths: map[string]string{"mono": "/work/mono"}}
	report, err := Analyze(r, Request{Target: "packs/flat", MaxDepth: 1})
	if err != nil {
		t.Fatal(err)
	}
	if report.SeedKind != "" {
		t.Errorf("seed kind %q; a package with code of its own is walked as itself", report.SeedKind)
	}
}

// A directory nothing names is still a path: answered from the code beneath it.
func TestAnalyze_UnnamedDirectoryPath(t *testing.T) {
	r := resolve.Resolver{Store: packStore(), RepoPaths: map[string]string{"mono": "/work/mono"}}
	report, err := Analyze(r, Request{Target: "packs/orders/app/domain/receipts", MaxDepth: 1})
	if err != nil {
		t.Fatal(err)
	}
	if report.SeedKind != SeedsDirectory || strings.Join(report.Seeds, ",") != "Receipts::Mailer" {
		t.Fatalf("seeds %v (%s)", report.Seeds, report.SeedKind)
	}
	if got := strings.Join(dependentNames(report), ","); got != "Admin::Export,Admin::Report" {
		t.Errorf("dependents = %s", got)
	}
}

// A class's impact is its own members' callers. A member it gained from a mixin is
// the mixin's node, shared with every other includer, so walking from it would
// report those includers as dependents of this class.
func TestAnalyze_ClassIgnoresMixedInMembers(t *testing.T) {
	r := resolve.Resolver{Store: packStore(), RepoPaths: map[string]string{"mono": "/work/mono"}}
	report, err := Analyze(r, Request{Target: "Orders::Ledger", MaxDepth: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got := dependentNames(report); len(got) != 0 {
		t.Errorf("dependents = %v; another includer of the same mixin is not one", got)
	}
}

// A Go package is a module with code in its own directory. Its functions are the
// package, not its dependents: they used to come back at depth 1 and bury the
// importers. The importing modules are listed by hop from a walk over imports.
func TestAnalyze_PackageListsImportersNotItsOwnSymbols(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		facts.Fact{Kind: facts.KindModule, Name: "modules/uri", File: "modules/uri", Props: map[string]any{"language": "go"}},
		facts.Fact{Kind: facts.KindModule, Name: "services/migrations", File: "services/migrations", Props: map[string]any{"language": "go"}},
		facts.Fact{Kind: facts.KindModule, Name: "routers/web", File: "routers/web", Props: map[string]any{"language": "go"}},
		facts.Fact{Kind: facts.KindSymbol, Name: "modules/uri.Open", File: "modules/uri/uri.go",
			Props: map[string]any{"symbol_kind": "function"}, Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: "modules/uri"}}},
		facts.Fact{Kind: facts.KindSymbol, Name: "services/migrations.Migrate", File: "services/migrations/m.go",
			Props: map[string]any{"symbol_kind": "function"}, Relations: []facts.Relation{
				{Kind: facts.RelDeclares, Target: "services/migrations"}, {Kind: facts.RelCalls, Target: "modules/uri.Open"}}},
		facts.Fact{Kind: facts.KindDependency, Name: "services/migrations -> modules/uri", File: "services/migrations/m.go",
			Relations: []facts.Relation{{Kind: facts.RelImports, Target: "modules/uri"}}},
		facts.Fact{Kind: facts.KindDependency, Name: "routers/web -> services/migrations", File: "routers/web/w.go",
			Relations: []facts.Relation{{Kind: facts.RelImports, Target: "services/migrations"}}},
	)
	store.BuildGraph()
	report, err := Analyze(resolve.Resolver{Store: store}, Request{Target: "modules/uri"})
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, m := range report.Modules {
		got = append(got, m.Module+":"+strconv.Itoa(m.Depth))
	}
	if want := []string{"services/migrations:1", "routers/web:2"}; !slices.Equal(got, want) {
		t.Errorf("Modules = %v, want %v", got, want)
	}
	if report.SeedKind != SeedsPackage || !slices.Equal(report.Seeds, []string{"modules/uri.Open"}) {
		t.Errorf("the package's own symbol should be a seed: kind=%q seeds=%v", report.SeedKind, report.Seeds)
	}
	for depth, nodes := range report.ByDepth {
		for _, n := range nodes {
			if depth > 0 && n.Name == "modules/uri.Open" {
				t.Errorf("the package's own function is listed as its dependent at depth %d", depth)
			}
		}
	}
}
