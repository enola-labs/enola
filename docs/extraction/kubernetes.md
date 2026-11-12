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

A directory is a module because it is what `kubectl apply -f` takes, the same choice
the Terraform extractor makes. Each symbol sits on the line its document starts on.
A `kind: List` contributes its items. When two objects of one kind and name share a
directory and differ only by namespace, each name ends in `@<namespace>`.

## Names resolve in the directory first, then only when unique

A reference resolves to the one object of that kind and name in the referrer's own
directory. If there is none, it resolves to the one object of that kind and name in
the whole repository. If there are several, or none, it draws nothing. Namespaces must
agree when both sides state one; cluster-scoped kinds (ClusterRole, StorageClass,
IngressClass and the like) match from any namespace.

In the fixture, `deploy/rbac/roles/reader/role.yaml` binds the ServiceAccount `api`,
which is declared once, in `k8s/`:

```
symbol  deploy/rbac/roles/reader.RoleBinding.api-reader
        relations: depends_on -> deploy/rbac/roles/reader.Role.reader,
                   depends_on -> k8s.ServiceAccount.api
```

A repository that keeps a copy of the same manifests per environment declares every
name more than once, so a reference across directories there draws nothing rather
than picking a copy. Kustomize overlays are the usual reason for such copies.

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
(`web-deployment.yaml`, `service.yaml`). Files inside a chart and dot directories do
not count. A manifest in a directory with none of those names, in a repository with
no other marker, is not read; that is a stated blind spot.

## Ingress paths are not routes

`k8s.Ingress.public` carries `ingress_paths: [shop.example.com/, shop.example.com/api]`
and no route fact. An ingress prefix is where traffic enters, not an endpoint, and as
a route it would be matched against every HTTP client in a cluster.

## What is deliberately not extracted

- **Rendering and substitution.** No Helm template, kustomize patch or `envsubst`
  placeholder is evaluated. A templated name draws nothing.
- **What a cluster adds at apply time.** Admission webhooks, defaulted fields,
  controllers creating objects (a Deployment's ReplicaSets, an operator's children)
  are runtime, not architecture.
- **Ambiguous names.** A name declared more than once outside the referrer's
  directory resolves to nothing, rather than to a guess.
- **Ingress paths as routes**, for the reason above.
- **Manifests outside the detection names**, in a repository with no other marker.
