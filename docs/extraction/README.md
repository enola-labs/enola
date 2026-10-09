# What enola extracts, per language

These pages answer one question: **given this code, what ends up in the graph?**

Every example is a file that ships in this repository, and every fact shown is copied
from the golden file the test suite asserts against. So none of it is a description of
intended behaviour — it is the behaviour, and if an extractor changes without these
pages changing, the golden tests fail first.

| | |
|---|---|
| Fixture sources | [`internal/engine/testdata/repos/`](../../internal/engine/testdata/repos/) |
| Expected facts | [`internal/engine/testdata/golden/`](../../internal/engine/testdata/golden/) |
| Measured on real repositories | [BENCHMARKS.md](../BENCHMARKS.md) |

## The pages

| Language | Routes and clients it understands | |
|---|---|---|
| [Go](go.md) | gorilla/mux, chi, gin, `net/http` clients, gRPC, Kafka | prefix composition across function boundaries; gin `Group` mounts joined, not concatenated |
| [TypeScript / JavaScript](typescript.md) | Express, NestJS, Next.js, Angular, `fetch`, axios, Prisma, TypeORM, Drizzle | Vue, Svelte, Ember, and file-based routing |
| [Python](python.md) | FastAPI, Flask, Django, SQLAlchemy, gRPC | `include_router` prefixes folded repo-wide |
| [Ruby](ruby.md) | Rails `routes.rb`, ActiveRecord, Sequel, graphql-ruby, Packwerk | nested `resource`/`resources` path shapes, GraphQL operation strings |
| [Java](java.md) | Spring MVC, RestTemplate, Feign, JPA, Dubbo SPI | |
| [Kotlin](kotlin.md) | Retrofit, Room, Compose, Hilt | |
| [Swift](swift.md) | URLSession, SwiftUI, UIKit | endpoint enums, protocol-extension prefixes |
| [Dart / Flutter](dart.md) | go_router, auto_route, `http`, dio, retrofit, drift, isar, Firestore | every framework pass gated on the file's own imports; navigation routes kept out of the HTTP graph |
| [PHP](php.md) | Laravel, Symfony, WordPress, Guzzle | `apiResource` expansion, YAML route config |
| [Rust](rust.md) | Axum route DSL | `.nest()` mounts composed crate-wide |
| [Scala](scala.md) | Play `conf/routes`, Pekko/Akka HTTP, http4s, Slick, sttp | `for … yield` discounted as a monadic bind, not a loop |
| [C / C++](cpp.md) | — | header/source method merging, namespaces, templates |
| [.NET](dotnet.md) | ASP.NET Core attribute + minimal-API routing, Blazor and Razor Pages `@page` | C#, VB.NET, Razor and XAML; MSBuild `ProjectReference` as the assembly graph; `partial` types merged across files and languages |
| [gRPC and OpenAPI](grpc-openapi.md) | `.proto` services, OpenAPI specs | the contract as the server side of an edge |
| [AsyncAPI](asyncapi.md) | AsyncAPI 2.x/3.x YAML and JSON specs | event channels as producer/consumer topic facts |
| [Terraform / HCL](hcl.md) | resources, modules, variables, outputs, locals | Terraform addresses as symbol names; declared-set bare references |
| [Ansible](ansible.md) | plays, roles, `include_role`/`import_role` | by-name structure read without rendering a template |

## Loops: what counts as nesting

Every extractor that reads function bodies records loops as props on the symbol:
`loop_depth` (lexical nesting), `scaling_loop_depth` (nesting counting only loops whose
trip count grows with some input), `calls_in_loop`, and `calls_in_scaling_loop` (the
calls that repeat, which are the N+1 candidates). The performance analyzer reads these
and nothing else, so what a language counts as a loop is decided here.

A loop adds to `loop_depth` and not to `scaling_loop_depth` when the extractor can prove
from syntax that it adds no factor of *n*. Each rule fails closed: a loop that is not
proven keeps counting.

| Rule | Example | Languages |
|---|---|---|
| A fixed-count loop | `for (i = 0; i < 3; i++)`, a loop over a literal | all with scaling props |
| Fixed by name | `for p in PACKAGES`, `0..CHUNK_SIZE`, `Scope.values()`, a local bound once to a literal | TypeScript, Python, Rust, Java, Kotlin, PHP, C#, Ruby |
| An infinite loop | `for {}`, `while (true)`: it repeats, and its calls stay candidates | all with scaling props |
| Reached through the outer element | `for job in jobs { for need in job.needs }` visits each need once | Go, TypeScript, Python, PHP, Kotlin, Rust; Java and C# for statement loops |
| A batch loop | one query per page or chunk, drained inside | Go, Python |

Three things hold across those rules.

- **A `for` is fixed-count only when both ends are constant.** `for (i = n - 1; i >= 0;
  i--)` compares against a literal and walks all *n*.
- **Reaching a collection through the element is narrower than mentioning it.** A member,
  a subscript or a keyed lookup of the element counts, and so does a method of it that
  takes no arguments. `xs.slice(i + 1)` mentions the outer index and is the all-pairs
  loop, so it keeps counting.
- **What a loop iterates is evaluated once.** A call in `for x in load()` is not in
  `calls_in_loop`, and an iterator in that position is beside the loop, not inside it.

Two further props say where a call sits and whose data a loop walks.

| Prop | Meaning | Languages |
|---|---|---|
| `calls_in_scaling_loop_depth` | for each entry of `calls_in_scaling_loop`, in order, the deepest scaling loop it is called in | Go, TypeScript, Python, PHP, Kotlin, Java, Rust, C#, C/C++ |
| `calls_on_loop_element`, `loops_over_param` | a call is handed the loop's element; a scaling loop walks what a parameter or the receiver holds | Go, TypeScript, Python |
| `calls_on_loop_element_arg`, `loops_over_param_index` | which argument carries the element and which parameters are walked, `-1` for the receiver | TypeScript, Python |

`scaling_loop_depth` is the function's deepest nest anywhere. A call-in-loop finding needs
the nesting around the call, which is what the first prop gives. The other two let the
analyzer see that `for frame in frames { render(frame) }` is one walk when `render` loops
over the frame it was handed, and that it is not when `render` loops over something else.

Ruby and Swift record `loop_depth` and `calls_in_loop` only. Ruby folds a constant-bounded
iterator (`6.times`, `STOP_CHARS.each`) out of `loop_depth` itself.

`recursive_self` is set only for a call written in a form that can reach the enclosing
function: `self.f()` or a bare `f()` in a free function, not `super.f()`, `base.F()`,
`parent::f()`, another receiver's `f`, or a parameter named `f`.

## How to read a page

Each one is organized the same way:

1. **At a glance** — a table from source construct to fact kind.
2. **What each construct produces** — the code, then the facts, then the query it unlocks.
3. **What is deliberately not extracted** — the limits, stated next to the capability.

That last section is not an apology. A missing edge shows up in `enola coverage` as an
unresolved count you can go and look at; a *wrong* edge is invisible and gets acted on.
Every extractor here reports the gap rather than inventing the edge, and the section
says where those gaps are.

## The fact model in one paragraph

Everything below is one of six kinds — `module`, `symbol`, `route`, `storage`,
`dependency`, `service` — plus two reference-only kinds (`file_ref`, `test_ref`) that
carry edges without being architecture themselves. Facts are name-keyed, carry a
`file:line`, and hold typed relations (`imports`, `calls`, `declares`, `handled_by`,
`depends_on`, …). [GRAPH.md](../GRAPH.md) has the full
model; these pages assume it only loosely.

## If you are adding one

Two things are contracts rather than conventions, and both are enforced by tests.

**Register the `source` value of any route you emit.** A route's `source` prop says which
pass produced it, and the cross-repo linker branches on it. The values live in
[`internal/facts/contract.go`](../../internal/facts/contract.go); emit the constant, never
the literal. When you add one, decide whether it is a **hand-written call site** (a human
wrote this request — `ts-http-client`, `retrofit`, `urlsession`) or **contract-derived**
(read from a spec or IDL — `openapi`, `grpc-proto`). Hand-written sources belong in
`HandWrittenClientSources` and link as `via: "http-client"`; the rest link as `via: "http"`.

That choice is the whole difference between "someone wrote this call" and "a spec implies
this call", which is what a reader of the graph wants to know. It is enforced because it
was once wrong: the linker kept a private copy of the hand-written set that had never
included the Java extractor's two values, so every `RestTemplate` and `@FeignClient` call
site linked as generic `via: "http"` for as long as that extractor had existed. Nothing
failed, because nothing tied the reading side to the writing side.

**The `source` prop carries two unrelated vocabularies.** On a `route` fact it is
provenance (`ts-http-client`, `grpc-proto`, …). On a `dependency` fact it is where an
import *resolves to* (`internal` / `external` / `stdlib` / `framework`). Reading it without checking
`Kind` first gets you a value from the wrong vocabulary. The overload is historical and not
worth a migration — renaming a prop key rewrites every golden and every saved snapshot —
but it is a real trap.

### Reading a file the globs exclude

Extractors receive the walked file list, which the `ignore` globs have already filtered.
That is right for source files and wrong for the handful of config-format files that carry
architectural meaning — an OpenAPI spec, Symfony's route YAML, `package.json`'s package
name, `tsconfig.json`'s path aliases. Those are read **directly from disk**, because the
globs exist to suppress config/data noise, not to hide files the graph depends on.

If you add such a read, walk from `repoPath` yourself and skip the excluded directories
explicitly — the globs no longer protect you, and `node_modules` is the one that matters:
a dependency's `package.json` read as if the repo published it is a fabricated cross-repo
edge.

This is not hypothetical. `package.json` was read from the filtered list, and the bundled
`mcp-arch.yaml` ignores `**/*.json`. Under it no `package_name` prop was emitted at all, so
the linker's own-`@scope` guard could not fire and a repo importing a sibling package it
publishes itself was reported as depending on another repo entirely. The golden fixtures
kept passing throughout, because they build their engine from `config.Default()`, which has
no such glob — the fixture and the shipped config had been disagreeing for as long as the
guard existed.

[docs/EXTENDING.md](../EXTENDING.md) covers the rest: binders, cross-repo signals, and the
`linking:` vocabulary.
