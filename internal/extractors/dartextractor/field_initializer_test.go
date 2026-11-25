package dartextractor

import (
	"context"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/orphans"
)

// TestDartFieldInitializerTearOffs pins the class-body field region. Constructor
// initializer lists and file-scope initializers already record the callables they
// name; a field initializer is the third place the same code is written, and the
// shape schema generators emit (`late final T t = T(columns: [_column_0])`).
func TestDartFieldInitializerTearOffs(t *testing.T) {
	ff := walkSource(t, "lib/schema.dart", `
Object _column_0(String alias) => alias;
Object _column_1(String alias) => alias;
class Schema {
  late final Object todos = Shape(columns: [_column_0]);
  static final callbacks = <Object>[_column_1];
  final Object direct = _check;
  Object _check(Object v) => v;
}
`)
	got := map[string]bool{}
	for _, f := range ff {
		for _, r := range f.Relations {
			if r.Kind == facts.RelNames {
				got[f.Kind+" "+f.Name+" -> "+r.Target] = true
			}
		}
	}
	for _, want := range []string{
		"file_ref lib/schema.dart -> lib._column_0",
		"file_ref lib/schema.dart -> lib._column_1",
		"file_ref lib/schema.dart -> lib.Schema._check",
	} {
		if !got[want] {
			t.Errorf("missing %q; got %v", want, got)
		}
	}

	store := facts.NewStore()
	store.Add(ff...)
	insights, err := orphans.NewExplainer().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range insights {
		if strings.Contains(in.Title, "lib._column_") {
			t.Errorf("field-initializer tear-off reported as dead: %s", in.Title)
		}
	}
}

// TestDartFieldInitializerShadowing keeps the class-body walk to value positions:
// a same-named field shadows a top-level function, an invocation is not a
// tear-off, and a field's own name is not a reference.
func TestDartFieldInitializerShadowing(t *testing.T) {
	ff := walkSource(t, "lib/schema.dart", `
Object helper(Object v) => v;
Object other(Object v) => v;
class Holder {
  final Object helper = 1;
  final Object a = [helper];
  final Object b = other(1);
  final Object other2 = (Object other) => [other];
}
`)
	for _, f := range ff {
		for _, r := range f.Relations {
			if r.Kind == facts.RelNames && (r.Target == "lib.helper" || r.Target == "lib.other") {
				t.Errorf("shadowed or invoked name became a tear-off: %s %s -> %s", f.Kind, f.Name, r.Target)
			}
		}
	}
}

// TestDartFileRefRelationsDeduplicated: file-scope and class-body initializers
// naming the same callable produce one relation on the single file_ref.
func TestDartFileRefRelationsDeduplicated(t *testing.T) {
	ff := walkSource(t, "lib/schema.dart", `
Object _cb(Object v) => v;
final top = [_cb];
class Holder {
  final Object inner = [_cb];
}
`)
	n, refs := 0, 0
	for _, f := range ff {
		if f.Kind != facts.KindFileRef {
			continue
		}
		refs++
		for _, r := range f.Relations {
			if r.Kind == facts.RelNames && r.Target == "lib._cb" {
				n++
			}
		}
	}
	if refs != 1 || n != 1 {
		t.Errorf("want one file_ref with one names edge, got %d file_refs, %d edges", refs, n)
	}
}
