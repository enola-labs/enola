# Swift — what enola extracts

Parsed with the [tree-sitter-swift](https://github.com/alex-pinkus/tree-sitter-swift)
grammar, vendored under
[`internal/extractors/swiftextractor/grammar/`](../../internal/extractors/swiftextractor/grammar/).
Detected by `Package.swift`, `.xcodeproj` or `.xcworkspace`.

Fixture: [`swift_sample`](../../internal/engine/testdata/repos/swift_sample/)

## At a glance

| You write | enola stores | Kind |
|---|---|---|
| an SPM target | a module with `spm_package`, `spm_target`, `module_role` | `module` |
| an Xcode app target | **several** modules, one per source directory | `module` |
| `class`, `struct`, `enum`, `protocol` | a symbol with `symbol_kind`, `final`, `enum` | `symbol` |
| a method | a symbol with `receiver`, `async`, `override` | `symbol` |
| a method that calls itself | `recursive_self=true` | props |
| `URLSession.shared.data(for:)` | `io_direct=true` **and** a client route | props + `route` |
| a method that only *calls* one that does I/O | `performs_io=true`, transitively | props |
| `#if DEBUG` around a declaration | `conditional=true` | props |
| a `TargetType`-style endpoint enum | one client route per case, at the composed path | `route` |
| `import OtherTarget` | a dependency with `spm=true` and the manifest that declares it | `dependency` |

## Modules — SPM targets and the Xcode app-target problem

```
module ./Sources/CoreKit    props: spm_package=CoreKit, spm_target=CoreKit, module_role=production
module Tests/CoreKitTests   props: spm_package=CoreKit, spm_target=CoreKitTests, module_role=test
dependency Tests/CoreKitTests -> ./Sources/CoreKit
           props: source=internal, spm=true, manifest=Package.swift
```

`module_role` separates production from test targets, so a test target's imports do not
count as production coupling.

An **Xcode app target** is the awkward case: a whole iOS app is often one target, which
would collapse the entire application into a single module and make every architectural
question about it unanswerable. App and app-extension targets declared in an XcodeGen
project are therefore subdivided into **one module per source directory**, with
intra-target type references contributing coupling between them. Frameworks and SPM
packages stay whole, because there the target boundary is already the architecture.

## Symbols

```
symbol …CoreKit.FeedService            props: symbol_kind=class, final=true,
                                              signature="let baseURL: URL\nfunc fetchFeed() async throws -> Data\n…"
symbol …CoreKit.FeedService.fetchFeed  props: async=true, io_direct=true, performs_io=true, receiver=FeedService
symbol …CoreKit.FeedService.loadFeed   props: async=true, performs_io=true, receiver=FeedService
symbol …CoreKit.TreeWalker.walk        props: recursive_self=true, cyclomatic=2
symbol …CoreKit.SelectableCell.setSelected  props: override=true
symbol …CoreKit.Gate                   props: conditional=true, final=true
```

### `io_direct` versus `performs_io`

```swift
func fetchFeed() async throws -> Data {
    let (data, _) = try await URLSession.shared.data(for: request)   // io_direct
}

func loadFeed() async throws -> Data {
    return try await fetchFeed()                                     // performs_io, transitively
}
```

`io_direct` means this function itself invokes a network primitive. `performs_io` is
propagated up the call graph, so a caller three hops from `URLSession` still reports that
it reaches the network. That is the difference between "this function does I/O" and "this
function is on an I/O path", and only the second answers *is it safe to call this in a
loop?*

## Routes — endpoint enums with a protocol-extension prefix

The version prefix is not in the enum. It is a default implementation on the protocol, in
a different declaration:

```swift
protocol APIEndpoint {
    var urlPrefixComponent: String { get }
    var urlPathComponent: String { get }
    var method: HTTPMethod { get }
}

extension APIEndpoint {
    var urlPrefixComponent: String { "v2" }      // repo-wide default
}

enum ReportsEndpoint: APIEndpoint {              // declares no prefix of its own
    case list
    case send([String: Any])
}

extension ReportsEndpoint {
    var urlPathComponent: String {
        switch self { case .list: return "reports.json"; case .send: return "reports.json" }
    }
    var method: HTTPMethod {
        switch self { case .list: return .get; case .send: return .post }
    }
}
```

Every case inherits `"v2"`, so the client routes are `v2/reports.json` for `GET` and for
`POST` — composed across a protocol extension that may be three files away. A direct
`URLSession` call is picked up too:

```
route  feed/items   Sources/CoreKit/Networking.swift:17
       props: role=client, framework=urlsession, source=urlsession, method=GET, api=Networking
```

This is the iOS half of *if I change this endpoint, which screens break?* — the mobile
client's call has to reach the backend route, and it only does if the prefix was resolved.

## Routes — a request base class

An app's own request type takes the path as `endpoint: String` and the verb as a
`method:` with a default. A type whose initializer has that shape is a request base,
whatever it is called, and every request made through it is a client route at the code
making it:

```swift
open class Request<T> {
    init(endpoint: String, method: HTTPMethod = .get, params: Params = [:]) { … }
}

final class Follow: Request<Empty> {
    init(id: Int) { super.init(endpoint: "users/\(id)/follow", method: .post) }
}

static var settings: Request<Settings> { .init(endpoint: "settings") }

let endpoint = Paths.normalize("/api/v1/items")      // generated client
return Request(host: .gateway, endpoint: endpoint, gatewayEndpointPrefix: "/catalogue")
```

```
route  users/{}/follow       POST   caller=…Follow.init
route  settings              GET    caller=…settings
route  catalogue/api/v1/items GET   caller=…ItemsAPI.listRoute
```

The endpoint is read from a literal, a call whose only argument is one, or a local bound
to either earlier in the file; a literal `gatewayEndpointPrefix:` is joined in. A path
held in a property set elsewhere stays `{}`.

## What is deliberately not extracted

- **Runtime-composed URLs.** A path assembled from a computed property with no literal
  binding is not guessed.
- **Objective-C interop.** `@objc` selectors and bridged call sites are not traced.
- **Protocol witness dispatch.** A call through a protocol resolves to the protocol
  requirement, not to every conforming type.
- **`#if` branches** are all parsed; enola does not evaluate build configurations, but the
  affected declarations carry `conditional=true` so you can tell.

---

Measured on real Swift repositories: [BENCHMARKS.md](../BENCHMARKS.md).
