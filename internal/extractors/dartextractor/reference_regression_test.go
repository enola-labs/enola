package dartextractor

import (
	"context"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/orphans"
)

func TestRegression_TearOffReferences(t *testing.T) {
	for _, use := range []string{
		"return items.map(_mapItem);",
		"return guard(map: _mapItem);",
		"return {'map': _mapItem};",
		"return Provider<Object>(_mapItem);",
		"return switch (mode) { 0 => _mapItem, _ => null };",
	} {
		t.Run(use, func(t *testing.T) {
			ff := walkSource(t, "lib/mapper.dart", "Object _mapItem(Object value) => value;\nObject run(dynamic items, int mode) { "+use+" }")
			if findFact(ff, facts.KindSymbol, "lib._mapItem") == nil {
				t.Fatal("fixture did not emit _mapItem")
			}
			store := facts.NewStore()
			store.Add(ff...)
			insights, err := orphans.NewExplainer().Explain(context.Background(), store)
			if err != nil {
				t.Fatal(err)
			}
			for _, in := range insights {
				if strings.HasSuffix(in.Title, " lib._mapItem") {
					t.Errorf("tear-off reported as dead: %s", in.Title)
				}
			}
		})
	}
}

func TestDartTearOffModuleInitializer(t *testing.T) {
	ff := walkSource(t, "lib/client.dart", `Object _buildClient(Object ref) => ref;
final clientProvider = Provider<Object>(_buildClient);`)
	for _, f := range ff {
		for _, r := range f.Relations {
			if r.Kind == facts.RelNames && r.Target == "lib._buildClient" {
				return
			}
		}
	}
	t.Fatal("module callback registration lost its reference")
}

func TestDartTearOffShadowingAndNonValues(t *testing.T) {
	for _, source := range []string{
		`Object run(dynamic _mapItem) => items.map(_mapItem);`,
		`Object run() { final _mapItem = external; return items.map(_mapItem); }`,
		`Object run() => guard(_mapItem: other);`,
		`Object run() => object._mapItem;`,
		`Object run() => {'_mapItem': other};`,
		`Object run() => _mapItem(other);`,
		`Object run(dynamic items) { for (final _mapItem in items) { consume(_mapItem); } return null; }`,
		`Object run() => _mapItem.field;`,
	} {
		t.Run(source, func(t *testing.T) {
			ff := walkSource(t, "lib/mapper.dart", "Object _mapItem(Object v) => v;\n"+source)
			for _, f := range ff {
				for _, r := range f.Relations {
					if r.Kind == facts.RelNames && (r.Target == "_mapItem" || r.Target == "lib._mapItem") {
						t.Errorf("non-value or shadowed name became a tear-off: %s", f.Name)
					}
				}
			}
		})
	}
}

func TestDartValueReferencesStayInLexicalScope(t *testing.T) {
	ff := walkSource(t, "lib/mapper.dart", `
Object callback(Object value) => value;
class Reader {
  final String message;
  Object? _cache;
  Object? callback;
  Reader(this.message);
  Object build() => consume(message, _cache, callback);
}
class Other {
  String get message => '';
  Object? get _cache => null;
}
class Mapper {
  Object mapItem(Object value) => value;
  Object run(dynamic items) => items.map(mapItem);
}
Object standalone() => consume(message);
`)
	for _, f := range ff {
		for _, r := range f.Relations {
			if r.Kind != facts.RelNames {
				continue
			}
			if f.Name != "lib.Mapper.run" || r.Target != "lib.Mapper.mapItem" {
				t.Errorf("non-callable or unrelated scope bound as a callback: %s -> %s", f.Name, r.Target)
			}
		}
	}
	f := findFact(ff, facts.KindSymbol, "lib.Mapper.run")
	if f == nil {
		t.Fatal("missing method fixture")
	}
	for _, r := range f.Relations {
		if r.Kind == facts.RelNames && r.Target == "lib.Mapper.mapItem" {
			return
		}
	}
	t.Fatal("same-class method tear-off was lost")
}

func TestDartTearOffScopeRestoration(t *testing.T) {
	for _, body := range []string{
		`items.forEach((callback) => consume(callback));`,
		`{ final callback = external; consume(callback); }`,
		`for(final callback in items) { consume(callback); }`,
		`try {} catch(callback) { consume(callback); }`,
		`void nested(callback) { consume(callback); }`,
	} {
		for _, outer := range []bool{false, true} {
			source := `import 'package:http/http.dart'; Object callback() { httpClient.get('/'); return null; } Object run(items) { ` + body
			if outer {
				source += ` consume(callback); `
			}
			source += ` return null; }`
			all := dartReviewFacts(t, map[string]string{"lib/a.dart": source})
			f := findFact(all, facts.KindSymbol, "lib.run")
			if f == nil {
				t.Fatal("missing run")
			}
			names := false
			for _, rel := range f.Relations {
				if rel.Kind == facts.RelNames && rel.Target == "lib.callback" {
					names = true
				}
			}
			if names != outer || hasReviewCall(f, "lib.callback") || f.PropBool("performs_io") || f.PropBool("recursive_self") {
				t.Errorf("scope %s (outer=%v): %+v", body, outer, f)
			}
		}
	}
}
