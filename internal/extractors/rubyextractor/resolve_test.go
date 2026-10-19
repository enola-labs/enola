package rubyextractor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// --- fixture builders ---

func symFact(name, dir, symbolKind string, rels ...facts.Relation) facts.Fact {
	all := []facts.Relation{{Kind: facts.RelDeclares, Target: dir}}
	all = append(all, rels...)
	return facts.Fact{
		Kind:      facts.KindSymbol,
		Name:      name,
		File:      dir + "/" + strings.ToLower(lastSegment(name)) + ".rb",
		Props:     map[string]any{"symbol_kind": symbolKind, "language": "ruby"},
		Relations: all,
	}
}

func depFactRuby(file string, props map[string]any, rel facts.Relation) facts.Fact {
	if props == nil {
		props = map[string]any{"language": "ruby"}
	}
	return facts.Fact{
		Kind:      facts.KindDependency,
		Name:      file + " -> " + rel.Target,
		File:      file,
		Props:     props,
		Relations: []facts.Relation{rel},
	}
}

func modFact(name string, rels ...facts.Relation) facts.Fact {
	return facts.Fact{Kind: facts.KindModule, Name: name, File: name, Relations: rels}
}

// hasEdge reports whether the synthetic facts contain a srcDir->dstDir coupling.
func hasEdge(out []facts.Fact, src, dst string) bool {
	for _, f := range out {
		if f.Name != src+" -> "+dst {
			continue
		}
		for _, r := range f.Relations {
			if r.Kind == facts.RelImports && r.Target == dst {
				return true
			}
		}
	}
	return false
}

// --- const index ---

func TestBuildConstIndex_QualifiedAndBare(t *testing.T) {
	ff := []facts.Fact{
		symFact("Orders::Order", "app/models/orders", facts.SymbolClass),
		symFact("Email::FromBuilder", "app/builders/email", facts.SymbolClass),
	}
	ix := buildConstIndex(ff)

	if got := ix.resolve("Orders::Order", "app/x"); got != "app/models/orders" {
		t.Errorf("qualified resolve = %q, want app/models/orders", got)
	}
	if got := ix.resolve("Order", "app/x"); got != "app/models/orders" {
		t.Errorf("bare resolve = %q, want app/models/orders", got)
	}
	if got := ix.resolve("::Order", "app/x"); got != "app/models/orders" {
		t.Errorf("leading-colon resolve = %q, want app/models/orders", got)
	}
	if got := ix.resolve("Nonexistent", "app/x"); got != "" {
		t.Errorf("unknown resolve = %q, want empty", got)
	}
}

// A qualified reference names its whole path: it resolves to a declaration ending
// in that path (written from inside an enclosing namespace), never to another
// namespace's constant that merely shares the last segment. A library's constant
// the repository does not declare resolves to nothing.
func TestConstIndex_QualifiedRefNeedsItsWholePath(t *testing.T) {
	ix := buildConstIndex([]facts.Fact{
		symFact("Exports::Metrics", "packs/exports/app/lib/exports", facts.SymbolInterface),
		symFact("Shop::Orders::Order", "packs/shop/app/models/shop/orders", facts.SymbolClass),
	})
	for _, tc := range []struct{ ref, want string }{
		{"Toolkit::Metrics", ""},                               // a gem's constant, not Exports::Metrics
		{"Metrics", "packs/exports/app/lib/exports"},           // a bare name still resolves by its segment
		{"Orders::Order", "packs/shop/app/models/shop/orders"}, // nested under Shop
		{"Billing::Order", ""},                                 // shares only the last segment
	} {
		if got := ix.resolve(tc.ref, "packs/other/app"); got != tc.want {
			t.Errorf("resolve(%q) = %q, want %q", tc.ref, got, tc.want)
		}
	}
}

// TestConstIndex_BareCollisionDeterministic covers the qualified-map collision
// path: two symbols with the SAME bare name resolve deterministically to the
// shortest declaring dir (unchanged by the namespace-aware bare disambiguation,
// which only applies to the bare fallback below).
func TestConstIndex_BareCollisionDeterministic(t *testing.T) {
	ff := []facts.Fact{
		symFact("Item", "engines/foo/app/models", facts.SymbolClass),
		symFact("Item", "app/models", facts.SymbolClass),
	}
	for run := 0; run < 3; run++ {
		ix := buildConstIndex(ff)
		if got := ix.resolve("Item", "lib/tasks"); got != "app/models" {
			t.Fatalf("run %d: collision resolve = %q, want shortest dir app/models", run, got)
		}
	}
}

// TestConstIndex_BareAmbiguityNamespaceAware covers the bare fallback: a short
// reference to classes declared with COLLIDING namespaced names is resolved to
// the candidate in the referrer's own namespace, and dropped when genuinely
// ambiguous.
func TestConstIndex_BareAmbiguityNamespaceAware(t *testing.T) {
	ff := []facts.Fact{
		symFact("Foo::Item", "engines/foo/app/models", facts.SymbolClass),
		symFact("App::Item", "app/models", facts.SymbolClass),
	}
	ix := buildConstIndex(ff)

	// A referrer inside the engine namespace resolves to the engine's Item.
	if got := ix.resolve("Item", "engines/foo/app/services"); got != "engines/foo/app/models" {
		t.Errorf("engine-referrer resolve = %q, want engines/foo/app/models", got)
	}
	// A referrer in the top-level app namespace resolves to the app's Item.
	if got := ix.resolve("Item", "app/controllers"); got != "app/models" {
		t.Errorf("app-referrer resolve = %q, want app/models", got)
	}
	// A referrer sharing no namespace with either candidate is genuinely
	// ambiguous — drop the edge rather than guess an arbitrary declarer.
	if got := ix.resolve("Item", "lib/tasks"); got != "" {
		t.Errorf("ambiguous resolve = %q, want empty (dropped)", got)
	}
}

func TestConstFromCall(t *testing.T) {
	cases := map[string]string{
		"Account.active":                   "Account",
		"Agents::DestroyJob.perform_later": "Agents::DestroyJob",
		"ActiveRecord::Base.transaction":   "ActiveRecord::Base",
		"::Account.active":                 "Account",
		"config.fetch":                     "", // lowercase receiver
		"Foo::Bar":                         "", // no method suffix
		"markdown_num":                     "", // bare method call (no receiver) — no coupling edge
		"authenticate_user!":               "", // bare DSL-referenced method — no coupling edge
		"render":                           "", // bare call — no coupling edge
	}
	for in, want := range cases {
		if got := constFromCall(in); got != want {
			t.Errorf("constFromCall(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- reference kinds ---

func TestResolveImports_Inheritance(t *testing.T) {
	ff := []facts.Fact{
		symFact("Email::FromBuilder", "app/builders/email", facts.SymbolClass,
			facts.Relation{Kind: facts.RelImplements, Target: "Mail::BaseBuilder"}),
		symFact("Mail::BaseBuilder", "app/mailers/mail", facts.SymbolClass),
	}
	out := resolveImports(ff, false)
	if !hasEdge(out, "app/builders/email", "app/mailers/mail") {
		t.Errorf("missing inheritance edge; got %+v", out)
	}
}

func TestResolveImports_Mixin(t *testing.T) {
	ff := []facts.Fact{
		symFact("Helpers::UrlHelper", "app/helpers", facts.SymbolInterface),
		depFactRuby("app/actions/contact.rb",
			map[string]any{"language": "ruby", "mixin_kind": "include"},
			facts.Relation{Kind: facts.RelImplements, Target: "UrlHelper"}),
		symFact("UrlHelper", "app/helpers", facts.SymbolInterface),
	}
	out := resolveImports(ff, false)
	if !hasEdge(out, "app/actions", "app/helpers") {
		t.Errorf("missing mixin edge; got %+v", out)
	}
}

func TestResolveImports_Association(t *testing.T) {
	ff := []facts.Fact{
		depFactRuby("app/models/order.rb",
			map[string]any{"language": "ruby", "association_kind": "has_many"},
			facts.Relation{Kind: facts.RelDependsOn, Target: "Item"}),
		symFact("Item", "app/models/items", facts.SymbolClass),
	}
	out := resolveImports(ff, false)
	if !hasEdge(out, "app/models", "app/models/items") {
		t.Errorf("missing association edge; got %+v", out)
	}
}

func TestResolveImports_MethodCall(t *testing.T) {
	ff := []facts.Fact{
		symFact("CleanupJob#perform", "app/jobs", facts.SymbolMethod,
			facts.Relation{Kind: facts.RelCalls, Target: "Account.active"}),
		symFact("Account", "app/models", facts.SymbolClass),
	}
	out := resolveImports(ff, false)
	if !hasEdge(out, "app/jobs", "app/models") {
		t.Errorf("missing method-call edge; got %+v", out)
	}
}

// edgeKind returns the coupling_kind prop of the synthetic src->dst edge, or "".
func edgeKind(out []facts.Fact, src, dst string) string {
	for _, f := range out {
		if f.Name == src+" -> "+dst {
			k, _ := f.Props[facts.PropCouplingKind].(string)
			return k
		}
	}
	return ""
}

// TestResolveImports_CouplingKindTagged: each emitted edge carries a coupling_kind
// matching the reference that produced it — association edges are tagged so the
// cycles explainer can exclude them.
func TestResolveImports_CouplingKindTagged(t *testing.T) {
	ff := []facts.Fact{
		depFactRuby("app/models/order.rb",
			map[string]any{"language": "ruby", "association_kind": "has_many"},
			facts.Relation{Kind: facts.RelDependsOn, Target: "Item"}),
		symFact("Item", "app/models/items", facts.SymbolClass),
	}
	out := resolveImports(ff, false)
	if got := edgeKind(out, "app/models", "app/models/items"); got != facts.CouplingAssociation {
		t.Errorf("association edge coupling_kind = %q, want %q", got, facts.CouplingAssociation)
	}
}

// TestResolveImports_ReferenceBeatsAssociation: when the same src->dst edge arises
// from BOTH a real method call and an association, the reference (harder) kind wins
// so the edge is NOT excluded from cycle detection as a mere association.
func TestResolveImports_ReferenceBeatsAssociation(t *testing.T) {
	ff := []facts.Fact{
		// Association Order -> Item.
		depFactRuby("app/models/order.rb",
			map[string]any{"language": "ruby", "association_kind": "has_many"},
			facts.Relation{Kind: facts.RelDependsOn, Target: "Item"}),
		// A real method call from the same source dir to the same target dir.
		symFact("Order#refresh", "app/models", facts.SymbolMethod,
			facts.Relation{Kind: facts.RelCalls, Target: "Item.stale"}),
		symFact("Item", "app/models/items", facts.SymbolClass),
	}
	out := resolveImports(ff, false)
	if got := edgeKind(out, "app/models", "app/models/items"); got != facts.CouplingReference {
		t.Errorf("mixed reference+association edge coupling_kind = %q, want %q", got, facts.CouplingReference)
	}
}

func TestResolveImports_SelfEdgeSkipped(t *testing.T) {
	ff := []facts.Fact{
		symFact("Account", "app/models", facts.SymbolClass,
			facts.Relation{Kind: facts.RelCalls, Target: "User.find"}),
		symFact("User", "app/models", facts.SymbolClass),
	}
	out := resolveImports(ff, false)
	if len(out) != 0 {
		t.Errorf("expected no edges for same-dir reference, got %+v", out)
	}
}

func TestResolveImports_DedupAndSorted(t *testing.T) {
	// Two distinct references producing the same edge → one fact.
	ff := []facts.Fact{
		symFact("A", "app/a", facts.SymbolClass,
			facts.Relation{Kind: facts.RelCalls, Target: "Z.foo"}),
		symFact("B", "app/a", facts.SymbolClass,
			facts.Relation{Kind: facts.RelImplements, Target: "Z"}),
		symFact("Z", "app/z", facts.SymbolClass),
	}
	out := resolveImports(ff, false)
	count := 0
	for _, f := range out {
		if f.Name == "app/a -> app/z" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected the duplicate edge deduped to 1 fact, got %d", count)
	}
	// Output is sorted by Name.
	for i := 1; i < len(out); i++ {
		if out[i-1].Name > out[i].Name {
			t.Errorf("output not sorted: %q before %q", out[i-1].Name, out[i].Name)
		}
	}
}

// --- require classification ---

func TestClassifyRequire_StdlibExternalRelative(t *testing.T) {
	ff := []facts.Fact{
		modFact("app/helpers"),
		depFactRuby("app/x/a.rb", map[string]any{"language": "ruby"},
			facts.Relation{Kind: facts.RelImports, Target: "set"}),
		depFactRuby("app/x/b.rb", map[string]any{"language": "ruby"},
			facts.Relation{Kind: facts.RelImports, Target: "net/http"}),
		depFactRuby("app/x/c.rb", map[string]any{"language": "ruby"},
			facts.Relation{Kind: facts.RelImports, Target: "sidekiq"}),
		depFactRuby("app/x/d.rb", map[string]any{"language": "ruby", "require_relative": true},
			facts.Relation{Kind: facts.RelImports, Target: "../helpers/url"}),
	}
	out := resolveImports(ff, false)

	wantSource := map[string]string{
		"set":            "stdlib",
		"net/http":       "stdlib",
		"sidekiq":        "external",
		"../helpers/url": "internal",
	}
	for _, f := range ff {
		if f.Kind != facts.KindDependency {
			continue
		}
		tgt := f.Relations[0].Target
		if want, ok := wantSource[tgt]; ok {
			if got, _ := f.Props["source"].(string); got != want {
				t.Errorf("require %q source = %q, want %q", tgt, got, want)
			}
		}
	}
	// require_relative "../helpers/url" from app/x → app/helpers module.
	if !hasEdge(out, "app/x", "app/helpers") {
		t.Errorf("missing require_relative edge to app/helpers; got %+v", out)
	}
}

// --- packwerk ---

func TestResolveImports_Packwerk(t *testing.T) {
	ff := []facts.Fact{
		modFact("packages/orders",
			facts.Relation{Kind: facts.RelDependsOn, Target: "packages/payments"},
			facts.Relation{Kind: facts.RelDependsOn, Target: "root"}),
		modFact("packages/payments"),
		modFact("root"),
	}
	out := resolveImports(ff, false)
	if !hasEdge(out, "packages/orders", "packages/payments") {
		t.Errorf("missing packwerk dependency edge; got %+v", out)
	}
	if !hasEdge(out, "packages/orders", ".") {
		t.Errorf("missing packwerk root edge (root→.); got %+v", out)
	}
}

// --- fileDir source-side contract (load-bearing) ---

// explainFileDir / graphFileDirectory mirror the consumer logic exactly so this
// test fails if either upstream helper or our sentinel File format drifts.
func explainFileDir(file string) string {
	parts := strings.Split(file, "/")
	if len(parts) <= 1 {
		return "."
	}
	return strings.Join(parts[:len(parts)-1], "/")
}

func graphFileDirectory(file string) string {
	if i := strings.LastIndex(file, "/"); i >= 0 {
		return file[:i]
	}
	return "."
}

func TestEmitEdges_FileDirRoundTrip(t *testing.T) {
	ff := []facts.Fact{
		symFact("CleanupJob#perform", "app/jobs", facts.SymbolMethod,
			facts.Relation{Kind: facts.RelCalls, Target: "Account.active"}),
		symFact("Account", "app/models", facts.SymbolClass),
	}
	out := resolveImports(ff, false)
	if len(out) == 0 {
		t.Fatal("expected an edge")
	}
	f := out[0]
	if explainFileDir(f.File) != "app/jobs" {
		t.Errorf("explainFileDir(%q) = %q, want app/jobs (source-side trap)", f.File, explainFileDir(f.File))
	}
	if graphFileDirectory(f.File) != "app/jobs" {
		t.Errorf("graphFileDirectory(%q) = %q, want app/jobs", f.File, graphFileDirectory(f.File))
	}
}

// --- end-to-end Extract ---

func TestExtract_EndToEnd_CouplingResolves(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"Gemfile":                 "source 'https://rubygems.org'\ngem 'rails'\n",
		"config/application.rb":   "module Demo\n  class Application\n  end\nend\n",
		"app/models/account.rb":   "class Account\n  def self.active\n  end\nend\n",
		"app/jobs/cleanup_job.rb": "class CleanupJob\n  def perform\n    Account.active\n  end\nend\n",
	}
	var rel []string
	for name, content := range files {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		rel = append(rel, name)
	}

	ff, err := New().Extract(context.Background(), dir, rel)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	moduleNames := map[string]bool{}
	for _, f := range ff {
		if f.Kind == facts.KindModule {
			moduleNames[f.Name] = true
		}
	}

	// Replicate computeHotspots' fan-in/out resolution.
	resolvedEdges := 0
	sawSynthetic := false
	for _, f := range ff {
		if f.Kind != facts.KindDependency {
			continue
		}
		if sc, _ := f.Props["synthetic_coupling"].(bool); sc {
			sawSynthetic = true
		}
		for _, r := range f.Relations {
			if r.Kind == facts.RelImports && moduleNames[r.Target] {
				resolvedEdges++
			}
		}
	}

	if !sawSynthetic {
		t.Error("expected at least one synthetic_coupling dependency fact")
	}
	if resolvedEdges == 0 {
		t.Errorf("expected resolvedEdges > 0; module names: %v", moduleKeys(moduleNames))
	}
}

func moduleKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
