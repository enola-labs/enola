package hierarchy

import (
	"reflect"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func typ(name, kind string, bases ...string) facts.Fact {
	f := facts.Fact{Kind: facts.KindSymbol, Name: name, Repo: "r", Props: map[string]any{"symbol_kind": kind}}
	for _, b := range bases {
		f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelImplements, Target: b})
	}
	return f
}

func meth(name string) facts.Fact {
	return facts.Fact{Kind: facts.KindSymbol, Name: name, Repo: "r", Props: map[string]any{"symbol_kind": facts.SymbolMethod}}
}

func TestSplitMember(t *testing.T) {
	for in, want := range map[string][2]string{
		"app/lib.Base.run":               {"app/lib.Base", "run"},
		`App\Base::run`:                  {`App\Base`, "run"},
		"src.Base::run":                  {"src.Base", "run"},
		"Admin::Base#run":                {"Admin::Base", "#run"},
		"Admin::Base.run":                {"Admin::Base", "run"},
		"example.com/fx/app.Base.Common": {"example.com/fx/app.Base", "Common"},
		"main":                           {"", "main"},
		"example.com/fx":                 {"", "example.com/fx"},
	} {
		o, m := SplitMember(in)
		if [2]string{o, m} != want {
			t.Errorf("SplitMember(%q) = %q, %q; want %q", in, o, m, want)
		}
	}
}

// A member is found on the type, else on the nearest ancestor, whichever way the
// extractor wrote the base: a relation, a superclass prop, a bare name.
func TestLookup_ClimbsBasesHoweverTheyAreSpelled(t *testing.T) {
	sup := typ("src.Concrete", facts.SymbolClass)
	sup.Props["superclass"] = "Base"
	ix := Build([]facts.Fact{
		typ("src.Base", facts.SymbolClass), meth("src.Base.run"), meth("src.Base.step"),
		typ("src.Mid", facts.SymbolClass, "src.Base"), meth("src.Mid.step"),
		typ("src.Leaf", facts.SymbolClass, "Mid"),
		sup,
	})
	cases := map[[2]string]string{
		{"src.Leaf", "run"}:     "src.Base.run",
		{"src.Leaf", "step"}:    "src.Mid.step",
		{"src.Concrete", "run"}: "src.Base.run",
		{"src.Base", "step"}:    "src.Base.step",
	}
	for in, want := range cases {
		if got, ok := ix.Lookup("r", in[0], in[1]); !ok || got != want {
			t.Errorf("Lookup(%s, %s) = %q, %v; want %q", in[0], in[1], got, ok, want)
		}
	}
	if _, ok := ix.Lookup("r", "src.Leaf", "missing"); ok {
		t.Error("a member nothing declares resolves to nothing")
	}
	if got := ix.Ancestors("r", "src.Leaf"); !reflect.DeepEqual(got, []string{"src.Mid", "src.Base"}) {
		t.Errorf("Ancestors = %v", got)
	}
}

// A bare base name shared by several types resolves to the one nearest the subclass,
// and to nothing when two are equally near. A name no type carries is no edge.
func TestBuild_ABareBaseResolvesOnlyWhenOneTypeIsNearest(t *testing.T) {
	ix := Build([]facts.Fact{
		typ("a/models.Model", facts.SymbolClass),
		typ("b/models.Model", facts.SymbolClass),
		typ("a/models.User", facts.SymbolClass, "Model"),
		typ("c.Thing", facts.SymbolClass, "Model"),
		typ("a/models.Error", facts.SymbolClass, "Exception"),
	})
	if got := ix.Ancestors("r", "a/models.User"); !reflect.DeepEqual(got, []string{"a/models.Model"}) {
		t.Errorf("User's base = %v, want the Model of its own package", got)
	}
	if got := ix.Ancestors("r", "c.Thing"); got != nil {
		t.Errorf("two Models equally near: base = %v, want none", got)
	}
	if got := ix.Ancestors("r", "a/models.Error"); got != nil {
		t.Errorf("a framework base is no edge: base = %v", got)
	}
}

// Nearness counts whole path segments: a type whose path only begins with the same
// characters (tools/ against test/, src/main against src/mainline) is no nearer, so
// candidates that share no more segments than each other tie and bind nothing.
func TestResolveType_NearnessCountsWholeSegments(t *testing.T) {
	ix := Build([]facts.Fact{
		typ("src/include/duckdb/main.duckdb::Connection", facts.SymbolClass),
		typ("tools/cpp.duckdb::cxx::Connection", facts.SymbolClass),
		typ("test/common.duckdb::Runner", facts.SymbolClass),
		typ("src/main.Base", facts.SymbolClass),
		typ("src/other.Base", facts.SymbolClass),
		typ("src/mainline.Sub", facts.SymbolClass, "Base"),
		typ("src/main/deep.Sub", facts.SymbolClass, "Base"),
	})
	if got := ix.ResolveType("r", "test/common.duckdb::Runner", "Connection"); got != "" {
		t.Errorf("Connection from test/common = %q, want none: tools/ is no nearer than src/", got)
	}
	if got := ix.Ancestors("r", "src/mainline.Sub"); got != nil {
		t.Errorf("src/mainline.Sub's base = %v, want none: src/main and src/other are equally near", got)
	}
	if got := ix.Ancestors("r", "src/main/deep.Sub"); !reflect.DeepEqual(got, []string{"src/main.Base"}) {
		t.Errorf("src/main/deep.Sub's base = %v, want the Base two segments away", got)
	}
}

// The overriders of a member are the first declaration down each branch.
func TestOverriders_TopmostOnEachBranch(t *testing.T) {
	ix := Build([]facts.Fact{
		typ(`App\Base`, facts.SymbolClass), meth(`App\Base::step`),
		typ(`App\A`, facts.SymbolClass, `App\Base`), meth(`App\A::step`),
		typ(`App\A1`, facts.SymbolClass, `App\A`), meth(`App\A1::step`),
		typ(`App\B`, facts.SymbolClass, `App\Base`),
		typ(`App\B1`, facts.SymbolClass, `App\B`), meth(`App\B1::step`),
	})
	got := ix.Overriders("r", `App\Base`, "step")
	if want := []string{`App\A::step`, `App\B1::step`}; !reflect.DeepEqual(got, want) {
		t.Errorf("Overriders = %v, want %v", got, want)
	}
}

// A cycle in the bases (a broken tree, two files each extending the other) ends.
func TestAncestors_ACycleEnds(t *testing.T) {
	ix := Build([]facts.Fact{typ("x.A", facts.SymbolClass, "x.B"), typ("x.B", facts.SymbolClass, "x.A")})
	if got := ix.Ancestors("r", "x.A"); !reflect.DeepEqual(got, []string{"x.B"}) {
		t.Errorf("Ancestors = %v", got)
	}
	if _, ok := ix.Lookup("r", "x.A", "m"); ok {
		t.Error("nothing declares m")
	}
}
