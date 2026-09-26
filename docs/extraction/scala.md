# Scala — what enola extracts

Parsed with tree-sitter. Detected by an sbt, Mill, Maven or Gradle build that names
Scala, or — failing all of those — by real `.scala` sources up to eight levels deep.

Fixture: [`scala_sample`](../../internal/engine/testdata/repos/scala_sample/) ·
Golden: [`scala_sample.facts.jsonl`](../../internal/engine/testdata/golden/scala_sample.facts.jsonl)

> **Scala rarely owns a repository alone.** The two largest Scala projects in the
> benchmark corpus hold 1,355 and 582 `.java` files beside their Scala sources, in the
> same packages. So the Java, Kotlin and Scala extractors all run over such a tree,
> each reading only the files it owns, and they resolve into one another's packages
> through a shared package index. Several extractors claiming one repository is the
> correct outcome here, not a conflict.

## At a glance

| You write | enola stores | Kind |
|---|---|---|
| a package directory | one module with `module_role` | `module` |
| `class`, `case class`, `object`, `trait`, `enum` | a symbol with `symbol_kind` | `symbol` |
| `def`, `val`, `var`, `type`, `given` | a member symbol with its `receiver` | `symbol` |
| `extends A with B` | one `implements` relation per supertype | relation |
| `new Foo(…)` / `Foo(…)` | an `instantiates` relation | relation |
| `import a.b.C` | a dependency tagged `internal` / `external` / `stdlib` | `dependency` |
| `conf/routes` (Play) | one server route per line, at its composed mount path | `route` |
| `path("x") { get { … } }` (Pekko/Akka HTTP) | one server route per verb | `route` |
| `case GET -> Root / "x"` (http4s) | a server route | `route` |
| `basicRequest.get(uri"/x")` | a **client** route the linker can join to its server | `route` |
| `extends Table[T](tag, "users")` (Slick) | a table | `storage` |
| `val ordersTopic = "orders-v1"` | a messaging topic | `storage` |
| a spec under `src/test/` | its references only, never its symbols | `test_ref` |

## Symbols

```scala
// core/src/main/scala/com/example/core/Base.scala
package com.example.core

trait Base { def id: Long }

sealed abstract class Entity extends Base

case class User(id: Long, name: String) extends Entity

object Registry {
  type Id = Long
  def next(): Id = 1
}
```

```
symbol  core/src/main/scala/com/example/core.Base       symbol_kind=interface
symbol  core/src/main/scala/com/example/core.Entity     symbol_kind=class, sealed, abstract
                                                        --implements--> …core.Base
symbol  core/src/main/scala/com/example/core.User       symbol_kind=class, case_class
                                                        --implements--> …core.Entity
symbol  core/src/main/scala/com/example/core.Registry   symbol_kind=class, scala_object
symbol  core/src/main/scala/com/example/core.Registry.next  symbol_kind=method, receiver=Registry
```

The fact **name** is directory-anchored like every other language; the **`fqn`** prop
follows the `package`, which Scala lets disagree with the file system. Chained
clauses (`package com.example` then `package model`) name one package.

A `trait` is an `interface` and an `object` is a `class` carrying `scala_object` —
the shared `symbol_kind` vocabulary gains no new values, and "how many classes" and
"which are singletons" stay separately answerable.

## Resolution: what is bound, and what is left short

Two references that look identical resolve differently, and the difference is
deliberate:

```scala
package com.example.app

class Service(repo: UserRepo) extends Base with Ordering {
  def load(id: Long) = repo.find(id)     // repo's type is DECLARED -> UserRepo.find
  def build()        = helper()          // not a member of Service -> bare `helper`
}
```

- **`Base`** resolves if `com.example.app` declares it, else stays bare. Java can
  qualify a bare name with the file's package because its imports are explicit;
  Scala auto-imports `scala.*`, `java.lang.*` and `Predef.*`, so a bare name is as
  likely to be stdlib. `Ordering` therefore stays unresolved rather than becoming
  `com.example.app.Ordering` — a name no package declares, which a name-keyed graph
  would materialize as a phantom node.
- **`repo.find`** binds to `UserRepo.find`, because Scala writes the parameter's type
  down. This is what lets `performs_io` cross the constructor-injection boundary that
  most Scala services are built on.
- **`helper()`** stays a bare short name: enough for dead-code matching, without
  inventing a member of `Service`.

## Loops: why `for … yield` is discounted

Scala spells iteration and effect-sequencing the same way, and getting this wrong
does not merely add noise — it puts a per-iteration-I/O finding on every effectful
method in the language.

```scala
for (u <- users) { load(u) }            // load runs once per user
for (a <- fetchA; b <- fetchB(a)) yield b   // fetchB runs exactly ONCE
```

The split is the `yield` keyword, and it was measured over the corpus's 8,119
production files rather than assumed — taking "the file imports an effect monad" as a
proxy for "this is a bind":

| Construct | Sites | Effect-typed | Counts as |
|---|---:|---:|---|
| `for … yield` | 3,038 | **60.4%** | repetition, **not scaling** |
| `for` (no yield) | 704 | 9.7% | scaling loop |
| `while` / `do-while` | 2,337 | 0% | scaling loop |
| `.flatMap {}` / `.fold {}` | 1,786 | ~49% | repetition, **not scaling** |
| `.map` / `.foreach` / `.filter` / `.foldLeft` | 8,559 | 7–14% | scaling loop |

An ambiguous construct raises `loop_depth` but not `scaling_loop_depth` — the same
discount applied to a constant-trip loop elsewhere — so a finding is **downgraded**
rather than fabricated or lost. `synchronized`, `getOrElse` and `Resource.use` take a
block that runs at most once and are not repetition at all.

The effect is real and asymmetric: on an effect-heavy application 66–75% of loops are
discounted, on a plain-collection Scala 2 application 14%.

## Routes

### Play — `conf/routes`

Read directly from disk, like the OpenAPI and Symfony route configs: the file has no
extension, so no ignore glob would admit it.

```
GET     /users/:id     controllers.Users.show(id: Long)
GET     /assets/*file  controllers.Assets.at(path="/public", file)
GET     /$lang<\w\w>/tv controllers.Tv.indexLang(lang: Language)
->      /admin         admin.Routes
```

```
route  /users/:id      method=GET, framework=play, handler=controllers.Users.show
route  /assets/:file   catch-all normalized
route  /:lang/tv       regex constraint dropped; it constrains matching, not identity
route  /admin/users    the include's routes, at their mount prefix
```

An include's prefix is composed onto the sub-router's paths **unless they already
carry it segment-wise**. Play prepends the prefix, so an included file is normally
written relative — but writing them absolute is common enough that it cannot be
treated as a mistake, and composing blindly turned `/team` mounted at `/team` into
`/team/team`, an endpoint the application does not serve. The check is segment-wise
because `/teams` under a `/team` mount shares a string prefix but not a segment, and
must still compose.

### Pekko / Akka HTTP

```scala
import org.apache.pekko.http.scaladsl.server.Directives._

pathPrefix("api") {
  path("state") { get { complete(OK) } } ~
  (path("disable") & post) { complete(OK) } ~
  pathPrefix("v2" / "admin") { path("ping") { complete(OK) } }
}
```

```
route  /api/state           method=GET
route  /api/disable         method=POST    (the `& post` conjunction)
route  /api/v2/admin/ping   method=*       (no verb directive: serves every method)
```

`path` matches to the end of the URL and terminates a route; `pathPrefix` composes and
keeps descending. A path block naming two verbs emits two routes. A prefix held in a
value (`pathPrefix(collection.path)`) cannot be resolved without evaluating Scala, so
the route keeps the resolvable half and is tagged `path_unresolved`.

### http4s

```scala
HttpRoutes.of[IO] {
  case GET  -> Root / "users" / LongVar(id) => Ok(id)
  case POST -> Root / "teams"               => Ok()
}
```

```
route  /users/:id   method=GET    (the extractor variable becomes the canonical :name)
route  /teams       method=POST
```

## Storage and outbound calls

```scala
class Accounts(tag: Tag) extends Table[Account](tag, "ACCOUNT")   // storage, table
val ordersTopic = "orders-v1"                                     // storage, topic
basicRequest.get(uri"/api/inventory")                             // route, role=client
basicRequest.get(uri"https://api.stripe.com/v1/charges")          // route, external
```

A topic is emitted only from a file that also imports a broker client, and an
absolute URL is tagged `external` with its host so a third-party call is not counted
as an unresolved internal edge.

## Tests

Test source sets (`src/test`, `src/it`, `src/multi-jvm`) are excluded from the graph,
so a spec never becomes a dead-code candidate. Their **references** are kept: one
`test_ref` fact per file carrying only `calls` edges, so a production symbol whose
only caller is a spec keeps an inbound edge. Assertions, matchers, mocking and the
spec-structure DSL are dropped, and a harness receiver disqualifies the bare method
name too — filtering only `Assert.equals` would let `equals` through, and production
code declares `equals`, so the harness would vouch for a symbol no test exercises.

Between 50% and 85% of the references a spec makes match a production symbol; the
rest are standard-library and framework calls that match nothing.

## What is deliberately not extracted

Each of these is a gap you can see in `enola coverage`, not a wrong edge you would
have to discover.

- **SQL string literals.** Other extractors read a `CREATE TABLE` literal as an
  application declaring its schema. That does not hold for a SQL *engine*, and the
  corpus contains one: 198 of its files carry such literals as grammar fixtures,
  planner tests and documentation. A rule that cannot tell a query from a parser test
  would attribute hundreds of phantom tables to the repository most likely to be
  analysed for its data model. Slick tables and topic constants carry the storage
  signal without that hazard.
- **Scalatra, Tapir, Finatra and Lift routes.** No route DSL beyond the three above.
- **Doobie, Quill and Anorm** schemas — only Slick's `Table[T](tag, "name")` form.
- **Implicit resolution, extension methods and macros.** A call reached through an
  implicit conversion or an extension method resolves to a bare short name; macros
  are opaque, with no expansion and no edges through them.
- **Class parameters as symbols**, and therefore no `injects` edges. A Scala primary
  constructor carries data fields and dependencies in one list, so emitting them all
  would bury the dependency signal in `String` and `Int`.
- **Type members of a `given` instance**, and typeclass derivation generally.
- **Interprocedural route mounting.** A Pekko route tree assembled across traits or
  passed between methods composes only what one file can see; the Go extractor's
  module-wide prefix fixpoint has no Scala equivalent yet.

## Grammar

`tree-sitter-scala` **v0.26.2**, vendored under
`internal/extractors/scalaextractor/grammar/` and regenerated at tree-sitter ABI 14.
Upstream ships v0.25.0 and later at ABI 15, which the vendored runtime rejects
**silently** — every file parses to nothing, which is indistinguishable from a
repository containing no Scala. A probe test asserts the grammar still loads rather
than trusting the vendored bytes to stay put.

The last ABI-14 release, v0.24.1, predates capture-checking syntax (`T^{x}`,
`A ->{x} B`, `class Box[T, C^]`). On a capture-checked codebase it dropped and
mis-nested symbols with no parse error reported, which is why the grammar is vendored
rather than pinned. ABI 14 cannot carry upstream's reserved-word set, so a keyword in
identifier position (`val val = ???`) parses rather than producing an ERROR node; only
source the compiler rejects is affected.

Measured over the corpus on v0.24.1, between 0% and 4.6% of files contain a construct the
grammar cannot parse in a way that costs the enclosing type; the outlier is Scala 3's
fewer-braces trailing-argument form (`f(x): arg =>`), which is not yet supported.
