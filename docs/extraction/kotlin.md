# Kotlin — what enola extracts

Parsed with tree-sitter. Detected by `build.gradle.kts` / `build.gradle` with Kotlin or
Android. Gradle alone is not enough to pick the Java extractor instead — the build file
has to say which.

Fixture: [`kotlin_sample`](../../internal/engine/testdata/repos/kotlin_sample/)

## At a glance

| You write | enola stores | Kind |
|---|---|---|
| a Gradle module / source directory | one module with `module_role` | `module` |
| `class`, `interface`, `object` | a symbol with `symbol_kind` | `symbol` |
| `data class` | `data_class=true` | props |
| `override fun` | `override=true` | props |
| a function that calls itself | `recursive_self=true` | props |
| `@GET("…")` or `@GET(value = "…")` on a Retrofit interface | a client route | `route` |
| `@Entity` / `@Dao` (Room) | a storage entity and a DAO | `storage` |
| a `@Query`/`@Insert` DAO method | `io_direct=true`, `performs_io=true` | props |
| `@Module` / `@Component` / `@Binds` (Dagger, Hilt) | `di_module`, `di_component`, `di_provider` | props |
| `@HiltViewModel`, repositories, … | `android_component=…`, `framework=android` | props |

## Retrofit — the client half of a mobile-to-backend edge

Both argument forms are read. Kotlin lets any single-argument annotation be written
with its argument named, and real Android codebases mix the two freely — matching only
the positional form once yielded *zero* routes for an entire application, and a
Retrofit interface with no routes contributes no mobile-to-backend edges at all.

```kotlin
interface ApiService {
    @GET("/api/users/active")
    suspend fun getActiveUsers(): Response<List<User>>      // line 19

    @POST("auth/login")
    suspend fun login(@Body body: LoginRequest): Response<Token>   // line 22

    @GET(value = "topics")                                         // named argument
    suspend fun topics(): Response<List<Topic>>
}
```

```
route  /api/users/active   ApiService.kt:19
       props: role=client, framework=retrofit, source=retrofit, method=GET, api=ApiService
route  auth/login          ApiService.kt:22
       props: role=client, framework=retrofit, source=retrofit, method=POST, api=ApiService
route  topics               ApiService.kt:25
       props: role=client, framework=retrofit, source=retrofit, method=GET, api=ApiService

symbol …ApiService.getActiveUsers   props: io_direct=true, performs_io=true, receiver=ApiService
```

Note that `auth/login` is stored **without** a leading slash, exactly as written. Retrofit
resolves relative paths against the client's base URL, and enola does not invent the
missing segment — the path is recorded as declared, and matching handles the difference.
Inventing a leading `/` would produce a confident edge to the wrong route.

## Room — storage with its DAO

```
storage  ….UserEntity   UserDao.kt:10   props: framework=room, storage_kind=entity
storage  ….UserDao      UserDao.kt:16   props: framework=room, storage_kind=dao
symbol   ….UserDao.getAll  props: io_direct=true, performs_io=true
```

The entity and the DAO are separate facts because they answer different questions: *what
tables does this app have* and *what code touches them*. `io_direct` on the DAO method
means an N+1 finding on a caller is about real database round-trips.

## Dependency injection

```
symbol ….NetworkModule                 props: di_module=true, android_component=di_module, framework=android
symbol ….NetworkModule.bindUserRepository  props: di_provider=true
symbol ….AppComponent                  props: di_component=true, framework=android
symbol ….UserRepository                props: android_component=repository
```

As in [Java](java.md#dependency-injection-which-is-where-callers-disappear), the point is
that a `@Binds` provider has no direct caller. Marking it as wiring rather than dead code
is the difference between a useful dead-code report and one nobody reads.

## Loops and recursion

```
symbol ….HomeViewModel.updateItem  props: loop_count=1, loop_depth=1, scaling_loop_depth=0,
                                          calls_in_loop=[….HomeViewModel.updateItem],
                                          calls_in_scaling_loop=[]
symbol ….fib                       props: recursive_self=true, cyclomatic=2
```

Same model as the other languages: a bounded loop records its calls but leaves the scaling
set empty, and self-recursion is marked rather than reported as a cycle.

A call that passes an argument by a name the function does not declare is another
function, so it is not recursion. That is every Gradle convention plugin: `apply(plugin =
"…")` inside `apply(target: Project)` is `Project.apply`.

The shared loop rules, including inner loops reached through the outer element (`for (dep
in config.dependencies)`), are in [the index](README.md#loops-what-counts-as-nesting).

## Compose and Android components

Composable functions, ViewModels, repositories and Hilt entry points are tagged with
`android_component` so a UI-layer question (*which screens read this repository?*) can be
answered by filtering rather than by guessing from file paths.

## What is deliberately not extracted

- **KSP / kapt generated sources.** `**/generated/**` and `build/` are ignored by default;
  a Room DAO implementation or a Hilt component generated at build time is not indexed.
  The annotated declaration is what carries the meaning.
- **Non-literal Retrofit paths.** `@GET(pathConstant)` is not resolved.
- **Kotlin multiplatform expect/actual pairing** across source sets.
- **Extension-function dispatch** resolves to the declared extension, not to receivers.

---

Measured on real Kotlin repositories: [BENCHMARKS.md](../BENCHMARKS.md).
