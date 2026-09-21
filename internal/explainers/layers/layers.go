package layers

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/explainers/common"
	"github.com/enola-labs/enola/internal/facts"
)

// LayerExplainer detects architectural patterns and layer violations.
type LayerExplainer struct{}

// New creates a new LayerExplainer.
func New() *LayerExplainer {
	return &LayerExplainer{}
}

func (e *LayerExplainer) Name() string {
	return "layers"
}

// layerDef defines how we detect architectural layers from module path patterns.
type layerDef struct {
	Name     string
	Patterns []string
	Level    int // Lower level = inner/domain, higher = outer/infra
	// Neutral marks a layer that is CLASSIFIED but never ordered: no import to or
	// from it can be a violation, whatever the levels say.
	//
	// Wiring is the case this exists for. A Hilt `di` package and a Spring
	// `@Configuration` package are referenced by every layer they wire and
	// reference every layer they wire — that is their whole job — so any level
	// they are given makes half of those edges a violation. Giving them one
	// produced 61 of thingsboard's 75 findings, 7 of dubbo's 7, and
	// nowinandroid's `data -> di`, none of which name a defect.
	//
	// It is the same argument the Go layout makes for `internal`/`pkg` and the
	// Rails one for `lib`, with one difference: those are collapsed into a shared
	// tier because they hold ANY layer, whereas wiring is a real, nameable thing
	// that simply has no place in a dependency order. Keeping it classified keeps
	// it out of the unclassified remainder, so coverage still counts it.
	Neutral bool
}

// Predefined layer patterns for common architectures.
var (
	// Hexagonal / Clean Architecture layers.
	// Order matters: more specific patterns (application, adapter) are checked before
	// broader ones (domain) to avoid misclassification of paths like Domain/UseCases.
	hexagonalLayers = []layerDef{
		{Name: "application", Patterns: []string{"application", "usecase", "usecases"}, Level: 1},
		{Name: "port", Patterns: []string{"port", "ports", "interface", "interfaces"}, Level: 1},
		{Name: "adapter", Patterns: []string{"adapter", "adapters", "infrastructure", "infra", "gateway", "network"}, Level: 2},
		{Name: "repository", Patterns: []string{"repository", "repositories", "repo", "repos", "store", "storage", "persistence", "db", "database"}, Level: 2},
		{Name: "presentation", Patterns: []string{"presentation", "ui", "view", "views", "screen", "screens", "page", "pages"}, Level: 3},
		{Name: "handler", Patterns: []string{"handler", "handlers", "controller", "controllers", "api", "http", "grpc", "rest"}, Level: 3},
		// `core` is deliberately absent, for the reason the Angular taxonomy gives
		// for the same word: it names a CONTAINER, not a layer. A PHP platform
		// keeping its whole product under src/Core/ had 1049 of its 1491 classified
		// modules read as domain on the strength of that one segment, which then
		// made every Core/… module reaching Core/Framework/Adapter or Core/…/Api a
		// violation — 339 of them, on a repository this taxonomy does not describe
		// at all.
		{Name: "domain", Patterns: []string{"domain", "entity", "entities", "model", "models"}, Level: 0},
	}

	// Next.js layers
	nextjsLayers = []layerDef{
		{Name: "pages", Patterns: []string{"pages", "app"}, Level: 3},
		{Name: "components", Patterns: []string{"components", "ui"}, Level: 2},
		{Name: "hooks", Patterns: []string{"hooks"}, Level: 1},
		{Name: "lib", Patterns: []string{"lib", "utils", "helpers"}, Level: 0},
		{Name: "api", Patterns: []string{"api"}, Level: 3},
		{Name: "services", Patterns: []string{"services"}, Level: 1},
		{Name: "types", Patterns: []string{"types"}, Level: 0},
	}

	// Nuxt layout.
	//
	// Unlike php-layered above, this is NOT derived from what repositories happen
	// to share: Nuxt PRESCRIBES its directory structure, and the framework gate
	// means the taxonomy cannot be applied to anything that is not a Nuxt
	// application. The one Nuxt repository in the corpus validates it rather than
	// defines it.
	//
	// `server` is classified and left unordered. It is the Nitro backend — a
	// different runtime that happens to live in the same tree — so it sits at no
	// point in the front end's dependency order, and plugins and middleware are
	// wiring for the reason the Spring config package is.
	nuxtLayers = []layerDef{
		{Name: "pages", Patterns: []string{"pages", "layouts"}, Level: 3},
		{Name: "components", Patterns: []string{"components"}, Level: 2},
		{Name: "composables", Patterns: []string{"composables", "stores"}, Level: 1},
		{Name: "utils", Patterns: []string{"utils", "types", "constants"}, Level: 0},
		{Name: "server", Patterns: []string{"server"}, Neutral: true},
		{Name: "wiring", Patterns: []string{"plugins", "middleware"}, Neutral: true},
	}

	// SvelteKit layout. Prescribed by the framework, like Nuxt above, and smaller
	// than any other taxonomy here because SvelteKit prescribes less: src/routes
	// holds what the router serves and src/lib holds everything it is built from.
	// Two tiers is the whole of the ordering it defines, so on most repositories
	// this will name a layout and grade nothing — which the statement says.
	svelteKitLayers = []layerDef{
		{Name: "routes", Patterns: []string{"routes"}, Level: 2},
		{Name: "lib", Patterns: []string{"lib"}, Level: 1},
	}

	// Ember (Octane) app layout. The ordering expresses the real smells: a
	// service or model importing a component, or anything importing a route,
	// runs against the resolver's direction of flow. Peers are collapsed to one
	// tier per the Rails/Go precedent — routes, controllers and route templates
	// are all delivery; components, helpers and modifiers are all rendering
	// (a modifier importing a service is normal, a service importing a
	// component is not); services and the ember-data quartet are all domain.
	// Angular's own conventions, as its style guide states them and as ten public
	// repositories write them.
	//
	// Four words a naive reading of the style guide would include are deliberately
	// absent, each because the corpus showed what claiming it costs:
	//
	//   - `core` and `shared` are CONTAINERS, not layers. A real application keeps
	//     `core/services/…` and `shared/components/…`, whose inner segment already
	//     names the layer, and claiming the container made every `core` service
	//     reaching into `shared` a violation — which is how those applications are
	//     built.
	//   - `layout` is a shell in an application and a family of layout components in
	//     a library, and it also appears NESTED (`core/services/layout`), where
	//     claiming it inverted the enclosing directory's own layer. 17 of one
	//     library's 21 violations came from it.
	//   - `selectors` means NgRx selectors in one convention and picker widgets in
	//     another; one workspace's `shared/lib/selectors` is components.
	//
	// What is left is the set whose meaning does not move between repositories.
	angularLayers = []layerDef{
		{Name: "pages", Patterns: []string{"pages", "features", "views", "containers", "screens"}, Level: 3},
		{Name: "components", Patterns: []string{"components"}, Level: 2},
		{Name: "directives", Patterns: []string{"directives"}, Level: 2},
		{Name: "pipes", Patterns: []string{"pipes"}, Level: 2},
		{Name: "services", Patterns: []string{"services", "guards", "resolvers", "interceptors"}, Level: 1},
		{Name: "store", Patterns: []string{"store", "effects", "reducers", "facades"}, Level: 1},
		{Name: "models", Patterns: []string{"models", "types", "interfaces", "constants"}, Level: 0},
	}

	emberLayers = []layerDef{
		{Name: "route", Patterns: []string{"routes"}, Level: 3},
		{Name: "controller", Patterns: []string{"controllers"}, Level: 3},
		{Name: "template", Patterns: []string{"templates"}, Level: 3},
		{Name: "component", Patterns: []string{"components"}, Level: 2},
		{Name: "helper", Patterns: []string{"helpers"}, Level: 2},
		{Name: "modifier", Patterns: []string{"modifiers"}, Level: 2},
		{Name: "service", Patterns: []string{"services"}, Level: 1},
		{Name: "model", Patterns: []string{"models"}, Level: 1},
		{Name: "adapter", Patterns: []string{"adapters"}, Level: 1},
		{Name: "serializer", Patterns: []string{"serializers"}, Level: 1},
		{Name: "transform", Patterns: []string{"transforms"}, Level: 1},
		// `lib` is deliberately NOT a util pattern. In Ember Octane utilities live in
		// `app/utils/`; `lib/` holds in-repo ADDONS, which are whole packages with their
		// own app trees and no business at the bottom of a dependency order. Claiming
		// the bare segment was also the single largest source of false layer violations
		// in the corpus: discourse is a Rails backend beside an Ember frontend, the
		// Ember pattern wins the repo, and Ruby's `lib/` — where a large Rails app keeps
		// most of its domain code — became level 0. 397 of discourse's 426 reported
		// violations were `util -> …` edges out of a Ruby lib directory calling the
		// models and services it is supposed to call.
		{Name: "util", Patterns: []string{"utils", "constants"}, Level: 0},
	}

	// Go standard project layout.
	//
	// `internal` and `pkg` are not layers. They are a VISIBILITY distinction the
	// compiler enforces — code under internal/ may only be imported from within the
	// module — and either may hold any layer. pkg/ wrapping internal/ is the standard
	// way to publish an API over a private implementation, and internal/ importing a
	// contract published in pkg/ is how a plugin interface gets implemented. Ranking
	// them made the explainer report both as violations on essentially every Go
	// repository that has both directories, including enola itself.
	//
	// So they are collapsed to one tier, exactly as the Rails layout below collapses
	// its domain directories and for the same reason: a distinction that is not a
	// dependency ordering must not be modelled as one, or the finding is noise and
	// readers learn to skip it.
	//
	// `api` joins them: in the standard layout it holds contract definitions (OpenAPI
	// specs, .proto files and the code generated from them), which implementations are
	// supposed to depend on.
	//
	// That leaves one genuine ordering, and the explainer still catches it: `cmd` holds
	// entrypoints, so anything importing INTO cmd is a library reaching into a binary —
	// a real smell, and the only one this layout expresses.
	goStdLayers = []layerDef{
		{Name: "cmd", Patterns: []string{"cmd"}, Level: 2},
		{Name: "internal", Patterns: []string{"internal"}, Level: 1},
		{Name: "pkg", Patterns: []string{"pkg"}, Level: 1},
		{Name: "api", Patterns: []string{"api"}, Level: 1},
	}

	// Ruby on Rails layout.
	//
	// Rails is not a hexagonal/onion architecture: models routinely call service
	// objects, jobs, mailers and helpers, and that is idiomatic — not a layer
	// violation. Modelling it with fine-grained inner/outer levels made the layers
	// explainer flag most Rails codebases (model -> service, model -> job,
	// service -> helper, ...). So we collapse it to two tiers:
	//
	//   delivery (Level 2): controllers, views — the HTTP/request surface
	//   domain   (Level 1): everything the app is built from (models, services,
	//                       jobs, mailers, helpers, interactors, presenters, ...)
	//
	// With every domain directory at the same level, intra-domain references are
	// same-level and never violations; only a domain module reaching UP into the
	// delivery layer (e.g. a model or service referencing a controller/view
	// constant) is reported — which is a genuine Rails smell.
	railsLayers = []layerDef{
		{Name: "controller", Patterns: []string{"controller", "controllers"}, Level: 2},
		{Name: "view", Patterns: []string{"view", "views"}, Level: 2},
		{Name: "model", Patterns: []string{"model", "models"}, Level: 1},
		{Name: "service", Patterns: []string{"service", "services"}, Level: 1},
		{Name: "job", Patterns: []string{"job", "jobs", "worker", "workers"}, Level: 1},
		{Name: "mailer", Patterns: []string{"mailer", "mailers"}, Level: 1},
		{Name: "helper", Patterns: []string{"helper", "helpers"}, Level: 1},
		{Name: "interactor", Patterns: []string{"interactor", "interactors"}, Level: 1},
		{Name: "presenter", Patterns: []string{"presenter", "presenters"}, Level: 1},
		{Name: "serializer", Patterns: []string{"serializer", "serializers"}, Level: 1},
		{Name: "notifier", Patterns: []string{"notifier", "notifiers"}, Level: 1},
		{Name: "policy", Patterns: []string{"policy", "policies"}, Level: 1},
		{Name: "decorator", Patterns: []string{"decorator", "decorators"}, Level: 1},
		{Name: "form", Patterns: []string{"form", "forms"}, Level: 1},
		{Name: "query", Patterns: []string{"query", "queries"}, Level: 1},
		{Name: "validator", Patterns: []string{"validator", "validators"}, Level: 1},
		{Name: "type", Patterns: []string{"type", "types"}, Level: 1},
		{Name: "lib", Patterns: []string{"lib"}, Level: 1},
	}

	// Android clean architecture / MVVM layout.
	//
	// The order is DATA at the bottom, then domain, then UI — Android's own
	// guidance, not the Uncle-Bob orientation the iOS layout below uses. The
	// difference is real rather than an oversight: Android's domain layer holds
	// use cases that call repositories, so `domain -> data` is the documented
	// direction of flow. nowinandroid's own architecture guide states it as
	// "with the data layer at the bottom", and every use case in it imports a
	// repository. Ranking domain innermost reported three of those imports as
	// violations on Google's reference application.
	androidLayers = []layerDef{
		{Name: "data", Patterns: []string{"data", "repository", "repositories"}, Level: 0},
		{Name: "domain", Patterns: []string{"domain"}, Level: 1},
		{Name: "ui", Patterns: []string{"ui", "presentation", "view", "views", "screen", "screens"}, Level: 3},
		{Name: "designsystem", Patterns: []string{"designsystem"}, Level: 3},
		{Name: "di", Patterns: []string{"di", "injection"}, Neutral: true},
	}

	// iOS clean architecture / MVVM layout
	iosLayers = []layerDef{
		{Name: "domain", Patterns: []string{"domain"}, Level: 0},
		{Name: "data", Patterns: []string{"data", "repository", "repositories"}, Level: 1},
		{Name: "ui", Patterns: []string{"ui", "presentation", "view", "views", "screen", "screens", "components"}, Level: 3},
		{Name: "designsystem", Patterns: []string{"designsystem"}, Level: 3},
	}

	// Spring layered architecture layout
	springLayers = []layerDef{
		{Name: "controller", Patterns: []string{"controller", "controllers", "rest", "web"}, Level: 3},
		{Name: "service", Patterns: []string{"service", "services"}, Level: 1},
		{Name: "repository", Patterns: []string{"repository", "repositories", "dao", "daos"}, Level: 1},
		{Name: "entity", Patterns: []string{"entity", "entities", "model", "models", "domain"}, Level: 0},
		{Name: "dto", Patterns: []string{"dto", "dtos"}, Level: 2},
		// Wiring, not a layer — see layerDef.Neutral. `dto` keeps its level: the
		// corpus produced no dto finding at all, and a rank nothing has exercised
		// is not evidence to change.
		{Name: "config", Patterns: []string{"config", "configuration"}, Neutral: true},
	}

	// .NET clean architecture. The layer is the last dot-separated component of a
	// project name (Ordering.Domain, Catalog.API), which is why this pattern opts
	// into dotted matching below.
	// Only the four layers whose DEPENDENCY DIRECTION Clean Architecture actually
	// fixes. `.Abstractions`, `.Shared`, `.Common` and `.Core` are deliberately
	// absent: they name a shared kernel that sits at no particular level, and
	// giving them one produced ~230 "violations" on OrchardCore — a modular CMS
	// that is not clean architecture and never claimed to be. A pattern that fires
	// on the wrong repo is worse than one that fires on fewer right ones.
	dotnetCleanLayers = []layerDef{
		{Name: "domain", Patterns: []string{"domain", "entities"}, Level: 0},
		{Name: "application", Patterns: []string{"application", "usecases"}, Level: 1},
		{Name: "infrastructure", Patterns: []string{"infrastructure", "persistence", "repositories"}, Level: 2},
		{Name: "api", Patterns: []string{"api", "webapp", "grpc"}, Level: 3},
	}

	// PHP application layout.
	//
	// There is no single PHP framework the way there is a single Rails, so this is
	// gated on the LANGUAGE and built only from the words that recur across
	// unrelated PHP codebases — a forum, a groupware server and its apps all use
	// Controller, Service, Db and Command to mean the same things, without sharing
	// a framework. Measured over the PHP modules of four such repositories this
	// vocabulary names 31%, 38%, 36% and 57% of them, and 17% of WordPress, which
	// has no such layering and therefore stays under the coverage floor rather
	// than being described by a taxonomy that does not fit it.
	//
	// Three tiers, not more. The corpus supports delivery above domain above data
	// and nothing finer, and a distinction that is not a dependency ordering must
	// not be modelled as one — the argument the Rails layout below makes at
	// length.
	//
	// Listeners, subscribers and service providers are classified and left
	// UNORDERED. They are invoked by the framework and reference whatever they
	// wire, which is the same shape as the Hilt and Spring wiring packages above,
	// and nothing in the corpus says which tier they belong to. Exceptions join
	// them because every layer defines and throws them.
	phpLayers = []layerDef{
		{Name: "controller", Patterns: []string{"controller", "controllers"}, Level: 2},
		{Name: "http", Patterns: []string{"http", "api", "middleware", "request", "requests"}, Level: 2},
		{Name: "command", Patterns: []string{"command", "commands", "console"}, Level: 2},
		{Name: "service", Patterns: []string{"service", "services", "job", "jobs", "handler", "handlers", "factory", "factories"}, Level: 1},
		{Name: "data", Patterns: []string{"db", "entity", "entities", "model", "models", "repository", "repositories", "migration", "migrations"}, Level: 0},
		{Name: "wiring", Patterns: []string{"listener", "listeners", "subscriber", "subscribers", "provider", "providers", "exception", "exceptions"}, Neutral: true},
	}

	// Django layout
	djangoLayers = []layerDef{
		{Name: "models", Patterns: []string{"model", "models"}, Level: 0},
		{Name: "views", Patterns: []string{"view", "views"}, Level: 3},
		{Name: "serializers", Patterns: []string{"serializer", "serializers"}, Level: 2},
		{Name: "urls", Patterns: []string{"url", "urls"}, Level: 3},
		{Name: "admin", Patterns: []string{"admin"}, Level: 3},
		{Name: "forms", Patterns: []string{"form", "forms"}, Level: 2},
	}
)

// patternDef defines an architecture pattern together with the signals that gate
// its detection. Without gating, generic directory names (api, app, ui, lib,
// model, ...) cause patterns to match repos of the wrong language/framework.
type patternDef struct {
	name   string
	layers []layerDef

	// appliesTo names the languages this taxonomy DESCRIBES. Empty means any.
	//
	// It replaces a gate on the repository's dominant language, which asked the
	// wrong question twice over. A Go application with a larger TypeScript front
	// end has a dominant language of typescript, so its Go layout was never even
	// considered — grafana's 954 Go modules got no statement at all. And a
	// taxonomy admitted by the dominant language then classified every module in
	// the repository, including the ones written in something else: a Rails
	// monolith that ships an Ember front end had its Ruby app/services and
	// app/serializers verdicted through Ember's layer order.
	//
	// Scoring a taxonomy over the modules it could describe, rather than over
	// every module in the repository, fixes both: the denominator is the cohort,
	// and a polyglot repository gets one statement per cohort instead of one
	// statement and a wrong one.
	appliesTo []string
	// frameworks, if non-empty, requires at least one of these frameworks to be
	// present in the facts for the pattern to be considered.
	frameworks []string
	// dottedSegments makes a path segment match on its dot-separated components as
	// well as whole. .NET names a project `<Product>.<Layer>` — `Ordering.Domain`,
	// `Catalog.API` — so the layer never equals the segment. Opt-in rather than
	// global: a dotted directory in another ecosystem (`cal.com`, `foo.web`) would
	// otherwise start matching layers it has nothing to do with.
	dottedSegments bool

	// signatureLayers, if non-empty, requires at least minSignatureLayers of the
	// matched layers to be distinctive ones from this set (so a pattern built
	// only from generic names — or from a single stray directory — does not
	// qualify).
	signatureLayers []string
	// autoloadRoot, if set, names a directory whose immediate children are
	// application layers by the framework's own contract even when no pattern
	// lists them. Rails/Zeitwerk autoloads every app/* directory as a root, so
	// app/tools or app/agents is as much a layer as app/models — and leaving it
	// unclassified makes every rule that governs it invisible to the explainer.
	autoloadRoot       string
	autoloadLevel      int
	minSignatureLayers int
}

// minClassifiedShare is the fraction of a repository's distinct modules a
// taxonomy must name before it is allowed to compete for the statement at all.
//
// CALIBRATED, NOT CHOSEN. Measured over the labelled corpus in
// enola-benchmarks/arch-expected.json, the classified share separates cleanly
// with a ten-point gap and nothing inside it:
//
//	 3%  a modular CMS named "dotnet-clean" — which it has never claimed to be
//	 9%  an RPC framework named "spring-layered" off one Spring sub-project
//	13%  a Go application named "go-standard" with no internal/ or pkg/ at all,
//	     on 42 directories under routers/api/** matching the word "api"
//	14%  a media server named "dotnet-clean"
//	     ── 0.20 ──
//	24%  the Android reference application, correctly named
//	31%  … and every repository above it, all correctly named
//
// So the floor is not tuned to a target: every repository below it is a wrong
// statement and every repository above it is a right one. What makes them wrong
// is the same thing in each case — a taxonomy recognising its own vocabulary in
// a repository built to a different plan — and a repository that follows a
// layout genuinely does match most of it (enola's own tree: 92%).
//
// A floor SUPPRESSES rather than downgrades, because a low-confidence
// architecture statement is not a weaker claim, it is a wrong one: nothing a
// reader does with "this is dotnet-clean" gets better for being told it was 3%.
//
// It is applied to the winning pattern only — see thickEnough.
const minClassifiedShare = 0.20

// FIVE ECOSYSTEMS ARE DELIBERATELY ABSENT, AND EACH WAS MEASURED BEFORE BEING
// LEFT OUT. The rule this file follows is that a taxonomy names only words whose
// meaning does not move between repositories; these four have no such words.
//
//	Python   The obvious gap — 2,265 unnamed modules across two repositories —
//	         and the vocabulary does not survive comparison. One workflow engine's
//	         recurring segments are hooks, operators, sensors and providers, which
//	         are its own PLUGIN KINDS sitting at one level rather than layers; a
//	         commerce platform's are graphql, mutations and migrations; an
//	         analytics platform's are commands and views; a library's are modules,
//	         infrastructure and tasks. What repeats across all four is `utils` and
//	         `common`. There is no Python layer vocabulary to encode, only four
//	         project vocabularies.
//
//	Rust     Three repositories, three unrelated layouts: an async runtime whose
//	         directories are domain modules (runtime, sync, io, net, time), a web
//	         application with routes/controllers/models, and a compiler split into
//	         crates named for products. Rust's unit of structure is the crate, and
//	         a crate boundary is already visible in the graph without a taxonomy
//	         inventing layers above it.
//
//	Swift    An SPM package's directories under Sources/ are TARGET names, which
//	         are products rather than tiers. The ios-clean taxonomy already covers
//	         the app layouts that do use layer names.
//
//	Go (web) One repository in the corpus uses the widely-copied
//	         routers/services/models/modules layout rather than the standard one,
//	         and adding those words to go-standard would re-rank pkg/services and
//	         pkg/models in every Go repository that has them — a change measured
//	         against one example, affecting many.
//
//	.NET     Beyond the clean-architecture layout already covered, the five .NET
//	         repositories in the corpus share no order. A modular CMS keeps
//	         Views/Services/ViewModels inside each of a thousand Modules; a
//	         component library has Components/Pages/Services; a UI framework and a
//	         file manager have product names. And the one that looks most like a
//	         layered application is the trap: a media server's
//	         `MediaBrowser.Controller` is its DOMAIN ABSTRACTIONS assembly —
//	         IServerApplicationHost.cs, Entities/, Dto/ — so a taxonomy matching
//	         the word Controller across dotted project names, which is how the
//	         .NET one has to match, would read fifty modules of interfaces as a
//	         delivery layer. Same failure as `core` in the hexagonal patterns: a
//	         word that names something else here.
//
// Each of these is worth revisiting when the corpus holds two or more unrelated
// repositories that agree. One repository is a validation; it is not evidence of
// a convention.

// patternDefs lists all known architecture patterns. Order does not affect the
// outcome; bestPattern selects by specificity then confidence.
var patternDefs = []patternDef{
	// Framework-gated patterns (most specific).
	{name: "nextjs", layers: nextjsLayers, frameworks: []string{"nextjs"}, appliesTo: []string{"typescript"}},
	{name: "rails-mvc", layers: railsLayers, frameworks: []string{"rails"}, appliesTo: []string{"ruby"},
		autoloadRoot: "app", autoloadLevel: 1},
	{name: "android-clean", layers: androidLayers, frameworks: []string{"android"}, appliesTo: []string{"kotlin", "java"}},
	{name: "ios-clean", layers: iosLayers, frameworks: []string{"swiftui", "uikit"}, appliesTo: []string{"swift"}},
	{name: "spring-layered", layers: springLayers, frameworks: []string{"spring"}, appliesTo: []string{"java", "kotlin"}},
	{name: "django", layers: djangoLayers, frameworks: []string{"django"}, appliesTo: []string{"python"}},
	{name: "dotnet-clean", layers: dotnetCleanLayers, frameworks: []string{"aspnetcore", "efcore"},
		appliesTo: []string{"csharp", "vbnet", "fsharp", "razor", "xaml"}, dottedSegments: true, signatureLayers: []string{"domain", "infrastructure", "application"},
		minSignatureLayers: 2},
	{name: "ember-octane", layers: emberLayers, frameworks: []string{"ember"}, appliesTo: []string{"typescript", "handlebars"}},
	// No signature gate on either of these, for the reason nextjs has none: the
	// framework fact already establishes that the repository is one of these
	// applications, and a prescribed layout does not need a second opinion.
	{name: "nuxt", layers: nuxtLayers, frameworks: []string{"nuxt"}, appliesTo: []string{"typescript"}},
	{name: "sveltekit", layers: svelteKitLayers, frameworks: []string{"sveltekit"}, appliesTo: []string{"typescript"}},
	// Two distinctive layers required: `components`/`services`/`models` are generic
	// enough that a single stray directory in a repository that merely contains some
	// Angular should not decide its architecture.
	{name: "angular-layered", layers: angularLayers, frameworks: []string{"angular"}, appliesTo: []string{"typescript"},
		signatureLayers: []string{"pages", "store", "directives", "pipes"}, minSignatureLayers: 2},

	// Language-gated patterns.
	{name: "go-standard", layers: goStdLayers, appliesTo: []string{"go"}},
	// Two distinctive layers required, for the reason the Angular pattern gives:
	// a repository holding one stray Api or Command directory has not thereby
	// declared an architecture.
	{name: "php-layered", layers: phpLayers, appliesTo: []string{"php"},
		signatureLayers: []string{"controller", "service", "data"}, minSignatureLayers: 2},

	// Language-agnostic patterns, gated on distinctive signature layers. Require
	// at least two distinct ports-and-adapters layers so a single stray
	// directory (e.g. one "infrastructure" test folder) does not trigger it.
	{name: "hexagonal", layers: hexagonalLayers, signatureLayers: []string{"application", "port", "adapter"}, minSignatureLayers: 2},
}

// TaxonomyNames returns the name of every architecture pattern this explainer can
// recognise, sorted. It exists so the documentation can be checked against the
// vocabulary rather than describing a count somebody typed: docs/EXPLAINERS.md and
// ARCHITECTURE.md both state how many taxonomies `layers` matches against, and both
// had drifted behind patternDefs before internal/docslint started asserting it.
func TaxonomyNames() []string {
	names := make([]string, 0, len(patternDefs))
	for _, d := range patternDefs {
		names = append(names, d.name)
	}
	sort.Strings(names)
	return names
}

// specificity ranks how targeted a pattern's gating is. Higher wins ties: a
// framework-specific pattern is preferred over a language-gated one, which is
// preferred over a generic (signature-only) pattern.
func (d patternDef) specificity() int {
	switch {
	case len(d.frameworks) > 0:
		return 2
	case len(d.appliesTo) > 0:
		return 1
	default:
		return 0
	}
}

// describes reports whether this taxonomy applies to a module's language.
func (d patternDef) describes(lang string) bool {
	if len(d.appliesTo) == 0 {
		return true
	}
	for _, l := range d.appliesTo {
		if l == lang {
			return true
		}
	}
	return false
}

// cohort returns the modules this taxonomy could describe, and the languages
// they are written in.
func (d patternDef) cohort(modules []facts.Fact) ([]facts.Fact, map[string]bool) {
	out := make([]facts.Fact, 0, len(modules))
	langs := map[string]bool{}
	for _, m := range modules {
		lang, _ := m.PropAny("language").(string)
		if !d.describes(lang) {
			continue
		}
		out = append(out, m)
		langs[lang] = true
	}
	return out, langs
}

// gateOK reports whether the pattern's framework requirement is met.
func (d patternDef) gateOK(frameworks map[string]bool) bool {
	if len(d.frameworks) > 0 {
		matched := false
		for _, f := range d.frameworks {
			if frameworks[f] {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

// archPattern represents a detected architecture pattern with its confidence.
type archPattern struct {
	Name        string
	Confidence  float64
	Specificity int // from patternDef.specificity(); used to break ties in bestPattern
	Layers      map[string]*layerDef
	Modules     map[string]string // module -> layer name

	// Scanned, Classified and Graded are the denominators the statement is built
	// from. Classified counts every module the taxonomy named; Graded counts the
	// subset sitting in a layer that carries a direction, which is the only part
	// the claimed ORDER describes. They diverge whenever a repository's matches
	// are mostly wiring — one Java project in the corpus classifies 84 modules of
	// which 45 are a config package, so an "ordered" reading of it rests on 39.
	Scanned    int
	Classified int
	Graded     int

	// Languages is the cohort this pattern was scored over — the languages of the
	// modules it could describe. Two patterns whose cohorts overlap are two
	// answers to one question and only the better may be reported; two whose
	// cohorts are disjoint describe different halves of a polyglot repository and
	// both are true.
	Languages map[string]bool
}

// label is the short name a finding cites the pattern by. A declared order is
// already named for its repository, and repeating that inside a violation title
// nests one parenthesis in another for no added information: the useful
// distinction there is declared against recognised.
func (p *archPattern) label() string {
	if strings.HasPrefix(p.Name, "declared (") {
		return "declared"
	}
	return p.Name
}

// conformance counts what the measured imports did with the order the pattern
// claims, over edges whose BOTH ends sit in a layer that carries a direction.
//
// This is the number the pattern insight was missing. Confidence was a coverage
// ratio over directory NAMES, so a repository whose names look hexagonal and
// whose edges do not scored the same as one where both agree — and the corpus
// has both. Inward and Against are the two halves of the answer; Against is
// exactly what the violations below the statement enumerate.
type conformance struct {
	Inward  int
	Against int
	Same    int
}

// obeys returns the share of ordered cross-layer imports that run inward, and
// whether the question applies at all: a repository whose modules span one level
// has no cross-layer edges to obey anything.
func (c conformance) obeys() (float64, bool) {
	total := c.Inward + c.Against
	if total == 0 {
		return 0, false
	}
	return float64(c.Inward) / float64(total), true
}

// Explain analyzes the fact store and detects architectural patterns, one
// repository at a time.
//
// A layer taxonomy is a property of one codebase, and every input this explainer
// gates on — the dominant language, the frameworks present, which directories
// exist — is a per-repository measurement. Read across a union the signals mix:
// the monolith's `rails` framework fact admitted the Rails taxonomy for every
// other repository in the snapshot, its module names collided with theirs, and
// the violations reported against one repository were built from another's
// imports. Single-repo snapshots are unaffected — every fact carries the one
// label, so the loop below runs once over exactly what it used to see.
func (e *LayerExplainer) Explain(ctx context.Context, store *facts.Store) ([]facts.Insight, error) {
	scopes := store.RepoLabels()
	sort.Strings(scopes)
	// Facts carrying no label are one more scope rather than a scope each: they
	// cannot be attributed to a repository, so the only honest grouping is
	// "everything unattributed", analysed together and never mixed with a
	// repository that named itself.
	scopes = append(scopes, "")

	var insights []facts.Insight
	for _, scope := range scopes {
		insights = append(insights, e.explainRepo(store, scope)...)
	}
	return insights, nil
}

// scopeFacts returns the facts carrying the given repo label. The store indexes
// only non-empty labels, so the unlabelled scope is collected by scan.
func scopeFacts(store *facts.Store, repo string) []facts.Fact {
	if repo != "" {
		return store.ByRepo(repo)
	}
	var out []facts.Fact
	// FactsRef, not All: this only reads, and the facts do not outlive the pass.
	for _, f := range store.FactsRef() {
		if f.Repo == "" {
			out = append(out, f)
		}
	}
	return out
}

// explainRepo runs the whole detection over one repository's facts.
func (e *LayerExplainer) explainRepo(store *facts.Store, repo string) []facts.Insight {
	scoped := scopeFacts(store, repo)

	// Test scaffolding is not architecture, and it is load-bearing here rather than
	// cosmetic: len(modules) is the denominator of the pattern's coverage confidence,
	// that confidence breaks ties in bestPattern, and a signature-layer gate can be
	// satisfied purely by test trees (src/test/…/adapter plus src/test/…/application
	// is enough to claim hexagonal). common.IsTestModule rather than a path test,
	// because `module_role` from a build file outranks what a path looks like.
	modules := make([]facts.Fact, 0, len(scoped))
	for _, m := range scoped {
		if m.Kind != facts.KindModule || common.IsTestModule(m) {
			continue
		}
		modules = append(modules, m)
	}
	if len(modules) == 0 {
		return nil
	}

	// Derive the framework signals used to gate pattern detection.
	frameworks := presentFrameworks(scoped)

	// Detect which architecture patterns match
	patterns := e.detectPatterns(modules, frameworks)

	var insights []facts.Insight

	// Declared layer patterns first: a repo whose intent facts declare layers
	// is verdicted against its declaration at confidence 1.0 by the same
	// violation machinery, and the heuristics below skip nothing — they still
	// serve every repo without a declaration.
	for _, dp := range declaredPatterns(store, repo) {
		mods := make([]string, 0, len(dp.Modules))
		for mod := range dp.Modules {
			mods = append(mods, mod)
		}
		sort.Strings(mods)
		evidence := make([]facts.Evidence, 0, len(mods))
		for _, mod := range mods {
			evidence = append(evidence, facts.Evidence{Fact: mod, Detail: fmt.Sprintf("module %q maps to declared layer %q", mod, dp.Modules[mod])})
		}
		// The violations are computed first here for the same reason they are for a
		// recognised pattern: the conformance counts come out of that walk, and a
		// declared order deserves the same numbers as a guessed one.
		violations, dconf := e.detectViolations(scoped, dp)
		dp.Scanned, dp.Classified = distinctModules(modules), len(dp.Modules)
		for _, layer := range dp.Modules {
			if def := dp.Layers[layer]; def != nil && !def.Neutral {
				dp.Graded++
			}
		}
		insights = append(insights, facts.Insight{
			Title:         fmt.Sprintf("Architecture pattern: %s", dp.Name),
			Description:   fmt.Sprintf("Declared layer order with %d layers and %d classified modules. Declared, not recognised: confidence is exact.", len(dp.Layers), len(dp.Modules)),
			Confidence:    1.0,
			Informational: true, // Describes the declaration; the violations below are the findings.
			Metrics:       patternMetrics(dp, dconf),
			Evidence:      evidence,
			Actions:       []string{"Keep the declaration beside the code it governs"},
		})
		insights = append(insights, vacuousDeclarationInsights(dp, modules)...)
		for i := range violations {
			violations[i].Confidence = 1.0
		}
		insights = append(insights, violations...)
	}

	// Report the architecture pattern of each language cohort.
	for _, best := range e.selectPatterns(patterns) {
		// Sort the classified modules so the evidence order is deterministic —
		// ranging best.Modules directly would follow Go's randomized map order.
		mods := make([]string, 0, len(best.Modules))
		for mod := range best.Modules {
			mods = append(mods, mod)
		}
		sort.Strings(mods)
		evidence := make([]facts.Evidence, 0, len(mods))
		for _, mod := range mods {
			layer := best.Modules[mod]
			evidence = append(evidence, facts.Evidence{
				Fact:   mod,
				Detail: fmt.Sprintf("module %q maps to layer %q", mod, layer),
			})
		}

		// The violations are computed BEFORE the statement is written, because the
		// statement quotes their denominator: how many imports obeyed the order is
		// not knowable until the same walk that found the ones that did not.
		violations, conf := e.detectViolations(scoped, best)

		insights = append(insights, facts.Insight{
			Title:         fmt.Sprintf("Architecture pattern: %s", best.Name),
			Description:   describePattern(best, conf, distinctModules(modules)),
			Confidence:    best.Confidence,
			Informational: true, // Which pattern was recognised is not a defect, at any confidence.
			Metrics:       patternMetrics(best, conf),
			Evidence:      evidence,
			Actions: []string{
				"Ensure new code follows the detected layer structure",
				"Review cross-layer dependencies for violations",
			},
		})
		insights = append(insights, violations...)
	}

	return insights
}

// cohortLabel names the languages a pattern was scored over, for the statement.
// Empty when the taxonomy describes every language, because there is no cohort
// to distinguish it from.
func cohortLabel(p *archPattern) string {
	if len(p.Languages) == 0 || len(p.Languages) > 2 {
		return ""
	}
	langs := make([]string, 0, len(p.Languages))
	for l := range p.Languages {
		if l == "" {
			return ""
		}
		langs = append(langs, l)
	}
	sort.Strings(langs)
	return strings.Join(langs, "/") + " "
}

// Metric keys the pattern insight publishes. One vocabulary, declared here rather
// than spelled out at each call site, because the readers are in other packages —
// the renderer builds the feature guide from the layer order, and the benchmark
// grades the denominators.
const (
	MetricModulesScanned    = "modules_scanned"
	MetricModulesClassified = "modules_classified"
	MetricModulesGraded     = "modules_graded"
	MetricImportsInward     = "imports_inward"
	MetricImportsAgainst    = "imports_against"
	MetricImportsSameLevel  = "imports_same_level"
	MetricLayersOrdered     = "layers_ordered"
	MetricLayersUnordered   = "layers_unordered"
	MetricLayerExamples     = "layer_examples"
	MetricLayerLevels       = "layer_levels"
	MetricCohortLanguages   = "cohort_languages"
)

// patternMetrics is the machine-readable copy of what describePattern says in
// prose. The two are built from the same values in the same place so they cannot
// disagree; a test asserts it.
//
// The layer order is the part no reader could reconstruct. A recognised pattern's
// order is in patternDefs, which a renderer could in principle look up by name — a
// DECLARED order is in the repository's intent facts and cannot be looked up at
// all, which is why the feature guide had nothing to say about repositories that
// state their own architecture.
func patternMetrics(p *archPattern, conf conformance) map[string]any {
	ordered, unordered := layerNamesByRank(p)
	m := map[string]any{
		MetricModulesScanned:    p.Scanned,
		MetricModulesClassified: p.Classified,
		MetricModulesGraded:     p.Graded,
		MetricImportsInward:     conf.Inward,
		MetricImportsAgainst:    conf.Against,
		MetricImportsSameLevel:  conf.Same,
		MetricLayersOrdered:     ordered,
		MetricLayersUnordered:   unordered,
		MetricLayerExamples:     layerExamples(p),
		// Levels, not just the order, because layers SHARING one are peers rather
		// than steps: the Rails taxonomy puts a dozen directories on its domain tier
		// deliberately, and a reader given only a sequence reads twelve steps that do
		// not exist. The renderer groups on this.
		MetricLayerLevels: layerLevels(p),
	}
	if langs := sortedKeys(p.Languages); len(langs) > 0 {
		m[MetricCohortLanguages] = langs
	}
	return m
}

// layerNamesByRank splits a pattern's layers into the ordered ones, OUTERMOST
// FIRST, and the unordered ones. Outermost first is the direction a feature is
// built in and the direction a dependency runs, so it is the order a reader wants
// and the order the guide prints.
func layerNamesByRank(p *archPattern) (ordered, unordered []string) {
	for name, def := range p.Layers {
		if def == nil {
			continue
		}
		if def.Neutral {
			unordered = append(unordered, name)
			continue
		}
		ordered = append(ordered, name)
	}
	sort.Strings(unordered)
	sort.Slice(ordered, func(i, j int) bool {
		li, lj := p.Layers[ordered[i]], p.Layers[ordered[j]]
		if li.Level != lj.Level {
			return li.Level > lj.Level
		}
		return ordered[i] < ordered[j]
	})
	return ordered, unordered
}

// layerExamples names one module per layer, taken from what this repository
// actually measured. It is what turns a layer ORDER into instructions: "components
// then composables" is a rule, and "app/components/card then app/composables/auth"
// is the same rule in the reader's own tree. Lowest name per layer, so the choice
// is stable across runs rather than following map order.
func layerExamples(p *archPattern) map[string]string {
	out := map[string]string{}
	for mod, layer := range p.Modules {
		if cur, ok := out[layer]; !ok || mod < cur {
			out[layer] = mod
		}
	}
	return out
}

// layerLevels reports each ordered layer's rank. Unordered layers are absent
// rather than given a sentinel: they have no rank, and a number would invite a
// reader to compare them.
func layerLevels(p *archPattern) map[string]any {
	out := map[string]any{}
	for name, def := range p.Layers {
		if def == nil || def.Neutral {
			continue
		}
		out[name] = def.Level
	}
	return out
}

// sortedKeys returns a set's members in a deterministic order.
func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// describePattern writes the statement enola makes about a repository.
//
// It states three things a reader can check, in place of one number nobody could:
// how much of the repository the taxonomy named, how much of that carries a
// direction, and what the measured imports did with that direction. "Recognised
// hexagonal, 66% confidence" and "the names say hexagonal and 340 imports run
// against it" are the same snapshot; only the second is worth reading.
func describePattern(p *archPattern, conf conformance, repoModules int) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Recognised %s from directory names: %d of %d %smodules classified",
		p.Name, p.Classified, p.Scanned, cohortLabel(p))
	// Name the cohort's size against the repository whenever it is not the whole
	// of it. A taxonomy is now scored over the modules it could describe, so a Go
	// SDK of 28 modules inside a Python repository of 1310 produces a true
	// statement that reads as a claim about the repository unless it says which
	// part of it was measured.
	if repoModules > p.Scanned {
		fmt.Fprintf(&sb, " — %.0f%% of this repository's %d modules",
			100*float64(p.Scanned)/float64(repoModules), repoModules)
	}
	if p.Graded != p.Classified {
		fmt.Fprintf(&sb, ", %d of them in layers that carry a direction (the rest are wiring, which is named but not ordered)", p.Graded)
	}
	sb.WriteString(". ")

	share, applies := conf.obeys()
	switch {
	case !applies:
		// Two taxonomies in this set deliberately collapse most of their
		// directories to one tier, so on many repositories they can express no
		// ordering at all. Saying so is the difference between a statement that
		// found nothing and a repository that breached nothing.
		sb.WriteString("No import in this repository crosses one of its ordered layers, so the pattern names a layout without grading anything: nothing here can breach it.")
	case conf.Against == 0:
		fmt.Fprintf(&sb, "All %d imports between ordered layers run inward; none run against the order.", conf.Inward)
	default:
		fmt.Fprintf(&sb, "Of %d imports between ordered layers, %d run inward and %d against it (%.0f%% obey the order); the %d are reported separately below.",
			conf.Inward+conf.Against, conf.Inward, conf.Against, share*100, conf.Against)
	}
	return sb.String()
}

func (e *LayerExplainer) detectPatterns(allModules []facts.Fact, frameworks map[string]bool) []*archPattern {
	var patterns []*archPattern

	for di := range patternDefs {
		def := patternDefs[di]

		// Skip patterns whose framework gate is not satisfied. This is what stops
		// a plain OOP repo from matching the generic "nextjs" directory names.
		if !def.gateOK(frameworks) {
			continue
		}

		// Score over the modules this taxonomy could describe, not over the whole
		// repository. Everything below — matchCount, the signature gate, both
		// denominators — is then a measurement of the cohort.
		modules, cohortLangs := def.cohort(allModules)
		if len(modules) == 0 {
			continue
		}

		pattern := &archPattern{
			Name:        def.name,
			Specificity: def.specificity(),
			Layers:      make(map[string]*layerDef),
			Modules:     make(map[string]string),
			Languages:   cohortLangs,
		}

		matchCount := 0
		for _, mod := range modules {
			if i, ok := classifyModule(mod.Name, def); ok {
				pattern.Layers[def.layers[i].Name] = &def.layers[i]
				pattern.Modules[mod.Name] = def.layers[i].Name
				matchCount++
			}
		}

		// Anything the taxonomy did not name but the framework autoloads is
		// still a layer. Without this a Rails app's own vocabulary — app/tools,
		// app/agents, app/schemas — is unclassified, and those are usually
		// exactly the directories a house rule is written about.
		autoLayers := 0
		if def.autoloadRoot != "" {
			for _, mod := range modules {
				if _, classified := pattern.Modules[mod.Name]; classified {
					continue
				}
				name, ok := autoloadedLayer(mod.Name, def.autoloadRoot)
				if !ok {
					continue
				}
				if _, exists := pattern.Layers[name]; !exists {
					pattern.Layers[name] = &layerDef{
						Name: name, Patterns: []string{name}, Level: def.autoloadLevel,
					}
					autoLayers++
				}
				pattern.Modules[mod.Name] = name
				matchCount++
			}
		}

		if matchCount == 0 || len(modules) == 0 {
			continue
		}

		// Count the classified modules that sit in a layer carrying a direction.
		// A module in a neutral layer is named but not ordered, so it is evidence
		// that the taxonomy fits and no evidence at all about the layering.
		graded := 0
		for _, layerName := range pattern.Modules {
			if def := pattern.Layers[layerName]; def != nil && !def.Neutral {
				graded++
			}
		}
		// All three counts are over DISTINCT module names, because the classified
		// one has to be: pattern.Modules is keyed by name, so two extractors
		// emitting the same directory (a grammar directory read by two of them, in
		// this repository) collapse there and not in a running total. Mixing the
		// bases made enola's own snapshot say 119 of 129 modules were classified
		// and 117 of those ordered — reading as two wiring modules where there
		// were none, only two duplicates.
		pattern.Scanned, pattern.Classified, pattern.Graded = distinctModules(modules), len(pattern.Modules), graded

		// Require enough distinctive signature layers when the pattern declares
		// them, so a match built only from generic names (e.g. just model + ui)
		// or from a single stray directory does not qualify.
		if len(def.signatureLayers) > 0 &&
			countSignatureLayers(pattern, def.signatureLayers) < def.minSignatureLayers {
			continue
		}

		// Confidence is how much of the repository the claimed ORDER describes,
		// and nothing else.
		//
		// It used to be `coverage*0.6 + layerCoverage*0.4`, where layerCoverage was
		// the share of the taxonomy's OWN layer names that appeared. That second
		// term rewarded a narrow taxonomy for being narrow and punished a wide one
		// for being wide: a modular CMS matching all four names of the four-layer
		// .NET taxonomy across 3% of its modules scored 0.42 — higher than several
		// repositories the taxonomy genuinely describes — because 0.4 of the score
		// was already banked before a single module was counted. The blend also had
		// no meaning a reader could state. This one does: 0.24 means the ordered
		// layers account for 24% of the modules measured.
		//
		// Ceiling is deliberately below 1.0: confidence 1.0 is reserved for a
		// structural fact, and a pattern match is a coverage ratio over directory
		// names — a well-supported guess, never a certainty.
		pattern.Confidence = float64(graded) / float64(pattern.Scanned)
		if pattern.Confidence > common.MaxHeuristicConfidence {
			pattern.Confidence = common.MaxHeuristicConfidence
		}

		// Everything that matched two layers enters the comparison. The coverage
		// floor is applied to the WINNER instead, in explainRepo — see
		// minClassifiedShare for why the difference matters.
		if len(pattern.Layers) >= 2 {
			patterns = append(patterns, pattern)
		}
	}

	return patterns
}

// distinctModules counts the distinct module NAMES in a scope. Two extractors
// can emit a fact for the same directory, and a denominator that counts both
// cannot be compared against a numerator keyed by name.
func distinctModules(modules []facts.Fact) int {
	seen := make(map[string]struct{}, len(modules))
	for _, m := range modules {
		seen[m.Name] = struct{}{}
	}
	return len(seen)
}

// classifyModule picks the layer a module belongs to: the first match in the
// taxonomy's declaration order, except that UNORDERED layers are considered
// first.
//
// Matching is position-blind — any path segment may match — so a wiring directory
// nested inside an ordered one takes the enclosing layer: an Android module at
// core/data/…/data/di classified as `data` rather than `di`, and a Spring package
// at …/service/config as `service` rather than `config`. Preferring the neutral
// layer is the fail-safe direction. Misclassifying INTO one only silences a
// verdict, because a neutral layer produces none; misclassifying OUT of one
// invents a verdict about a directory whose whole role is to be referenced by
// every layer it wires.
//
// Deciding by position instead — the deepest matching segment — was measured and
// rejected: it reclassified `adapter/rest` from adapter to handler and broke
// hexagonal detection on a canonical ports-and-adapters layout, with nothing in
// the corpus improved.
func classifyModule(name string, def patternDef) (int, bool) {
	for _, wantNeutral := range []bool{true, false} {
		for i := range def.layers {
			if def.layers[i].Neutral != wantNeutral {
				continue
			}
			if matchesLayerIn(name, def.layers[i].Patterns, def.dottedSegments) {
				return i, true
			}
		}
	}
	return 0, false
}

// countSignatureLayers returns how many of the given distinctive layer names the
// detected pattern matched.
//
// PRESENCE, NOT WEIGHT, AND THAT WAS TRIED. Requiring two modules per signature
// layer removes exactly one wrong statement from the corpus — WordPress named
// php-layered on 45 modules of which 44 belong to libraries vendored into
// wp-includes — and costs a right one: a Java platform's Angular front end is
// 232 of 269 modules at 99% obedience, and its distinctive evidence is 54 `pages`
// directories plus a single `directives`. No absolute threshold separates those
// two, because what is wrong about the WordPress claim is not its size but that
// the code is somebody else's; and no relative one does either, since a correct
// Angular match sits at 3% signature share and the wrong PHP one at 20%.
//
// So the wrong statement stays, recorded as a known gap in the benchmark corpus
// rather than tuned away with a threshold that takes a correct one with it. The
// real fix is excluding vendored trees from architecture the way test trees
// already are, which is its own piece of work — and note that the existing
// vendored-candidates heuristic would not catch this one either: wp-includes is
// not a conventional vendor directory name.
func countSignatureLayers(pattern *archPattern, signature []string) int {
	n := 0
	for _, name := range signature {
		if _, ok := pattern.Layers[name]; ok {
			n++
		}
	}
	return n
}

// selectPatterns returns the patterns to report: the best one for each cohort of
// languages, strongest first, with no two cohorts overlapping.
//
// A repository used to get exactly one statement, which is right only when it is
// written in one thing. A Rails monolith shipping an Ember front end matched both
// taxonomies at equal specificity, so confidence alone decided, and the loser's
// half of the repository was then described by the winner's layer order. Both are
// true here, over disjoint sets of modules, and both are reported.
//
// Overlapping cohorts are still one question with one answer: the ungated
// hexagonal taxonomy describes every language, so it is reported only where no
// gated taxonomy claimed those modules first.
func (e *LayerExplainer) selectPatterns(patterns []*archPattern) []*archPattern {
	ranked := append([]*archPattern(nil), patterns...)
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].Specificity != ranked[j].Specificity {
			return ranked[i].Specificity > ranked[j].Specificity
		}
		if ranked[i].Confidence != ranked[j].Confidence {
			return ranked[i].Confidence > ranked[j].Confidence
		}
		return ranked[i].Name < ranked[j].Name
	})

	claimed := map[string]bool{}
	var out []*archPattern
	for _, p := range ranked {
		if overlaps(p.Languages, claimed) {
			continue
		}
		// The floor is applied per cohort, and a floored winner still CLAIMS its
		// languages: suppression must not promote, and a worse-fitting taxonomy
		// over the same modules is exactly what would take its place.
		for lang := range p.Languages {
			claimed[lang] = true
		}
		if thickEnough(p) != nil {
			out = append(out, p)
		}
	}
	return out
}

// overlaps reports whether any language in langs is already claimed.
func overlaps(langs, claimed map[string]bool) bool {
	for lang := range langs {
		if claimed[lang] {
			return true
		}
	}
	return false
}

// thickEnough applies the coverage floor to the pattern that WON, and returns
// nil when it does not clear it.
//
// The floor is applied here rather than at admission, because suppression must
// not promote. Applied at admission it removed a modular CMS's thin
// `dotnet-clean` match and handed the repository to the generic `hexagonal`
// pattern, which had recognised `Interfaces` and `Infrastructure` across a
// quarter of it — trading a wrong statement for a worse one. If the taxonomy
// that fits a repository best does not describe enough of it, no taxonomy does,
// and the repository has no statement.
func thickEnough(best *archPattern) *archPattern {
	if best == nil || best.Scanned == 0 {
		return nil
	}
	if float64(best.Classified)/float64(best.Scanned) < minClassifiedShare {
		return nil
	}
	return best
}

// presentFrameworks collects the set of frameworks present in the given facts,
// using the Props["framework"] attribute extractors set on routes, symbols and
// modules (e.g. nextjs, rails, django, android, spring). The caller passes one
// repository's facts: a framework is a property of the repository that uses it,
// and read across a union one repository's `rails` opened the Rails taxonomy to
// every other repository in the snapshot.
func presentFrameworks(ff []facts.Fact) map[string]bool {
	out := make(map[string]bool)
	for _, f := range ff {
		if fw, ok := f.PropAny("framework").(string); ok && fw != "" {
			out[fw] = true
		}
	}
	return out
}

// ONLY `imports` IS READ, AND THAT WAS MEASURED RATHER THAN ASSUMED. The obvious
// next move is to verdict `calls`, `instantiates`, `injects` and `implements` as
// well, on the reasoning that an autoloaded language has no import statements to
// find. It does not survive contact with the corpus. Resolving every one of those
// edges to a module pair (symbol name to its declaring module, names declared in
// two modules dropped as ambiguous) and subtracting the pairs `imports` already
// yields:
//
//	a Rails monolith        150 new module pairs   0 of them wrong-direction
//	a Spring application     680 new module pairs   0
//	a Go application         779 new module pairs   0
//	a Rails + Angular app   1455 new module pairs   5
//
// Three thousand pairs for five findings, every one of them the same shape as
// findings the import edges already reported — and bought with a resolution step
// whose unresolved tail runs from 27,000 to 105,000 targets per repository, each
// one a chance to attribute an edge to the wrong module.
//
// The premise was wrong twice. In every language here a reference across a module
// boundary requires an import, so `calls` is a SUBSET of `imports` once both are
// reduced to module granularity. And the autoloaded language is not an exception:
// the Ruby extractor synthesizes coupling edges from constant references, which is
// why a Rails monolith with no `require` in its application code still has 11,486
// import edges. Where a Rails taxonomy reports nothing it is because that taxonomy
// deliberately collapses its domain directories to one tier, not because the
// evidence is missing.

// detectViolations checks for layer boundary violations (inner layer importing
// outer layer). Each distinct (source module -> target module) pair is reported
// once: two files in the same module importing the same outer module are one
// violation, not two, so the count the renderer derives isn't inflated. Relative
// import targets (`./x`, `../y`) are resolved against the source module the same
// way the shared module-graph builder does, so JS/TS-style relative imports match
// a classified layer instead of silently missing. Output is sorted for
// determinism.
//
// The facts passed in are one repository's, so an import can only ever be
// verdicted against the taxonomy of the repository it was measured in.
func (e *LayerExplainer) detectViolations(scoped []facts.Fact, pattern *archPattern) ([]facts.Insight, conformance) {
	projectOf := moduleProjects(scoped)
	type violation struct {
		sourceModule, targetModule string
		sourceLayer, targetLayer   string
		sourceLevel, targetLevel   int
		file                       string
		// depName and rawTarget are the two strings the snapshot diff can actually
		// match. touchedNames holds fact names plus both endpoints of added/removed
		// edges, and an import edge is {Source: dep.Name, Target: rel.Target} — the
		// RAW target, before the relative-import rewrite below. The importing file is
		// never a fact name, so evidence citing only the file can never be attributed
		// to a change, which routes every new violation to incidental and out of the
		// gate. Both are captured from the dependency that survives the dedup.
		depName, rawTarget string
	}
	seen := make(map[string]bool)
	var violations []violation
	var conf conformance

	for _, dep := range scoped {
		if dep.Kind != facts.KindDependency {
			continue
		}
		// Test code is not architecture. Gate on the file rather than the module:
		// resolveLayerModule walks UP to the nearest classified module, so a test or
		// mock nested inside a production module (Sources/Foo/Mocks/X.swift) would
		// otherwise have its imports attributed to the production layer.
		if facts.IsTestPath(dep.File) {
			continue
		}
		// Resolve the importing file's directory up to its nearest classified module,
		// so a file nested below the module root (Swift/Xcode) still attributes to a
		// layer instead of being dropped. Both the raw and repo-stripped directories
		// are tried: the stripped one is required in an append-mode snapshot (the file
		// is repo-prefixed and module names are not, so the raw dir yields the repo
		// label and nothing resolves), while the raw one is required in a single-repo
		// layout whose top-level package carries the repo's own name, where stripping
		// removes a real path segment. Using either alone silences this explainer for
		// one of the two shapes.
		sourceModule, sourceOK := resolveLayerModuleFor(dep, pattern.Modules)
		if !sourceOK {
			continue
		}
		sourceLayer := pattern.Modules[sourceModule]

		for _, rel := range dep.Relations {
			if rel.Kind != facts.RelImports {
				continue
			}

			rawTarget := rel.Target
			target := rawTarget
			if strings.HasPrefix(target, ".") {
				target = common.ResolveRelativeImport(sourceModule, target)
			}

			// Resolve the target up to its enclosing classified module. Import targets
			// carry differing granularity across extractors — a module dir + symbol
			// name (Kotlin/Java `import a.b.C` -> "a/b/C") or a file stem (TypeScript) —
			// so an exact module lookup silently drops every class-/file-suffixed
			// target (which was hiding all Kotlin-sourced layer violations). Mirrors
			// package_metrics' resolveToModule; a no-op for bare-module-dir targets.
			targetModule, targetOK := resolveLayerModule(target, pattern.Modules)
			if !targetOK {
				continue
			}
			targetLayer := pattern.Modules[targetModule]
			target = targetModule

			sourceDef := pattern.Layers[sourceLayer]
			targetDef := pattern.Layers[targetLayer]
			if sourceDef == nil || targetDef == nil {
				continue
			}
			// A neutral layer is classified but unordered: it sits in no dependency
			// direction, so neither end of an edge touching one can be verdicted —
			// and it must not be counted as conformance either way.
			if sourceDef.Neutral || targetDef.Neutral {
				continue
			}
			// Every ordered edge is counted, whichever way it runs. The edges that
			// OBEY the order are the denominator that makes the ones that breach it
			// mean something, and they were never measured before.
			switch {
			case sourceDef.Level > targetDef.Level:
				conf.Inward++
				continue
			case sourceDef.Level == targetDef.Level:
				conf.Same++
				continue
			}
			conf.Against++
			// Same assembly, no violation. .NET's layer boundary is the PROJECT, and
			// two directories inside one compile into the same DLL — the reason the
			// cycles explainer already says an intra-assembly cycle is a coupling
			// signal rather than a build problem. Without this, eShop reported
			// Basket.API/Repositories -> Basket.API/Model as infrastructure -> api,
			// because the sub-directory takes its layer from its own name while its
			// sibling inherits one from the project name they share.
			if p := projectOf[sourceModule]; p != "" && p == projectOf[target] {
				continue
			}

			key := sourceModule + "\x00" + target
			if seen[key] {
				continue
			}
			seen[key] = true
			violations = append(violations, violation{
				sourceModule: sourceModule, targetModule: target,
				sourceLayer: sourceLayer, targetLayer: targetLayer,
				sourceLevel: sourceDef.Level, targetLevel: targetDef.Level,
				file:    dep.File,
				depName: dep.Name, rawTarget: rawTarget,
			})
		}
	}

	sort.Slice(violations, func(i, j int) bool {
		if violations[i].sourceModule != violations[j].sourceModule {
			return violations[i].sourceModule < violations[j].sourceModule
		}
		return violations[i].targetModule < violations[j].targetModule
	})

	insights := make([]facts.Insight, 0, len(violations))
	for _, v := range violations {
		// The file entry stays FIRST: the dashboard's firstEvidence and the gate's
		// writeFindings both render the first qualifying entry, so the entities the
		// diff needs are appended behind it rather than prepended in front of it.
		evidence := []facts.Evidence{
			{File: v.file, Detail: fmt.Sprintf("import of %s", v.targetModule)},
		}
		seenEntity := make(map[string]bool, 4)
		addEntity := func(name, file, detail string) {
			if name == "" || seenEntity[name] {
				return
			}
			seenEntity[name] = true
			evidence = append(evidence, facts.Evidence{Fact: name, File: file, Detail: detail})
		}
		// Only the dependency fact is known to live in v.file: it IS that import
		// statement, so the file is its own. The other three name things declared
		// elsewhere, and stamping the importing file on them would send a reader
		// to the wrong place.
		//
		// It matters beyond the reader. One module can import the same target from
		// several files, so the dependency NAME alone can belong to more than one
		// fact — 459 such names in one measured repository — and a citation that
		// carries no file leaves a consumer unable to tell which one is meant.
		addEntity(v.depName, v.file, "import edge")
		addEntity(v.rawTarget, "", "import edge target")
		addEntity(v.sourceModule, "", "importing module")
		addEntity(v.targetModule, "", "imported module")

		insights = append(insights, facts.Insight{
			// The taxonomy is named because a polyglot repository now gets one
			// statement per language cohort, and two of them can produce the same
			// pair of layer names: openproject reports `components -> controller`
			// out of Ruby view components and `components -> pages` out of Angular
			// ones, and without this there is nothing in the finding that says
			// which order was applied.
			Title: fmt.Sprintf("Layer violation: %s -> %s (%s)", v.sourceLayer, v.targetLayer, pattern.label()),
			Description: fmt.Sprintf(
				"Module %q (layer: %s, level %d) imports module %q (layer: %s, level %d). "+
					"Inner layers should not depend on outer layers.",
				v.sourceModule, v.sourceLayer, v.sourceLevel,
				v.targetModule, v.targetLayer, v.targetLevel,
			),
			Confidence: 0.8,
			Evidence:   evidence,
			Actions: []string{
				"Introduce an interface/port in the inner layer",
				"Move shared types to a common package",
				"Invert the dependency using dependency injection",
			},
		})
	}

	return insights, conf
}

// resolveLayerModule walks path up its directory segments until it names a
// classified module (a key in modules), returning that module and true. If no
// ancestor is classified it returns false. This absorbs the granularity
// differences in extractor import targets (bare module dir, module dir + symbol
// name, or file stem) and in nested source layouts, so a layer violation is not
// missed just because the raw endpoint carried a trailing symbol/file segment.
func resolveLayerModule(path string, modules map[string]string) (string, bool) {
	cur := path
	for {
		if _, ok := modules[cur]; ok {
			return cur, true
		}
		i := strings.LastIndex(cur, "/")
		if i < 0 {
			return "", false
		}
		cur = cur[:i]
	}
}

// resolveLayerModuleFor resolves a dependency fact's own file to a classified module,
// trying every directory the file may name (see common.ModuleDirCandidates).
//
// Exact matches are tried across all candidates before any is walked up, for the same
// reason as common.resolveModuleDir: a walk-up can reach a short ancestor that exists
// only because of the OTHER snapshot shape, and letting it beat an exact match would
// attribute the import to the wrong module.
func resolveLayerModuleFor(dep facts.Fact, modules map[string]string) (string, bool) {
	candidates := common.ModuleDirCandidates(dep)
	for _, c := range candidates {
		if _, ok := modules[c]; ok {
			return c, true
		}
	}
	for _, c := range candidates {
		if m, ok := resolveLayerModule(c, modules); ok {
			return m, true
		}
	}
	return "", false
}

// matchesLayer checks if a module path contains any of the given patterns.
func matchesLayer(modulePath string, patterns []string) bool {
	return matchesLayerIn(modulePath, patterns, false)
}

// matchesLayerIn compares whole path segments, and with dotted=true each
// dot-separated component of a segment as well.
func matchesLayerIn(modulePath string, patterns []string, dotted bool) bool {
	for _, part := range strings.Split(strings.ToLower(modulePath), "/") {
		candidates := []string{part}
		if dotted && strings.Contains(part, ".") {
			candidates = append(candidates, strings.Split(part, ".")...)
		}
		for _, c := range candidates {
			for _, pattern := range patterns {
				if c == pattern {
					return true
				}
			}
		}
	}
	return false
}

// moduleProjects maps each module to the assembly it compiles into, from the
// `project` prop the MSBuild pass sets. Empty for languages that have no such
// unit, which leaves their violation behaviour unchanged.
func moduleProjects(ff []facts.Fact) map[string]string {
	out := map[string]string{}
	for _, m := range ff {
		if m.Kind != facts.KindModule {
			continue
		}
		if p, ok := m.PropAny("project").(string); ok && p != "" {
			out[m.Name] = p
		}
	}
	return out
}

// autoloadedLayer names the layer an autoloaded module belongs to: the first
// path segment under the framework's autoload root. app/tools/replan_week is
// part of "tools", the same way app/models/coaching is part of "model".
//
// The root must be the *first* segment, not any segment. A monolith that also
// contains a front-end app has directories like ember_app/app/routes, and
// matching "app" anywhere swept a second framework's whole layout into the
// Rails taxonomy — which inflated its coverage until it displaced the pattern
// that was correctly winning, replacing a real analysis with a wrong one.
// Nested Rails roots (packwerk packages/*/app) are deliberately not claimed:
// classifying too little is recoverable, classifying the wrong framework is not.
func autoloadedLayer(modulePath, root string) (string, bool) {
	parts := strings.Split(strings.ToLower(modulePath), "/")
	if len(parts) < 2 || parts[0] != root || parts[1] == "" {
		return "", false
	}
	if notAutoloaded[parts[1]] {
		return "", false
	}
	return parts[1], true
}

// notAutoloaded names the children of Rails' `app/` that the framework does NOT
// autoload, so the autoload rule above cannot claim them as layers.
//
// Rails::Engine builds the eager-load paths from `app/*` minus exactly these
// three, because they hold assets and templates rather than Ruby constants.
// `frontend` joins them as vite_rails' documented root — the same thing under a
// different name. The rule matters because these directories hold a WHOLE OTHER
// APPLICATION: chatwoot keeps a Vue app in app/javascript, which the autoload
// rule made a domain-tier Rails layer, and its five entrypoints importing
// app/views were reported as layer violations. A frontend reading the templates
// it mounts into is how Rails and Vite are wired together, not a defect.
var notAutoloaded = map[string]bool{
	"assets":     true,
	"javascript": true,
	"views":      true,
	"frontend":   true,
}
