# TypeScript / JavaScript — what enola extracts

Parsed with tree-sitter's TypeScript grammar, which handles JavaScript natively — there
is no separate JS extractor. Detected by `tsconfig.json`, `tsconfig.base.json`, or a
`package.json` with TypeScript, at the root or one level deep, so monorepos are picked up
per package. Vue, Svelte and Ember are the same extractor with their own
single-file-component / template handling.

Fixtures: [`ts_sample`](../../internal/engine/testdata/repos/ts_sample/) ·
[`ts_express_multirepo`](../../internal/engine/testdata/repos/ts_express_multirepo/) ·
[`ts_nest_multirepo`](../../internal/engine/testdata/repos/ts_nest_multirepo/) ·
[`ts_orm_sample`](../../internal/engine/testdata/repos/ts_orm_sample/) ·
[`ts_ember_sample`](../../internal/engine/testdata/repos/ts_ember_sample/)

## At a glance

| You write | enola stores | Kind |
|---|---|---|
| a source directory | one module per directory | `module` |
| `function`, `class`, `interface`, `const fn = …` | a symbol with `symbol_kind` | `symbol` |
| `import … from` | a dependency, `internal` / `external` | `dependency` |
| `app.get("/x", h)` (Express) | a server route | `route` |
| `@Get()` on a `@Controller("v2/slots")` | a server route at the **composed** path | `route` |
| `@controller(…)` (InversifyJS) | a server route | `route` |
| `fetch("/x")`, `axios.post("/y")` | a client route with `role: client` | `route` |
| `pages/about.vue` (Nuxt) | a route derived from the file path | `route` |
| a Vue template's `<UserCard @submit="save" />` | resolved component and handler edges | `symbol` |
| Vue `defineProps` / `defineEmits` / `defineSlots` macros | component public-surface markers | `symbol` |
| Vue Router `{ path, component, children }` records | composed page routes and `handled_by` component edges | `route` |
| a Nuxt auto-imported `useAuth()` | a call to its unique `composables/` declaration | `file_ref` / `symbol` |
| `src/routes/blog/[slug]/+page.svelte` | a route derived from the file path | `route` |
| a Prisma / TypeORM / Drizzle model | an entity with its table name | `storage` |
| a `.gts`/`.gjs` Glimmer component | a component symbol with its template's references | `symbol` |
| `this.route('book', { path: '/:book_id' })` (Ember) | a page route at the composed path | `route` |
| an ember-data `Model` subclass | a model with its dasherized name | `storage` |
| `@Component`/`@Directive`/`@Pipe`/`@Injectable`/`@NgModule` | a symbol with its container role and selector | `symbol` |
| `constructor(private users: UserService)`, `inject(X)` | an `injects` edge to the class that declares it | `symbol` |
| `RouterModule.forRoot([…])`, `provideRouter(routes)` | page routes at their **composed** paths | `route` |
| `loadChildren: () => import('./admin')` | the mounted module's routes, prefixed | `route` |
| `{{ total }}`, `(click)="save()"`, `<app-card>` | the component's edges to its own members and children | `symbol` |
| `@NgModule({declarations, imports, providers})` | `depends_on` edges to what it composes | `symbol` |
| `this.http.get('api/v1/x')` (Angular) | a client route with `role: client` | `route` |
| an Nx `project.json` / `angular.json` project | the `workspace_project` a module belongs to | `module` |
| top-level statements | a `file_ref` carrying the call edges | `file_ref` |
| `*.test.ts`, `*.spec.tsx` | a reference-only `test_ref` | `test_ref` |
| `constructor(…)` | a symbol like any other member | `symbol` |
| `@Input()` on a class member | the member's `decorators` prop | `symbol` |
| `get total()` | a symbol, not a skipped accessor | `symbol` |
| `class A extends B` | an `extends` prop naming the base and the module it came from | `symbol` |
| `exports.name = function` (CommonJS) | a declared symbol | `symbol` |
| `this.x = …` inside a method | the method's `assigns_fields` prop | `symbol` |
| any function or method | `takes_parameters`, whether it declares one at all | `symbol` |

## Routes — NestJS controller prefixes

```ts
// api/src/slots/slots.controller.ts
@Controller('v2/slots')
export class SlotsController {
  @Get('available')  getAvailableSlots() {}   // line 10
  @Post('reserve')   reserveSlot() {}         // line 15
}
```

```
route  /v2/slots/available   api/src/slots/slots.controller.ts:10
       props: framework=nestjs, method=GET, handler=getAvailableSlots, role=server
route  /v2/slots/reserve     api/src/slots/slots.controller.ts:15
       props: framework=nestjs, method=POST, handler=reserveSlot, role=server
```

The class-level prefix is folded onto each method decorator, so the stored path is the
one a client actually calls. InversifyJS `@controller` prefixes are handled the same way
(`/api/orders`, `/api/orders/:id/cancel` in the same fixture).

## Routes — Express

```
route  /healthcheck        server/index.js:8    props: framework=express, method=GET
route  /healthcheck        server/index.js:9    props: framework=express, method=OPTIONS
route  /go/:name           server/index.js:10   props: framework=express, method=GET,
                                                       unmatched_by_clients=true
route  /admin/users/:id/ban server/index.js:20  props: framework=express, method=POST
```

`unmatched_by_clients=true` is what the `unused-routes` explainer reports: an endpoint
this repository serves that no loaded client calls. It is a *candidate* at confidence
`0.6`, not a verdict — the client may simply not be in the graph.

### Mounts across files

A sub-router is almost never mounted in the file that declares it. The paths written
there are **fragments** — `router.post('/login')` in `routes/webhooks.js` serves
`/webhooks/login`, because `index.js` mounts it — so neither file can produce the
route on its own, and emitting the fragment would be a wrong fact rather than a
missing one.

A repo-wide pass resolves the two halves, the same way the Go extractor composes
gorilla/mux subrouters and the Rust one composes Axum's `.nest()`:

```js
// server/index.js
const webhookRoutes = require('./routes/webhooks');
app.use('/webhooks', webhookRoutes);
```

```js
// server/routes/webhooks.js
router.post('/login', handler);
module.exports = router;
```

```
route  /webhooks/login  server/routes/webhooks.js:12  props: framework=express, method=POST,
                                                             mount_composed=true
```

`mount_composed=true` marks a path assembled from more than one file. Resolution
covers ESM and CommonJS, renamed named exports (`export { router as api }`), a router
returned by a factory (`app.use('/api', routes())`), and mounts nested several files
deep; a router mounted at two prefixes emits its routes at both.

What it will not do is guess. A non-literal prefix (`app.use(base, router)`), a router
imported from an external package, and a router nothing mounts all emit **nothing** —
the same silence as before, on the grounds that a wrong path can false-match another
repository's route, which is worse than a missing one.

## Client calls, and how a near-miss is reported

```ts
await axios.get('/healthcheck');
await axios.get('/admin/users');
await axios.post(`/admin/users/${id}/ban`);
await axios.get('/not/served/anywhere');
```

```
route  /healthcheck           props: role=client, framework=axios,
                                     unmatched_by_server=true, unmatched_reason=generic_path
route  /admin/users           props: role=client, framework=axios          → resolves
route  /admin/users/{}/ban    props: role=client, framework=axios          → resolves
route  /not/served/anywhere   props: role=client, framework=axios,
                                     unmatched_by_server=true, unmatched_reason=path_unknown
```

Two things worth noticing. A template literal becomes `{}`, which matches the server's
`:id` — that is how `/admin/users/${id}/ban` links to `/admin/users/:id/ban`. And the two
misses carry **different reasons**: `/healthcheck` is too generic to attribute to one
server with confidence, while `/not/served/anywhere` has no candidate at all. The service
tally is `detected: 4, resolved: 2, unresolved: 2` — reported, not rounded up.

## Declared in-house clients

A client the config declares under `clients:` (see
[EXTENDING.md](../EXTENDING.md#teaching-enola-your-http-client)) is read by its receiver's
declared type, in any repository, Angular or not. From
[examples/custom-client](../../examples/custom-client/README.md):

```ts
@Injectable()
export class ResourceConnector {
  private readonly serviceName = "resource-api";
  private readonly basePath: string = "/v1/resources/";

  constructor(private readonly httpRequestService: IHttpRequestService) {}

  async getCatalogItems() {
    const url = `${this.basePath}catalog/items`;
    return this.httpRequestService.sendRequest<string[]>(this.serviceName, url, { method: GET });
  }

  async startImport(body: unknown) {
    await this.httpRequestService.sendRequest<void>(this.serviceName, "/v1/catalog/imports", {
      method: HttpMethod.POST,
      body,
    });
  }

  async getByKey(key: string) {
    return this.httpRequestService.sendRequest<string>(this.serviceName, this.pathFor(key), { method: GET });
  }
}
```

```
route       /v1/resources/catalog/items  props: role=client, source=configured-http-client,
                                                client_spec=sdk-http, method=GET, target_hint=resource-api
route       /v1/catalog/imports          props: role=client, source=configured-http-client,
                                                client_spec=sdk-http, method=POST, target_hint=resource-api
extraction  typescript:client:sdk-http   props: client_spec=sdk-http, receivers=1, skipped=dynamic_path=1,
                                                edge_coverage=[configured_http_call detected=3 resolved=2 unresolved=1]
```

| Written | Read as |
|---|---|
| `${this.basePath}catalog/items` through `const url` | the class field joined with the literal: `/v1/resources/catalog/items` |
| `"/v1/catalog/imports"` | the literal |
| `"/v1/items/" + id` | `/v1/items/{}`: an unknown operand inside the path is a parameter |
| a template whose leading operand is unknown | no route: the prefix is unknown |
| `this.pathFor(key)` | no route, counted `dynamic_path` |
| `"https://api.example.com/v1/items"` | no route, counted `non_route_path` |
| a call passing fewer arguments than `path_arg` names | no route, counted `missing_path_argument` |
| `{ method: GET }`, `{ method: HttpMethod.POST }`, `{ method: "put" }` | GET, POST, PUT |

The limits, stated. `this.<field>` resolves against the enclosing class only: an SDK
gives every connector its own `basePath`, and a repository-wide table would find the name
bound several times and resolve none of them. A constant on another class (`Other.BASE`)
is therefore not resolved. A local assigned more than once anywhere in the member folds to
nothing. The receiver must be `this.<member>`: a local variable of the configured type is
not read. Test files are excluded, as for every route pass.

## File-based routing

Nuxt pages and SvelteKit routes have no decorator to read; the path *is* the route.

```
pages/about.vue                          → route /about          framework=nuxt
src/routes/+page.svelte                  → route /               type=page
src/routes/about/+page.svelte            → route /about          type=page
src/routes/blog/[slug]/+page.svelte      → route /blog/[slug]    type=page
src/routes/api/users/+server.ts          → route /api/users      type=server, method=ALL
src/routes/(app)/dashboard/+page.svelte  → route /dashboard      route group stripped
src/routes/+layout.svelte                → route /               type=layout
```

Two deliberate non-routes, both covered by tests: `+page.server.ts` is a load function
rather than an endpoint and emits nothing, and a `.svelte` file outside `src/routes/`
(a component in `src/lib/`) is not a route.

## Ember — template tags, classic templates, and the resolver

Detected by an `ember-source` dependency in `package.json`. A Glimmer template-tag
file is TypeScript with embedded `<template>` blocks; the blocks are blanked in
place (newlines preserved) so the remainder parses with the standard grammar and
every fact's line number stays true to the original file:

```ts
// app/components/book-card.gts
import Component from '@glimmer/component';
import { service } from '@ember/service';
import Badge from 'shelf/components/badge';

export default class BookCard extends Component {   // line 5
  @service declare library: unknown;

  <template>
    <Badge>{{@status}}</Badge>
  </template>
}
```

```
symbol  app/components.BookCard   app/components/book-card.gts:5
        props: symbol_kind=class, web_component=component, framework=ember,
               ember_injected_services=[library]
        relations: calls -> app/components.Badge
                   injects -> app/services.LibraryService
```

Two things worth noticing. The template's `<Badge>` reference resolved through the
file's own imports — Glimmer strict mode guarantees that anything a template
renders is either imported or local, so import bindings plus the file's own
declarations are the exact resolution set, and a `{{this.title}}` or an `@arg`
can never produce a wrong edge. And the `injects` edge targets `LibraryService`,
the class `app/services/library.ts` *actually declares* — not a name guessed from
the service's `library` lookup key. That join happens in the post-link
`ember-resolver` binder, where the whole store is visible.

All three template positions the RFC allows are handled, each owning its own
template's references: a class-body template (above), a named binding
(`export const Question = <template>…` — the symbol classifies as a component and
carries exactly its own template's edges), and a standalone/default-export
template (the file's default component, synthesized when nothing claims the
segment). A class that embeds a template is a component whatever its superclass.

Classic `.hbs` templates have no imports to anchor on, so their invocations are
recorded and resolved the same way — via Ember's resolver layout, one candidate
file and one plausible symbol required, anything ambiguous skipped and counted in
`ember_unresolved` rather than guessed:

```
{{format-count this.rounded}}   →  calls -> app/helpers.formatCount
<Badge>…</Badge>                →  calls -> app/components.Badge
{{auto-focus}}                  →  calls -> app/modifiers.autoFocus
{{title}}                       →  nothing: indistinguishable from a property
```

Lookups anchor to the `app/` tree — Ember's own resolver rule — across
`components/`, `helpers/`, `modifiers/` and pods (`pods/<name>/component`),
with the classic `app/templates/components/` split recognized too — layouts
are candidate fragments, never a per-repo mode, so a mid-migration app
resolves all its vintages at once. A declaration-less re-export stub (a v1
addon's `app/`-tree publish, or any barrel) is chased to the file it
republishes, so `lib/<addon>/` components resolve into the host namespace;
engine templates (`lib/<engine>/addon/`) resolve in their own isolated tree.
Container-resolved classes (adapters, serializers, transforms, initializers,
routes, controllers) carry `framework_registered`, so the dead-code detector
stops flagging live singletons; `@attr('type')` binds a model to its
app-defined transform. `this.mount('shop')` becomes an `engine_mount` route
and the engine's own `buildRoutes` map composes onto it when the mount is
unique. Contextual components join two recorded literals — a yield-hash entry
(`(hash Item=(component "card-item"))`, or a strict-mode imported identifier)
and a block-param consumption (`<Card as |card|> … <card.Item/>`). File-local
single-assignment string constants fold into name arguments (derivation, not
inference), and irreducibly dynamic sites are counted with capped samples
(`ember_dynamic_count`/`_samples`) — visible, never guessed; a lookalike path elsewhere cannot
shadow the real file, and a co-located template is one component with its class,
not an ambiguity. A component template with no co-located class is a
template-only component and synthesizes its component symbol. A **route
template** (`app/templates/catalog.hbs`) is owned by its route class
(`app/routes/catalog.ts`, falling back to the controller), so components
rendered only from route templates still show their consumers in
`impact_analysis`. A `.hbs` file in a repo without `ember-source` emits nothing.

`Router.map` declarations become page routes with parent paths composed the way
the router composes them:

```
route  /catalog                    app/router.ts:6   ember_route_name=catalog
route  /catalog/:book_id           app/router.ts:7   ember_route_name=catalog.book
route  /catalog/:book_id/reviews   app/router.ts:8   ember_route_name=catalog.book.reviews
route  /my-account                 app/router.ts:11  ember_route_name=account
```

These carry `type=page`, `framework=ember` — UI routes in the same sense as Nuxt
pages and SvelteKit routes, never HTTP contracts (page-type routes are excluded
from cross-repo HTTP matching and can never surface as "unused routes").
`resetNamespace: true` is honored as the router defines it: the route *name*
restarts at that segment while the URL path keeps nesting. Each route gains a
`handled_by` edge to the route class its dot-name resolves to (`catalog.book` →
`app/routes/catalog/book.*`), and both a template's
`<LinkTo @route="catalog.book">` and a literal
`router.transitionTo('catalog.book')` / `replaceWith` in code become navigation
edges to the route fact (the implicit `.index` child resolves to its parent), so
the route graph answers "what implements this route" and "what navigates to it".

Ember's test tree stays out of the production graph: `tests/**/*-test.{js,ts,gjs,gts}`
is ignored for indexing and collected for reference-only `test_ref` extraction,
exactly like the dotted `.test.ts` convention — the hyphenated suffix is only
reserved *inside* `tests/`, so the directory is demanded (a production
`ab-test.ts` keeps its facts). An ember-data `Model` subclass
additionally emits a storage companion (`storage_kind=model`,
`framework=ember-data`, `table` holding the dasherized model name), the same
shape ActiveRecord models get in the Ruby extractor — and its
`@belongsTo`/`@hasMany` fields become `depends_on` edges to the storage facts of
the models they name (`ember_relationships` records the declared set; a bare
`@hasMany` is skipped, since recovering a singular model name from a plural
field would be a guess). The rest of the per-model quartet joins the graph:
classes under `adapters/`, `serializers/` and `transforms/` carry
`ember_data_role` and bind `depends_on` to the model their file base names (the
reserved `application` base is the app-wide fallback and names none). Container
lookups with a literal `service:` key — `owner.lookup('service:current')` —
merge into the same injection pipeline as `@service` fields.

## Angular — decorators, templates, the router and the composition graph

Detected by an `@angular/core` dependency in a `package.json`, searched two levels
down as well as at the TypeScript root — one corpus application is an Angular
frontend inside a Rails monolith, another sits in `apps/<app>/` of an Nx workspace,
and a root-only check reads both as not-Angular.

Everything below is gated on that. A class decorated `@Component` in a repository
with no Angular dependency models nothing, the same rule `@Entity` is held to.

### The class model

```ts
// src/app/user-card.component.ts
@Component({ selector: 'app-user-card', templateUrl: './user-card.component.html' })
export class UserCardComponent {
  constructor(private readonly users: UserService) {}
  private readonly layout = inject(LayoutService);
}
```

```
symbol  src/app.UserCardComponent   src/app/user-card.component.ts:2
        props: symbol_kind=class, web_component=component, framework=angular,
               angular_selector=app-user-card,
               angular_template_url=src/app/user-card.component.html
        relations: injects -> src/app/services.UserService
                   injects -> src/app/core.LayoutService
```

`web_component` is one of `component`, `directive`, `pipe`, `service`, `ng_module`.
Both injection dialects are read — a constructor parameter property and an
`inject()` field — because a real codebase uses both, often in the same class, and
an `@Inject(TOKEN)` parameter names its token rather than the type beside it.

**An `injects` edge is derived, never guessed.** The target resolves through the
file's own import table or a class the file declares; a type from a package resolves
to nothing and is COUNTED, in `typescript:angular-di`, under a cause read off the
import statement rather than inferred from the identifier. Across ten public
repositories that is 14,947 injection sites, 53% of which name a class in their own
repository and resolve to it.

Only `ng_module` carries `framework_registered`. The prop means *this class's use is
not derivable from the graph*, and after the passes below a component is named by a
template tag, a route or a `declarations:` array, a pipe by its name in an
expression, and a service by an injection site. Flagging those too would suppress the
dead code those edges make findable.

### Routes

A route array states a path fragment; the prefix it hangs under is decided somewhere
else — by a parent's `children:`, by the entry whose `loadChildren` lazily loads the
module the array belongs to, or by nothing at all. Paths are therefore composed by a
repo-wide walk outward from the application roots (`RouterModule.forRoot` and
`provideRouter`), the shape the Express, gorilla/mux and Axum passes already share:

```
route  /admin/users        src/app/admin/admin-routing.module.ts:6   type=page,
                                                                     mount_composed=true
       handled_by -> src/app/admin.UsersComponent
       depends_on -> src/app.AuthGuard        (canActivate)
```

Every fact carries `type=page`, so an application's navigation is excluded from
cross-repo HTTP matching and can never surface as an unused route — the same
contract Ember's router map and Nuxt's pages have.

A lazy `loadChildren` names a MODULE, not an array, so the array is found by an exact
export name, by the target file's single `forChild` array, or by the single one among
that file's own imports — one candidate required, and anything ambiguous is counted.
An array no root reaches emits **nothing**: a component library whose only router
call is `forChild` contributes no routes, which is the correct reading of a library.

Two path forms are read beyond the literal: `[…] as Routes` behind a default export,
and a factory-wrapped route object (`route({path: …, loadComponent: …})`). A path
that names a constant is folded to the literal through an enum or an `as const` map,
in this file or the one it was imported from; one that resolves to nothing is refused
rather than written out as an identifier.

### Templates

In Angular a component member is very often referenced ONLY from its template, and so
is a child component, which appears as a tag and nowhere else in the class:

```html
<h1>{{ total }}</h1>
<button (click)="save()">save</button>
@if (loading) { <app-spinner/> } @else { <app-card [item]="row"/> }
```

```
src/app.PageComponent  calls -> src/app.PageComponent.total
                       calls -> src/app.PageComponent.save
                       calls -> src/app.PageComponent.loading
                       calls -> src/app.SpinnerComponent
                       calls -> src/app.CardComponent
```

Both dialects are read — `*ngIf`/`{{ }}` and Angular 17 `@if`/`@for`/`@switch`/
`@defer` blocks — because both are live in real repositories and frequently in the
same file. Inline `template:` strings are scanned exactly as external ones.

Three resolution regimes, in decreasing order of certainty:

- **a binding identifier is an edge only when it names a member the component
  declares.** `{{ title }}` where the class has no `title` is indistinguishable from a
  local, an `@Input` alias or a global, and produces nothing.
- **a tag resolves against a DECLARED selector, matched whole.**
  `tui-data-list-wrapper[labels]` selects that element carrying that attribute, and
  matching either half alone attaches the component to templates that never render
  it. `selector: 'app-icon:not([badge])'` is an ordinary element selector with an
  exclusion attached, and is indexed as one.
- **everything else is counted, never guessed** — an unknown custom-element tag, a
  selector two classes claim, a pipe name nothing declares. Angular's own
  `ng-template`/`ng-container`/`ng-content`/`router-outlet` are neither.

Measured over four repositories, 200 of 200 sampled template edges were justified by
the template they came from.

### Composition and the workspace

An Angular application's dependency structure is not in its import statements: a
component can render another only because some `@NgModule` declared the first and
imported the module exporting the second, or because the component's own `imports:`
array names it. Both become `depends_on` edges, with the names recorded per array
(`angular_declarations`, `angular_module_imports`, `angular_module_exports`,
`angular_providers`). A `SharedModule.forRoot()` entry names its RECEIVER — the call
configures the module, it does not name something else — and a provider literal
`{provide: TOKEN, useClass: Impl}` names both halves.

Module facts additionally carry `workspace_project`, read from an Nx `project.json`
or an `angular.json` `projects` map. In a monorepo the unit of ownership is the
project, not the directory.

### Requests

The general client pass requires a `/`-rooted literal, which is what keeps
`map.get("key")` out of the graph. A class that injects `HttpClient` has a member
whose declared type already says what the call is, so two further shapes are read:

```ts
this.http.post<T>('api/v1/appdeployment', spec)             // no leading slash
this.authHttp.get<T>(VideoService.BASE_URL + '/' + id + '/stats')
```

```
route  /api/v1/appdeployment          role=client, api=angular-httpclient
route  /api/v1/videos/{}/stats        role=client, api=angular-httpclient
```

A class-static base is folded repo-wide, because it belongs to the service that owns
the resource and is named by every service that touches it. An unresolved LEADING
operand means the prefix is unknown and the call contributes nothing; anywhere else
an unresolved operand is a path parameter.

## Storage — three ORMs, one shape

```
storage  prisma.Post   prisma/schema.prisma:5   props: framework=prisma,  table=Post
storage  src.User      src/entity.ts:5          props: framework=typeorm, table=users
storage  src.Session   src/entity.ts:15         props: framework=typeorm, table=Session
storage  src.orders    src/schema.ts:4          props: framework=drizzle, table=orders
```

`table` is the *physical* name — `@Entity('users')` gives `users`, while an undecorated
`Session` class keeps its class name. That distinction matters when the same database is
reached from another repository in another language.

## GraphQL — client operations and schema-first Node servers

A `gql`-tagged template literal or a standalone `.graphql` operation document names its
operation kind and root fields literally; each root field becomes a client-role route
fact (`Query.pageViews`) — the joinable name the cross-repo `graphql` signal matches
against a server's root fields. This is the client half of the seam.

The server half reads SDL used by Apollo Server, GraphQL Yoga, Mercurius,
express-graphql, graphql-http, GraphQL Tools, and GraphQL.js `buildSchema`. It also reads
code-first root fields declared by NestJS GraphQL and TypeGraphQL
`@Query`/`@Mutation`/`@Subscription` decorators; Nexus's `queryType`, `mutationType`,
`subscriptionType`, root `extendType` definitions, and named or callback
`queryField`/`mutationField`/`subscriptionField` shorthands; and the corresponding singular
Pothos builder methods. Package provenance is required, so unrelated decorators
or builder methods with those generic names remain inert. A schema may
be a plain template literal or a `gql`-tagged one, assigned through `typeDefs`, `schema`,
or conventional suffixed bindings such as `gqlSchema` and `userTypeDefs`; TypeScript
annotations such as `const schema: string = ...` are accepted. Standalone `.graphql` and
`.gql` schema documents are read when a server file imports them or they live under
Hasura's metadata convention; unrelated benchmark and schema-copy documents stay inert. Each
`Query`/`Mutation`/`Subscription` root field (including one declared with `extend type`)
becomes a server-role route fact with the same name shape as the client side. Detection
is repository-wide and syntax-based, so a constructor in `server.ts` activates a modular
SDL definition in `schema.ts`, while a `buildSchema(...)` example in documentation does
not. A client-only repository carrying a schema copy remains inert because it has no
GraphQL server signal.

Relay's `graphql` tag and the `gql` tags commonly used by Apollo, urql, and
graphql-request share the same operation extraction. Static untagged operation strings
are also read in graphql-request modules and when they are the `query` property of a
plain `fetch` JSON body; arbitrary operation-looking strings elsewhere remain inert.
In Vue and Nuxt single-file components the same extraction runs inside `<script>` and
`<script setup>`, covering Nuxt Apollo calls such as ``useAsyncQuery(gql`…`)`` and
``useMutation(gql`…`)`` while preserving the operation's line in the `.vue` file.

This reads the schema surface only: which root fields exist, not which resolver
implements one. Resolver-to-field binding — matching a `resolvers.Query.book` handler
back to its schema field, the way Kafka call sites bind to AsyncAPI operations — is a
separate, later capability.

## Kafka and AsyncAPI contracts

Files importing KafkaJS, node-rdkafka, or kafka-node emit directional topic facts
for KafkaJS `producer.send({ topic })` and `consumer.subscribe({ topic })`, and for
node-rdkafka-style `producer.produce(topic, ...)`. Topics may be string literals,
static template literals, or immutable file-local `const` bindings. Each fact carries
the enclosing function or class-method symbol and can bind to a unique AsyncAPI
operation with the same topic and publish/subscribe direction.

The package-import gate keeps unrelated event buses and WebSocket APIs with methods
named `send` or `subscribe` out. Dynamically assembled topics and mutable `let`/`var`
bindings are left unresolved rather than guessed, and test files contribute no
production messaging operations.

## What is deliberately not extracted

- **Non-literal paths.** `axios.get(url)` where `url` is a variable with no literal
  binding produces no route. A base URL resolved from a file-local literal *is* followed;
  one injected from config is not.
- **`openGraph` and metadata objects** that contain URL-shaped strings are not requests,
  and are not recorded as client routes.
- **Prefetch and refetch helpers** are separated from real request boundaries, so a
  query-cache warm-up does not double-count as an outbound call.
- **Paths without a leading `/`.** A bare `"users"` string is too ambiguous to treat as a
  request path — *unless* the receiver's declared type is Angular's `HttpClient`, which
  settles what the call is before the argument is read.
- **Runtime-registered routes** — an Express router assembled in a loop over a config
  array is not unrolled, and a mount whose prefix is a variable rather than a literal
  is not resolved.
- **Angular's dynamic surface.** A component rendered through
  `ViewContainerRef.createComponent` or a runtime registry (`[component]="i | pick"`)
  has no static reference to find; a route table supplied through a `ROUTES` provider
  factory is not written down to read, and is reported as `runtime_route_provider`
  rather than as an empty application. A string or `InjectionToken` DI token resolves
  to a class only when the token itself is a declaration in the repository.
- **Ember names the default resolver cannot map.** Addon components (they resolve
  into `node_modules`), pods layout, custom resolvers, and engine mount points
  produce no edge; the misses are recorded in `ember_unresolved`, not guessed at.
- **GraphQL resolver maps.** A `resolvers` object passed alongside SDL
  is not read, so a root field's implementing function is not (yet) part of the graph —
  only that the field exists. Code-first schemas (decorators or builder APIs with no SDL
  string) are not detected either.

---

Measured on real TypeScript repositories: [BENCHMARKS.md](../BENCHMARKS.md).
