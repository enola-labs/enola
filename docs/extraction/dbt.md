# dbt — models, their lineage and their sources

A dbt project is found by its `dbt_project.yml`, at any depth; a repository may
hold several. Its `model-paths` and `snapshot-paths` (default `models` and
`snapshots`) are read, and its `seed-paths` (default `seeds`) for the CSVs a
`ref` can name; analyses, tests, macros, installed packages
(`dbt_packages/`) and compiled output (`target/`) are not.

## At a glance

| You write | enola stores | Kind |
|---|---|---|
| `models/marts/fct_orders.sql` in project `orders` | a symbol `orders.fct_orders`, `language: dbt`, a framework root | `symbol` |
| `{{ ref("stg_orders") }}` in it | a `depends_on` edge to `orders.stg_orders` | relation |
| `{{ ref("shared", "dim_dates") }}` | a `depends_on` edge to `shared.dim_dates` | relation |
| `{{ source("raw", "orders") }}` | a `depends_on` edge to `raw.orders` | relation |
| a `sources:` table `orders` of source `raw` | a fact `raw.orders`, `storage_kind: table`, `engine: dbt_source`, with its database and schema | `storage` |
| a model's `description:` in a properties file | the model symbol's doc | prop |
| the table a model builds (`fct_orders`, or its `alias`) | a fact `fct_orders`, `storage_kind: table`, `engine: dbt_model`, depending on the model | `storage` |
| `seeds/country_codes.csv` | a symbol `orders.country_codes`, `dbt_resource: seed`, which `ref("country_codes")` reaches | `symbol` |

A `ref` without a package names a model of the same project; failing that, the
one other project in the repository that declares it, which is how a shared
package checked in beside the projects is reached.

Every model is a framework root. dbt builds all of them, and a mart nothing
refs is read by a dashboard or an export rather than dead, so none is reported
as dead code. Impact runs the other way: what depends on a staging model, or on
a source table, is every model downstream of it.

## Where a source's rows come from

A source names a table something outside the project writes. After every
repository is extracted, the `warehouse-lineage` binder links each source to what
loads it, and records the rule on the source as `lineage`:

| The source | is linked to | `lineage` |
|---|---|---|
| `marts.fct_orders` (any source naming a table another dbt project builds) | that project's table `fct_orders`; among several, the one under a directory named like the source's `schema` | `model` |
| `contracts.shop_orders_order_placed` | the Avro record or Kafka topic `contracts.shop.orders.order_placed` (separators aside, the namespace's first segment optional) | `named` |
| `cdc_changelog.shop_orders` | the change-feed record `cdc.shop.orders` | `named` |
| `raw.shop__line_items` (no record or topic) | the table `line_items` of the loaded repository `shop` | `cdc` |

A change-feed record or topic, `cdc.<database>.<table>`, is itself linked to the
table its database's repository declares (a Rails model's table, a `structure.sql`
table), the database read as written, with underscores as hyphens, and without an
environment suffix (`_sandbox`, `_prod`, …). A walk from a service's table thus
reaches its change feed, the sources landing it and every model downstream. A name
with more than one candidate links nothing.

## What is deliberately not extracted

A `ref` or `source` whose arguments are not literals (built from a variable or a
loop) names nothing. Columns, tests, macros and exposures are not read, nor a seed's contents. Models
carry no `exported` prop: dbt has no visibility, so a project-wide public surface
says nothing. A model many others select from is not reported as a god class
(splitting a shared table is not advice that applies); hotspots still name the
central ones.
