package dartextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

const crossFileUtil = `
bool get supportsWorkers => true;
String nameBuilder(String s) => s;
void unrelated() {}
`

func TestDartImportedTearOffsAndGetterReads(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{
		"lib/src/util.dart": crossFileUtil,
		"lib/use.dart": `import 'src/util.dart';
class Item { const Item({Object? builder}); }
final item = Item(builder: nameBuilder);
final hasWorker = supportsWorkers;
final named = Item(builder: () => nameBuilder('x'));
Object pick() => nameBuilder;
void probe(List<int> rows) { for (final r in rows) { consume(supportsWorkers); } }
`,
		"lib/host.dart": `part 'host_part.dart';
Object pickPart() => partHelper;
bool readPart() => partFlag;
`,
		"lib/host_part.dart": `part of 'host.dart';
void partHelper() {}
bool get partFlag => true;
set partFlag(bool v) {}
void writePart(List<Object> l) { partFlag = false; l[0] = partFlag; (partFlag) ? 1 : 2; }
`,
	})
	if !fileRefRelation(all, "lib/use.dart", facts.RelNames, "lib/src.nameBuilder") {
		t.Error("module initializer tear-off of an imported function recorded no use")
	}
	if !fileRefRelation(all, "lib/use.dart", facts.RelCalls, "lib/src.nameBuilder") {
		t.Error("module initializer invocation of an imported function recorded no use")
	}
	if !fileRefRelation(all, "lib/use.dart", facts.RelCalls, "lib/src.supportsWorkers") {
		t.Error("module initializer read of an imported getter recorded no use")
	}
	for name, rel := range map[string]facts.Relation{
		"lib.pick":     {Kind: facts.RelNames, Target: "lib/src.nameBuilder"},
		"lib.probe":    {Kind: facts.RelCalls, Target: "lib/src.supportsWorkers"},
		"lib.pickPart": {Kind: facts.RelNames, Target: "lib.partHelper"},
		"lib.readPart": {Kind: facts.RelCalls, Target: "lib.partFlag"},
	} {
		if f := findFact(all, facts.KindSymbol, name); !hasRelation(f, rel.Kind, rel.Target) {
			t.Errorf("%s: no %s edge to %s: %+v", name, rel.Kind, rel.Target, f)
		}
	}
	if f := findFact(all, facts.KindSymbol, "lib.writePart"); f == nil || len(f.Relations) == 0 {
		t.Errorf("a read beside a write of the getter recorded nothing: %+v", f)
	}
	written := dartReviewFacts(t, map[string]string{"lib/w.dart": "bool get flag => true;\nset flag(bool v) {}\nvoid write() { flag = false; }\n"})
	if f := findFact(written, facts.KindSymbol, "lib.write"); f == nil || !hasRelation(f, facts.RelCalls, "lib.flag") {
		t.Errorf("a write of a top-level accessor pair did not run its setter: %+v", f)
	}
	probe := findFact(all, facts.KindSymbol, "lib.probe")
	if loops, _ := probe.Props["calls_in_loop"].([]string); len(loops) == 0 || loops[len(loops)-1] != "lib/src.supportsWorkers" {
		t.Errorf("imported getter read in a loop is not per-iteration: %v", probe.Props["calls_in_loop"])
	}
	for i := range all {
		for _, r := range all[i].Relations {
			if r.Target == "lib/src.unrelated" {
				t.Errorf("%s gained an edge to a function nobody names", all[i].Name)
			}
		}
	}
}

func TestDartImportedValueScope(t *testing.T) {
	for _, use := range []string{
		`void run(Object nameBuilder, bool supportsWorkers) { consume(nameBuilder); consume(supportsWorkers); }`,
		`void run() { final nameBuilder = 1, supportsWorkers = 2; consume(nameBuilder); consume(supportsWorkers); }`,
		`final nameBuilder = 1; bool supportsWorkers = false; Object run() => [nameBuilder, supportsWorkers];`,
		`class C { final Object nameBuilder = 1; bool get supportsWorkers => false; Object run() => [nameBuilder, supportsWorkers]; }`,
		`Object run(Object o) => [o.nameBuilder, o.supportsWorkers, Item(nameBuilder: 1)];`,
		`final a = Item(nameBuilder: 1); final b = (Object nameBuilder) => nameBuilder(1); final c = o.nameBuilder(1);`,
	} {
		t.Run(use, func(t *testing.T) {
			all := dartReviewFacts(t, map[string]string{
				"lib/src/util.dart": crossFileUtil,
				"lib/use.dart":      "import 'src/util.dart';\nclass Item { const Item({Object? nameBuilder}); }\n" + use,
			})
			for i := range all {
				for _, r := range all[i].Relations {
					if r.Target == "lib/src.nameBuilder" || r.Target == "lib/src.supportsWorkers" {
						t.Errorf("shadowed or member name bound to the import: %s -%s-> %s", all[i].Name, r.Kind, r.Target)
					}
					if all[i].Kind == facts.KindFileRef && r.Kind == facts.RelCalls && r.Target == "lib.Item.Item" {
						t.Errorf("a file-scope construction became a call: %+v", r)
					}
				}
			}
		})
	}
	for _, imp := range []string{`import 'src/util.dart' hide nameBuilder, supportsWorkers;`, `import 'src/util.dart' as u;`, ``} {
		t.Run(imp, func(t *testing.T) {
			all := dartReviewFacts(t, map[string]string{
				"lib/src/util.dart": crossFileUtil,
				"lib/use.dart":      imp + "\nObject run() => [nameBuilder, supportsWorkers];",
			})
			for i := range all {
				for _, r := range all[i].Relations {
					if r.Target == "lib/src.nameBuilder" || r.Target == "lib/src.supportsWorkers" {
						t.Errorf("a name the import does not make visible bound to it: %s -%s-> %s", all[i].Name, r.Kind, r.Target)
					}
				}
			}
		})
	}
	all := dartReviewFacts(t, map[string]string{
		"lib/src/util.dart": crossFileUtil,
		"lib/use.dart":      "import 'src/util.dart';\nObject run() => nameBuilder('x');",
	})
	if f := findFact(all, facts.KindSymbol, "lib.run"); hasRelation(f, facts.RelNames, "lib/src.nameBuilder") || !hasRelation(f, facts.RelCalls, "lib/src.nameBuilder") {
		t.Errorf("an invocation is a call, not a tear-off: %+v", f)
	}
}

func TestDartModuleInitializerGetterReadsAndCallMethod(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{"lib/state.dart": `
Object get _store => 1;
Object _handle(Object? s) => s;
final a = _store;
final b = _handle.call(1);
final c = [for (final _store in const [1]) _store];
`})
	if !fileRefRelation(all, "lib/state.dart", facts.RelCalls, "lib._store") {
		t.Error("module initializer getter read recorded no use")
	}
	if !fileRefRelation(all, "lib/state.dart", facts.RelCalls, "lib._handle") {
		t.Error("module initializer .call on a file function recorded no use")
	}
	shadowed := dartReviewFacts(t, map[string]string{"lib/state.dart": `
Object _handle(Object? s) => s;
final b = (Object _handle) => _handle.call(1);
final c = other._handle.call(1);
`})
	if fileRefRelation(shadowed, "lib/state.dart", facts.RelCalls, "lib._handle") {
		t.Error("shadowed or member .call bound to the file function")
	}
}

func TestDartPlainWriteRunsTheSetter(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{
		"lib/src/prefs.dart": `bool get darkMode => false;
set darkMode(bool v) {}
`,
		"lib/state.dart": `import 'src/prefs.dart';
int _phase = 0;
int get phase => _phase;
set phase(int v) { _phase = v; }
set onlySet(int v) {}
int plainTop = 0;
void writeTop() { phase = 1; onlySet = 2; plainTop = 3; darkMode = true; }
int readTop() => phase;
class Base {
  int _value = 0;
  int get value => _value;
  set value(int v) { _value = v; }
  int counter = 0;
}
class Box extends Base {
  bool _open = false;
  bool get open => _open;
  set open(bool v) { _open = v; }
  int size = 0;
  void toggle() { open = !open; }
  void resize() { size = 3; counter = 4; }
  void inherited() { value = 5; }
}
class Fragment { Fragment? next; }
mixin Typed { int get kind => 0; set kind(int v) {} }
mixin Declares { int get kind; }
class Decl with Typed, Declares {
  @override
  int get kind => 1;
  void build() { kind = 2; }
}
class Lens extends Base {
  @override
  int get value => 1;
  void reset() { value = 0; }
}
class Sub extends Fragment {
  @override
  Sub? get next => null;
  void link(Sub s) { next = s; }
}
`,
	})
	for name, target := range map[string]string{
		"lib.writeTop":      "lib.phase",
		"lib.readTop":       "lib.phase",
		"lib.Box.toggle":    "lib.Box.open",
		"lib.Box.inherited": "lib.Base.value",
		"lib.Lens.reset":    "lib.Base.value",
		"lib.Decl.build":    "lib.Typed.kind",
	} {
		if f := findFact(all, facts.KindSymbol, name); !hasRelation(f, facts.RelCalls, target) {
			t.Errorf("%s: no calls edge to %s: %+v", name, target, f)
		}
	}
	for name, target := range map[string]string{"lib.writeTop": "lib/src.darkMode"} {
		if f := findFact(all, facts.KindSymbol, name); !hasRelation(f, facts.RelCalls, target) {
			t.Errorf("%s: write of an imported setter recorded no call to %s: %+v", name, target, f)
		}
	}
	// A top-level setter with no getter has no symbol to carry the edge.
	if f := findFact(all, facts.KindSymbol, "lib.writeTop"); f == nil || len(f.Relations) != 2 {
		t.Errorf("top-level writes: want the two accessor calls only: %+v", f)
	}
	for _, name := range []string{"lib.Box.resize"} {
		if f := findFact(all, facts.KindSymbol, name); f == nil || len(f.Relations) > 1 {
			t.Errorf("%s: a plain field write recorded a use: %+v", name, f)
		}
	}
	if f := findFact(all, facts.KindSymbol, "lib.Sub.link"); f == nil || hasRelation(f, facts.RelCalls, "lib.Sub.next") || hasRelation(f, facts.RelCalls, "lib.Fragment.next") {
		t.Errorf("an overriding getter's inherited field write recorded a use: %+v", f)
	}
	if f := findFact(all, facts.KindSymbol, "lib.writeTop"); hasRelation(f, facts.RelCalls, "lib.plainTop") {
		t.Errorf("a top-level variable write recorded a use: %+v", f)
	}
}
