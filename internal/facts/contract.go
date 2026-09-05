package facts

import "strings"

// This file is the SHARED VOCABULARY that crosses the extractor -> linker boundary.
//
// An extractor writes a prop value; a linker, binder or explainer in a different
// package reads it back and changes its behavior based on the exact string. That makes
// the value a contract between two packages that never reference each other — and until
// these constants existed, both sides simply spelled the literal out and hoped.
//
// They did not always agree. The cross-repo linker classified an edge as
// via="http-client" from a private list of ten `source` values; the Java extractor emits
// two more ("java-http-client", "feign") that were never added to it, so every Java
// RestTemplate and Feign call site linked as a generic OpenAPI-style "http" edge. No test
// failed, because nothing tied the writing side to the reading side. That is the class of
// bug this file exists to make impossible: the values live here, beside the registry that
// consumes them, and TestContractVocabulary_NoUnregisteredLiterals fails the build if an
// extractor spells one out by hand.
//
// SCOPE. Only values that are READ outside the package that writes them belong here. A
// `framework` value no one branches on (e.g. "gorilla/mux") is descriptive metadata, not
// a contract, and stays a literal at its emission site.

// Prop keys whose VALUES form a cross-package contract (below). Only these keys are
// listed: this is not a general prop-key namespace, and the many descriptive props
// (line counts, exported flags, complexity metrics) deliberately stay literals.
const (
	// PropSource records WHERE a fact came from — which extractor pass, DSL or
	// generator produced it. See the RouteSource* and DepSource* blocks: the key
	// carries two unrelated vocabularies, discriminated by the fact's Kind.
	PropSource = "source"
	// PropFramework names the framework a fact was extracted from. Mostly
	// descriptive; only the values in the Framework* block are branched on.
	PropFramework = "framework"
	// PropRole is which side of a call a route fact represents (RoleClient/RoleServer).
	PropRole = "role"
	// PropRouteOrigin separates an interface declared by a contract from a route
	// implemented in executable code and an outbound client call site.
	PropRouteOrigin = "route_origin"
	// PropRouteType sub-classifies a route beyond HTTP (RouteTypeGRPC,
	// RouteTypeMiddleware). Absent means a plain HTTP route.
	PropRouteType = "type"
	// PropMessaging names the messaging protocol carried by a topic operation.
	PropMessaging = "messaging"
	// PropMessagingRole identifies which side of a topic operation this fact models.
	PropMessagingRole = "messaging_role"
	// PropMessagingOperation normalizes protocol/spec vocabulary to publish/subscribe.
	PropMessagingOperation = "messaging_operation"
)

// Messaging operation values shared by contract and code extractors. AsyncAPI
// v3 send/receive normalize to these values, as do language call sites.
const (
	MessagingOperationPublish   = "publish"
	MessagingOperationSubscribe = "subscribe"
)

// Messaging fact sources that cross extractor/binder boundaries.
const (
	MessagingSourceAsyncAPI    = "asyncapi"
	MessagingSourceGoKafkaCall = "go-kafka-call"
	MessagingSourceTSKafkaCall = "typescript-kafka-call"
)

// IsMessagingCodeSource is the central registry of extractor sources that may
// implement a messaging contract. New language integrations extend this one
// boundary instead of teaching every binder and explainer their source names.
func IsMessagingCodeSource(source string) bool {
	switch source {
	case MessagingSourceGoKafkaCall, MessagingSourceTSKafkaCall:
		return true
	default:
		return false
	}
}

// MessagingProtocolFamily normalizes protocol aliases shared by contract binding
// and cross-repository signals. Security suffixes describe transport/authentication,
// not a distinct broker technology.
func MessagingProtocolFamily(protocol string) string {
	switch normalized := strings.ToLower(strings.TrimSpace(protocol)); normalized {
	case "kafka", "kafka-secure":
		return "kafka"
	default:
		return normalized
	}
}

func IsKafkaProtocol(protocol string) bool {
	return MessagingProtocolFamily(protocol) == "kafka"
}

// Props written by the messaging contract binder.
const (
	PropMessagingContractBound        = "messaging_contract_bound"
	PropMessagingContractOperationID  = "messaging_contract_operation_id"
	PropMessagingContractFile         = "messaging_contract_file"
	PropMessagingImplementationCount  = "messaging_implementation_count"
	PropMessagingImplementedBy        = "messaging_implemented_by"
	PropMessagingContractStatus       = "messaging_contract_status"
	PropMessagingContractCandidates   = "messaging_contract_candidate_count"
	PropMessagingImplementationStatus = "messaging_implementation_status"
	// PropMessagingDuplicateOf lists files declaring a semantically conflicting
	// version of this operation. Equivalent bundled copies are canonicalized.
	PropMessagingDuplicateOf   = "messaging_duplicate_of"
	PropMessagingCanonicalFile = "messaging_canonical_file"
)

// Messaging contract binding verdicts. These make a missing binding
// explainable: absence, ambiguity and protocol incompatibility are different
// architectural conditions and must not collapse into an absent boolean.
const (
	MessagingContractStatusBound            = "bound"
	MessagingContractStatusUndeclared       = "undeclared"
	MessagingContractStatusAmbiguous        = "ambiguous"
	MessagingContractStatusProtocolMismatch = "protocol_mismatch"
	MessagingImplementationImplemented      = "implemented"
	MessagingImplementationUnimplemented    = "unimplemented"
)

// Via kinds — the `via` labels a cross-repo edge carries, naming HOW the edge
// was established. Signals write them; intent declarations (enola-intent.yaml
// and the cluster config's intent: block) validate against AllViaKinds at
// parse time, so a declared seam can never name a mechanism the linker does
// not produce. The same drift rule as RouteSource: values live here, beside
// the registry that makes free-form spellings impossible.
const (
	ViaHTTP       = "http"        // OpenAPI/spec-derived client call
	ViaHTTPClient = "http-client" // hand-written HTTP client call site
	ViaGRPC       = "grpc"        // gRPC call site or service stub
	ViaGraphQL    = "graphql"     // GraphQL operation matched to a root field
	ViaKafka      = "kafka"       // topic produced by one repo, consumed by another
	ViaImport     = "import"      // shared-library import
	// ViaSharedSymbols is the sharedcode signal's edge: two repos declaring the
	// same exported symbols (a vendored or copied module), evidence of shared
	// code rather than a call.
	ViaSharedSymbols = "shared_symbols"
	// ViaObjectStorage is a storage-mediated seam: one repo writes objects to a
	// bucket path another repo reads (an S3-style export/import handoff). Like
	// kafka it is asynchronous coupling a call graph structurally cannot see;
	// unlike kafka no linker measures it yet, so today it exists for intent
	// declarations — a declared object-storage seam states a data dependency
	// extraction cannot confirm or deny.
	ViaObjectStorage = "object-storage"
)

// AllViaKinds is every registered via value, for intent-declaration validation
// and the conformance test that keeps signal emissions inside this registry.
var AllViaKinds = map[string]bool{
	ViaHTTP:          true,
	ViaHTTPClient:    true,
	ViaGRPC:          true,
	ViaGraphQL:       true,
	ViaKafka:         true,
	ViaImport:        true,
	ViaSharedSymbols: true,
	ViaObjectStorage: true,
}

// Package ecosystems — the `ecosystem` prop a declared external dependency
// carries, naming the packaging system it was declared in. The manifests
// extractor writes them; intent declarations validate against
// AllPackageEcosystems at parse time, for the same reason a seam's via is
// validated: a declaration that names an ecosystem nothing measures would sit
// in the file looking enforced and match nothing forever.
const (
	EcosystemGo       = "go"
	EcosystemNPM      = "npm"
	EcosystemRubyGems = "rubygems"
	EcosystemCargo    = "cargo"
	EcosystemPub      = "pub"
	EcosystemPyPI     = "pypi"
)

// AllPackageEcosystems is every registered ecosystem value.
var AllPackageEcosystems = map[string]bool{
	EcosystemGo:       true,
	EcosystemNPM:      true,
	EcosystemRubyGems: true,
	EcosystemCargo:    true,
	EcosystemPub:      true,
	EcosystemPyPI:     true,
}

// TypePackage is the `type` prop marking a KindDependency fact as a DECLARED
// EXTERNAL PACKAGE rather than an import edge between this repository's own
// modules. Both are dependencies; the prop is what a selector narrows on.
const TypePackage = "package"

// Route role values (the PropRole prop on a KindRoute fact): which side of the call
// this route represents. The cross-repo HTTP linker matches RoleClient routes against
// RoleServer ones; a route with no role is treated as a server route, because an
// extractor that found a route declaration without a call site found a served endpoint.
const (
	RoleClient                = "client"
	RoleServer                = "server"
	RouteOriginContract       = "contract"
	RouteOriginImplementation = "implementation"
	RouteOriginClientCall     = "client_call"
)

// RouteOrigin returns the semantic origin of a route, including a fallback for
// facts written before route_origin existed.
func RouteOrigin(f Fact) string {
	if origin := f.PropString(PropRouteOrigin); origin != "" {
		return origin
	}
	if f.PropString(PropRole) == RoleClient {
		return RouteOriginClientCall
	}
	switch f.PropString(PropSource) {
	case RouteSourceOpenAPI, RouteSourceGRPCProto:
		return RouteOriginContract
	default:
		return RouteOriginImplementation
	}
}

// Route type values (the PropRouteType prop on a KindRoute fact).
const (
	// RouteTypeGRPC marks a route derived from a .proto service definition or a gRPC
	// call site, rather than an HTTP path. Its path is the wire path
	// "/pkg.Service/Method", and grpcimpl binding keys on it.
	RouteTypeGRPC = "grpc"

	// RouteTypeGraphQL marks a GraphQL operation-surface route: a server root
	// field (`Query.pageViews`) or a client operation's root field. Kept out of
	// HTTP path matching the way gRPC is; the graphql cross-repo signal owns
	// the join.
	RouteTypeGraphQL = "graphql"
	// RouteTypeMiddleware marks a registration that wraps other routes rather than
	// serving one. Handler binding skips these: a middleware's "handler" is a
	// middleware func, and binding it to a route would pollute handled_by.
	RouteTypeMiddleware = "middleware"
)

// Framework values that are BRANCHED ON outside the extractor that writes them. Every
// other framework value is descriptive and stays a literal at its emission site.
const (
	// FrameworkGRPC marks a route as a gRPC call site or service definition. The
	// cross-repo linker reads it to label the edge via="grpc" rather than
	// via="http-client", so a gRPC dependency is distinguishable from an HTTP one.
	FrameworkGRPC = "grpc"
)

// RouteSource values — the PropSource prop on a KindRoute fact. This is the provenance
// of the route: which extractor pass or contract format produced it.
//
// The cross-repo linker reads these to decide HOW an edge was established (see
// HandWrittenClientSources), and the gRPC client-FQN binder reads
// RouteSourcePythonGRPCClient to find the routes it must rewrite. Both are behavior
// changes keyed on the exact string, which is what makes these a contract rather than a
// label.
const (
	// Hand-written HTTP call sites: code a human wrote that issues a request.
	RouteSourceGoHTTPClient   = "go-http-client"
	RouteSourceTSHTTPClient   = "ts-http-client"
	RouteSourceRubyHTTPClient = "ruby-http-client"
	RouteSourcePHPHTTPClient  = "php-http-client"
	RouteSourceJavaHTTPClient = "java-http-client" // Spring RestTemplate / WebClient
	RouteSourceFeign          = "feign"            // Spring Cloud @FeignClient interface
	RouteSourceRetrofit       = "retrofit"         // Kotlin/Java Retrofit service interface
	RouteSourceURLSession     = "urlsession"       // Swift URLSession
	RouteSourceSwiftEndpoint  = "swift-endpoint"   // Swift endpoint enum / protocol extension

	// Hand-written gRPC call sites. Same "a human wrote this call" property as the
	// HTTP sources above, over a different transport.
	RouteSourceGoGRPCClient     = "go-grpc-client"
	RouteSourceTSGRPCClient     = "ts-grpc-client"
	RouteSourcePythonGRPCClient = "python-grpc-client"

	// Contract-derived routes: read from a spec or IDL rather than from call-site
	// code. These describe an interface, so they are NOT hand-written call sites.
	RouteSourceGRPCProto         = "grpc-proto"         // .proto service definition
	RouteSourceOpenAPI           = "openapi"            // OpenAPI/Swagger spec
	RouteSourceOpenAPITypeScript = "openapi-typescript" // generated TS client from a spec

	// GraphQL contract sources: the graphql-ruby field DSL (server), a
	// schema-first Node server SDL (server), gql-tagged template
	// literals (hand-written client operations), standalone .graphql operation
	// documents (Apollo codegen inputs — client operations), and Ruby
	// operation-string literals (a Rails service calling a sibling service's
	// GraphQL API — client operations).
	RouteSourceGraphQLRubyDSL    = "graphql-ruby-dsl"
	RouteSourceGraphQLSDL        = "graphql-sdl"
	RouteSourceGraphQLTag        = "graphql-tag"
	RouteSourceGraphQLOperation  = "graphql-operation-file"
	RouteSourceGraphQLRubyString = "graphql-ruby-string"
	RouteSourceGraphQLDecorator  = "graphql-code-first-decorator"
	RouteSourceGraphQLNexus      = "graphql-nexus"
	RouteSourceGraphQLPothos     = "graphql-pothos"
	RouteSourceGraphQLClientCall = "graphql-client-call"
	RouteSourceSymfonyConfig     = "symfony-config" // Symfony YAML/XML route config

	// Scala. The Play routes file is a DSL of its own rather than Scala source, and
	// declares a whole application's HTTP surface in one place; the other two are
	// route trees written in Scala. sttp is the hand-written client call site.
	RouteSourceAngularRouter   = "angular-router"    // Angular Routes array, composed through forRoot/forChild/loadChildren
	RouteSourcePlayRoutes      = "play-routes"       // Play conf/routes and its included *.routes
	RouteSourcePekkoHTTP       = "pekko-http"        // Pekko/Akka HTTP routing directives
	RouteSourceHTTP4s          = "http4s"            // http4s HttpRoutes.of pattern match
	RouteSourceScalaHTTPClient = "scala-http-client" // sttp / Play WS / http4s client

	// Dart covers both spellings with one value, because both are hand-written
	// requests and the linker has no reason to tell them apart: a package:http /
	// dio call site, and a retrofit/chopper annotated interface method. The latter
	// generates its implementation into a .g.dart the extractor excludes, so the
	// annotation IS the call site as far as extraction is concerned.
	RouteSourceDartHTTPClient = "dart-http-client"

	// A call site read through an in-house client the config declares (clients:, see
	// internal/clientspec). Hand-written like the sources above, but recognised because
	// config named the client's type rather than because enola knows the library. Its
	// own value, so a reader can tell a route that exists only because of that config.
	RouteSourceConfiguredHTTPClient = "configured-http-client"
)

// PropClientSpec is the prop naming the declared client (the spec's name) a
// RouteSourceConfiguredHTTPClient route was read through.
const PropClientSpec = "client_spec"

// HandWrittenClientSources is the set of RouteSource values that mean "a human wrote
// this call site", as opposed to a route derived from a generated client or a contract
// spec. The cross-repo linker reads it to label an edge via="http-client" instead of the
// generic via="http", so a reader can tell a call someone wrote from one a spec implies.
//
// It lives here, beside the constants extractors emit, rather than inside the linker.
// The linker previously kept a private copy, and it silently omitted the two Java
// sources for as long as the Java HTTP-client extractor has existed — the drift this
// file is designed to prevent. Membership is a property of the source, so it is declared
// where the source is.
//
// gRPC sources are members: they are hand-written call sites too. The linker labels them
// via="grpc" first (FrameworkGRPC wins), so their membership only matters if that
// framework prop is ever absent.
var HandWrittenClientSources = map[string]bool{
	RouteSourceGraphQLTag:           true,
	RouteSourceGraphQLRubyString:    true,
	RouteSourceGraphQLDecorator:     true,
	RouteSourceGraphQLNexus:         true,
	RouteSourceGraphQLPothos:        true,
	RouteSourceGraphQLClientCall:    true,
	RouteSourceGoHTTPClient:         true,
	RouteSourceTSHTTPClient:         true,
	RouteSourceRubyHTTPClient:       true,
	RouteSourcePHPHTTPClient:        true,
	RouteSourceJavaHTTPClient:       true,
	RouteSourceFeign:                true,
	RouteSourceRetrofit:             true,
	RouteSourceURLSession:           true,
	RouteSourceSwiftEndpoint:        true,
	RouteSourceGoGRPCClient:         true,
	RouteSourceTSGRPCClient:         true,
	RouteSourcePythonGRPCClient:     true,
	RouteSourceScalaHTTPClient:      true,
	RouteSourceDartHTTPClient:       true,
	RouteSourceAngularRouter:        true,
	RouteSourceConfiguredHTTPClient: true,
}

// NativeAppClientSources is the set of RouteSource values only a native application
// emits — a mobile or desktop client that issues HTTP requests and cannot serve them.
// The vendored-spec binder reads it as a POSITIVE marker: a repo containing one of
// these ships an HTTP client, so an OpenAPI spec sitting in it describes an API the
// repo CALLS, not one it serves.
//
// It lives here for the same reason HandWrittenClientSources does: membership is a
// property of the source, so it is declared beside the constants extractors emit
// rather than copied into the reader.
//
// The Java sources are deliberately absent. RouteSourceFeign and
// RouteSourceJavaHTTPClient are emitted by Spring services that legitimately serve
// endpoints while calling others, so their presence proves nothing about whether the
// repo is a server — which is the entire question this set answers. Every member below
// belongs to a platform that has no server story at all.
var NativeAppClientSources = map[string]bool{
	RouteSourceRetrofit:       true, // Kotlin/Java Retrofit service interface (Android)
	RouteSourceURLSession:     true, // Swift URLSession (iOS/macOS)
	RouteSourceSwiftEndpoint:  true, // Swift endpoint enum / protocol extension
	RouteSourceDartHTTPClient: true, // Flutter package:http / dio / chopper / retrofit
}

// AllRouteSources is every registered RouteSource value. It exists for the conformance
// test, which checks that no route fact in the golden corpus carries a source this file
// does not know about — the check that catches a new extractor pass inventing a value
// and never telling the linker.
var AllRouteSources = map[string]bool{
	RouteSourceGraphQLRubyDSL:    true,
	RouteSourceGraphQLSDL:        true,
	RouteSourceGraphQLTag:        true,
	RouteSourceGraphQLOperation:  true,
	RouteSourceGraphQLRubyString: true,
	RouteSourceGoHTTPClient:      true,
	RouteSourceTSHTTPClient:      true,
	RouteSourceRubyHTTPClient:    true,
	RouteSourcePHPHTTPClient:     true,
	RouteSourceJavaHTTPClient:    true,
	RouteSourceFeign:             true,
	RouteSourceRetrofit:          true,
	RouteSourceURLSession:        true,
	RouteSourceSwiftEndpoint:     true,
	RouteSourceGoGRPCClient:      true,
	RouteSourceTSGRPCClient:      true,
	RouteSourcePythonGRPCClient:  true,
	RouteSourceGRPCProto:         true,
	RouteSourceOpenAPI:           true,
	RouteSourceOpenAPITypeScript: true,
	RouteSourceSymfonyConfig:     true,
	RouteSourcePlayRoutes:        true,
	RouteSourcePekkoHTTP:         true,
	RouteSourceHTTP4s:            true,
	RouteSourceScalaHTTPClient:   true,
	RouteSourceDartHTTPClient:    true,

	RouteSourceConfiguredHTTPClient: true,
}

// DepSource values — the PropSource prop on a KindDependency fact. A SECOND, unrelated
// vocabulary on the same prop key, discriminated by the fact's Kind: here `source`
// classifies where an import RESOLVES TO, not which pass produced the fact.
//
// The overload is historical and not worth a migration (renaming a prop key rewrites
// every golden and every saved snapshot), but it must be stated somewhere, because
// reading `source` without first checking Kind gets you a value from the wrong
// vocabulary. Registered here so the conformance test can tell the two apart rather than
// reporting every "internal" as an unknown route source.
const (
	DepSourceInternal = "internal" // resolves to a module inside this repo
	DepSourceExternal = "external" // resolves to a third-party package
	DepSourceStdlib   = "stdlib"   // resolves to the language's standard library
)

// CompilationUnitProps name the module props that identify the unit a module is
// COMPILED INTO — the assembly, crate or binary the build system produces.
//
// It is a contract for the same reason the rest of this file is: an extractor
// writes the prop and the cycles explainer reads it back, and the two packages
// never reference each other. It is a TABLE rather than a branch so that teaching
// another language is a row.
//
// Why an explainer wants to know. A dependency cycle between modules is a
// build-order defect only when those modules are separately compiled. Every build
// system that admits sub-units forbids cycles between the units themselves —
// MSBuild rejects a circular ProjectReference, Cargo a circular crate dependency
// — so a cycle enola finds in such a language is necessarily WITHIN one unit,
// where mutual references are legal and ordinary. Reporting it as something that
// "can cause initialization issues" is then simply untrue.
//
// Membership rule: a prop belongs here only when SEVERAL module facts can share
// one value. Swift's `spm_target` / `xcode_target` are deliberately absent —
// swiftextractor names each module fact BY its target, so two modules never share
// one, and a cycle between them really is a cycle between separately built units.
var CompilationUnitProps = []string{
	"crate",      // Rust: the Cargo crate a module directory belongs to
	"project",    // C#: the MSBuild project (assembly) a module directory belongs to
	"jvm_module", // Scala: the sbt/Maven/Gradle module a source directory compiles into
	// Dart: the pub package a library belongs to. Dart is the most permissive case
	// this table covers — where MSBuild and Cargo FORBID a cycle between units, Dart
	// does not even forbid one between libraries INSIDE a unit: circular imports are
	// legal, compile, and are common in practice (a model importing its repository
	// while the repository imports the model). So a Dart cycle is never a build-order
	// defect, and without this row the explainer would report a mutual import as
	// something that "can cause initialization issues" — which for Dart is simply not
	// true.
	"pub_package",
}

// CompilationUnit returns the build unit a module fact belongs to, or "" when the
// language does not model one. Only module facts carry these props.
func CompilationUnit(f Fact) string {
	for _, key := range CompilationUnitProps {
		if v, ok := f.Props[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}
