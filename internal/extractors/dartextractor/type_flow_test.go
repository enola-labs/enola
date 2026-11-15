package dartextractor

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/perf"
)

func TestDartTypeFlowReceivers(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{
		"lib/service.dart": `import 'package:http/http.dart';
 class Service {Service();void load() {httpClient.get('/');}}
 class Registry { static final Service _one=Service(); static Service get instance=>_one; static Service create()=>Service(); }
 final Service globalService=Service(); final serviceAlias=globalService;
 Service openService()=>Service(); Future<Service> openAsync() async=>Service();
 class Base { final Service _service=Service(); Service get service=>_service; }
 class Child extends Base {void run(){service.load();}}
 `,
		"lib/run.dart": `import 'service.dart'; import 'service.dart' as api;
 void singleton(rows) {for(final row in rows){Registry.instance.load();}}
 void globalAlias(rows) {final a=serviceAlias; final b=a;for(final row in rows){b.load();}}
 void factory(rows) {final a=Registry.create();for(final row in rows){a.load();}}
 void returned(rows) {openService().load();}
 Future awaited(rows) async {final a=await openAsync();for(final row in rows){a.load();}}
 void prefixed(rows) {final a=api.Service();for(final row in rows){a.load();}}
 `})
	for _, name := range []string{"lib.singleton", "lib.globalAlias", "lib.factory", "lib.returned", "lib.awaited", "lib.prefixed", "lib.Child.run"} {
		f := findFact(all, facts.KindSymbol, name)
		if !hasReviewCall(f, "lib.Service.load") || !f.PropBool("performs_io") {
			t.Errorf("%s did not propagate its actual receiver: %+v", name, f)
		}
	}
	for _, name := range []string{"lib.singleton", "lib.globalAlias", "lib.factory", "lib.awaited", "lib.prefixed"} {
		f := findFact(all, facts.KindSymbol, name)
		calls, _ := f.Props["calls_in_loop"].([]string)
		if !slices.Contains(calls, "lib.Service.load") {
			t.Errorf("%s lost canonical loop target: %+v", name, f)
		}
	}
	store := facts.NewStore()
	store.Add(all...)
	found := map[string]bool{}
	for _, f := range perf.Analyze(store) {
		if f.Kind == "call-in-loop" {
			found[f.Symbol] = true
		}
	}
	for _, name := range []string{"lib.singleton", "lib.globalAlias", "lib.factory", "lib.awaited", "lib.prefixed"} {
		if !found[name] {
			t.Errorf("%s lost its actual IO loop finding", name)
		}
	}
}
func TestDartTypeFlowPreservesFalseMatchRemovals(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{
		"lib/run.dart": `import 'package:foreign/git.dart'; import 'dart:math' as math;
 class GradleProject {void runCommand() {}}
 class ReviewMonthKey {int compareTo(other)=>0;}
 class Input {set max(int x) {}}
 class Wrong {void load() {}}
 dynamic unknown()=>external;
 void run(dynamic gitDir,dynamic value) {gitDir.runCommand(); math.max(1,2); value.compareTo(1); final a=unknown(); a.load();}
 void outer() {void toIdentifier() {} toIdentifier();}
 `,
		"other/bench.dart": `void toIdentifier() {}`})
	f := findFact(all, facts.KindSymbol, "lib.run")
	for _, bad := range []string{"lib.GradleProject.runCommand", "lib.ReviewMonthKey.compareTo", "lib.Input.max", "lib.Wrong.load"} {
		if hasReviewCall(f, bad) {
			t.Errorf("restored false global match %s", bad)
		}
	}
	if hasReviewCall(findFact(all, facts.KindSymbol, "lib.outer"), "other.toIdentifier") {
		t.Fatal("local helper rebound to an unrelated library")
	}
}
func TestDartTypeFlowDeclaredReturnAndShadowing(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{"lib/a.dart": `
 class A {void m() {}} class B {void m() {}}
 A get global=>A(); A get a=>A();
 class Repo {B build()=>B(); void run(){final value=build();value.m();}}
 void run(dynamic global){global.m();{final x=A();x.m();}{final x=B();x.m();}}
 void aliases(){final x=global;final y=x;y.m();}
 `})
	if f := findFact(all, facts.KindSymbol, "lib.Repo.run"); !hasReviewCall(f, "lib.B.m") || hasReviewCall(f, "lib.A.m") {
		t.Fatalf("return type guessed from caller: %+v", f)
	}
	if f := findFact(all, facts.KindSymbol, "lib.aliases"); !hasReviewCall(f, "lib.A.m") {
		t.Fatalf("top-level getter alias lost: %+v", f)
	}
	f := findFact(all, facts.KindSymbol, "lib.run")
	if !hasReviewCall(f, "lib.A.m") || !hasReviewCall(f, "lib.B.m") || !hasReviewCall(f, "(global).m") {
		t.Fatalf("lexical scopes merged: %+v", f)
	}
}
func TestDartSDKLibraryIdentities(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"sdk/lib/libraries.json":       `{"comment:0":"generated manifest","vm":{"libraries":{"core":{"uri":"core/core.dart"},"_runtime":{"uri":"runtime/runtime.dart"},"_helper":{"uri":"helper/helper.dart"},"_wasm":{"uri":"wasm/wasm.dart"}}}}`,
		"sdk/lib/core/core.dart":       `library dart.core; part 'uri.dart';`,
		"sdk/lib/core/uri.dart":        `part of 'core.dart'; class UriData {UriData(); factory UriData.parse()=>UriData();bool isMimeType()=>true;}`,
		"sdk/lib/helper/helper.dart":   `void jsHelper() {}`,
		"sdk/lib/runtime/runtime.dart": `library dart._runtime; import 'dart:_helper'; part 'classes.dart';`,
		"sdk/lib/runtime/classes.dart": `part of 'dart:_runtime'; void entry(){jsHelper();}`,
		"sdk/lib/wasm/wasm.dart":       `class Word {int toIntUnsigned()=>0;}`,
		"test/use.dart":                `import 'dart:_runtime';import 'dart:_wasm' as w;void run(w.Word x){entry();x.toIntUnsigned();UriData.parse().isMimeType();}`,
	}
	var paths []string
	for file, src := range files {
		p := filepath.Join(root, file)
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0600); err != nil {
			t.Fatal(err)
		}
		if isDartFile(file) {
			paths = append(paths, file)
		}
	}
	slices.Sort(paths)
	all, err := New().Extract(context.Background(), root, paths)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ src, target string }{{"sdk/lib/runtime.entry", "sdk/lib/helper.jsHelper"}, {"test.run", "sdk/lib/runtime.entry"}, {"test.run", "sdk/lib/wasm.Word.toIntUnsigned"}, {"test.run", "sdk/lib/core.UriData.isMimeType"}} {
		if f := findFact(all, facts.KindSymbol, tc.src); !hasReviewCall(f, tc.target) {
			t.Errorf("SDK library resolution %s -> %s failed: %+v", tc.src, tc.target, f)
		}
	}
}

func TestDartTypeFlowValueNamespaceShadowsImportedConstructor(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{
		"lib/types.dart": `class A {void m() {}} class B {void m() {}}`,
		"lib/run.dart":   `import 'types.dart'; dynamic A()=>external; void run(){final a=A();a.m();} class Repo {B A()=>B();void run(){final a=A();a.m();}} class DynamicRepo {final Function A=external;void run(){final a=A();a.m();}}`,
	})
	if f := findFact(all, facts.KindSymbol, "lib.run"); hasReviewCall(f, "lib.A.m") {
		t.Fatalf("local function became imported constructor: %+v", f)
	}
	if f := findFact(all, facts.KindSymbol, "lib.Repo.run"); !hasReviewCall(f, "lib.B.m") || hasReviewCall(f, "lib.A.m") {
		t.Fatalf("member function lost lexical precedence: %+v", f)
	}
	if f := findFact(all, facts.KindSymbol, "lib.DynamicRepo.run"); hasReviewCall(f, "lib.A.m") {
		t.Fatalf("function-valued field became constructor: %+v", f)
	}
}

func TestDartTypeFlowLoopMetricNamespaces(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{"lib/use.dart": `import '../other/util.dart' as lib; void helper() {} void run(rows) {for(final row in rows){helper();lib.helper();}}`, "other/util.dart": `void helper() {}`})
	f := findFact(all, facts.KindSymbol, "lib.run")
	calls, _ := f.Props["calls_in_loop"].([]string)
	if !slices.Equal(calls, []string{"lib.helper", "other.helper"}) {
		t.Fatalf("a resolved name was rewritten twice: %v", calls)
	}
}

func TestDartTypeFlowMetadataRoundTrip(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{"lib/host.dart": `part 'piece.dart';`, "lib/piece.dart": `part of 'host.dart'; class Service {Service(); void load() {}} final Service global=Service(); class Registry {static final Service instance=Service();}`})
	store := facts.NewStore()
	store.Add(all...)
	var cold, warm bytes.Buffer
	if err := store.WriteJSONL(&cold); err != nil {
		t.Fatal(err)
	}
	decoded := facts.NewStore()
	if err := decoded.ReadJSONL(bytes.NewReader(cold.Bytes())); err != nil {
		t.Fatal(err)
	}
	if err := decoded.WriteJSONL(&warm); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(cold.Bytes(), warm.Bytes()) {
		t.Fatal("value metadata changes its serialized representation after caching")
	}
}

func TestDartTypeFlowCascadesAndFreshDeterminism(t *testing.T) {
	var previous []byte
	for i := 0; i < 20; i++ {
		all := dartReviewFacts(t, map[string]string{"lib/a.dart": `class A {A();void prepare() {}void load() {}} dynamic unknown()=>external;void run(){final a=A();final b=unknown();final c=(a..prepare());c.load();b().load();}`})
		f := findFact(all, facts.KindSymbol, "lib.run")
		if !hasReviewCall(f, "lib.A.load") {
			t.Fatalf("cascade did not preserve receiver type: %+v", f)
		}
		for _, fact := range all {
			for _, rel := range fact.Relations {
				if strings.Contains(rel.Target, "@value:") || strings.Contains(rel.Target, "@type{") || strings.HasPrefix(rel.Target, "@") {
					t.Fatalf("private value marker leaked: %+v", rel)
				}
			}
		}
		store := facts.NewStore()
		store.Add(all...)
		var buf bytes.Buffer
		if err := store.WriteJSONL(&buf); err != nil {
			t.Fatal(err)
		}
		if i > 0 && !bytes.Equal(previous, buf.Bytes()) {
			t.Fatal("fresh extraction depends on local binding allocation order")
		}
		previous = append(previous[:0], buf.Bytes()...)
	}
}

func TestDartTypeFlowUninitializedGlobalsAndReturnParameters(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{"lib/a.dart": `class A {void m(){}} class B {void m(){}} A? global; A value=A();echo(value)=>value; B typed(B value)=>value; void run(){global?.m(); final a=echo(external);a.m();final b=typed(B());b.m();}`})
	f := findFact(all, facts.KindSymbol, "lib.run")
	if !hasReviewCall(f, "lib.A.m") || !hasReviewCall(f, "lib.B.m") {
		t.Fatalf("global/typed return lost: %+v", f)
	}
	all = dartReviewFacts(t, map[string]string{"lib/a.dart": `class A {void m(){}} A value=A();echo(value)=>value;void run(){final a=echo(external);a.m();}`})
	if f := findFact(all, facts.KindSymbol, "lib.run"); hasReviewCall(f, "lib.A.m") {
		t.Fatalf("a return parameter was rebound to a global: %+v", f)
	}
}

func TestDartTypeFlowRejectsForeignSameNameMembers(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{"lib/a.dart": `class A {A();} void run(){final a=A();a.m();}`, "lib/other.dart": `class A {void m() {}}`})
	if f := findFact(all, facts.KindSymbol, "lib.run"); hasReviewCall(f, "lib.A.m") {
		t.Fatalf("member came from an unrelated A declaration: %+v", f)
	}
}
func TestDartTypeFlowFutureShadowing(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{"lib/a.dart": `class Future<T> {Future();} class Service {void load(){}} Future<Service> create()=>Future(); Future run() async {final a=await create();a.load();}`})
	if f := findFact(all, facts.KindSymbol, "lib.run"); hasReviewCall(f, "lib.Service.load") {
		t.Fatalf("a project class named Future was treated as dart:async Future: %+v", f)
	}
}

func TestDartTypeFlowVoidDoesNotAcquireExpressionType(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{"lib/a.dart": `class Service {void load(){}} void create()=>Service();void run(){final a=create();a.load();}`})
	if f := findFact(all, facts.KindSymbol, "lib.run"); hasReviewCall(f, "lib.Service.load") {
		t.Fatalf("a void expression acquired a receiver type: %+v", f)
	}
}

func TestDartTypeFlowUnknownValuesDoNotBecomeClassNamespaces(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{"lib/types.dart": `class Service {void load(){}} class A {static Service get instance=>Service();}`, "lib/run.dart": `import 'types.dart';dynamic A;void run(){A.instance.load();}class Holder {dynamic A;void run(){A.instance.load();}} class GetterHolder {dynamic get A=>external;void run(){A.instance.load();}}`})
	for _, name := range []string{"lib.run", "lib.Holder.run", "lib.GetterHolder.run"} {
		if f := findFact(all, facts.KindSymbol, name); hasReviewCall(f, "lib.Service.load") {
			t.Fatalf("unknown value became an imported class namespace: %+v", f)
		}
	}
}
func TestDartTypeFlowInheritedDynamicContract(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{"lib/a.dart": `class Service {void load(){}}class Base {dynamic make()=>external;}class Sub extends Base {make()=>Service();}void run(Sub source){final a=source.make();a.load();}`})
	if f := findFact(all, facts.KindSymbol, "lib.run"); hasReviewCall(f, "lib.Service.load") {
		t.Fatalf("dynamic inherited return contract was narrowed from an override body: %+v", f)
	}
}

func TestDartTypeFlowInterfaceContract(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{"lib/common.dart": `abstract class Contract {void load();} abstract class Other {void load();}`, "lib/run.dart": `import 'common.dart' as common; abstract class Contract implements common.Contract {} abstract class Ambiguous implements common.Contract,common.Other {} void run(Contract value){value.load();} void unknown(Ambiguous value){value.load();}`})
	if f := findFact(all, facts.KindSymbol, "lib.run"); !hasReviewCall(f, "lib.Contract.load") {
		t.Fatalf("interface member lost: %+v", f)
	}
	if f := findFact(all, facts.KindSymbol, "lib.unknown"); hasReviewCall(f, "lib.Contract.load") || hasReviewCall(f, "lib.Other.load") {
		t.Fatalf("competing interfaces guessed: %+v", f)
	}
}
func TestDartTypeFlowGetterFunctionCollision(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{"lib/function.dart": `String value()=>'';void run(){value();}`, "lib/getter.dart": `String get value=>'';String get current=>value;`})
	store := facts.NewStore()
	store.Add(all...)
	var buf bytes.Buffer
	if err := store.WriteJSONL(&buf); err != nil {
		t.Fatal(err)
	}
	for _, line := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
		if bytes.Contains(line, []byte(`"name":"lib.run"`)) && !bytes.Contains(line, []byte(`"kind":"calls","target":"lib.value","target_id":`)) {
			t.Fatalf("getter made existing function ambiguous: %s", line)
		}
	}
	if f := findFact(all, facts.KindSymbol, "lib/getter.value"); f == nil {
		t.Fatal("colliding getter needs file namespace")
	}
}

func TestDartTypeFlowNonSDKImportPrecedence(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{"sdk/lib/libraries.json": `{"vm":{"libraries":{"io":{"uri":"io/io.dart"}}}}`, "sdk/lib/io/io.dart": `class ProcessInfo {static void load(){}}void helper(){}`, "lib/custom.dart": `class ProcessInfo {static void load(){}}void helper(){}`, "lib/run.dart": `import 'dart:io';import 'custom.dart';void run(){ProcessInfo.load();helper();}`})
	f := findFact(all, facts.KindSymbol, "lib.run")
	if !hasReviewCall(f, "lib.ProcessInfo.load") || !hasReviewCall(f, "lib.helper") {
		t.Fatalf("SDK declarations hid non-SDK imports: %+v", f)
	}
	if hasReviewCall(f, "sdk/lib/io.ProcessInfo.load") || hasReviewCall(f, "sdk/lib/io.helper") {
		t.Fatalf("SDK conflict selected wrong library: %+v", f)
	}
}

func TestDartTypeFlowImplementationPrecedesInterface(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{"lib/a.dart": `abstract class Contract {void load();} mixin Implementation {void load(){}} mixin Defaults implements Contract {} class Combined with Implementation,Defaults {} void run(Combined value){value.load();}`})
	if f := findFact(all, facts.KindSymbol, "lib.run"); !hasReviewCall(f, "lib.Implementation.load") || hasReviewCall(f, "lib.Contract.load") {
		t.Fatalf("interface shadowed actual mixin implementation: %+v", f)
	}
}
