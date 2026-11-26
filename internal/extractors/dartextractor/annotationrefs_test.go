package dartextractor

import (
	"context"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/orphans"
)

func relationSet(all []facts.Fact, kind string) map[string]bool {
	got := map[string]bool{}
	for _, f := range all {
		for _, r := range f.Relations {
			if r.Kind == kind {
				got[f.Kind+" "+f.Name+" -> "+r.Target] = true
			}
		}
	}
	return got
}

func orphanTitles(t *testing.T, all []facts.Fact) string {
	t.Helper()
	store := facts.NewStore()
	store.Add(all...)
	insights, err := orphans.NewExplainer().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, in := range insights {
		titles = append(titles, in.Title)
	}
	return strings.Join(titles, "\n") + "\n"
}

// TestDartDefaultConstructionsAndAbstractDefaults pins parameter defaults outside a
// body: `= const Opt()` builds Opt, and an abstract method's defaults are read too.
func TestDartDefaultConstructionsAndAbstractDefaults(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{"lib/a.dart": `
class Opt {
  const Opt([int x = 0]);
}
class Gadget {
  const Gadget();
}
class $Gen {
  const $Gen();
}
class Host {
  void run({Object o = const Opt(1)}) {}
  void prefixed({Object o = const i0.Gadget(), Object n = new i0.Opt()}) {}
  void builtin({bool b = const bool.fromEnvironment('x'), Object g = const $Gen()}) {}
}
abstract class Api {
  void fetch({Object g = const Gadget(), Object cb = _cb});
}
void _cb() {}
`})
	inst := relationSet(all, facts.RelInstantiates)
	for _, want := range []string{
		"symbol lib.Host.run -> lib.Opt",
		"symbol lib.Api.fetch -> lib.Gadget",
		"symbol lib.Host.prefixed -> lib.Gadget",
		"symbol lib.Host.prefixed -> lib.Opt",
		"symbol lib.Host.builtin -> sdk/lib/core.bool",
		"symbol lib.Host.builtin -> lib.$Gen",
	} {
		if !inst[want] {
			t.Errorf("missing instantiates %q; got %v", want, inst)
		}
	}
	if inst["symbol lib.Host.prefixed -> i0"] {
		t.Errorf("an import prefix read as the class built: %v", inst)
	}
	if names := relationSet(all, facts.RelNames); !names["symbol lib.Api.fetch -> lib._cb"] {
		t.Errorf("abstract default tear-off not recorded; got %v", names)
	}
	titles := orphanTitles(t, all)
	for _, dead := range []string{" lib.Opt\n", " lib.Gadget\n", " lib._cb\n"} {
		if strings.Contains(titles, dead) {
			t.Errorf("%q reported as dead:\n%s", dead, titles)
		}
	}
}

// TestDartFieldInitializerConstructions pins a construction in a class-body field
// initializer: the file records the class it builds, and a same-named field or a
// function is not mistaken for a class.
func TestDartFieldInitializerConstructions(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{"lib/a.dart": `
class Controller {
  Controller({int a = 0});
}
class Pager<T> {
  Pager(T x);
}
Object Make() => 1;
class Host {
  late final _c = Controller(a: 1);
  final p = Pager<int>(1);
  final m = Make();
  final f = () { Object Local() => 1; return Local(); };
}
`})
	inst := relationSet(all, facts.RelInstantiates)
	for _, want := range []string{
		"file_ref lib/a.dart -> lib.Controller",
		"file_ref lib/a.dart -> lib.Pager",
	} {
		if !inst[want] {
			t.Errorf("missing instantiates %q; got %v", want, inst)
		}
	}
	for edge := range inst {
		if strings.HasSuffix(edge, "Make") || strings.HasSuffix(edge, "Local") {
			t.Errorf("function invocation read as a construction: %s", edge)
		}
	}
}

// TestDartAnnotationArgumentReferences pins what metadata arguments name: a table
// class listed in `@DriftDatabase(tables: [...])` in another file of the library, a
// function passed to `@JsonKey`, and a construction in `@Default(...)`. An unknown
// capitalised name records nothing.
func TestDartAnnotationArgumentReferences(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{
		"lib/tables.dart": `
class Users {}
class Posts {}
class Mode { const Mode(); }
`,
		"lib/db.dart": `
import 'tables.dart';

@DriftDatabase(tables: [Users, Unknown], daos: [])
class Db {}

class Row {
  @JsonKey(fromJson: _parse)
  final int id;
  @Default(Mode()) final Object mode;
  Row(this.id, this.mode);
  static int _parse(Object v) => 1;
}

@Riverpod(dependencies: [Posts])
int counter() => 1;

@Box(counter())
class NotConstant {}
`,
	})
	names := relationSet(all, facts.RelNames)
	for _, want := range []string{
		"symbol lib.Db -> lib.Users",
		"file_ref lib/db.dart -> lib.Posts",
		"file_ref lib/db.dart -> lib.Row._parse",
	} {
		if !names[want] {
			t.Errorf("missing names %q; got %v", want, names)
		}
	}
	if inst := relationSet(all, facts.RelInstantiates); !inst["file_ref lib/db.dart -> lib.Mode"] {
		t.Errorf("annotation construction not recorded; got %v", inst)
	}
	for _, f := range all {
		for _, r := range f.Relations {
			if r.Kind == "dart_type_ref" || strings.Contains(r.Target, "Unknown") || (r.Kind == facts.RelCalls && strings.HasSuffix(r.Target, "counter")) {
				t.Errorf("unresolved annotation name leaked: %s %s -> %s %s", f.Kind, f.Name, r.Kind, r.Target)
			}
		}
	}
	titles := orphanTitles(t, all)
	for _, dead := range []string{" lib.Users\n", " lib.Posts\n", " lib.Mode\n", " lib.Row._parse\n"} {
		if strings.Contains(titles, dead) {
			t.Errorf("%q reported as dead:\n%s", dead, titles)
		}
	}
}
