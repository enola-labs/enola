package dartextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

const prefixedStaticsLib = `class K {
  static int sm(int x) => x;
  static int get g => 1;
  int im(int x) => x;
  static int field = 0;
}
int sm(int x) => x;
`

func TestDartImportPrefixedStaticMembers(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{
		"lib/src/a.dart": prefixedStaticsLib,
		"lib/b.dart": `import 'src/a.dart' as p;
final f = p.K.sm;
final v = p.K.g;
Object tearOff() => p.K.sm;
int read() => p.K.g;
void loop(List<int> xs) { for (final x in xs) { consume(p.K.g); } }
int called() => p.K.sm(1);
Object field() => p.K.field;
`,
	})
	if !fileRefRelation(all, "lib/b.dart", facts.RelNames, "lib/src.K.sm") {
		t.Error("module initializer static tear-off through an import prefix recorded no use")
	}
	if !fileRefRelation(all, "lib/b.dart", facts.RelCalls, "lib/src.K.g") {
		t.Error("module initializer static getter read through an import prefix recorded no use")
	}
	for name, rel := range map[string]facts.Relation{
		"lib.tearOff": {Kind: facts.RelNames, Target: "lib/src.K.sm"},
		"lib.read":    {Kind: facts.RelCalls, Target: "lib/src.K.g"},
		"lib.loop":    {Kind: facts.RelCalls, Target: "lib/src.K.g"},
		"lib.called":  {Kind: facts.RelCalls, Target: "lib/src.K.sm"},
	} {
		if f := findFact(all, facts.KindSymbol, name); !hasRelation(f, rel.Kind, rel.Target) {
			t.Errorf("%s: no %s edge to %s: %+v", name, rel.Kind, rel.Target, f)
		}
	}
	if f := findFact(all, facts.KindSymbol, "lib.called"); hasRelation(f, facts.RelNames, "lib/src.K.sm") {
		t.Errorf("an invocation through a prefix is a call, not a tear-off: %+v", f)
	}
	loop := findFact(all, facts.KindSymbol, "lib.loop")
	if calls, _ := loop.Props["calls_in_loop"].([]string); len(calls) == 0 || calls[len(calls)-1] != "lib/src.K.g" {
		t.Errorf("prefixed static getter read in a loop is not per-iteration: %v", loop.Props["calls_in_loop"])
	}
	for i := range all {
		for _, r := range all[i].Relations {
			if r.Kind != facts.RelDeclares && (r.Target == "lib/src.sm" || r.Target == "lib/src.K.field" || r.Target == "lib/src.K.im") {
				t.Errorf("%s gained a %s edge to %s", all[i].Name, r.Kind, r.Target)
			}
		}
	}
}

func TestDartImportPrefixedStaticMemberScope(t *testing.T) {
	for _, tc := range []struct{ imp, use string }{
		// An instance member is not reached through the class name.
		{`import 'src/a.dart' as p;`, `Object run() => p.K.im;`},
		// A prefix that is not this file's, or hides the class, binds nothing.
		{`import 'src/a.dart' as q;`, `Object run() => [p.K.sm, p.K.g];`},
		{`import 'src/a.dart' as p hide K;`, `Object run() => [p.K.sm, p.K.g];`},
		// A local named like the prefix hides it.
		{`import 'src/a.dart' as p;`, `Object run(Object p) => [p.K.sm, p.K.g];`},
	} {
		t.Run(tc.imp+tc.use, func(t *testing.T) {
			all := dartReviewFacts(t, map[string]string{
				"lib/src/a.dart": prefixedStaticsLib,
				"lib/b.dart":     tc.imp + "\n" + tc.use,
			})
			for i := range all {
				for _, r := range all[i].Relations {
					if r.Kind != facts.RelDeclares && (r.Target == "lib/src.K.sm" || r.Target == "lib/src.K.g" || r.Target == "lib/src.K.im") {
						t.Errorf("a member not reached through the prefix bound to it: %s -%s-> %s", all[i].Name, r.Kind, r.Target)
					}
				}
			}
		})
	}
	// A part reads through its library's prefixes.
	parts := dartReviewFacts(t, map[string]string{
		"lib/src/a.dart": prefixedStaticsLib,
		"lib/host.dart":  "import 'src/a.dart' as p;\npart 'piece.dart';\n",
		"lib/piece.dart": "part of 'host.dart';\nObject pick() => p.K.sm;\nint flag() => p.K.g;\n",
	})
	for name, rel := range map[string]facts.Relation{
		"lib.pick": {Kind: facts.RelNames, Target: "lib/src.K.sm"},
		"lib.flag": {Kind: facts.RelCalls, Target: "lib/src.K.g"},
	} {
		if f := findFact(parts, facts.KindSymbol, name); !hasRelation(f, rel.Kind, rel.Target) {
			t.Errorf("part %s: no %s edge to %s: %+v", name, rel.Kind, rel.Target, f)
		}
	}
}

const prefixedCtorLib = `class Foo { const Foo(); const Foo.named(); }
class Bar { Bar(); }
class Baz {}
Baz makeBaz() => Baz();
`

func TestDartImportPrefixedConstructions(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{
		"lib/src/a.dart": prefixedCtorLib,
		"lib/b.dart": `import 'src/a.dart' as p;
class Foo {}
class Baz {}
void defaults([Object o = const p.Foo()]) {}
final x = p.Baz();
final y = new p.Bar();
Object newBody() => new p.Bar();
Object constBody() => const p.Foo();
Object implicitBody() => p.Baz();
Object explicitBody() => p.Bar();
class C {
  final Object field = p.Foo.named();
}
`,
	})
	if !fileRefRelation(all, "lib/b.dart", facts.RelInstantiates, "lib/src.Baz") {
		t.Error("file-scope construction through an import prefix recorded no instantiation")
	}
	// A class-body field initializer records on the file reference too (v738).
	if !fileRefRelation(all, "lib/b.dart", facts.RelInstantiates, "lib/src.Bar") {
		t.Error("file-scope new through an import prefix did not resolve through it")
	}
	for name, typ := range map[string]string{
		"lib.defaults":     "lib/src.Foo",
		"lib.newBody":      "lib/src.Bar",
		"lib.constBody":    "lib/src.Foo",
		"lib.implicitBody": "lib/src.Baz",
		"lib.explicitBody": "lib/src.Bar",
	} {
		f := findFact(all, facts.KindSymbol, name)
		if !hasRelation(f, facts.RelInstantiates, typ) {
			t.Errorf("%s: no instantiates %s: %+v", name, typ, f)
		}
	}
	if f := findFact(all, facts.KindSymbol, "lib.explicitBody"); !hasRelation(f, facts.RelCalls, "lib/src.Bar.Bar") {
		t.Errorf("an explicit constructor through a prefix lost its call: %+v", f)
	}
	// The same-named local classes are not what the prefixed spelling builds.
	for i := range all {
		for _, r := range all[i].Relations {
			if r.Kind == facts.RelInstantiates && (r.Target == "lib.Foo" || r.Target == "lib.Baz") {
				t.Errorf("%s: a prefixed construction bound to a same-named local class %s", all[i].Name, r.Target)
			}
		}
	}
}

func TestDartImportPrefixedConstructionScope(t *testing.T) {
	for _, tc := range []struct{ imp, use string }{
		// A local or parameter named like the prefix is a receiver, not a prefix.
		{`import 'src/a.dart' as p;`, `Object run(Holder p) => p.Baz();`},
		{`import 'src/a.dart' as p;`, `Object run(Holder h) { final p = h; return p.Baz(); }`},
		// A prefix that is not this file's, or a function behind the prefix, builds nothing.
		{`import 'src/a.dart' as q;`, `Object run() => p.Baz();`},
		{`import 'src/a.dart' as p;`, `Object run() => p.Missing();`},
	} {
		t.Run(tc.imp+tc.use, func(t *testing.T) {
			all := dartReviewFacts(t, map[string]string{
				"lib/src/a.dart": prefixedCtorLib,
				"lib/b.dart":     tc.imp + "\nclass Holder { Object Baz() => 1; }\n" + tc.use,
			})
			for i := range all {
				if all[i].File != "lib/b.dart" {
					continue
				}
				for _, r := range all[i].Relations {
					if r.Kind == facts.RelInstantiates || r.Kind == relDartPrefixedCtor {
						t.Errorf("%s: unexpected %s %s", all[i].Name, r.Kind, r.Target)
					}
				}
			}
		})
	}
	// An unresolved prefixed new or const keeps the bare class name it recorded before.
	ext := dartReviewFacts(t, map[string]string{"lib/b.dart": "import 'package:other/other.dart' as o;\nObject run() => const o.Thing();\n"})
	if f := findFact(ext, facts.KindSymbol, "lib.run"); !hasRelation(f, facts.RelInstantiates, "Thing") {
		t.Errorf("an unresolved prefixed construction changed spelling: %+v", f)
	}
}
