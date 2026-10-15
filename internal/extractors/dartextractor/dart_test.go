package dartextractor

import (
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// walkSource runs the extractor over one in-memory file.
func walkSource(t *testing.T, relFile, src string) []facts.Fact {
	t.Helper()
	res := extractFile([]byte(src), relFile, &packageIndex{byName: map[string]*pubPackage{}}, nil)
	return res.facts
}

func findFact(all []facts.Fact, kind, name string) *facts.Fact {
	for i := range all {
		if all[i].Kind == kind && all[i].Name == name {
			return &all[i]
		}
	}
	return nil
}

func factsOfKind(all []facts.Fact, kind string) []facts.Fact {
	var out []facts.Fact
	for _, f := range all {
		if f.Kind == kind {
			out = append(out, f)
		}
	}
	return out
}

func hasRelation(f *facts.Fact, kind, target string) bool {
	if f == nil {
		return false
	}
	for _, r := range f.Relations {
		if r.Kind == kind && r.Target == target {
			return true
		}
	}
	return false
}

// TestExtractDartSymbols pins the symbol model: what each construct becomes, how it is
// named, and the two props that are read outside this package.
func TestExtractDartSymbols(t *testing.T) {
	src := `
import 'dart:async';
import 'package:flutter/material.dart';
import '../models/user.dart';

typedef Json = Map<String, dynamic>;

abstract class Repository {
  Future<void> save(String id);
}

mixin Loggable {
  void log(String m);
}

mixin Timestamped {
  String stamp() => '';
}

enum Role { admin, guest }

class UserRepo extends Repository with Timestamped {
  final Api api;
  final String _secret;
  UserRepo(this.api);

  @override
  Future<void> save(String id) async {
    api.persist(id);
  }

  static void reset() {}
}
`
	all := walkSource(t, "lib/data/user_repo.dart", src)

	for _, tc := range []struct{ name, kind string }{
		{"lib/data.Repository", facts.SymbolClass},
		{"lib/data.Loggable", facts.SymbolInterface},
		{"lib/data.Role", facts.SymbolEnum},
		{"lib/data.Json", facts.SymbolType},
		{"lib/data.UserRepo", facts.SymbolClass},
		{"lib/data.UserRepo.save", facts.SymbolMethod},
		{"lib/data.UserRepo.reset", facts.SymbolMethod},
	} {
		f := findFact(all, facts.KindSymbol, tc.name)
		if f == nil {
			t.Errorf("missing symbol %s", tc.name)
			continue
		}
		if got := f.PropString("symbol_kind"); got != tc.kind {
			t.Errorf("%s: symbol_kind = %q, want %q", tc.name, got, tc.kind)
		}
	}

	// `abstract` is authoritative for package-metrics abstractness, and Dart cannot
	// read it off the keyword: EVERY class is an implicit interface, and a mixin
	// routinely carries a full implementation. So it is computed from the members.
	for _, tc := range []struct {
		name string
		want bool
		why  string
	}{
		{"lib/data.Repository", true, "declared abstract"},
		{"lib/data.Loggable", true, "mixin whose every method is abstract"},
		{"lib/data.Timestamped", false, "mixin that carries an implementation"},
		{"lib/data.UserRepo", false, "concrete class"},
	} {
		f := findFact(all, facts.KindSymbol, tc.name)
		if f == nil {
			t.Fatalf("missing %s", tc.name)
		}
		if got, _ := f.Props["abstract"].(bool); got != tc.want {
			t.Errorf("%s: abstract = %v, want %v (%s)", tc.name, got, tc.want, tc.why)
		}
	}

	// A private field is deliberately not a symbol — private state is implementation
	// detail, and on a large codebase emitting it multiplies the count without adding
	// a node anyone traverses. The public one is.
	if findFact(all, facts.KindSymbol, "lib/data.UserRepo._secret") != nil {
		t.Error("private field _secret must not become a symbol")
	}
	if findFact(all, facts.KindSymbol, "lib/data.UserRepo.api") == nil {
		t.Error("public field api should be a symbol")
	}

	// extends/with both collapse into `implements`, which is enola's single
	// conformance relation.
	repo := findFact(all, facts.KindSymbol, "lib/data.UserRepo")
	if !hasRelation(repo, facts.RelImplements, "Repository") {
		t.Error("UserRepo should implement Repository (extends)")
	}
	if !hasRelation(repo, facts.RelImplements, "Timestamped") {
		t.Error("UserRepo should implement Timestamped (with)")
	}

	// Import classification: the three Dart URI schemes map onto the three classes.
	// Dependency facts are named "<importer> -> <imported>"; key the assertions on the
	// imported side, which is what the relation targets and what classification is about.
	deps := map[string]string{}
	for _, f := range factsOfKind(all, facts.KindDependency) {
		name := f.Name
		if _, imported, ok := strings.Cut(name, " -> "); ok {
			name = imported
		}
		deps[name] = f.PropString(facts.PropSource)
	}
	if deps["dart:async"] != facts.DepSourceStdlib {
		t.Errorf("dart:async should be stdlib, got %q", deps["dart:async"])
	}
	if deps["flutter"] != facts.DepSourceExternal {
		t.Errorf("package:flutter should be external and named by PACKAGE, got %q", deps["flutter"])
	}
	if deps["lib/models"] != facts.DepSourceInternal {
		t.Errorf("relative import should resolve to an internal module, got %v", deps)
	}
}

// TestNavigationRoutesArePageType is the guard on the decision that keeps a Flutter
// screen out of the cross-repo HTTP graph.
//
// A navigation path is a destination the APP navigates to internally, not an endpoint
// anything can call. routeindex.IsUIRoute keys on type "page" to exclude it from the
// server-route index and from unused-routes. If this prop is ever dropped, a Flutter
// app indexed beside its own backend starts matching its `/users/:id` SCREEN against
// the backend's real `/users/:id` ENDPOINT — an edge in the wrong direction, plus the
// backend's endpoint reported as served by the phone.
func TestNavigationRoutesArePageType(t *testing.T) {
	src := `
import 'package:go_router/go_router.dart';

final router = GoRouter(routes: [
  GoRoute(path: '/users', builder: (c, s) => const UsersScreen(), routes: [
    GoRoute(path: ':id', builder: (c, s) => const UserDetailScreen()),
  ]),
  GoRoute(path: SettingsScreen.routeName, builder: (c, s) => const SettingsScreen()),
]);
`
	all := walkSource(t, "lib/routing/router.dart", src)
	routes := factsOfKind(all, facts.KindRoute)
	if len(routes) != 3 {
		t.Fatalf("expected 3 routes, got %d: %+v", len(routes), routes)
	}
	for _, r := range routes {
		if got := r.PropString(facts.PropRouteType); got != "page" {
			t.Errorf("route %s: type = %q, want \"page\" — a navigation route must be "+
				"excluded from cross-repo HTTP matching", r.Name, got)
		}
	}

	// A relative sub-route composes onto its parent; storing the bare child path would
	// put a destination in the graph the app never navigates to.
	if findFact(all, facts.KindRoute, "/users/:id") == nil {
		t.Errorf("nested route should compose to /users/:id, got %v", routeNames(routes))
	}

	// A path declared as a constant is carried as a reference for the repo-wide
	// resolution pass, and dropped entirely if it never resolves — a fact named after
	// a Dart constant would be matched against real routes by the linker.
	ref := findFact(all, facts.KindRoute, "SettingsScreen.routeName")
	if ref == nil {
		t.Fatal("a constant-referenced path should be carried through to resolution")
	}
	if ref.PropString(pathRefProp) == "" {
		t.Error("the unresolved route must carry its reference")
	}
	if got := resolveRoutePathRefs(all, nil); findFact(got, facts.KindRoute, "SettingsScreen.routeName") != nil {
		t.Error("an unresolvable path reference must take its route with it")
	}
	resolved := resolveRoutePathRefs(walkSource(t, "lib/routing/router.dart", src),
		map[string]string{"SettingsScreen.routeName": "/settings"})
	if findFact(resolved, facts.KindRoute, "/settings") == nil {
		t.Error("a resolvable path reference should become its literal")
	}
}

func routeNames(routes []facts.Fact) []string {
	out := make([]string, 0, len(routes))
	for _, r := range routes {
		out = append(out, r.Name)
	}
	return out
}

// TestImportGatingSuppressesLookalikes is the guard on the decision the whole extractor
// rests on.
//
// Dart's framework vocabulary is ordinary vocabulary. `Table` is a Flutter LAYOUT
// WIDGET as well as a drift table; `get` is an HTTP verb and a map accessor;
// `collection` is a Firestore call and a common noun. Matching them structurally is
// what produced phantom routes and topics in the Scala extractor.
//
// Dart's defence is that imports are mandatory and there is no ambient namespace, so a
// file that has not imported the package CANNOT be using it — a language rule, not a
// probability. Each case below is byte-identical to a real usage apart from its import.
func TestImportGatingSuppressesLookalikes(t *testing.T) {
	cases := []struct {
		name       string
		gatedSrc   string
		ungatedSrc string
		kind       string
	}{
		{
			name: "drift Table vs Flutter Table widget",
			gatedSrc: `import 'package:drift/drift.dart';
class TodoItems extends Table { IntColumn get id => integer()(); }`,
			ungatedSrc: `import 'package:flutter/material.dart';
class TodoItems extends Table { IntColumn get id => integer()(); }`,
			kind: facts.KindStorage,
		},
		{
			name: "http client vs a map accessor named get",
			gatedSrc: `import 'package:http/http.dart' as http;
Future<void> f(var client) async { await client.get(Uri.parse('/api/items')); }`,
			ungatedSrc: `import 'dart:convert';
Future<void> f(var client) async { await client.get(Uri.parse('/api/items')); }`,
			kind: facts.KindRoute,
		},
		{
			name: "firestore collection vs a method named collection",
			gatedSrc: `import 'package:cloud_firestore/cloud_firestore.dart';
void f(var db) { db.collection('users'); }`,
			ungatedSrc: `import 'dart:async';
void f(var db) { db.collection('users'); }`,
			kind: facts.KindStorage,
		},
		{
			name: "go_router route vs a constructor named GoRoute",
			gatedSrc: `import 'package:go_router/go_router.dart';
final r = GoRouter(routes: [GoRoute(path: '/home')]);`,
			ungatedSrc: `import 'dart:async';
final r = GoRouter(routes: [GoRoute(path: '/home')]);`,
			kind: facts.KindRoute,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gated := factsOfKind(walkSource(t, "lib/a.dart", tc.gatedSrc), tc.kind)
			if len(gated) == 0 {
				t.Fatalf("with the import present, expected at least one %s fact", tc.kind)
			}
			ungated := factsOfKind(walkSource(t, "lib/a.dart", tc.ungatedSrc), tc.kind)
			if len(ungated) != 0 {
				t.Errorf("without the import, expected no %s facts, got %d: %v",
					tc.kind, len(ungated), routeNames(ungated))
			}
		})
	}
}

// TestGeneratedCodeYieldsNothing pins the exclusion. Generated Dart is the majority of
// files in a build_runner project, and indexing it manufactures god-class and
// complexity findings about machine output.
func TestGeneratedCodeYieldsNothing(t *testing.T) {
	e := New()
	for _, path := range []string{
		"lib/models/user.g.dart", "lib/models/user.freezed.dart",
		"lib/api/client.mocks.dart", "lib/routing/router.gr.dart",
		"lib/di/injection.config.dart", "lib/proto/messages.pb.dart",
	} {
		if !isGeneratedDart(path) {
			t.Errorf("%s should be recognised as generated", path)
		}
		if e.OwnsFile(path) {
			t.Errorf("%s must not be owned by the extractor", path)
		}
	}
	// A hand-written file whose name merely contains one of the tokens is not
	// generated: the match is on the SUFFIX, not on a substring.
	for _, path := range []string{"lib/config.dart", "lib/gr.dart", "lib/pbx.dart"} {
		if isGeneratedDart(path) {
			t.Errorf("%s should NOT be treated as generated", path)
		}
	}
}

// TestIOClosureCrossesWrappers pins the transitive performs_io propagation. In a
// Flutter app the network is almost always two layers down — a widget calls a
// repository which calls a data source which calls http — so without the closure the
// performance analyzer cannot see that an in-loop call reaches the network.
func TestIOClosureCrossesWrappers(t *testing.T) {
	src := `
import 'package:http/http.dart' as http;

class DataSource {
  Future<String> fetch(String id) async {
    final r = await httpClient.get(Uri.parse('/api/items/x'));
    return r.body;
  }
}

class Repo {
  Future<String> load(String id) => source.fetch(id);
}

class Screen {
  Future<void> refresh() => repo.load('1');
}
`
	all := resolveCallTargets(walkSource(t, "lib/data/source.dart", src))
	computeDartPerformsIO(all)

	direct := findFact(all, facts.KindSymbol, "lib/data.DataSource.fetch")
	if direct == nil {
		t.Fatal("missing DataSource.fetch")
	}
	if v, _ := direct.Props["io_direct"].(bool); !v {
		t.Error("a body invoking http.get directly should be io_direct")
	}
	for _, name := range []string{"lib/data.Repo.load", "lib/data.Screen.refresh"} {
		f := findFact(all, facts.KindSymbol, name)
		if f == nil {
			t.Fatalf("missing %s", name)
		}
		if v, _ := f.Props["performs_io"].(bool); !v {
			t.Errorf("%s should inherit performs_io through the call graph", name)
		}
	}
}

// TestTestRefsCarryNoSymbols pins the two halves of the test-ref contract: a test file
// contributes references and nothing else, and the harness vocabulary is dropped
// INCLUDING its bare names.
func TestTestRefsCarryNoSymbols(t *testing.T) {
	src := `
import 'package:flutter_test/flutter_test.dart';
import 'package:sample/repo.dart';

void main() {
  test('loads', () {
    final r = UserRepo(client);
    r.load('1');
    expect(find.text('x'), findsOneWidget);
  });
}
`
	f := extractTestRefs([]byte(src), "test/repo_test.dart")
	if f.Kind != facts.KindTestRef {
		t.Fatalf("expected a test_ref fact, got %q", f.Kind)
	}
	targets := map[string]bool{}
	for _, r := range f.Relations {
		if r.Kind != facts.RelCalls {
			t.Errorf("a test_ref must carry only calls relations, got %q", r.Kind)
		}
		targets[r.Target] = true
	}
	for _, want := range []string{"UserRepo", "load"} {
		if !targets[want] {
			t.Errorf("expected the test to reference %q, got %v", want, keysOf(targets))
		}
	}
	// `find` and `expect` are harness words, and production code really does declare
	// methods called `find`. Filtering only the qualified form would let the bare name
	// through and vouch for a symbol no test exercises.
	for _, unwanted := range []string{"find", "expect", "test", "findsOneWidget"} {
		if targets[unwanted] {
			t.Errorf("harness name %q must not become a reference", unwanted)
		}
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestPartInheritsHostImports pins that a `part` is walked with its host library's
// import scope. A part declares no directives of its own, so walked in isolation every
// framework gate closes and its routes, clients and tables all go unseen.
func TestPartInheritsHostImports(t *testing.T) {
	part := `part of 'db.dart';

class TodoItems extends Table { IntColumn get id => integer()(); }`

	alone := factsOfKind(walkSource(t, "lib/data/tables.dart", part), facts.KindStorage)
	if len(alone) != 0 {
		t.Fatalf("a part walked without its host imports should produce no storage, got %v", alone)
	}
	withHost := extractFile([]byte(part), "lib/data/tables.dart",
		&packageIndex{byName: map[string]*pubPackage{}}, []string{"package:drift/drift.dart"})
	if len(factsOfKind(withHost.facts, facts.KindStorage)) == 0 {
		t.Error("with the host library's imports, the drift table should be extracted")
	}
	if !strings.HasSuffix(withHost.partOf, "db.dart") {
		t.Errorf("part_of should resolve to the host file, got %q", withHost.partOf)
	}
}

func TestRetrofitMethodIsIO(t *testing.T) {
	src := `
import 'package:dio/dio.dart';
import 'package:retrofit/retrofit.dart';

@RestApi()
abstract class MeClient {
  factory MeClient(Dio dio) = _MeClient;

  @GET('/v1/me')
  Future<String> me();

  String describe();
}

class MeSource {
  Future<String> load() => client.me();
  String label() => client.describe();
}
`
	all := resolveCallTargets(walkSource(t, "lib/api/me_client.dart", src))
	computeDartPerformsIO(all)
	for name, want := range map[string]bool{
		"lib/api.MeClient.me":       true,
		"lib/api.MeSource.load":     true,
		"lib/api.MeClient.describe": false,
		"lib/api.MeSource.label":    false,
	} {
		f := findFact(all, facts.KindSymbol, name)
		if f == nil {
			t.Fatalf("missing %s", name)
		}
		if got, _ := f.Props["performs_io"].(bool); got != want {
			t.Errorf("%s performs_io = %v, want %v", name, got, want)
		}
	}
}
