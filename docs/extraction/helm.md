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
| a key in `values.yaml`, two levels deep | a symbol `<dir>.values.<path>`, `helm_kind: value`, `symbol_kind: variable` | `symbol` |
| a `.Values` read | `names` to the value, from the define, resource or file reading it | relation |
| calls and reads in `NOTES.txt`, a file with no `kind:`, or `values.yaml` | the same edges, from a `file_ref` for the file | `file_ref` |
| conditionals, `tpl` output, the values given at install | nothing: rendering decides them | none |

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

## Values are variables, and a value nothing reads is dead

```yaml
# charts/shop/values.yaml
fullnameOverride: ""
replicaCount: 2
image:
  repository: example/shop
  tag: "4.2.1"
service:
  port: 80
  annotations:
    prometheus.io/scrape: "true"
legacyCheckout: false
postgresql:
  enabled: false
```

```
symbol    charts/shop.values.image.tag   charts/shop/values.yaml:5   props: helm_kind=value
symbol    charts/shop.Deployment.deployment
          relations: names -> charts/shop.values.fullnameOverride,
                     names -> charts/shop.values.image, ...image.repository, ...image.tag,
                     names -> charts/shop.values.replicaCount
file_ref  charts/shop/templates/NOTES.txt
          relations: calls -> charts/common.template.common.names.fullname,
                     names -> charts/shop.values.service, ...service.port,
                     names -> charts/shop.values.fullnameOverride
```

Keys two levels deep become symbols: deep enough to name what a chart exposes
(`image.tag`, `service.port`) without turning every nested default into a node;
bitnami/charts declares 23,804 of them at that depth. `global` and a key naming a
chart this one loads (`postgresql` above, a dependency's alias, a subchart) are
values passed to another chart, not this one's, and are left out.

A read is a `.Values` path in any form a chart writes it: `.Values.a.b`,
`$.Values.a`, a variable holding the root context (`$root.Values.a`), a dict key it
was passed under (`.context.Values.a`, bitnami's idiom), `index .Values "a" "b"`, an
`index` with a variable key (a read of the whole map it indexes), a keyword glued to
it (`with.Values.a`), and a dotted path written as a string for a helper to look up
(`"providedValues" (list "server.masterKey")`), counted only when the chart declares
that path. Template actions spanning several lines are read whole. A read that stops
at a map (`toYaml .Values.service.annotations`, `with .Values.persistence`) reads
every key under it, and a bare `.Values` reads every value: airflow passes it whole to
its security-context helpers (`include "localPodSecurityContext" (list .Values.redis
.Values)`), so each resource doing so names all of airflow's values, 9,644 edges in all.
That errs toward an edge too many, never toward a value reported unused that is read.

Two readers are easy to miss and are not missed. `values.yaml` itself can hold
templates, rendered with `tpl`: bitnami's rabbitmq writes its configuration there,
from `memoryHighWatermark.value`. And a named template of another chart reads the
values of the chart including it: `common.names.fullname` reads shop's
`fullnameOverride`. That read is credited to shop's Deployment, Service and
`NOTES.txt`, the carriers that reach the define, and never to the define itself,
which would draw an edge from the library chart into every chart using it.

Every read of a value is therefore an edge, so a value with none does nothing when
it is set: `find_orphans` reports it at **high** confidence, like an uncalled
function. `legacyCheckout` above is one. On the corpus, the values nothing reads are
2 of 23,804 in bitnami/charts, 7 of 3,189 in prometheus-community/helm-charts and 50 of
317 in kubeflow, whose experimental charts carry keys their templates never use. A
sample of each, checked by hand, held no false positive. Among them is a real bug:
prometheus-modbus-exporter declares `configReloaderSidecar.enabled` and its template
reads `.enable`.

A named template nothing calls is dead the same way, outside a library chart: a
library chart's module is `published`, since its templates are written for charts
elsewhere.

## Kustomize inflating a chart

A kustomization's `helmCharts:` entry depends on the chart when the repository holds
it under the chart home (`helmGlobals.chartHome`, `charts` by default); otherwise the
chart's name is listed in `remote_charts`. In the fixture, `deploy.Kustomization`
depends on `charts/shop.Chart.shop`. See [Kubernetes](kubernetes.md).

## What is deliberately not extracted

- **Rendering.** No template is executed, so nothing that depends on values is known:
  which conditional branch is taken, what a templated name expands to, which
  optional dependency a `condition` turns on.
- **Values deeper than two levels**, and a read through a variable bound to part of
  the values (`$cfg := .Values.config`, then `$cfg.port`), which reads `config` as a
  whole rather than `config.port`.
- **References between a chart's resources.** A Service's selector and a pod's
  ConfigMap name are templated in nearly every chart, so the edges the kubernetes
  extractor draws between manifests are not drawn between chart resources.
- **Packaged subcharts** (`charts/*.tgz`) are archives, not directories, and are not
  opened.
- **A parent chart's templates seen from a subchart.** Helm makes them visible at
  install time; a subchart's include resolves only within what it loads itself.
