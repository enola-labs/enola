package dartextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/perf"
)

func TestDartReceiverDoesNotBindUnrelatedMethod(t *testing.T) {
	all := walkSource(t, "lib/format.dart", `
import 'package:http/http.dart';
class Storage {
 void replaceAll() { httpClient.post('/save'); }
}
String clean(String text) => text.trim().toLowerCase().replaceAll('a','b');
class Label { Label.named(String text) { clean(text); } }
void run(List<String> values) { for (final value in values) { clean(value); Label.named(value); } }
`)
	all = resolveCallTargets(all)
	computeDartPerformsIO(all)
	clean := findFact(all, facts.KindSymbol, "lib.clean")
	if hasRelation(clean, facts.RelCalls, "lib.Storage.replaceAll") {
		t.Fatal("string call bound to storage")
	}
	if !hasRelation(clean, facts.RelCalls, "(text).trim().toLowerCase().replaceAll") {
		t.Fatalf("lost chained receiver: %+v", clean.Relations)
	}
	for _, name := range []string{"lib.clean", "lib.Label.Label.named", "lib.run"} {
		f := findFact(all, facts.KindSymbol, name)
		if f == nil || f.PropBool("performs_io") {
			t.Errorf("pure code incorrectly marked I/O: %s", name)
		}
	}
	store := facts.NewStore()
	store.Add(all...)
	for _, f := range perf.Analyze(store) {
		if f.Kind == "call-in-loop" && f.Symbol == "lib.run" {
			t.Errorf("false I/O finding: %+v", f)
		}
	}
}

func TestDartTypedReceiversResolveAmbiguousMethods(t *testing.T) {
	all := walkSource(t, "lib/service.dart", `
import 'package:http/http.dart';
class Source { void fetch() { httpClient.get('/data'); } }
class Other { void fetch() {} }
class Repo {
 final Source source;
 Repo(this.source);
 void load() { source.fetch(); }
 void local(Other source) { source.fetch(); }
 void explicit(Other source) { this.source.fetch(); }
 void unknown(dynamic source) { source.fetch(); }
 void nested(Source source) { source.fetch().fetch(); }
}
void run(Source source) { for (var i=0;i<2;i++) { source.fetch(); } }
void local() { Source source = external; source.fetch(); }
`)
	all = resolveCallTargets(all)
	computeDartPerformsIO(all)
	for _, name := range []string{"lib.Repo.load", "lib.Repo.explicit", "lib.run", "lib.local"} {
		f := findFact(all, facts.KindSymbol, name)
		if !hasRelation(f, facts.RelCalls, "lib.Source.fetch") || !f.PropBool("performs_io") {
			t.Errorf("typed receiver failed: %s %+v", name, f)
		}
	}
	f := findFact(all, facts.KindSymbol, "lib.Repo.local")
	if !hasRelation(f, facts.RelCalls, "lib.Other.fetch") || f.PropBool("performs_io") {
		t.Errorf("parameter did not shadow field: %+v", f)
	}
	f = findFact(all, facts.KindSymbol, "lib.Repo.unknown")
	if !hasRelation(f, facts.RelCalls, "(source).fetch") || f.PropBool("performs_io") {
		t.Errorf("untyped receiver fabricated target: %+v", f)
	}
	f = findFact(all, facts.KindSymbol, "lib.Repo.nested")
	if !hasRelation(f, facts.RelCalls, "(source).fetch().fetch") {
		t.Errorf("return type assumed equal to receiver: %+v", f)
	}
	f = findFact(all, facts.KindSymbol, "lib.run")
	calls, _ := f.Props["calls_in_loop"].([]string)
	if len(calls) != 1 || calls[0] != "lib.Source.fetch" {
		t.Errorf("loop metric disagrees with call edge: %v", calls)
	}
}

func TestDartUnknownReceiverDoesNotPropagateIO(t *testing.T) {
	all := walkSource(t, "lib/client.dart", `
import 'package:http/http.dart';
class Source { void fetch() { httpClient.get('/data'); } }
void run(dynamic other) { other.fetch(); }
`)
	all = resolveCallTargets(all)
	computeDartPerformsIO(all)
	f := findFact(all, facts.KindSymbol, "lib.run")
	if !hasRelation(f, facts.RelCalls, "(other).fetch") || f.PropBool("performs_io") {
		t.Fatalf("untyped call borrowed I/O: %+v", f)
	}
}

func TestDartAmbiguousTargetsDoNotPropagateIO(t *testing.T) {
	all := []facts.Fact{
		{Kind: facts.KindSymbol, Name: "lib.Client.fetch", File: "lib/io.dart", Props: map[string]any{"io_direct": true}},
		{Kind: facts.KindSymbol, Name: "lib.Client.fetch", File: "lib/pure.dart"},
		{Kind: facts.KindSymbol, Name: "lib.run", File: "lib/main.dart", Relations: []facts.Relation{{Kind: facts.RelCalls, Target: "lib.Client.fetch"}}},
	}
	computeDartPerformsIO(all)
	if all[2].PropBool("performs_io") {
		t.Fatal("ambiguous same-name declarations supplied I/O")
	}
}

func TestDartPrefixedAndGenericReceiverTypes(t *testing.T) {
	all := walkSource(t, "lib/types.dart", `
import 'package:external/client.dart' as ext;
class T { void fetch() {} }
class ext { void fetch() {} }
class Repo<T> {
 final T source;
 final ext.Client client;
 Repo(this.source,this.client);
 void generic() { source.fetch(); this.source.fetch(); }
 void prefixed() { client.fetch(); }
}
void run<R>(R other) { other.fetch(); }
`)
	all = resolveCallTargets(all)
	for _, name := range []string{"lib.Repo.generic", "lib.Repo.prefixed", "lib.run"} {
		f := findFact(all, facts.KindSymbol, name)
		if f == nil {
			t.Fatalf("missing %s", name)
		}
		if hasRelation(f, facts.RelCalls, "lib.T.fetch") || hasRelation(f, facts.RelCalls, "lib.ext.fetch") {
			t.Errorf("generic/prefixed type bound to unrelated type: %+v", f)
		}
	}
	if f := findFact(all, facts.KindSymbol, "lib.Repo.prefixed"); !hasRelation(f, facts.RelCalls, "ext.Client.fetch") {
		t.Errorf("import prefix was lost: %+v", f.Relations)
	}
}

func TestDartConstructorInitializersKeepLocalCalls(t *testing.T) {
	all := walkSource(t, "lib/database.dart", `
Object _openConnection() => 1;
Object _makeValue() => 2;
class Database extends Base {
 Database() : super(_openConnection());
 Database.named() : value = _makeValue() { _openConnection(); }
 Object? value;
}
`)
	// A same-named helper in another file must not hide lexical initializer use.
	all = append(all, walkSource(t, "other/helper.dart", `Object _openConnection() => 3; Object _makeValue() => 4;`)...)
	all = resolveCallTargets(all)
	for _, tc := range []struct{ name, target string }{
		{"lib.Database.Database", "lib._openConnection"},
		{"lib.Database.Database.named", "lib._makeValue"},
		{"lib.Database.Database.named", "lib._openConnection"},
	} {
		f := findFact(all, facts.KindSymbol, tc.name)
		if !hasRelation(f, facts.RelCalls, tc.target) {
			t.Errorf("initializer/body use missing: %s -> %s (%+v)", tc.name, tc.target, f)
		}
	}
}

func TestDartReceiverDelegationIsNotSelfRecursion(t *testing.T) {
	all := walkSource(t, "lib/delegation.dart", `
class Source { void fetch() {} }
class Repo {
 final Source source;
 Repo(this.source);
 void fetch() { this.source.fetch(); }
 void recurse() { this.recurse(); }
}
void callback(dynamic callback) { callback(); }
`)
	for _, name := range []string{"lib.Repo.fetch", "lib.callback"} {
		if f := findFact(all, facts.KindSymbol, name); f == nil || f.PropBool("recursive_self") {
			t.Errorf("delegation/callback marked self-recursive: %+v", f)
		}
	}
	if f := findFact(all, facts.KindSymbol, "lib.Repo.recurse"); f == nil || !f.PropBool("recursive_self") {
		t.Errorf("actual recursion was lost: %+v", f)
	}
}

func TestDartValueReceiverCannotBecomeTypeNamespace(t *testing.T) {
	all := walkSource(t, "lib/shadow.dart", `
class Source { static void fetch() {} static void call() {} }
void run(dynamic Source) { Source.fetch(); Source(); }
`)
	all = resolveCallTargets(all)
	f := findFact(all, facts.KindSymbol, "lib.run")
	if hasRelation(f, facts.RelCalls, "lib.Source.fetch") || hasRelation(f, facts.RelCalls, "lib.Source.call") {
		t.Errorf("value binding became a class namespace: %+v", f)
	}
	if !hasRelation(f, facts.RelCalls, "(Source).fetch") {
		t.Errorf("value receiver not retained: %+v", f)
	}
	if hasRelation(f, facts.RelInstantiates, "lib.Source") || !hasRelation(f, facts.RelCalls, "(Source).call") {
		t.Errorf("shadowed value call became construction: %+v", f)
	}
}
