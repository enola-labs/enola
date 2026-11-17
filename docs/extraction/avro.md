# Avro — schemas as types

Every Avro IDL protocol (`.avdl`) and JSON schema (`.avsc`) is read.

## At a glance

| You write | enola stores | Kind |
|---|---|---|
| `@namespace("events.orders") protocol p { record order_placed { … } }` | a symbol `avro.events.orders.order_placed`, `avro_kind: record` | `symbol` |
| a field typed `Line`, `array<Line>`, `union { null, Channel }` or `events.common.EventMetadata` | a `depends_on` edge to that named type, a bare name resolved in the protocol's namespace first | relation |
| a record | a `names` edge to its bare full name, `events.orders.order_placed` | relation |
| `enum`, `fixed`, `error` | symbols of their own, `avro_kind` saying which | `symbol` |
| a `/** … */` comment above a type (annotations may sit between) | the type's doc | prop |
| `{"type": "record", "name": …, "namespace": …}` in a `.avsc`, nested types included | the same symbols | `symbol` |

The `names` edge is the join to messaging. Avro's record-name subject strategy
names a schema's subject after the record's full name, and a topic that carries
one record type is often named the same; a topic fact of that name, and the
code producing and consuming it, then meet the schema on one node.

Named types are framework roots: a schema is read by producers and consumers
through a registry, not by a call, so none is reported as dead code.

## What is deliberately not extracted

Fields are not symbols, messages and RPCs of a protocol are not read, and
annotation arguments (`@owner({…})`) are ignored, `@namespace` aside. A type
named by no schema in the snapshot draws no edge.
