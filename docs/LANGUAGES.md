# Supported languages

Nothing here is a setting. enola looks for the markers below and runs whatever it finds, so a repository that is two languages is indexed as two languages without being told - and a language you don't see listed is a gap worth [reporting](https://github.com/enola-labs/enola/issues), not a verdict on whether enola is for you.

| Language   | Detected by |
|------------|-------------|
| Go         | `go.mod` (gorilla/mux + chi route composition / gRPC clients / Kafka topics aware) |
| Java       | `pom.xml` (Maven) or `.java` sources (Spring routes / JPA / Lombok DI / Dubbo SPI aware) |
| JavaScript | `tsconfig.json` / `package.json` with TypeScript (parsed by the TypeScript extractor) |
| TypeScript | `tsconfig.json` / `package.json` with TypeScript (Next.js, React Navigation & monorepo aware; Express sub-router mounts composed across files) |
| Vue        | `package.json` with `vue` dependency (Nuxt / Vue Router / Composition API aware) |
| Svelte     | `package.json` with `svelte` dependency (SvelteKit routing / `$lib` alias aware) |
| Ember      | `package.json` with `ember-source` dependency (`.gts`/`.gjs` template tags, `.hbs` templates, router map, ember-data) |
| Angular    | `package.json` with `@angular/core`, or an `angular.json` (component/directive/pipe/service/module roles; constructor and `inject()` DI; `.html` and inline templates in both the `*ngIf` and the `@if` dialect; router paths composed across lazy `loadChildren`; NgModule and standalone composition; `HttpClient` call sites; Nx/`angular.json` project boundaries) |
| Python     | `pyproject.toml`, `requirements.txt`, `setup.py`, … (FastAPI / Django / SQLAlchemy aware) |
| Kotlin     | `build.gradle(.kts)` with Kotlin/Android (Compose / Hilt / Room aware) |
| Swift      | `Package.swift`, `.xcodeproj`, `.xcworkspace` (SwiftUI / UIKit aware) |
| Dart / Flutter | `pubspec.yaml` (root or up to 4 levels deep), or any non-generated `.dart` source (pub packages as modules; go_router / auto_route / core `routes:` navigation; `http`, `dio`, retrofit & chopper clients; drift / isar / hive / objectbox / floor / Firestore storage; generated `.g.dart`, `.freezed.dart`, `.mocks.dart` skipped) |
| Ruby       | `Gemfile`, any loose `.rb`/`.rake`, or a Rails **engine** (`config/routes.rb` beside `lib/**/engine.rb`) — Rails routes across every engine and plugin route file, `mount` composed into the mounted engine's own routes, controller actions resolved from `resources`; **Grape** APIs found by transitive inheritance with mount prefixes composed across files; ActiveRecord / Sequel / Packwerk aware |
| Rust       | `Cargo.toml` (workspace or single crate; crate/module/`impl`/trait aware; Axum route DSL aware) |
| Scala      | an sbt/Mill/Maven/Gradle build naming Scala, or any `.scala` source (Play `conf/routes`, Pekko/Akka HTTP and http4s routes; Slick storage; sttp clients; `for … yield` read as a bind, not a loop) |
| C / C++    | `.c`/`.h` (tree-sitter-c) or `.cpp`/`.hpp`/… (tree-sitter-cpp), or `CMakeLists.txt`/`Makefile` + header (per-fact `language`, header/source method merging, namespaces, templates) |
| .NET       | `.sln`/`.slnx`/`.csproj`/`.fsproj`/`.vbproj`, or any `.cs`/`.vb`/`.fs`/`.razor`/`.cshtml`/`.xaml` source (C#, VB.NET, F#, Razor/Blazor, XAML; MSBuild `ProjectReference` as the assembly graph; ASP.NET Core attribute, minimal-API and conventional routing; EF Core/Dapper storage; `HttpClient`/Refit clients; `partial` types merged across files and languages) |
| PHP        | `composer.json`, WordPress markers, or any `.php` source (WordPress / Laravel / Symfony route + outbound HTTP-client aware) |
| Terraform / HCL | any `.tf`/`.hcl` file (blocks as Terraform addresses; prefixed and declared-set bare references; local module sources draw directory dependencies) |
| Ansible    | `ansible.cfg` or a `roles/` directory beside plays (plays → roles by name; `include_role`/`import_role`; templates counted, never rendered) |
| AsyncAPI   | any AsyncAPI 2.x/3.x YAML or JSON spec (channels and producer/consumer operations → messaging topics; local `$ref` and payload-schema identity) |
| OpenAPI    | any spec with an `openapi:` / `swagger:` key |
| gRPC       | any `.proto` file (proto services → routes; TypeScript gRPC-web client calls detected) |
| GraphQL    | graphql-ruby root types (server) + gql tags, `.graphql` operation documents and Ruby operation strings (clients); operation documents activate detection without a TypeScript root |

Framework- and platform-specific detection for each language is described in **[ARCHITECTURE.md → Supported languages](../ARCHITECTURE.md#supported-languages)**.

> Python, Ruby, PHP, Rust and Dart are parsed with tree-sitter and contribute call and dependency edges to the graph, so `traverse`, `find_path`, and `impact_analysis` reach into them - not just modules and routes.
