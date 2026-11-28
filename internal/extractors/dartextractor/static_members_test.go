package dartextractor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// dartRepoFacts runs the whole extractor over files written to a temporary
// repository, so part files are walked again with their library's imports.
func dartRepoFacts(t *testing.T, files map[string]string) []facts.Fact {
	t.Helper()
	root := t.TempDir()
	var paths []string
	for name, src := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, name)
	}
	all, err := New().Extract(context.Background(), root, paths)
	if err != nil {
		t.Fatal(err)
	}
	return all
}

const staticMembersLib = `class K {
  static int sm(int x) => x;
  static int get g => 1;
  static set s(int v) {}
  static int get gs => 1;
  static set gs(int v) {}
  int im(int x) => x;
  int get ig => 1;
  set is_(int v) {}
  static int field = 0;
}
`

func TestDartBareClassStaticMemberReads(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{
		"lib/src/a.dart": staticMembersLib,
		"lib/b.dart": `import 'src/a.dart';
final f = K.sm;
final v = K.g;
Object tearOff() => K.sm;
int read() => K.g;
int chained() => K.g.abs();
void loop(List<int> xs) { for (final x in xs) { consume(K.g); } }
int called() => K.sm(1);
Object others() => [K.im, K.ig, K.field];
`,
	})
	if !fileRefRelation(all, "lib/b.dart", facts.RelNames, "lib/src.K.sm") {
		t.Error("module initializer static tear-off recorded no use")
	}
	if !fileRefRelation(all, "lib/b.dart", facts.RelCalls, "lib/src.K.g") {
		t.Error("module initializer static getter read recorded no use")
	}
	for name, rel := range map[string]facts.Relation{
		"lib.tearOff": {Kind: facts.RelNames, Target: "lib/src.K.sm"},
		"lib.read":    {Kind: facts.RelCalls, Target: "lib/src.K.g"},
		"lib.chained": {Kind: facts.RelCalls, Target: "lib/src.K.g"},
		"lib.loop":    {Kind: facts.RelCalls, Target: "lib/src.K.g"},
		"lib.called":  {Kind: facts.RelCalls, Target: "lib/src.K.sm"},
	} {
		if f := findFact(all, facts.KindSymbol, name); !hasRelation(f, rel.Kind, rel.Target) {
			t.Errorf("%s: no %s edge to %s: %+v", name, rel.Kind, rel.Target, f)
		}
	}
	if f := findFact(all, facts.KindSymbol, "lib.called"); hasRelation(f, facts.RelNames, "lib/src.K.sm") {
		t.Errorf("an invocation is a call, not a tear-off: %+v", f)
	}
	loop := findFact(all, facts.KindSymbol, "lib.loop")
	if calls, _ := loop.Props["calls_in_loop"].([]string); len(calls) == 0 || calls[len(calls)-1] != "lib/src.K.g" {
		t.Errorf("static getter read in a loop is not per-iteration: %v", loop.Props["calls_in_loop"])
	}
	for i := range all {
		for _, r := range all[i].Relations {
			if r.Kind != facts.RelDeclares && (r.Target == "lib/src.K.im" || r.Target == "lib/src.K.ig" || r.Target == "lib/src.K.field") {
				t.Errorf("%s gained a %s edge to an instance member or field %s", all[i].Name, r.Kind, r.Target)
			}
		}
	}
}

func TestDartBareClassStaticMemberScope(t *testing.T) {
	for _, tc := range []struct{ name, use string }{
		// A local or parameter named like the class is a value, not the class.
		{"parameter", `Object run(Object K) => [K.sm, K.g];`},
		{"local", `Object run() { final K = 1; return [K.sm, K.g]; }`},
		// A class this file does not see binds nothing.
		{"unimported", `Object run() => [Q.sm, Q.g];`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			all := dartReviewFacts(t, map[string]string{
				"lib/src/a.dart": staticMembersLib + "class Q { static int sm(int x) => x; static int get g => 1; }\n",
				"lib/c/q.dart":   "class Q { static int sm(int x) => x; static int get g => 1; }\n",
				"lib/b.dart":     "import 'src/a.dart' hide Q;\n" + tc.use,
			})
			for i := range all {
				if all[i].File != "lib/b.dart" {
					continue
				}
				for _, r := range all[i].Relations {
					if r.Kind != facts.RelDeclares && r.Kind != facts.RelImports {
						t.Errorf("%s: unexpected %s %s", all[i].Name, r.Kind, r.Target)
					}
				}
			}
		})
	}
}

func TestDartStaticSetterWrites(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{
		"lib/src/a.dart": staticMembersLib,
		"lib/b.dart": `import 'src/a.dart' as p;
void prefixed() { p.K.s = 1; }
void prefixedPair() { p.K.gs = 1; }
void prefixedCompound() { p.K.gs += 1; }
void prefixedField() { p.K.field = 1; }
`,
		"lib/c.dart": `import 'src/a.dart';
void bare() { K.s = 1; }
void barePair() { K.gs = 1; }
void bareField() { K.field = 1; }
void instanceSetter() { K.is_ = 1; }
`,
	})
	for name, target := range map[string]string{
		"lib.prefixed":         "lib/src.K.s",
		"lib.prefixedPair":     "lib/src.K.gs",
		"lib.prefixedCompound": "lib/src.K.gs",
		"lib.bare":             "lib/src.K.s",
		"lib.barePair":         "lib/src.K.gs",
	} {
		if f := findFact(all, facts.KindSymbol, name); !hasRelation(f, facts.RelCalls, target) {
			t.Errorf("%s: no call of the static setter %s: %+v", name, target, f)
		}
	}
	for _, name := range []string{"lib.prefixedField", "lib.bareField", "lib.instanceSetter"} {
		f := findFact(all, facts.KindSymbol, name)
		for _, r := range f.Relations {
			if r.Kind == facts.RelCalls {
				t.Errorf("%s: a field or instance member write gained a call %s", name, r.Target)
			}
		}
	}
}

func TestDartPartFileConstructionThroughLibraryPrefix(t *testing.T) {
	all := dartRepoFacts(t, map[string]string{
		"pubspec.yaml":     "name: app\n",
		"lib/src/foo.dart": "class Foo { const Foo(); Foo.named(); }\nclass Bar {}\n",
		"lib/x/foo.dart":   "class Foo { const Foo(); }\nclass Bar {}\n",
		"lib/host.dart":    "import 'src/foo.dart' as p;\nimport 'x/foo.dart';\npart 'piece.dart';\n",
		"lib/piece.dart": `part of 'host.dart';
Object build() => p.Foo();
Object buildConst() => const p.Foo();
Object buildNew() => new p.Foo();
Object buildNamed() => p.Foo.named();
final top = p.Bar();
class Holder { Holder(p.Bar bar); }
`,
	})
	for _, name := range []string{"lib.build", "lib.buildConst", "lib.buildNew", "lib.buildNamed"} {
		f := findFact(all, facts.KindSymbol, name)
		if !hasRelation(f, facts.RelInstantiates, "lib/src.Foo") {
			t.Errorf("%s: a construction through the library's prefix did not bind through it: %+v", name, f)
		}
		if hasRelation(f, facts.RelInstantiates, "lib/x.Foo") {
			t.Errorf("%s: bound to the unprefixed Foo: %+v", name, f)
		}
	}
	if !fileRefRelation(all, "lib/piece.dart", facts.RelInstantiates, "lib/src.Bar") {
		t.Error("file-scope construction in a part did not bind through the library's prefix")
	}
	if f := findFact(all, facts.KindSymbol, "lib.Holder.Holder"); !hasRelation(f, facts.RelInjects, "lib/src.Bar") {
		t.Errorf("a parameter type in a part did not bind through the library's prefix: %+v", f)
	}
}

func TestDartPrefixedParameterTypeInjects(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{
		"lib/src/a.dart": "class Options { const Options(); }\nclass Store {}\n",
		"lib/x/a.dart":   "class Options {}\nclass Store {}\n",
		"lib/b.dart": `import 'src/a.dart' as common;
import 'package:other/other.dart' as ext;
class Verifier {
  Verifier(common.Store store, ext.Thing thing);
  void validate({common.Options options = const common.Options()}) {}
}
`,
	})
	ctor := findFact(all, facts.KindSymbol, "lib.Verifier.Verifier")
	if !hasRelation(ctor, facts.RelInjects, "lib/src.Store") {
		t.Errorf("constructor parameter through a prefix did not bind through it: %+v", ctor)
	}
	// A prefixed type that binds nothing keeps the bare spelling it had before.
	if !hasRelation(ctor, facts.RelInjects, "Thing") {
		t.Errorf("an unresolved prefixed parameter type changed spelling: %+v", ctor)
	}
	if m := findFact(all, facts.KindSymbol, "lib.Verifier.validate"); !hasRelation(m, facts.RelInjects, "lib/src.Options") {
		t.Errorf("method parameter through a prefix did not bind through it: %+v", m)
	}
	for i := range all {
		for _, r := range all[i].Relations {
			if r.Kind == facts.RelInjects && (r.Target == "lib/x.Options" || r.Target == "lib/x.Store" || r.Target == "common") {
				t.Errorf("%s: injects %s", all[i].Name, r.Target)
			}
		}
	}
}

const conditionalStub = "bool get isTest => false;\nvoid save(String s) {}\nObject load() => 1;\nset mode(int v) {}\nint get mode => 0;\n"
const conditionalIO = "bool get isTest => true;\nvoid save(String s) {}\nObject load() => 2;\nset mode(int v) {}\nint get mode => 1;\n"

func TestDartConditionalImportCreditsEachAlternative(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{
		"lib/io/stub.dart": conditionalStub,
		"lib/io/real.dart": conditionalIO,
		"lib/use.dart": `import 'io/stub.dart' if (dart.library.io) 'io/real.dart' as file_io;
bool check() => file_io.isTest;
void store() { file_io.save('x'); }
Object pick() => file_io.load;
void set() { file_io.mode = 2; }
`,
		"lib/bare.dart": `import 'io/stub.dart' if (dart.library.io) 'io/real.dart';
bool check2() => isTest;
void store2() { save('x'); }
`,
		// A library that conditionally re-exports, imported plainly.
		"lib/io/ops.dart": "export 'stub.dart' if (dart.library.io) 'real.dart';\n",
		"lib/viaexport.dart": `import 'io/ops.dart' as ops;
bool check3() => ops.isTest;
`,
	})
	for name, rels := range map[string][]facts.Relation{
		"lib.check": {{Kind: facts.RelCalls, Target: "lib/io/stub.isTest"}, {Kind: facts.RelCalls, Target: "lib/io/real.isTest"}},
		// Functions of one name in one directory share it, so one edge reaches both.
		"lib.store":  {{Kind: facts.RelCalls, Target: "lib/io.save"}},
		"lib.pick":   {{Kind: facts.RelNames, Target: "lib/io.load"}},
		"lib.set":    {{Kind: facts.RelCalls, Target: "lib/io/stub.mode"}, {Kind: facts.RelCalls, Target: "lib/io/real.mode"}},
		"lib.check3": {{Kind: facts.RelCalls, Target: "lib/io/stub.isTest"}, {Kind: facts.RelCalls, Target: "lib/io/real.isTest"}},
		"lib.check2": {{Kind: facts.RelCalls, Target: "lib/io/stub.isTest"}, {Kind: facts.RelCalls, Target: "lib/io/real.isTest"}},
		"lib.store2": {{Kind: facts.RelCalls, Target: "lib/io.save"}},
	} {
		f := findFact(all, facts.KindSymbol, name)
		for _, rel := range rels {
			if !hasRelation(f, rel.Kind, rel.Target) {
				t.Errorf("%s: no %s edge to %s: %+v", name, rel.Kind, rel.Target, f)
			}
		}
	}
}

func TestDartAmbiguityOutsideOneConditionalImportStaysUnbound(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{
		"lib/io/stub.dart": conditionalStub,
		"lib/io/real.dart": conditionalIO,
		// Two ordinary imports declaring one name: a compile error, not alternatives.
		"lib/two.dart": `import 'io/stub.dart' as p;
import 'io/real.dart' as p;
bool check() => p.isTest;
void store() { p.save('x'); }
`,
	})
	for _, name := range []string{"lib.check", "lib.store"} {
		f := findFact(all, facts.KindSymbol, name)
		for _, r := range f.Relations {
			if r.Kind == facts.RelCalls && (r.Target == "lib/io/stub.isTest" || r.Target == "lib/io/real.isTest" || r.Target == "lib/io.save") {
				t.Errorf("%s: two plain imports declaring one name bound %s", name, r.Target)
			}
		}
	}
}
