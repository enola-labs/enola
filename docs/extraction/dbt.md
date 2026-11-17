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
| `seeds/country_codes.csv` | a symbol `orders.country_codes`, `dbt_resource: seed`, which `ref("country_codes")` reaches | `symbol` |

A `ref` without a package names a model of the same project; failing that, the
one other project in the repository that declares it, which is how a shared
package checked in beside the projects is reached.

Every model is a framework root. dbt builds all of them, and a mart nothing
refs is read by a dashboard or an export rather than dead, so none is reported
as dead code. Impact runs the other way: what depends on a staging model, or on
a source table, is every model downstream of it.

## What is deliberately not extracted

A `ref` or `source` whose arguments are not literals (built from a variable or a
loop) names nothing. Columns, tests, macros and exposures are not read, nor a seed's contents. Models
carry no `exported` prop: dbt has no visibility, so a project-wide public surface
says nothing. A model many others select from is not reported as a god class
(splitting a shared table is not advice that applies); hotspots still name the
central ones.
