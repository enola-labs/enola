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
`,
	})
	if !fileRefRelation(all, "lib/use.dart", facts.RelNames, "lib/src.nameBuilder") {
		t.Error("module initializer tear-off of an imported function recorded no use")
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
