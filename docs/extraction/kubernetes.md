# Kubernetes: objects joined by the names they use

A manifest's structure is by name and by label, and both are literal. A Service
selects the pods whose labels it names, an Ingress names the Services it sends to, a
Deployment names the ConfigMaps, Secrets, claims and service account its pods use, a
RoleBinding names its Role. The extractor reads those names and resolves them against
the objects the repository declares. Nothing is rendered or substituted.

Fixture: [`k8s_sample`](../../internal/engine/testdata/repos/k8s_sample/)

## At a glance

| You write | enola stores | Kind |
|---|---|---|
| a directory holding manifests | one module, keyed by that path | `module` |
| a document with `apiVersion`, `kind` and a literal `metadata.name` | a symbol named `<dir>.<Kind>.<name>`, with `k8s_kind`, `api_version`, `namespace` | `symbol` |
| a pod template's container images | `images` on the workload, sorted | props |
| a Service's `selector` | `depends_on` to every workload whose pod labels carry it | relation |
| an Ingress backend, `ingressClassName` | `depends_on` to the Service, the IngressClass | relation |
| an HTTPRoute (or GRPC, TCP, TLS, UDP route) | `depends_on` to its `parentRefs` and `backendRefs` | relation |
| `envFrom`, `valueFrom`, `volumes`, `imagePullSecrets`, `serviceAccountName` | `depends_on` from the workload to each ConfigMap, Secret, claim and ServiceAccount | relation |
| a RoleBinding or ClusterRoleBinding | `depends_on` to its `roleRef` and ServiceAccount subjects | relation |
| an HPA, VPA or PodDisruptionBudget | `depends_on` to the workload it scales or protects | relation |
| an Ingress's hosts and paths | `ingress_paths` on the Ingress, never a route | props |
| an env value or ConfigMap entry that is a Service's address | `depends_on` to that Service | relation |
| an image `skaffold.yaml` builds | `build_contexts` on the workload, and an `imports` dependency from its directory to the code modules there | props, `dependency` |
| a `kustomization.yaml` | a symbol `<dir>.Kustomization` (or `.Component`) | `symbol` |
| its `resources`, `bases`, `components` | `depends_on` to the objects in the files it lists and to the kustomizations it includes | relation |
| its `patches` | `depends_on` to each object a patch targets; the patch file is not an object | relation |
| `configMapGenerator`, `secretGenerator` | an object like any other, with `generator` naming the field, on the line naming it | `symbol` |
| `namespace`, `namePrefix`, `nameSuffix`, `images`, remote resources | props on the kustomization, applied to no object | props |
| an Argo CD Application | `destination_cluster`, `destination_namespace`, `argocd_project`, `source_repo`, `source_path`, `target_revision`, `chart`, `chart_version`, `source_plugin`, `release_name` | props |
| its `project`, its source path in this repository | `depends_on` to the AppProject, and to the kustomization or the objects in that path | relation |
| the values files beside an Application whose chart is remote | `value_files`, `images`, `replicas`, `min_replicas`, `max_replicas`, `resources`, `env`, `addresses` on the Application | props |
| an Application's served names and environment | `serves`, `environment` | props |
| an address in those values | `depends_on` to the Application serving it, joined across repositories by the release-addresses binder | relation |
| the same object in several files of one directory | one symbol, `declared_in` listing the files | props |
| the loaded repository whose code an Application runs | `deploys_repo` on it, and a service edge via `deployment` | props, `dependency` |
| `{{ .Values.x }}`, `${VAR}` | nothing: a template is not a name | none |

## Objects become symbols in their directory's module

```yaml
# k8s/api.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
spec:
  template:
    metadata:
      labels:
        app: api
    spec:
      serviceAccountName: api
      containers:
        - name: api
          image: example/api:1.4
          envFrom:
            - configMapRef:
                name: api-config
          env:
            - name: DB_PASSWORD
              valueFrom:
                secretKeyRef:
                  name: db
                  key: password
---
apiVersion: v1
kind: Service
metadata:
  name: api
spec:
  selector:
    app: api
```

```
module  k8s
symbol  k8s.Deployment.api   k8s/api.yaml:1   props: k8s_kind=Deployment, images=[example/api:1.4]
        relations: depends_on -> k8s.ConfigMap.api-config,
                   depends_on -> k8s.Secret.db,
                   depends_on -> k8s.ServiceAccount.api
symbol  k8s.Service.api      k8s/api.yaml:28  props: k8s_kind=Service
        relations: depends_on -> k8s.Deployment.api
```

A GitOps repository often writes one copy of an object per cluster in one directory
(`eu1-serviceaccount.yaml`, `us1-serviceaccount.yaml`, the same ServiceAccount in
each). That is one symbol, on the first file, with `declared_in` listing every file
declaring it, so which clusters have it stays answerable while a reference to it still
resolves to one object.

A directory is a module because it is what `kubectl apply -f` takes, the same choice
the Terraform extractor makes. Each symbol sits on the line its document starts on.
A `kind: List` contributes its items. When two objects of one kind and name share a
directory and differ only by namespace, each name ends in `@<namespace>`.

## Names resolve in the directory, then in each build, then only when unique

A reference resolves to the one object of that kind and name in the referrer's own
directory. If there is none, every kustomize build holding the referrer answers on
its own: a build that emits exactly one object of that kind and name contributes an
edge to it. If no build emits one, the reference resolves to the one object of that
kind and name in the whole repository. If there are several, or none, it draws
nothing. Namespaces must
agree when both sides state one; cluster-scoped kinds (ClusterRole, StorageClass,
IngressClass and the like) match from any namespace.

In the fixture, `deploy/rbac/roles/reader/role.yaml` binds the ServiceAccount `api`,
which is declared once, in `k8s/`:

```
symbol  deploy/rbac/roles/reader.RoleBinding.api-reader
        relations: depends_on -> deploy/rbac/roles/reader.Role.reader,
                   depends_on -> k8s.ServiceAccount.api
```

A repository that keeps a copy of the same manifests per environment, with no
kustomization joining them, declares every name more than once, so a reference across
directories there draws nothing rather than picking a copy.

## A kustomization is a build

```yaml
# overlays/prod/kustomization.yaml
namespace: shop
namePrefix: prod-
resources:
  - ../../k8s
  - https://github.com/example/observability//collector?ref=v2.1.0
images:
  - name: example/web
    newTag: "2.1"
configMapGenerator:
  - name: web-flags
    literals:
      - CHECKOUT_V2=true
patches:
  - path: web-replicas.yaml
```

```
symbol  overlays/prod.Kustomization   overlays/prod/kustomization.yaml:1
        props: namespace=shop, name_prefix=prod-, patches=1,
               image_overrides=[example/web=example/web:2.1],
               remote_resources=[https://github.com/example/observability//collector?ref=v2.1.0]
        relations: depends_on -> k8s.Deployment.web,
                   depends_on -> k8s.Kustomization,
                   depends_on -> overlays/prod.ConfigMap.web-flags
symbol  overlays/prod.ConfigMap.web-flags   overlays/prod/kustomization.yaml:12
        props: generator=configMapGenerator, namespace=shop
```

`web-replicas.yaml` is a partial Deployment, so it is not an object; it becomes the
edge to `k8s.Deployment.web`, the object it patches in this build. Patches are read in
all three forms: a path, a `target` with an inline JSON 6902 patch, and an inline
strategic-merge patch whose `kind` and `metadata.name` name its target.

The prefix, the namespace and the image override are recorded and applied to nothing.
One base serves every overlay that includes it, and each overlay renames it
differently; the base's objects keep the names their files state, which are also the
names every reference in the base uses.

A base can mean a different object in each build that includes it. In kubeflow's
katib, the `db-manager` component names the Secret `katib-mysql-secrets`: one install
generates it, another ships a manifest for it. Each build emits exactly one, so the
Deployment carries one edge per build. A remote resource is counted, never fetched.

## What is not an object

A document counts only when its `apiVersion` is one Kubernetes serves (`v1`, a legacy
group such as `apps/v1`, or a dotted group such as `networking.k8s.io/v1`) and it has
a kind and a literal `metadata.name`. That keeps out a tool's own configuration that
happens to carry `apiVersion` and `kind`: `skaffold.yaml`, a kubeconfig, a kind
cluster config, a `kustomization.yaml`. A document that fails to parse ends its file.

The extractor reads YAML itself, because YAML is ignore-globbed by default, and skips
dot directories, `testdata`, `vendor`, `node_modules` and every Helm chart's tree.

## Detection is by file name

The set of extractors a repository is detected for is part of its snapshot, so
detection must not flip on every YAML file a repository carries. It reads names only:
a `kustomization.yaml`, or a YAML file under a directory such as `k8s/`,
`kubernetes/`, `manifests/`, `deploy/` or `overlays/`, or one whose name is a kind
(`web-deployment.yaml`, `service.yaml`, `crashing-pod.yaml`). Files inside a chart and dot directories do
not count. A manifest in a directory with none of those names, in a repository with
no other marker, is not read; that is a stated blind spot.

## Ingress paths are not routes

`k8s.Ingress.public` carries `ingress_paths: [shop.example.com/, shop.example.com/api]`
and no route fact. An ingress prefix is where traffic enters, not an endpoint, and as
a route it would be matched against every HTTP client in a cluster.

## Workloads find each other by address

Services in a cluster are told where their peers are through literal values:
microservices-demo's checkout service gets `CART_SERVICE_ADDR=cartservice:7070`. An env
value, or an entry of a ConfigMap a pod reads, that is an address of a Service the
repository declares is a dependency on that Service:

```yaml
# k8s/web.yaml (excerpt)
env:
  - name: API_URL
    value: http://api:80
  - name: FEATURE
    value: api
```

```
symbol  k8s.Deployment.web
        relations: depends_on -> k8s.Service.api
```

Env values are arbitrary text and Services are often named after common words, so an
address is narrow: a URL, a `host:port` pair, a gRPC `dns:///host:port` target, or a
cluster name ending in `.svc` or `.svc.cluster.local`. `FEATURE=api` above names
nothing. A second label is read as the namespace only before `.svc`
(`cart.shop.svc.cluster.local`), so `cart.example.com` is never the cart Service.
When the address states a port and the Service declares ports, the port must be one
of them. The name then resolves by the rules above.

On microservices-demo this is the application's whole service graph, and it matches
the project's architecture diagram edge for edge: the frontend reaches eight
services, checkout six, cart reaches redis, recommendation the product catalog, the
load generator the frontend. On the other corpus repositories it adds six edges, each
checked by hand: gateways reaching `istiod.istio-system.svc:15012`, a cache-warming
CronJob calling `http://frontend`, a user service reaching `user-db:27017`.

## A workload runs the code its image is built from

`skaffold.yaml` states which directory each image is built from. A workload running
one of those images (by name, or by the image's last path segment when the artifact
is named without a registry, which is how skaffold's default repository rewrites it)
carries `build_contexts`, and a binder joins each directory to the modules the other
extractors found in it:

```
symbol      k8s.Deployment.api   props: images=[example/api:1.4], build_contexts=[src/api]
dependency  k8s -> example.com/shop/src/api   props: source=build_context
            relations: imports -> example.com/shop/src/api
```

The edge sits at the module layer because a manifest deploys a program, not one of its
functions. It is drawn by a binder, after every extractor, because the module's name
is the other extractor's to decide: a Go package is named by its import path, a C#
project by its directory. Change microservices-demo's `src/checkoutservice` and impact
analysis reaches `kubernetes-manifests`, `kustomize/base` and `release`, the three
places that deploy it.

## An Argo CD Application is a release

In a GitOps repository most objects are Applications, and an Application's name says
little. What it states is the release: the cluster and namespace it deploys to, the
project governing it, and where its manifests come from. Those become props.

```yaml
# apps/web/eu1-application.yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: web-eu1
  namespace: argocd
spec:
  destination: {name: eu1, namespace: shop}
  project: shop
  source:
    repoURL: https://git.example.com/acme/shop-deploy.git
    path: apps/web/
    plugin:
      name: chart-resolver
      env:
        - {name: CHART_NAME, value: base-service}
        - {name: CHART_VERSION, value: 1.42.0}
```

```
symbol  apps/web.Application.web-eu1
        props: destination_cluster=eu1, destination_namespace=shop, argocd_project=shop,
               source_repo=https://git.example.com/acme/shop-deploy.git,
               source_path=apps/web, chart=base-service, chart_version=1.42.0,
               source_plugin=chart-resolver
        relations: depends_on -> apps.AppProject.shop
```

A Helm source states its chart as `chart` and pins it with `targetRevision`. A
config-management plugin that resolves the chart itself is handed it in its env or
parameters; `CHART_NAME`/`CHART` and `CHART_VERSION`, with or without a `HELM_`
prefix, are read as the chart and its version. A multi-source Application
(`spec.sources`) takes each field from the first source stating it.

The props answer fleet questions directly: every release on a chart older than a
version is `query_facts(kind=symbol, prop=chart_version, prop_op=lt,
prop_value=0.150)`, compared as a version, with `prop=chart` to narrow to one chart
in a second query.

The source path is this repository's when the directory exists and the source
repository, if stated, has this repository's name: the checkout's directory, or the
origin remote's. A plain path source then depends on what that path applies: its
kustomization, or else every object in it. A plugin or chart source renders the
release instead, so the objects beside it, often its siblings for other clusters,
are not part of it.

## A remote chart's values are the release's configuration

A GitOps repository rarely holds the chart it deploys: the Application names a chart
in a registry and keeps its configuration in values files beside it. Those files say
what runs, so when an Application's source path is in this repository and holds no
chart and no kustomization, its values files are read into props on the
Application.

The files are the ones `helm.valueFiles` lists, relative to the source path (a
`$ref/` file or a URL belongs to another source and is skipped). A plugin source lists
none and reads its path by its own convention, so then every YAML file there that
holds no object and has `values` in its name is read. A file named for one of the
clusters the Applications sharing the path deploy to belongs to that cluster's
Application alone: with `web-eu1` and `web-us1` both pointing at `apps/web/`,
`eu1-values.yaml` is `web-eu1`'s, and `values.yaml` is both. Files merge in order,
later keys winning: shared before a cluster's own, and a file named `override` last.

```yaml
# apps/web/values.yaml
image: {repository: acme/web, tag: "1.0"}
resources:
  requests: {cpu: 100m, memory: 128Mi}
# apps/web/eu1-values.yaml
replicaCount: 6
autoscaling: {minReplicas: 6, maxReplicas: 20}
environment:
  CART_URL: http://cart-api.cart.svc:8080
# apps/web/eu1-values.override.yaml
image: {tag: "1.1"}
```

```
symbol  apps/web.Application.web-eu1
        props: value_files=[apps/web/values.yaml, apps/web/eu1-values.yaml,
                            apps/web/eu1-values.override.yaml],
               images=[acme/web:1.1], replicas=6, min_replicas=6, max_replicas=20,
               resources=[requests.cpu=100m, requests.memory=128Mi],
               env=[CART_URL=http://cart-api.cart.svc:8080], addresses=[cart-api.cart:8080]
```

What is read follows the keys `helm create` writes, which most charts keep: `image`
anywhere in the tree (a string, or `repository` and `tag`), top-level `replicaCount`,
`autoscaling.minReplicas` and `maxReplicas`, and `resources.requests` and `limits`.
`env` is every UPPER_SNAKE key holding a scalar, wherever the chart keeps it, and
every name/value pair in a list named `env` or `extraEnv`; a value is cut at 200
characters. `addresses` are the in-cluster addresses in any string, by the rules a
pod's env follows, recorded only when they name a namespace: a bare `host:port` in a
values file is as often a sidecar or a local port as a peer.

Some plugins also layer shared values under an Application's own, from a directory
named for values beside an ancestor of its path, choosing files by name:

```
shop/_layered_values/01-global.yaml           every Application
shop/_layered_values/20-production.yaml       the production environment's
shop/_layered_values/30-eu1.yaml              region eu1's
shop/_layered_values/40-eu1prod1.yaml         cluster eu1prod1's
shop/_layered_values/50-web.yaml              the web workload's
shop/_layered_values/60-web-production.yaml   web's in production
shop/live/web/eu1prod1-application.yaml
```

A layered file named `global`, `common`, `default(s)`, `all`, `base` or `shared`
applies to every Application below. Any other applies when its name, without the
leading number, splits wholly into names the Application answers to: its cluster, its
region (the cluster name's leading letters and digits), its workload (the source
path's last directory, or its path below the values directory's parent joined by
dashes, `live-web`), its label values, its project and each dash-separated part of
it, its namespace, its own name and its release name. A part none of those accounts
for excludes the file: `50-web.yaml` is not `web-admin`'s, `60-web-sandbox.yaml` is
not production's. The nearest ancestor holding a values directory answers; its files
merge in name order, before the Application's own.

## An address in a release names the Application serving it

The chart renders the Service a values address names, so the repository holds no
Service object to resolve it to. The Application deploying that Service is what
answers, and it often lives in another repository than the caller: a platform's
GitOps repository beside one generated per service. The extractor reads one
repository at a time, so it states both sides, and the `release-addresses` binder
joins them once every repository is read.

On the serving side, `serves` lists the names an Application deploys a Service
under: its release name, its own name and that name without its cluster appended
(`cart-api-eu1` serves `cart-api`), and its `app`, `app.kubernetes.io/name` and
`app.kubernetes.io/instance` labels. `environment` is its `environment` or `env`
label, else the one environment word among its project name's parts
(`shop-production`); production/prod, sandbox, staging/stage, development/dev, test
and qa are spelled one way each.

`http://cart-api.cart.svc:8080` then names the Applications deploying to namespace
`cart` that serve `cart-api`:

```
symbol  apps/web.Application.web-eu1      (destination eu1, env CART_URL=http://cart-api.cart.svc:8080)
        relations: depends_on -> apps/cart.Application.cart-api-eu1
dependency  module-edge: apps/web -> apps/cart
```

A cluster name resolves inside the caller's cluster, so the Applications in the
caller's own cluster answer. Failing that, a mesh spanning clusters routes to a
neighbour: those in clusters of the caller's region (the cluster name's leading
letters and digits) and environment answer, so a monolith on its own cluster reaches
the service on the application cluster beside it. Failing that, the one candidate
there is, and never a pick among several: across clusters, several are usually
production's and sandbox's. A port is not checked, since the chart decides the
Service's ports. Only addresses naming a namespace are joined. A Service the
repository declares still answers by the usual rules, in the extractor.

Edges within one repository roll up to module edges between release directories.
On two GitOps repositories holding 13,000 Applications the binder draws 34,400
edges, 14,500 of them from one repository into the other.

## A release runs the code of another repository

A GitOps repository deploys code that lives elsewhere, and only once both are loaded
can the two meet. The `deployed-code` binder sets `deploys_repo` on an Application
when its release runs a loaded repository's code: an image path segment below the
registry host that names the repository (`registry.example.com/shop-web/app:1.2`
runs `shop-web`), else the destination namespace, else a name it serves. When its
images name several repositories, the namespace must name one of them, or it names
none; its own repository never answers. Names compare as cross-repo linking compares
them, case, `-` and `_` folded.

The `deployment` cross-repo signal turns those props into service edges, via
`deployment`, from the repository holding the Applications to the one whose code they
run, with the Applications as `deployment_samples`:

```
dependency  shop-deploy -> shop-web   props: via=[deployment], deployment_count=14
```

It reports no coverage: most releases in a GitOps repository deploy software no
loaded repository holds, and that is not a blind spot.

## What is deliberately not extracted

- **Rendering and substitution.** No Helm template or `envsubst` placeholder is
  evaluated, and no kustomize build is run: prefixes, suffixes, namespaces,
  `replacements` and `vars` are not applied. A templated name draws nothing.
- **Remote kustomize resources** are listed on the kustomization, never fetched.
- **Kustomizations inside a Helm chart's tree** belong to the chart and are not read.
- **What a cluster adds at apply time.** Admission webhooks, defaulted fields,
  controllers creating objects (a Deployment's ReplicaSets, an operator's children)
  are runtime, not architecture.
- **Ambiguous names.** A name declared more than once outside the referrer's
  directory resolves to nothing, rather than to a guess.
- **Ingress paths as routes**, for the reason above.
- **Addresses that are not literal** (`$(CART_HOST):7070`, a value from a Secret), a
  bare service name, and a host outside the cluster's service names.
- **Image builds other than skaffold's.** A Dockerfile path, a Makefile or a CI
  pipeline that builds an image states its directory in a form this does not read.
- **Manifests outside the detection names**, in a repository with no other marker.
