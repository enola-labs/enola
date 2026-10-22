# Java — what enola extracts

Parsed with tree-sitter. Detected by a `pom.xml` (Maven) or any `.java` source. A Gradle
build file alone does **not** trigger it — Kotlin and Android use Gradle too, and guessing
wrong there means running the wrong extractor over an entire repository.

Fixture: [`java_sample`](../../internal/engine/testdata/repos/java_sample/)

## At a glance

| You write | enola stores | Kind |
|---|---|---|
| a package directory | one module | `module` |
| `class` / `interface` / `enum` | a symbol with `fqn` and `abstract` | `symbol` |
| a method | a symbol with `receiver`, `cyclomatic` | `symbol` |
| a method that calls itself | `recursive_self=true` | props |
| `import …` | a dependency tagged `internal` / `external` | `dependency` |
| `@Repository`, `@Service`, `@Controller` | `framework=spring`, `component=repository` | props |
| a JPA repository method | `performs_io=true`, `io_direct=true` | props |
| `@Component` / `@Module` (Dagger, Hilt) | `di_component=true` / `di_module=true` | props |
| a Dubbo SPI entry in `META-INF/` | a `file_ref` with a call edge to the implementation | `file_ref` |
| `restTemplate.exchange(url, GET, …)` | a client route | `route` |
| `@FeignClient` + `@GetMapping` | a client route with a `target_hint` | `route` |

## Symbols

```
symbol …compute.Calculator       props: symbol_kind=class, abstract=true, fqn=com.example.compute.Calculator
symbol …compute.Calculator.fib   props: symbol_kind=method, receiver=Calculator,
                                        cyclomatic=2, recursive_self=true
symbol …repo.WidgetRepository    props: symbol_kind=interface, framework=spring, component=repository
symbol …repo.WidgetRepository.findByOwner
                                 props: performs_io=true, io_direct=true, receiver=WidgetRepository
```

`fqn` is kept alongside the module-scoped name because Java identity is the fully
qualified name — that is what an annotation, a config file or another repository refers
to. `performs_io` is what makes an N+1 finding actionable: a call in a loop matters much
more when the callee touches the database.

## Dependency injection, which is where callers disappear

A Dagger or Hilt graph wires implementations to interfaces with no direct call anywhere:

```
symbol …di.AppComponent   props: di_component=true, symbol_kind=interface
symbol …di.NetworkModule  props: di_module=true,    symbol_kind=class
```

Marking these means a provider method is understood as a wiring point rather than an
unreferenced function. Lombok-generated members are recognised for the same reason —
otherwise every `@Builder` and `@RequiredArgsConstructor` class looks like it has no
constructor.

## Dubbo SPI — a caller that is not code

```
# src/main/resources/META-INF/dubbo/internal/com.example.spi.Strategy
random=com.example.spi.RandomStrategy
```

```
file_ref  src/main/resources/META-INF/dubbo/internal/com.example.spi.Strategy
          --calls--> src/main/java/com/example/spi.RandomStrategy
```

`RandomStrategy` is loaded by name through the `ExtensionLoader` and has no Java caller
at all. Reading the SPI resource file gives it one. This is the same class of problem as
[WordPress hooks](php.md#routes--wordpress-hooks) and
[C macro callbacks](cpp.md#the-three-ways-a-c-callback-gets-its-only-caller): the
framework is the caller, and the framework's registry is a file.

## Outbound HTTP

```java
restTemplate.exchange("/api/inventory/items/{id}", HttpMethod.GET, …);   // line 15
restTemplate.postForObject("/api/inventory/items", …);                   // line 19
```

```
route  /api/inventory/items/{id}   InventoryClient.java:15
       props: role=client, framework=resttemplate, api=InventoryClient, method=GET
route  /api/inventory/items        InventoryClient.java:19
       props: role=client, framework=resttemplate, api=InventoryClient, method=POST
```

Declarative Feign clients carry a service name, which becomes a matching hint:

```java
@FeignClient(name = "shipping")
interface ShippingClient {
    @GetMapping("/api/shipping/{id}") Shipment get(@PathVariable String id);
}
```

```
route  /api/shipping/{id}   ShippingClient.java:12
       props: role=client, framework=feign, source=feign, target_hint=shipping
```

`target_hint` is the service the annotation names. When that repository is loaded, the
hint disambiguates between two services that happen to serve the same path.

## Spring server routes

`@RestController` and `@RequestMapping` / `@GetMapping` produce server routes with the
class-level prefix composed onto each method, the same shape as
[NestJS](typescript.md#routes--nestjs-controller-prefixes).

## What is deliberately not extracted

- **Runtime proxies and AOP.** A call intercepted by a Spring proxy resolves to the
  declared method.
- **Reflection.** `Class.forName(name)` resolves to nothing; the SPI file above is read
  because it is a *declared* registry, not because reflection is traced.
- **Non-literal paths.** A `@RequestMapping` built from a constant expression is not
  evaluated.
- **Interface dispatch** resolves to the interface method, not to every implementation.
- **Imports of generated code** (`R`, `BuildConfig`) stay external dependencies rather than
  resolving to the package around them.

---

Measured on real Java repositories: [BENCHMARKS.md](../BENCHMARKS.md).
