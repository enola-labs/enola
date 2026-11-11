# Vespa — document types as storage

A Vespa application declares each document type it stores in a schema
definition file (`.sd`). Every `.sd` file in the repository is read; nothing
else in the application package is.

## At a glance

| You write | enola stores | Kind |
|---|---|---|
| `document items { … }` | a fact named `items`, `storage_kind: document_type`, `engine: vespa` | `storage` |
| `document items_uk inherits items { }` | `items_uk`, with `inherits: [items]` | `storage` |
| a `#` comment | nothing | — |

The fact is named by the document type alone, which is the name a YQL query
reads it by: `select * from items where …` in Go is already a table reference
named `items`, so the query and the schema meet on one node, and a walk from a
route that runs the query ends at the document type.

## What is deliberately not extracted

Fields, rank profiles, `services.xml` and searchers are not read. A query
built by a YQL builder rather than written as a string names its source in a
method call, which no extractor reads as storage yet.
