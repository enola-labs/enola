# Ruby — what enola extracts

Parsed with tree-sitter. Detected by a `Gemfile` at the repository root. Rails
conventions are recognised, but a plain Ruby project still produces modules, symbols and
call edges.

Fixture: [`ruby_sample`](../../internal/engine/testdata/repos/ruby_sample/)

For the workflow rather than the fact model — binding the shipped Rails convention
recipes, pinning a baseline and grading a change — see [RAILS.md](../RAILS.md).

## At a glance

| You write | enola stores | Kind |
|---|---|---|
| `app/services`, `app/models/concerns`, … | one module per directory, `framework=rails` | `module` |
| `class Foo < Bar` | a symbol with `superclass` | `symbol` |
| `module Foo` | a symbol with `symbol_kind=interface` and an `abstract` verdict | `symbol` |
| `def x` / `def self.x` | `Foo#x` (instance) / `Foo.x` (class) | `symbol` |
| `STOP_WORDS = …` | a `constant` symbol | `symbol` |
| `resources :things` in `routes.rb` | one route per generated path **and** method | `route` |
| a class-body DSL macro (`track_metrics :runs`) | a `calls` edge on the class fact | relation |
| `delegate :a, :b, to: :x` | `calls` edges for each delegated name | relation |
| `public_send(:"report_#{t}")` | a `dynamic_send_prefixes` hint on the file | `file_ref` |
| an ERB template | a `file_ref` carrying its helper and presenter calls | `file_ref` |
| `spec/**/*_spec.rb`, `test/**/*_test.rb` | a reference-only `test_ref` | `test_ref` |
| `class X < Sequel::Model(:customers)` | a model with the literal dataset table | `storage` |
| `has_many :versions` | the model's declared relation to another model | `association` |
| `db/schema.rb` / `db/structure.sql` | each table's column census, folded onto the model that reads it | `storage` |
| `data-controller="autocomplete"` in an ERB view | a `stimulus-binding:` dependency at `markup-declared` | `dependency` |
| `turbo_frame_tag :post_1` | a `turbo-frame:` dependency naming the frame id | `dependency` |
| `broadcasts_to :room` | a `broadcast:` dependency at `literal-declared` | `dependency` |
| `field :page_views` in a root type | a server GraphQL route `Query.pageViews` | `route` |
| `"query { pageViews { total } }"` | a client GraphQL route per root field | `route` |
| `connection.post(build_url("/pageview"))` | a client route derived through the wrapper literal | `route` |

## Routes — nested resource path shapes

23 lines of routing DSL:

```ruby
Rails.application.routes.draw do
  namespace :admin do
    resources :reports, only: [:index, :show] do
      resource  :export,   only: [:show, :create]
      resources :sections, only: [:index, :show]
    end
  end

  resource :session, only: [:show, :create, :destroy]
  root to: "home#index"
end
```

Ten route facts, each with the method and the controller action:

```
GET    /admin/reports                              action=index
GET    /admin/reports/:id                          action=show
GET    /admin/reports/:report_id/export            action=show      ← singular: no :id of its own
POST   /admin/reports/:report_id/export            action=create
GET    /admin/reports/:report_id/sections          action=index
GET    /admin/reports/:report_id/sections/:id      action=show      ← plural: keeps its own :id
GET    /session                                    action=show
POST   /session                                    action=create
DELETE /session                                    action=destroy
GET    /                                           handler=home#index
```

The distinction the extractor has to get right is the third line versus the sixth. A
singular `resource` nested in a plural `resources` hangs off the parent **member**
(`/:report_id`) and has no `:id`; a plural `resources` keeps one. Get that wrong and a
client calling `/admin/reports/7/export` matches nothing. `scope` and `namespace`
prefixes compose the same way.

## Modules, classes and the `abstract` verdict

Ruby modules do two unrelated jobs, and enola separates them:

```ruby
module Trackable                 # a mixin: defines instance methods
  extend ActiveSupport::Concern
  def track!; touch(:tracked_at); end
end

module Reporting                 # a namespace: only wraps a class
  class ReportWorker < BaseWorker; end
end
```

```
symbol Trackable   props: symbol_kind=interface, concern=true, abstract=true
symbol Reporting   props: symbol_kind=interface,               abstract=false
symbol Reporting::ReportWorker  props: symbol_kind=class, superclass=BaseWorker
```

`abstract` feeds package-metrics abstractness. A Concern that defines instance methods is
never instantiated on its own and *is* an abstraction; a Rails namespace module is not,
and counting it as one skews every module's abstractness score.

## DSL macros, delegation and dynamic dispatch

Ruby's metaprogramming is where naive extraction produces false dead code. Three cases,
all resolved into ordinary edges:

```ruby
class ReportWorker < BaseWorker
  track_metrics :runs                              # class-body macro
  delegate :formatted_name, :subtitle, to: :presenter

  def self.build(type)
    public_send(:"report_#{type}", type)           # dynamic dispatch
  end

  def report_daily(type); end
  def report_weekly(type); end
end
```

- The macro is recorded as a `calls` edge (`track_metrics`) **on the class fact**, so the
  base method backing it is not reported as dead.
- `delegate` folds each delegated name into `calls`, so `formatted_name` and `subtitle`
  have callers.
- The interpolated symbol contributes `dynamic_send_prefixes: ["report_"]` to the file,
  and same-prefix methods are treated as dispatched rather than dead. The prefix is only
  committed because this scope also calls `send`/`public_send` — proximity to a
  dispatcher is what makes the guess safe enough to act on.

### Loop counting that does not cry wolf

```ruby
def summarize(users)
  users.each { |user| notify(user); user.recent_posts }  # scaling → loop_depth 1
  6.times { |i| bucket(i) }                              # constant-bounded → no depth
  STOP_WORDS.each { |w| index(w) }                       # ALL-CAPS constant → bounded
  %w[csv pdf].map.each { |fmt| render_format(fmt) }      # bounded literal behind a chain
end

def reindex(model)
  model.find_in_batches { |batch| batch.each { |r| persist(r) } }   # depth 1, not 2
end
```

```
symbol …#summarize  props: loop_count=4, loop_depth=1, calls_in_loop=[notify, recent_posts]
symbol …#reindex    props: loop_count=1, loop_depth=1, calls_in_loop=[persist]
```

Four loops, one of them scaling. `find_in_batches` yields a *batch*, so the inner `.each`
is the real per-element loop and the nesting is depth 1 — reporting depth 2 there would
flag the standard Rails batching idiom as a performance problem in every Rails codebase.

### `super` is not recursion

```ruby
def display_label
  decorate(__method__)
  super
end
```

`super` records a call to the same-named ancestor method and is deliberately *not*
flagged `recursive_self` — it climbs the ancestor chain and terminates.

## Views and tests

```
file_ref app/views/reports/show.html.erb  --calls--> ReportPresenter.render_summary
                                          --calls--> current_user
                                          --calls--> can_view_reports?
test_ref spec/services/report_worker_spec.rb --calls--> Reporting::ReportWorker.build
```

ERB templates carry the only caller many presenters and helpers have. Specs are excluded
from indexing but recovered as reference-only facts, so a method used only by a spec has
an incoming edge without the spec becoming architecture.

### One naming hazard, handled

`app/jobs/cache_warmup_ab_test.rb` is production code whose basename ends in `test`. The
test globs are **directory-scoped** (`**/test/**/*_test.rb`), not a bare `*_test.rb`
suffix, so this file is indexed as source. A bare suffix glob both ignored it and routed
it to reference-only extraction, and the class vanished from the graph entirely.

## Storage — ActiveRecord and Sequel

An ActiveRecord model emits a `storage` fact beside its class symbol, with the
table inferred from the class name unless `self.table_name` states it. A
`Sequel::Model` subclass gets the same companion shape, and the dataset form's
literal argument wins as the physical table:

```ruby
class CustomerRecord < Sequel::Model(:customers)
end
```

```
symbol   CustomerRecord   props: superclass=Sequel::Model
         relations: implements -> Sequel::Model
storage  CustomerRecord   props: storage_kind=model, table=customers, framework=sequel
```

The call-form superclass is read whole so the table literal survives, and
stripped back to the base name everywhere the class name alone is meant —
`superclass` and the `implements` target say `Sequel::Model`, never
`Sequel::Model(:customers)`. (Until v154 the call form was dropped entirely
and the dataset idiom emitted no storage fact.)

### Where a table name comes from, and how sure enola is

Every model storage fact carries `table_source`, because "the table this model
reads" is answered by four different mechanisms with four different strengths:

| `table_source` | Came from |
|---|---|
| `derived` | the class name, pluralised — a convention, not a statement |
| `declared` | `self.table_name = "…"` in the model |
| `prefixed` | a namespace's `def self.table_name_prefix` prepended to a derived name |

A declared table **corrects the model's own fact** rather than adding a second one.
Until v208 `self.table_name = "active_admin_comments"` emitted a free-floating
`storage` fact under that name while the model beside it still claimed the derived
`comments`, so the graph asserted a table that did not exist and missed the one that
did. There is now one fact per thing: `ActiveAdmin::Comment` with
`table=active_admin_comments, table_source=declared`. The interpolated form Rails
apps actually write — `"#{table_name_prefix}active_admin_comments#{table_name_suffix}"`
— resolves to its literal part.

A namespace prefix is applied only to a *derived* name, because Rails does not prefix
a table you stated yourself; prefixing a declared one would replace a fact with a
guess. And a computed or interpolated `table_name_prefix` states nothing, so nothing
is applied.

**An abstract class has no table.** `self.abstract_class = true` means the class
exists to be inherited from, so it emits no storage fact at all. `ActiveStorage::Record`
previously claimed a table named `records`.

### The schema files, read as the database's own account

`db/schema.rb` and `db/structure.sql` state what the model layer can only infer. Each
`create_table` / `CREATE TABLE` contributes a column census, and where a model already
claims that table the census is **folded onto the model's existing fact** — one table,
one storage identity — rather than becoming a second node:

```
storage  ApiKey  props: storage_kind=model, table=api_keys, table_source=derived,
                        columns="created_at expires_at hashed_key id …"
```

`structure.sql` additionally yields single-column `fk_constraints` as
`"column->reftable"`. Both readers are line- and regex-based over the shapes the
dumpers actually emit: composite foreign keys and unrecognised lines are skipped, never
guessed. The correction above runs *before* the fold, so a prefixed table's census
lands on the model that reads it rather than on whichever model the unprefixed name
collided with.

## Associations — what a model says it is related to

`has_many`, `has_one`, `belongs_to` and `has_and_belongs_to_many` each emit an
`association` fact naming the declaring model, the macro, and the target:

```
association  ApiKey#api_key_rubygem_scope
             props: model=ApiKey, macro=has_one, association=api_key_rubygem_scope,
                    target=ApiKeyRubygemScope, target_source=derived
```

`target_source` is `derived` when the class name was inferred from the association
name and `declared` when `class_name:` stated it. These are what let
[`enola endpoint`](../CLI.md) walk from a URL to the tables behind it: route →
controller → the models it touches → the models *those* are associated with → tables.

Associations are deliberately **not** import edges. A bidirectional Rails relation
would otherwise manufacture a dependency cycle that does not exist in the load order;
see [EXPLAINERS.md](../EXPLAINERS.md).

## The view layer — bindings stated in markup

Rails puts real wiring in HTML attributes, where no amount of Ruby parsing will find
it. Three shapes are read, each recorded at the honesty level it actually has rather
than resolved into a call edge it cannot justify:

```erb
<div data-controller="autocomplete"
     data-action="click->autocomplete#choose keydown->autocomplete#highlight">
```

```
dependency  stimulus-binding: app/views/home/index.html.erb -> autocomplete
            props: framework=stimulus, binding=data-action,
                   resolution_level=markup-declared,
                   stimulus_handlers="choose highlight"
            relations: calls -> app/javascript/controllers.AutocompleteController.choose
```

The fact links to `app/javascript/controllers/<name>_controller.(js|ts)` only when that
conventional file exists; otherwise it stays name-only. An ERB interpolation in the
attribute is not a plain Stimulus token and declares nothing.

Literal Turbo frame ids — `turbo_frame_tag :post_1`, `data-turbo-frame="results"` —
become `turbo-frame:` dependencies, because a frame id is an identity two markup sites
share. `dom_id` calls, interpolation and the reserved `_top` target emit nothing.
Model-side `broadcasts_to` with a literal symbol or string stream becomes
`broadcast: <Model> -> <stream>` at `literal-declared`; the common lambda form computes
its stream per record at runtime and emits nothing.

**importmap-rails apps are JavaScript projects.** The TypeScript extractor claims every
`.js` file, but detection knew only `package.json` and `tsconfig` shapes — so a Rails
app whose pins live in `config/importmap.rb` and which ships no `package.json` never
ran it. On one 8-repo census that was 74 of 100 skipped-with-cause files, every one a
claimed, parseable, unparsed source file. `config/importmap.rb` now switches the
extractor on; vendored minified bundles under `vendor/javascript` are still skipped.

## GraphQL — the server half and the Ruby client half

Root types declare the operation surface; each `field` in a
`QueryType`/`MutationType`/`SubscriptionType` class — namespace-qualified
forms like `class Types::QueryType` included — becomes a server route named by
its camelized root field, the joinable name the graphql cross-repo signal
matches:

```ruby
# app/graphql/types/query_type.rb
class Types::QueryType < Types::BaseObject
  field :page_views, [Types::PageViewType], null: false
end
```

```
route  Query.pageViews   props: role=server, type=graphql, source=graphql-ruby-dsl
```

Non-root types' fields are schema internals and emit nothing. Camelization
follows graphql-ruby's default; a schema overriding it shows up as an
unmatched client op, never as a wrong edge.

A Rails service *calling* a sibling's GraphQL API writes the operation as a
plain Ruby string, and that is the client half — a quoted literal or heredoc
body whose content is structurally an operation head:

```ruby
def stats_query
  "query {
    pageViews {
      total
    }
  }"
end
```

```
route  Query.pageViews   props: role=client, type=graphql, source=graphql-ruby-string
```

The anchor is an OPENING quote on the same line as the keyword, and the head
must be structurally an operation (optional name, optional parenthesized
variables, brace) — so Ruby's own block syntax (`query { |x| x.active }`) and
a closing quote followed by Ruby code can never match. Files under a
`graphql/` tree and files declaring a root type are excluded as server side.

## Outbound HTTP — literal paths, and literals one derivation away

Hand-written client calls with literal paths emit client routes as written
(`conn.get "users/123"`). Two bounded derivations extend that reach (shared
with the TypeScript extractor via `litfold`):

```ruby
connection.post(build_url("/pageview"), attributes)
```

```
route  /pageview   props: role=client, method=POST, derived=wrapper-literal,
                          target_hint=metricshost
```

A wrapper call's single string argument derives the path only when it is
"/"-rooted — `t("labels.metrics")` derives nothing — and the `derived` prop
says a folded literal apart from an inline one. The `target_hint` falls back
to the first base-URL environment variable read in the file.

## Event publications and subscriptions

A publication or subscription that names its topic becomes a topic operation, the
shape the Go and TypeScript Kafka readers emit, so it binds to the AsyncAPI operation
it implements and links across repositories:

```ruby
class Orders::Events::OrderPlaced
  TOPIC = SCHEMA_SUBJECT = "shop.#{PORTAL}.orders.order_placed".freeze

  def run
    Broker.outbox.publish_event(payload, topic: TOPIC, key: id)   # publish
  end
end

config.subscribe_event "billing.order_placed" do |s|                # subscribe,
  s.topic   = "shop.#{PORTAL}.orders.order_placed"                   # one per handler
  s.handler = Billing::OrderPlacedConsumer
end
```

```
storage  shop.{portal}.orders.order_placed   props: messaging_operation=publish,
         source=ruby-messaging-call, code_symbol=Orders::Events::OrderPlaced#run
storage  shop.{portal}.orders.order_placed   props: messaging_operation=subscribe,
         source=ruby-messaging-call, code_symbol=Billing::OrderPlacedConsumer
```

A publication is a `topic:` keyword on a call to a publish/produce/deliver method, on
any other call when the topic is a dotted name (an outbox writer's `run(topic: …)`), or
in a message hash carrying `topic:` and `payload:` together. A subscription is a block
assigning `topic` and `handler` or `handlers`. The topic is a literal or a constant
resolving to one, in the file or, by its full name or a bare name no other constant
shares, anywhere in the repository; an interpolation is a `{parameter}`. No `messaging`
protocol is stated: the source does not name the broker. A `{parameter}` segment binds
to a contract naming one value there, and the reverse.

## What is deliberately not extracted

- **Routes drawn dynamically** — a `routes.rb` that loops over a config array is not
  unrolled.
- **`method_missing` and `define_method` with computed names**, beyond the
  dispatcher-proximity prefix heuristic above.
- **`ApplicationRecord`, `*BaseController` and `*::Base`** are excluded from god-class
  candidacy: their fan-in comes from every subclass inheriting them, which is the
  framework working, not a design problem.
- **GraphQL operations built by string concatenation or interpolation of the
  operation head** — only a literal head names its root fields.
- **Multi-step derivations** — `litfold` folds one step, never chases a chain,
  and a name assigned twice folds nothing.

---

Measured on real Ruby repositories: [BENCHMARKS.md](../BENCHMARKS.md).
