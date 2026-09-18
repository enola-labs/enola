# Dart and Flutter

Fixtures: [`dart_sample`](../../internal/engine/testdata/repos/dart_sample/) ·
[`dart_multirepo`](../../internal/engine/testdata/repos/dart_multirepo/)

Every example on this page is a file that ships in those fixtures, and every fact shown
is copied from the golden the test suite asserts against.

## At a glance

| Source construct | Becomes |
|---|---|
| a directory holding `.dart` | `module`, carrying `pub_package` |
| `class` / `enum` | `symbol` (`class` / `enum`) |
| `mixin` | `symbol` (`interface`) |
| `extension`, `extension type`, `typedef` | `symbol` (`type`) |
| method, getter, setter, constructor | `symbol` (`method`) |
| top-level function | `symbol` (`function`) |
| **public** field, enum constant | `symbol` (`variable` / `constant`) |
| `extends` / `with` / `implements` | `implements` edge |
| constructor parameter of a declared type | `injects` edge |
| `import` / `export` | `dependency` (`stdlib` / `internal` / `external`) |
| `GoRoute`, `AutoRoute`, `routes:` map | `route` (`type: page`) |
| `http`/`dio` call site, `@GET(...)` | `route` (`role: client`) |
| drift `Table`, isar/hive/objectbox/floor, Firestore | `storage` |

## The module model is the pub package

A Dart repository is frequently a **workspace of many packages** — `flutter/packages`
holds over a hundred — so the package is never assumed to be the repo. Every
`pubspec.yaml` is read up front (a line scan, no YAML parser) into a name → directory
index, which is what lets a `package:` import resolve during the importing file's own
walk rather than needing a second pass.

Those pubspecs are read **directly from disk**, not from the file list the engine hands
the extractor. `**/*.yaml` is in the default ignore globs, so a `pubspec.yaml` never
reaches an extractor at all — measured on appflowy, 0 of 4,114 walked files. Reading the
walker's list instead yields an empty index, and nothing fails: modules simply carry no
`pub_package`, the repo's own `package:` imports classify as external, and Flutter is
never detected from the manifest. The globs exist to suppress config and data noise, and
a pubspec is the definition of the compilation unit, so this is the same deliberate
bypass the OpenAPI extractor and PHP's Symfony route config already make.

Module facts carry `pub_package`, registered in
[`facts.CompilationUnitProps`](../../internal/facts/contract.go). That registration is
what makes the cycles explainer word a Dart cycle correctly, and Dart is the most
permissive entry in that table. C# and Rust *forbid* cycles between compilation units,
so a cycle found in either is necessarily inside one. Dart goes further: **circular
imports between libraries are legal**, compile, and are ordinary in practice. A Dart
cycle is therefore a coupling signal and never a build-order defect, and reporting one
as something that "can cause initialization issues" would simply be untrue.

## Imports map onto exactly three classes

Dart has three URI schemes and they line up with enola's three dependency classes with
no heuristics involved:

```dart
import 'dart:async';                        // stdlib
import 'package:flutter/material.dart';     // external -> dependency "flutter"
import 'package:sample_app/models/user.dart'; // internal (this repo declares it)
import '../models/user.dart';               // internal
```

An external import is attributed to the **package**, not the file. Importing twenty
widgets from `package:flutter` is one dependency edge, not twenty — otherwise every
Flutter app's dependency count would be a count of its widgets.

## Import gating

This is the decision the rest of the extractor rests on.

Dart's framework vocabulary is ordinary vocabulary. `Table` is a drift database table
**and** a Flutter layout widget. `get` is an HTTP verb **and** a map accessor.
`collection` is a Firestore call **and** a common noun. `go` is a go_router navigation
**and** an unremarkable method name. Matching those structurally is exactly what
produced four phantom routes from a metrics timer and seven phantom Kafka topics from a
forum's `closeTopic` when the Scala extractor tried it.

Dart has a defence no other language here offers: **imports are mandatory and
explicit**. There is no ambient namespace and no unqualified access to a package a file
has not named, so "could this file be using drift?" is answered by a language rule
rather than by a probability.

```dart
import 'package:drift/drift.dart';
class TodoItems extends Table { ... }   // -> storage table "todo_items"
```
```dart
import 'package:flutter/material.dart';
class TodoItems extends Table { ... }   // -> nothing. Table here is a layout widget.
```

`TestImportGatingSuppressesLookalikes` pins each gated pass against a byte-identical
lookalike differing only in its import.

A `part` file declares no directives of its own — it shares its host library's import
scope — so it is re-walked with the host's imports rather than being left with every
gate closed.

## Navigation routes are page routes

```dart
final router = GoRouter(routes: [
  GoRoute(path: '/', builder: ..., routes: [
    GoRoute(path: 'detail/:id', builder: ...),
  ]),
  GoRoute(path: SettingsScreen.routeName, builder: ...),
]);
```
```
route  /             type=page  framework=go_router  handler=HomeScreen
route  /detail/:id   type=page  framework=go_router  handler=DetailScreen
route  /settings     type=page  framework=go_router  handler=SettingsScreen
```

`type: "page"` is load-bearing, not descriptive.
[`routeindex.IsUIRoute`](../../internal/linkers/crossrepo/routeindex/routeindex.go)
keys on it to keep a route out of the cross-repo server index and out of
`unused-routes`, exactly as it already does for Next.js and Nuxt pages, SvelteKit and
Ember.

The failure it prevents is concrete. A Flutter client and its backend are frequently in
one snapshot; both have a `/users/:id`; indexing the app's **screen** as a served route
would match it against the backend's real **endpoint** — an edge in the wrong
direction, and the backend's endpoint reported as served by the phone.

Three shapes are read:

- **go_router** — nested `routes:` compose onto their parent, and a child path
  beginning with `/` is absolute and replaces it (go_router's own rule; composing
  blindly is what produced `/user/user/settings` in Scala).
- **auto_route** — the dominant idiom declares no path at all (`AutoRoute(page:
  LoginRoute.page)`; all 59 of immich's declarations), so the path is derived by
  auto_route's documented kebab-case rule, and an `initial: true` child mounts at its
  parent rather than at the root.
- the core-Flutter **`routes:` map** on `MaterialApp`.

**A path declared as a constant** — `path: SettingsScreen.routeName`, how appflowy
declares 34 of its 35 routes — is carried as a reference and dereferenced repo-wide
once every file has been walked. One that never resolves **takes its route with it**: a
fact named `SettingsScreen.routeName` would be a path no app navigates to, and the
linker would then match it against real routes.

## Outbound HTTP is what puts a Flutter app in the cross-repo graph

A mobile client serves nothing, imports nothing from its backend and shares no code
with it. A call site is the only structural evidence that the app and the service belong
to one system.

```dart
await httpClient.get(Uri.parse('/api/users/$id'));         // client route, internal
await httpClient.post(Uri.parse('/api/orders'));           // client route, internal
await httpClient.post(Uri.parse('https://crash.example.com/v1/report'));
                                                           // external=true, host recorded
```

`Uri.parse(...)` and `Uri.https(...)` are unwrapped. An absolute URL is tagged
`external` with its host so a third-party call is not counted as an unresolved internal
edge; a relative path stays internal and therefore linkable. An interpolated path is
skipped rather than published with its `$id` intact.

**retrofit and chopper** are read from their annotations rather than their call sites,
and that is not politeness — their implementation is generated into a `.g.dart` this
extractor excludes, so the annotation *is* the call site.

[`dart_multirepo`](../../internal/engine/testdata/repos/dart_multirepo/) pins the whole
join: a Flutter client's three call sites resolving to a Go `net/http` server, with the
one route no client calls staying an `unused-routes` candidate.

## Generated code produces nothing

`.g.dart`, `.freezed.dart`, `.mocks.dart`, `.gr.dart`, `.config.dart` and `.pb*.dart`
yield no facts at all — not an empty symbol set, nothing, so a directory of only
generated files emits no module either.

This is not tidiness. Generated Dart is the **majority of files** in a `build_runner`
project: one `@freezed` model yields hundreds of generated `copyWith`/`==` lines plus a
`.g.dart` of serialization, none of it code anybody navigates, all of it inflating
symbol counts and manufacturing god-class and complexity findings about machine output.
The C# extractor draws the same line for the same reason.

## Abstractness is computed, not read off the keyword

`abstract` is authoritative for the package-metrics explainer, and Dart
cannot let the keyword decide. **Every Dart class is an implicit interface** others may
`implement`, so "is implementable" says nothing; and a `mixin` routinely carries its
whole implementation, exactly as a Scala trait does.

So a type is abstract when it is declared `abstract` or `sealed`, **or** when it
declares methods and every one of them is abstract. A type with no methods at all is
data, not an abstraction — and one with public fields and no behaviour additionally gets
`data_holder`, which spares its package the "extract interfaces" advice.

## The call graph, and what it refuses to guess

A call is emitted as it is *written*, then bound against the assembled fact set. The
binding rule is narrow on purpose: a bare name is rewritten only when the project
declares **exactly one callable** with it.

- **Callable-only is load-bearing.** Dart's short names collide across kinds. One app
  declares the enum constant `LogLevel.severe` *and* calls `log.severe(...)` on a
  logger; with every kind in the index the constant was the unique `severe`, so 117 call
  sites bound to it and the god-class explainer reported a data constant as a
  high-fan-in symbol. A call resolves to something callable or it stays bare — and a
  bare target still lets dead-code matching see the symbol used.
- **An ambiguous name stays bare.** `build`, `dispose`, `toJson` and `copyWith` are
  declared by hundreds of types in any Flutter app, so this is the common case, not the
  corner one. Picking one candidate would fabricate an edge into whichever module sorted
  first.
- **`_FooState()` is a construction, not a call.** Privacy is spelled with a leading
  underscore, so a type test on the raw first character classifies every private class
  as lowercase — and `_FooState` is the State class behind every StatefulWidget.
- **Closure bodies count.** `onPressed: () => save()` puts the invocation in a direct
  child sequence of the closure node; a walk that descends past it without scanning
  loses the call entirely. Arrow closures are pervasive in Flutter, and while this was
  missing, functions plainly in use were reported as dead.

## Complexity metrics

The standard props — `cyclomatic`, `loop_count`, `loop_depth`, `scaling_loop_depth`,
`calls_in_loop`, `recursive_self` — plus `io_direct` and its transitive closure
`performs_io`. Three of them needed Dart-specific care, and each was wrong in a way that
looked fine until it was measured against real code:

- **Logical operators are counted on the operator node.** Dart does not model `&&` as a
  generic binary expression; it has `logical_and_expression` with a `logical_and_operator`
  child. Matching a generic binary node counted *nothing*, so every logical operator in
  the corpus was invisible. Counting occurrences in the enclosing expression's text
  would be wrong the other way, since `a && b && c` nests and the outer node's operators
  would be recounted at each level. `??` counts too: it short-circuits, exactly as `||`
  does.
- **A literal-bounded loop adds no scaling depth.** `for (var i = 0; i < 10; i++)` and
  `for (final x in items)` are the *same node kind*, told apart by whether
  `for_loop_parts` holds a `relational_expression`. Without that, every fixed loop
  inflated the depth and turned an honest O(n) into a fabricated O(n²).
- **Recursion means reaching *this* symbol.** Matching the short name alone made
  `dispose()` calling `controller.dispose()` recursive — ordinary delegation to another
  object, and pervasive in Flutter. On one mid-size app that produced 64 false findings,
  **63 of the 75** the performance analyzer emitted. The receiver must be absent, `this`,
  or the bare name; `super.dispose()` is excluded, since it dispatches to the ancestor.

`io_direct` is set only when the file imports something that *can* do I/O, then
propagated up the call graph by a cycle-safe fixpoint. That matters more in Flutter than
elsewhere: the network is almost always two wrapper layers below the widget that
triggers it, so without the closure an in-loop call to `loadPage` carries no evidence
that it reaches the network at all.

The `analyze_performance` tool reads these through a Dart-specific gate of
its own. Dart is the fourth ecosystem to need one: the shared keyword list carries `where` for Ruby, where
`Model.where(...)` is a lazy query — but Dart's `.where()` is `Iterable.where`, the
in-memory filter and the direct equivalent of JavaScript's `.filter()`.

## What is deliberately not extracted

- **Server-side Dart.** shelf, dart_frog and serverpod have no route awareness, so a
  Dart backend is a route *consumer* but never a *provider*.
- **Generated OpenAPI Dart clients**, for a reason worth stating precisely because it
  is not an extraction limit: the generated package is frequently **not committed**.
  immich's mobile app depends on `package:openapi` via a path dependency into
  `mobile/generated/`, which its `.gitignore` excludes — the client is produced from the
  spec at build time and never exists in a clone. No extractor can read code that is not
  there, so immich contributes navigation routes and storage but no client routes. Where
  such a client *is* committed, its call shape (a local `path` variable passed to
  `invokeAPI(...)`, rather than a literal at the call site) is still not recognised.
- **A call on an arbitrary receiver** draws a bare short-name `calls` edge rather than a
  guessed canonical target, since the receiver's static type is not tracked. Dead-code
  matching still sees the symbol used.
- **Primary constructors** (`class const Foo(final String name)`) are not accepted by
  the vendored grammar. Measured across a ten-repository corpus this affects only the
  Dart SDK's own `pkg/front_end`, which dogfoods the unreleased feature: 64 of its 307
  library files, against zero elsewhere. `pkg/analyzer` next door parses at 0.00%.
- **A computed store name** (`collection(path)`, a non-literal drift `tableName`) is
  skipped rather than invented.
