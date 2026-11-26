package dartextractor

import (
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

const prefixedLib = `int handler(int x) => x;
bool get value => true;
set value(bool v) {}
int counter = 0;
class K { static int sm(int x) => x; }
void unrelated() {}
`

func TestDartImportPrefixedTearOffsAndGetterReads(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{
		"lib/src/a.dart": prefixedLib,
		"lib/b.dart": `import 'src/a.dart' as p;
final f = p.handler;
final v = p.value;
final n = p.counter;
Object tearOff() => p.handler;
void mapped(List<int> xs) { xs.map(p.handler); }
bool read() => p.value;
void write() { p.value = false; }
void loop(List<int> xs) { for (final x in xs) { consume(p.value); } }
int called() => p.handler(1);
Object namespace() => p.K.sm(1);
`,
	})
	if !fileRefRelation(all, "lib/b.dart", facts.RelNames, "lib/src.handler") {
		t.Error("module initializer tear-off through an import prefix recorded no use")
	}
	if !fileRefRelation(all, "lib/b.dart", facts.RelCalls, "lib/src.value") {
		t.Error("module initializer getter read through an import prefix recorded no use")
	}
	for name, rel := range map[string]facts.Relation{
		"lib.tearOff": {Kind: facts.RelNames, Target: "lib/src.handler"},
		"lib.mapped":  {Kind: facts.RelNames, Target: "lib/src.handler"},
		"lib.read":    {Kind: facts.RelCalls, Target: "lib/src.value"},
		"lib.write":   {Kind: facts.RelCalls, Target: "lib/src.value"},
		"lib.loop":    {Kind: facts.RelCalls, Target: "lib/src.value"},
		"lib.called":  {Kind: facts.RelCalls, Target: "lib/src.handler"},
	} {
		if f := findFact(all, facts.KindSymbol, name); !hasRelation(f, rel.Kind, rel.Target) {
			t.Errorf("%s: no %s edge to %s: %+v", name, rel.Kind, rel.Target, f)
		}
	}
	if f := findFact(all, facts.KindSymbol, "lib.called"); hasRelation(f, facts.RelNames, "lib/src.handler") {
		t.Errorf("an invocation through a prefix is a call, not a tear-off: %+v", f)
	}
	loop := findFact(all, facts.KindSymbol, "lib.loop")
	if calls, _ := loop.Props["calls_in_loop"].([]string); len(calls) == 0 || calls[len(calls)-1] != "lib/src.value" {
		t.Errorf("prefixed getter read in a loop is not per-iteration: %v", loop.Props["calls_in_loop"])
	}
	for i := range all {
		for _, r := range all[i].Relations {
			if r.Target == "lib/src.unrelated" || r.Target == "lib/src.counter" || r.Target == "lib/src.K" {
				t.Errorf("%s gained a %s edge to %s", all[i].Name, r.Kind, r.Target)
			}
		}
	}
}

func TestDartImportPrefixedValueScope(t *testing.T) {
	for _, use := range []string{
		// A local or parameter named like the prefix hides it.
		`Object run(Object p) => [p.handler, p.value];`,
		`Object run() { final p = Other(); return [p.handler, p.value]; }`,
		// A class member named like the prefix hides it inside the class.
		`class C { final Other p = Other(); Object run() => [p.handler, p.value]; }`,
		// A member of something else is not reached through the prefix.
		`Object run(Other o) => [o.p.handler, o.p.value];`,
	} {
		t.Run(use, func(t *testing.T) {
			all := dartReviewFacts(t, map[string]string{
				"lib/src/a.dart": prefixedLib,
				"lib/b.dart":     "import 'src/a.dart' as p;\nclass Other { Object handler = 1; bool value = false; Other get p => this; }\n" + use,
			})
			for i := range all {
				for _, r := range all[i].Relations {
					if r.Target == "lib/src.handler" || r.Target == "lib/src.value" {
						t.Errorf("a name not reached through the prefix bound to the import: %s -%s-> %s", all[i].Name, r.Kind, r.Target)
					}
				}
			}
		})
	}
	// A prefix that is not this file's, or an unprefixed import, binds nothing.
	for _, imp := range []string{`import 'src/a.dart' as q;`, `import 'src/a.dart' as p hide handler, value;`} {
		t.Run(imp, func(t *testing.T) {
			all := dartReviewFacts(t, map[string]string{
				"lib/src/a.dart": prefixedLib,
				"lib/b.dart":     imp + "\nObject run() => [p.handler, p.value];",
			})
			for i := range all {
				for _, r := range all[i].Relations {
					if r.Target == "lib/src.handler" || r.Target == "lib/src.value" {
						t.Errorf("a name the prefix does not make visible bound to it: %s -%s-> %s", all[i].Name, r.Kind, r.Target)
					}
				}
			}
		})
	}
	// A part reads through its library's prefixes; a name that is no prefix there binds nothing.
	parts := dartReviewFacts(t, map[string]string{
		"lib/src/a.dart": prefixedLib,
		"lib/host.dart":  "import 'src/a.dart' as p;\npart 'piece.dart';\n",
		"lib/piece.dart": "part of 'host.dart';\nObject pick() => p.handler;\nbool flag() => p.value;\nObject other() => [q.handler, q.value];\n",
	})
	for name, rel := range map[string]facts.Relation{
		"lib.pick": {Kind: facts.RelNames, Target: "lib/src.handler"},
		"lib.flag": {Kind: facts.RelCalls, Target: "lib/src.value"},
	} {
		if f := findFact(parts, facts.KindSymbol, name); !hasRelation(f, rel.Kind, rel.Target) {
			t.Errorf("part %s: no %s edge to %s: %+v", name, rel.Kind, rel.Target, f)
		}
	}
	if f := findFact(parts, facts.KindSymbol, "lib.other"); f == nil || len(f.Relations) > 0 {
		t.Errorf("a name that is no prefix of the library bound through one: %+v", f)
	}
	// Like a bare name, a prefixed read never binds to a bundled SDK signature.
	sdk := dartReviewFacts(t, map[string]string{"lib/b.dart": "import 'dart:io' as io;\nint code() => io.exitCode;\nvoid set() { io.exitCode = 2; }\n"})
	for i := range sdk {
		for _, r := range sdk[i].Relations {
			if strings.HasPrefix(r.Target, "sdk/") {
				t.Errorf("a prefixed SDK value read bound to its signature: %s -%s-> %s", sdk[i].Name, r.Kind, r.Target)
			}
		}
	}
}
