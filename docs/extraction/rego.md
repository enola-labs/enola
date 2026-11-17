# Rego — policies as rules that reference rules

Every Open Policy Agent policy (`.rego`) is read, test files (`*_test.rego`)
excepted.

## At a glance

| You write | enola stores | Kind |
|---|---|---|
| `package orders.sandbox` … `allow if { … }` | a symbol `orders.sandbox.allow`, a framework root | `symbol` |
| several definitions of one rule (`default allow := false`, `allow if {…}`) | one symbol, at the first, carrying every body's references | `symbol` |
| `has_permission(…)` in a body, a rule of the same package | a `depends_on` edge to `orders.sandbox.has_permission` | relation |
| `import data.shared` … `shared.token.verified` | a `depends_on` edge to `shared.token.verified` | relation |
| `data.shared.token.verified` | the same edge | relation |

A path resolves on the longest package any policy declares that it starts with,
then the rule after it when that package defines one; a path into a data
document rather than a rule names nothing.

Rules are framework roots: OPA evaluates them by path from outside the code, so
none is reported as dead code.

## What is deliberately not extracted

Which route or service a policy guards is not inferred, comprehensions and
`with` overrides are not read, and `input` is never a reference.
