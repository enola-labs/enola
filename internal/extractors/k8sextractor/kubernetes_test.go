package k8sextractor

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func extract(t *testing.T, files map[string]string) []facts.Fact {
	t.Helper()
	dir := t.TempDir()
	for f, c := range files {
		p := filepath.Join(dir, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ff, err := New().Extract(context.Background(), dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	return ff
}

func find(ff []facts.Fact, kind, name string) *facts.Fact {
	for i := range ff {
		if ff[i].Kind == kind && ff[i].Name == name {
			return &ff[i]
		}
	}
	return nil
}

func dependsOn(f *facts.Fact) []string {
	var out []string
	for _, r := range f.Relations {
		if r.Kind == facts.RelDependsOn {
			out = append(out, r.Target)
		}
	}
	return out
}

const app = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
spec:
  selector:
    matchLabels: {app: web}
  template:
    metadata:
      labels: {app: web, tier: front}
    spec:
      serviceAccountName: web-sa
      imagePullSecrets: [{name: regcred}]
      initContainers:
        - name: migrate
          image: example/migrate:1.0
      containers:
        - name: web
          image: example/web:1.2
          envFrom:
            - configMapRef: {name: web-config}
          env:
            - name: DB_PASSWORD
              valueFrom:
                secretKeyRef: {name: db-secret, key: password}
            - name: TEMPLATED
              valueFrom:
                configMapKeyRef: {name: "${CM}", key: x}
      volumes:
        - name: data
          persistentVolumeClaim: {claimName: web-data}
        - name: certs
          secret: {secretName: tls-certs}
---
apiVersion: v1
kind: Service
metadata:
  name: web
spec:
  selector: {app: web}
  ports: [{port: 80}]
---
apiVersion: v1
kind: ConfigMap
metadata: {name: web-config}
---
apiVersion: v1
kind: Secret
metadata: {name: db-secret}
---
apiVersion: v1
kind: Secret
metadata: {name: tls-certs}
---
apiVersion: v1
kind: Secret
metadata: {name: regcred}
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: web-data}
spec: {storageClassName: fast}
---
apiVersion: v1
kind: ServiceAccount
metadata: {name: web-sa}
`

func TestManifestObjectsAndTheNamesTheyUse(t *testing.T) {
	ff := extract(t, map[string]string{"deploy/app.yaml": app})

	if find(ff, facts.KindModule, "deploy") == nil {
		t.Fatal("the directory is not a module")
	}
	d := find(ff, facts.KindSymbol, "deploy.Deployment.web")
	if d == nil {
		t.Fatal("Deployment missing")
	}
	want := []string{
		"deploy.ConfigMap.web-config", "deploy.PersistentVolumeClaim.web-data",
		"deploy.Secret.db-secret", "deploy.Secret.regcred", "deploy.Secret.tls-certs",
		"deploy.ServiceAccount.web-sa",
	}
	if got := dependsOn(d); !reflect.DeepEqual(got, want) {
		t.Errorf("Deployment depends_on = %v, want %v", got, want)
	}
	if got := d.Props["images"]; !reflect.DeepEqual(got, []string{"example/migrate:1.0", "example/web:1.2"}) {
		t.Errorf("images = %v", got)
	}
	if d.Line != 1 || d.Props["k8s_kind"] != "Deployment" || d.Props["api_version"] != "apps/v1" {
		t.Errorf("Deployment fact = %+v", d)
	}
	svc := find(ff, facts.KindSymbol, "deploy.Service.web")
	if svc == nil || !reflect.DeepEqual(dependsOn(svc), []string{"deploy.Deployment.web"}) {
		t.Errorf("Service = %+v, want it to select the Deployment by label", svc)
	}
	if svc.Line != 35 {
		t.Errorf("Service line = %d, want the line its document starts on", svc.Line)
	}
	// The StorageClass is not declared here, so the claim names nothing.
	if pvc := find(ff, facts.KindSymbol, "deploy.PersistentVolumeClaim.web-data"); len(dependsOn(pvc)) != 0 {
		t.Errorf("claim drew %v to an undeclared StorageClass", dependsOn(pvc))
	}
}

func TestIngressGatewayRBACAndScalers(t *testing.T) {
	ff := extract(t, map[string]string{
		"k8s/svc.yaml": `apiVersion: v1
kind: Service
metadata: {name: api}
spec: {selector: {app: api}}
---
apiVersion: v1
kind: Service
metadata: {name: legacy}
spec: {type: ExternalName, externalName: legacy.example.com, selector: {app: api}}
`,
		"k8s/workload.yaml": `apiVersion: apps/v1
kind: StatefulSet
metadata: {name: api}
spec:
  template:
    metadata: {labels: {app: api}}
    spec: {containers: [{name: api, image: api:1}]}
---
apiVersion: batch/v1
kind: CronJob
metadata: {name: nightly}
spec:
  jobTemplate:
    spec:
      template:
        metadata: {labels: {app: nightly}}
        spec:
          serviceAccountName: api
          containers: [{name: n, image: nightly:1}]
`,
		"k8s/edge.yaml": `apiVersion: networking.k8s.io/v1
kind: Ingress
metadata: {name: api}
spec:
  ingressClassName: nginx
  defaultBackend: {service: {name: legacy, port: {number: 80}}}
  rules:
    - host: api.example.com
      http:
        paths:
          - path: /v1
            backend: {service: {name: api, port: {number: 80}}}
          - path: /old
            backend: {serviceName: legacy, servicePort: 80}
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata: {name: api}
spec:
  parentRefs: [{name: public}]
  rules:
    - backendRefs: [{name: api, port: 80}]
---
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata: {name: public}
spec: {gatewayClassName: istio}
---
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata: {name: api}
spec: {scaleTargetRef: {apiVersion: apps/v1, kind: StatefulSet, name: api}}
---
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata: {name: api}
spec: {selector: {matchLabels: {app: api}}}
`,
		"k8s/rbac.yaml": `apiVersion: v1
kind: ServiceAccount
metadata: {name: api, namespace: prod}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: reader}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: api-reader, namespace: prod}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: reader}
subjects:
  - {kind: ServiceAccount, name: api, namespace: prod}
  - {kind: User, name: alice}
`,
	})
	cases := map[string][]string{
		"k8s.Ingress.api":                 {"k8s.Service.api", "k8s.Service.legacy"},
		"k8s.HTTPRoute.api":               {"k8s.Gateway.public", "k8s.Service.api"},
		"k8s.HorizontalPodAutoscaler.api": {"k8s.StatefulSet.api"},
		"k8s.PodDisruptionBudget.api":     {"k8s.StatefulSet.api"},
		"k8s.RoleBinding.api-reader":      {"k8s.ClusterRole.reader", "k8s.ServiceAccount.api"},
		"k8s.CronJob.nightly":             {"k8s.ServiceAccount.api"},
		"k8s.Service.api":                 {"k8s.StatefulSet.api"},
		// An ExternalName Service selects nothing, whatever its selector says.
		"k8s.Service.legacy": nil,
	}
	for name, want := range cases {
		f := find(ff, facts.KindSymbol, name)
		if f == nil {
			t.Errorf("%s missing", name)
			continue
		}
		if got := dependsOn(f); !reflect.DeepEqual(got, want) {
			t.Errorf("%s depends_on = %v, want %v", name, got, want)
		}
	}
	ing := find(ff, facts.KindSymbol, "k8s.Ingress.api")
	if got := ing.Props["ingress_paths"]; !reflect.DeepEqual(got, []string{"api.example.com/old", "api.example.com/v1"}) {
		t.Errorf("ingress_paths = %v", got)
	}
	for _, f := range ff {
		if f.Kind == facts.KindRoute {
			t.Errorf("an ingress path became a route: %s", f.Name)
		}
	}
}

// A name declared in the referrer's directory wins; failing that, a name declared
// once in the repository; a name declared twice elsewhere draws nothing.
func TestReferencesResolveInScope(t *testing.T) {
	cm := func(name string) string {
		return "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: " + name + "}\n"
	}
	dep := func(cm string) string {
		return `apiVersion: apps/v1
kind: Deployment
metadata: {name: app}
spec:
  template:
    spec:
      containers: [{name: a, image: a, envFrom: [{configMapRef: {name: ` + cm + `}}]}]
`
	}
	ff := extract(t, map[string]string{
		"a/dep.yaml": dep("shared"),
		"a/cm.yaml":  cm("shared"),
		"b/cm.yaml":  cm("shared"),
		"c/dep.yaml": dep("shared"),
		"d/dep.yaml": dep("once"),
		"e/cm.yaml":  cm("once"),
		"f/dep.yaml": dep("nowhere"),
	})
	cases := map[string][]string{
		"a.Deployment.app": {"a.ConfigMap.shared"},
		"c.Deployment.app": nil,
		"d.Deployment.app": {"e.ConfigMap.once"},
		"f.Deployment.app": nil,
	}
	for name, want := range cases {
		if got := dependsOn(find(ff, facts.KindSymbol, name)); !reflect.DeepEqual(got, want) {
			t.Errorf("%s depends_on = %v, want %v", name, got, want)
		}
	}
}

func TestNamespacesMustAgree(t *testing.T) {
	ff := extract(t, map[string]string{"x/all.yaml": `apiVersion: v1
kind: ConfigMap
metadata: {name: cfg, namespace: staging}
---
apiVersion: v1
kind: ConfigMap
metadata: {name: cfg, namespace: prod}
---
apiVersion: v1
kind: Pod
metadata: {name: p, namespace: prod}
spec:
  containers: [{name: c, image: c, envFrom: [{configMapRef: {name: cfg}}]}]
`})
	p := find(ff, facts.KindSymbol, "x.Pod.p")
	if got := dependsOn(p); !reflect.DeepEqual(got, []string{"x.ConfigMap.cfg@prod"}) {
		t.Errorf("Pod depends_on = %v, want the prod ConfigMap only", got)
	}
}

func TestWhatIsNotAnObject(t *testing.T) {
	ff := extract(t, map[string]string{
		// A tool's own config with apiVersion and kind.
		"skaffold.yaml":             "apiVersion: skaffold/v4beta6\nkind: Config\nmetadata: {name: app}\n",
		"deploy/kustomization.yaml": "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: [a.yaml]\n",
		"deploy/kubeconfig.yaml":    "apiVersion: v1\nkind: Config\ncurrent-context: x\n",
		// A templated name is not a name.
		"deploy/tpl.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: '{{ .Release.Name }}'}\n",
		// A chart's tree is the helm extractor's, even its plain-YAML parts.
		"charts/app/Chart.yaml":         "apiVersion: v2\nname: app\n",
		"charts/app/crds/crd.yaml":      "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata: {name: foos.example.com}\n",
		"testdata/pod.yaml":             "apiVersion: v1\nkind: Pod\nmetadata: {name: fixture}\n",
		".github/workflows/deploy.yaml": "on: push\n",
		"deploy/broken.yaml":            "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: ok}\n---\nkind: [\n---\napiVersion: v1\nkind: ConfigMap\nmetadata: {name: after}\n",
		"deploy/list.yaml": `apiVersion: v1
kind: List
items:
  - apiVersion: v1
    kind: ConfigMap
    metadata: {name: from-list}
`,
	})
	var names []string
	for _, f := range ff {
		if f.Kind == facts.KindSymbol {
			names = append(names, f.Name)
		}
	}
	// The kustomization is a symbol of its own, never an object (kustomize_test.go).
	want := []string{"deploy.Kustomization", "deploy.ConfigMap.ok", "deploy.ConfigMap.from-list"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("symbols = %v, want %v", names, want)
	}
}

func TestDetectFilesByName(t *testing.T) {
	yes := [][]string{
		{"overlays/prod/kustomization.yaml"},
		{"kubernetes-manifests/frontend.yaml"},
		{"ops/web-deployment.yaml"},
		{"config/service.yaml"},
		{"hack/test-resources/crashing-pod.yaml"},
	}
	no := [][]string{
		{"config/settings.yaml", ".github/workflows/ci.yml"},
		{"charts/app/Chart.yaml", "charts/app/templates/deployment.yaml"},
		{".deploy/x.yaml"},
		{"deploy/README.md"},
	}
	for _, files := range yes {
		if ok, _ := New().DetectFiles("", files); !ok {
			t.Errorf("DetectFiles(%v) = false, want true", files)
		}
	}
	for _, files := range no {
		if ok, _ := New().DetectFiles("", files); ok {
			t.Errorf("DetectFiles(%v) = true, want false", files)
		}
	}
}

func TestOneObjectPerClusterFileIsOneSymbolDeclaredInEach(t *testing.T) {
	const sa = "apiVersion: v1\nkind: ServiceAccount\nmetadata:\n  name: web\n  namespace: shop\n"
	const rb = `apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: web-exec, namespace: shop}
roleRef: {kind: ClusterRole, name: exec}
subjects: [{kind: ServiceAccount, name: web}]
`
	ff := extract(t, map[string]string{
		"shop/us1-serviceaccount.yaml": sa,
		"shop/eu1-serviceaccount.yaml": sa,
		"shop/eu1-rolebinding.yaml":    rb,
	})
	got := find(ff, "symbol", "shop.ServiceAccount.web")
	if got == nil {
		t.Fatal("no ServiceAccount symbol")
	}
	if want := []string{"shop/eu1-serviceaccount.yaml", "shop/us1-serviceaccount.yaml"}; !reflect.DeepEqual(got.Props["declared_in"], want) {
		t.Errorf("declared_in = %v, want %v", got.Props["declared_in"], want)
	}
	if got.File != "shop/eu1-serviceaccount.yaml" {
		t.Errorf("file = %s, want the first", got.File)
	}
	// The binding still resolves: one symbol, not one per cluster.
	if deps := dependsOn(find(ff, "symbol", "shop.RoleBinding.web-exec")); !reflect.DeepEqual(deps, []string{"shop.ServiceAccount.web"}) {
		t.Errorf("binding depends_on = %v", deps)
	}
	if find(ff, "symbol", "shop.RoleBinding.web-exec").Props["declared_in"] != nil {
		t.Error("an object declared once has no declared_in")
	}
}
