# Helm: charts, named templates and what they load, read without rendering

A chart's output depends on values given at install time, but its structure does
not. `Chart.yaml` names the chart, its type and its dependencies. A chart's `charts/`
directory holds the subcharts Helm loads with it. `{{ define "x" }}` declares a named
template, and `{{ include "x" . }}` and `{{ template "x" . }}` call it by that literal
name. The `kind:` a template file emits is a literal line in practice. The extractor
reads all of that and renders nothing, the rule the Ansible extractor follows for
Jinja.

Fixture: [`helm_sample`](../../internal/engine/testdata/repos/helm_sample/)

## At a glance

| You write | enola stores | Kind |
|---|---|---|
| a directory holding `Chart.yaml` | one module, keyed by that path | `module` |
| the chart | a symbol `<dir>.Chart.<name>` with `chart_type`, `version`, `app_version` | `symbol` |
| a subchart under `charts/`, a `file://` dependency | `depends_on` from the chart to it | relation |
| a registry dependency the repository holds | `depends_on` to that chart, named in `dependencies_by_name` | relation |
| any other registry dependency | its name in `remote_dependencies`, and a declared package (`pkg:helm/<name>`) | props, `dependency` |
| `{{ define "x" }}` | a symbol `<dir>.template.x`, `helm_kind: named_template` | `symbol` |
| `include "x"`, `template "x"` | `calls` to the named template, from the define or the resource it sits in | relation |
| a document in a template file with a literal `kind:` | a symbol `<dir>.<Kind>.<name>`, `helm_kind: resource` | `symbol` |
| a manifest under `crds/` | a symbol, `helm_kind: crd` | `symbol` |
| an include no loaded chart defines | `unresolved_includes` on the caller | props |
| `.Values`, `tpl`, conditionals | nothing: values are given at install time | none |

## A chart depends on what it loads and what it emits

```yaml
# charts/shop/Chart.yaml
apiVersion: v2
name: shop
version: 1.3.0
dependencies:
  - name: common
    version: 2.x.x
    repository: oci://registry.example.com/charts
  - name: worker
    version: 0.1.0
    repository: file://../worker
  - name: postgresql
    version: 16.x.x
    repository: oci://registry-1.docker.io/bitnamicharts
```

```
symbol  charts/shop.Chart.shop   charts/shop/Chart.yaml:1
        props: chart_type=application, version=1.3.0, app_version=4.2.1,
               dependencies_by_name=[common], remote_dependencies=[postgresql]
        relations: depends_on -> charts/common.Chart.common,
                   depends_on -> charts/shop/charts/cache.Chart.cache,
                   depends_on -> charts/worker.Chart.worker,
                   depends_on -> charts/shop.Deployment.deployment, ...
```

Three ways a chart loads another:

- **A subchart** in the chart's `charts/` directory. Helm loads every one of them,
  listed or not, so `charts/shop/charts/cache` is a dependency without a line in
  `Chart.yaml`.
- **A `file://` dependency**, resolved against the chart's directory.
- **A registry dependency the repository itself holds.** A chart monorepo publishes
  to the registry its charts depend on: bitnami's charts depend on `common` from
  `oci://registry-1.docker.io/bitnamicharts`, and `bitnami/common` sits beside them.
  The dependency resolves to the chart of that name when exactly one chart in the
  repository has it **and its version satisfies the constraint**. In bitnami, 18
  charts depend on `postgresql 16.x.x` while the repository's `postgresql` is
  17.1.2, so those 18 install a different release from the registry and draw no
  edge.

Everything else stays with its registry. The manifests extractor records each
`Chart.yaml` dependency as a declared package in the `helm` ecosystem, resolved
against the `Chart.lock` beside that chart (never a parent's), so the dependency
questions in [INTENT.md](../INTENT.md) cover charts too.

## Named templates are functions, called by literal name

```yaml
# charts/shop/templates/deployment.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ include "common.names.fullname" . }}
  labels: {{- include "common.labels.standard" . | nindent 4 }}
spec:
  template:
    spec:
      serviceAccountName: {{ include "shop.serviceAccountName" . }}
```

```
symbol  charts/shop.Deployment.deployment   charts/shop/templates/deployment.yaml:1
        props: helm_kind=resource, k8s_kind=Deployment, api_version=apps/v1
        relations: calls -> charts/common.template.common.labels.standard,
                   calls -> charts/common.template.common.names.fullname,
                   calls -> charts/shop.template.shop.serviceAccountName
symbol  charts/shop.template.shop.serviceAccountName   charts/shop/templates/_helpers.tpl:1
        relations: calls -> charts/common.template.common.names.fullname
```

Named templates share one namespace across a chart and everything it loads, so a
call resolves in the chart's own templates first and then across the charts it
loads, transitively. It draws an edge only when exactly one of them defines the name.
A call inside a `define` belongs to that define, however many `if`, `range` and
`with` blocks it is nested in. Only text inside `{{ }}` is read for calls: a config
file a template emits that happens to contain `template "x"` is not a call.

A name no loaded chart defines is counted on the caller as `unresolved_includes`, and
it is worth reading. On bitnami/charts every one of the 14 callers carrying it is a
defect in the chart, each inside a branch the default values never take: a typo
(`commot.tplvalues.render`), a template copied from another chart (tomcat's
NetworkPolicy calls `wordpress.databasePort`, pytorch calls `mxnet.*`), and names
defined nowhere (`janusgraph.fullname`, `superset.postgresql.port`).

## Resources are named by their template file

A resource's `metadata.name` is almost always templated, so a resource is named by
its literal `metadata.name` when it has one and otherwise by its path under
`templates/` without the extension: `charts/shop.Deployment.deployment`,
`bitnami/superset.NetworkPolicy.web/networkpolicy`. Several documents of one kind
in one file keep the first name, and the rest take their line. A document whose
`kind:` is templated is not read. Each resource sits on the line its document starts.

## Kustomize inflating a chart

A kustomization's `helmCharts:` entry depends on the chart when the repository holds
it under the chart home (`helmGlobals.chartHome`, `charts` by default); otherwise the
chart's name is listed in `remote_charts`. In the fixture, `deploy.Kustomization`
depends on `charts/shop.Chart.shop`. See [Kubernetes](kubernetes.md).

## What is deliberately not extracted

- **Rendering.** No template is executed, so nothing that depends on values is known:
  which conditional branch is taken, what a templated name expands to, which
  optional dependency a `condition` turns on.
- **References between a chart's resources.** A Service's selector and a pod's
  ConfigMap name are templated in nearly every chart, so the edges the kubernetes
  extractor draws between manifests are not drawn between chart resources.
- **Packaged subcharts** (`charts/*.tgz`) are archives, not directories, and are not
  opened.
- **A parent chart's templates seen from a subchart.** Helm makes them visible at
  install time; a subchart's include resolves only within what it loads itself.
