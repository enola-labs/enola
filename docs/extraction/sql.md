# SQL — migration DDL as storage ownership

SQL migrations are scanned without executing them. Enola records tables a repository
creates or alters so data-heavy systems do not report an empty storage surface merely
because their schema is expressed outside an ORM.

Fixture coverage lives in [`internal/extractors/sqlextractor/sql_test.go`](../../internal/extractors/sqlextractor/sql_test.go).

## At a glance

| You write | Enola stores | Kind |
|---|---|---|
| `CREATE TABLE traces (…)` | the declared table and migration location | `storage` |
| `ALTER TABLE traces …` | the table touched by the migration | `storage` |
| `SELECT … FROM traces` | nothing | — |

## Table DDL becomes storage

```sql
CREATE TABLE IF NOT EXISTS traces (id UUID);
ALTER TABLE traces ADD COLUMN name text;
```

Both statements identify `traces`. Across the repository they produce one storage
fact per migration directory and table, preferring its `CREATE` statement as provenance,
with `storage_kind=table`, `framework=sql-ddl`, and the physical name in `table`.
Quoted identifiers and optional schema-qualified names are preserved after removing
identifier quotes.

## What is deliberately not extracted

- Queries do not establish schema ownership, so `SELECT`, `INSERT`, `UPDATE`, and
  `DELETE` do not create storage facts.
- Columns, constraints, indexes, views, functions, and procedural SQL are not modeled.
- Dynamic SQL and template expansion are not evaluated.
- DDL dialect semantics are not interpreted; this is a declaration scanner, not a SQL
  parser or migration runner.
