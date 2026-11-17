# LookML — views, explores and the tables they read

Every `.lkml` and `.lookml` file is read. A model file's name (`shop.model.lkml`
→ `shop`) names the explores it declares.

## At a glance

| You write | enola stores | Kind |
|---|---|---|
| `view: orders { sql_table_name: proj.marts.fct_orders ;; }` | a symbol `lookml.orders` depending on `fct_orders` | `symbol` |
| `derived_table: { sql: … FROM proj.marts.fct_orders JOIN proj.marts.dim_dates … ;; }` | `depends_on` edges to `fct_orders` and `dim_dates` | relation |
| `extends: [orders]` | a `depends_on` edge to `lookml.orders` | relation |
| `explore: orders { join: users { … } }` in `shop.model.lkml` | a symbol `lookml.shop.orders` depending on `lookml.orders` and `lookml.users` | `symbol` |
| `join: buyers { from: users }`, `explore: x { from: y }` | the view named by `from` (or `view_name`) | relation |

A table is named by the last segment of its qualified name, which is the name
the dbt extractor gives the table a model builds. In a snapshot holding both
the dbt and the Looker repository, a change to a model therefore reaches, by
impact, the views reading its table and the explores built on them.

Views and explores are framework roots: Looker serves them, and nothing in the
repository calls them, so none is reported as dead code.

## What is deliberately not extracted

Dimensions, measures, parameters, dashboards, `include:` globs and Liquid
templating are not read. A templated table name (`${…}`, `{% … %}`) names no
table.
