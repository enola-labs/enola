package k8sextractor

import (
	"reflect"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func releaseApp(name, cluster, path string, extra string) string {
	return `apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: ` + name + `
  namespace: argocd
spec:
  destination: {name: ` + cluster + `, namespace: storefront}
  source:
    path: ` + path + `
` + extra
}

const plugin = `    plugin:
      name: chart-resolver
      env:
        - {name: CHART_NAME, value: base-service}
`

func strs(f *facts.Fact, k string) []string {
	v, _ := f.Props[k].([]string)
	return v
}

func TestValuesFilesOfARemoteChartAreTheRelease(t *testing.T) {
	ff := extract(t, map[string]string{
		"storefront/web/eu1-application.yaml": releaseApp("web-eu1", "eu1", "storefront/web", plugin),
		"storefront/web/us1-application.yaml": releaseApp("web-us1", "us1", "storefront/web", plugin),
		"storefront/web/values.yaml": `image:
  repository: acme/web
  tag: "1.0"
replicaCount: 2
resources:
  requests: {cpu: 100m, memory: 128Mi}
  limits: {cpu: 1, memory: 512Mi}
environment:
  LOG_LEVEL: info
`,
		"storefront/web/eu1-values.yaml": `replicaCount: 6
autoscaling: {enabled: true, minReplicas: 6, maxReplicas: 20}
environment:
  CART_URL: http://cart-api.cart.svc:8080
  SIDECAR: localhost:9000
  REGION: eu1
extraEnv:
  - {name: TRACE_ENDPOINT, value: "collector.tracing.svc.cluster.local:4317"}
jobs:
  migrate:
    image: acme/migrate:3
`,
		"storefront/web/eu1-values.override.yaml": "image:\n  tag: \"1.1\"\n",
		"storefront/web/us1-values.yaml":          "replicaCount: 3\n",
	})
	eu := find(ff, "symbol", "storefront/web.Application.web-eu1")
	if got, want := strs(eu, "value_files"), []string{
		"storefront/web/values.yaml",
		"storefront/web/eu1-values.yaml",
		"storefront/web/eu1-values.override.yaml",
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("value_files = %v, want %v", got, want)
	}
	if got, want := strs(eu, "images"), []string{"acme/migrate:3", "acme/web:1.1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("images = %v, want %v", got, want)
	}
	for k, v := range map[string]int{"replicas": 6, "min_replicas": 6, "max_replicas": 20} {
		if eu.Props[k] != v {
			t.Errorf("%s = %v, want %d", k, eu.Props[k], v)
		}
	}
	if got, want := strs(eu, "resources"), []string{"requests.cpu=100m", "requests.memory=128Mi", "limits.cpu=1", "limits.memory=512Mi"}; !reflect.DeepEqual(got, want) {
		t.Errorf("resources = %v, want %v", got, want)
	}
	if got, want := strs(eu, "env"), []string{
		"CART_URL=http://cart-api.cart.svc:8080",
		"LOG_LEVEL=info",
		"REGION=eu1",
		"SIDECAR=localhost:9000",
		"TRACE_ENDPOINT=collector.tracing.svc.cluster.local:4317",
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("env = %v, want %v", got, want)
	}
	// localhost:9000 is an address with no namespace: not recorded.
	if got, want := strs(eu, "addresses"), []string{"cart-api.cart:8080", "collector.tracing:4317"}; !reflect.DeepEqual(got, want) {
		t.Errorf("addresses = %v, want %v", got, want)
	}

	us := find(ff, "symbol", "storefront/web.Application.web-us1")
	if got, want := strs(us, "value_files"), []string{"storefront/web/values.yaml", "storefront/web/us1-values.yaml"}; !reflect.DeepEqual(got, want) {
		t.Errorf("us1 value_files = %v, want %v: eu1's files are not us1's", got, want)
	}
	if us.Props["replicas"] != 3 {
		t.Errorf("us1 replicas = %v, want 3", us.Props["replicas"])
	}
}

func TestListedValueFilesAreReadInTheirOrder(t *testing.T) {
	ff := extract(t, map[string]string{
		"apps/web.yaml": releaseApp("web", "eu1", "deploy/web", `    chart: base-service
    helm:
      valueFiles:
        - common.yaml
        - ../shared/eu.yaml
        - $values/elsewhere.yaml
`),
		"deploy/web/common.yaml": "replicaCount: 1\n",
		"deploy/web/values.yaml": "replicaCount: 9\n",
		"deploy/shared/eu.yaml":  "replicaCount: 4\n",
	})
	web := find(ff, "symbol", "apps.Application.web")
	if got, want := strs(web, "value_files"), []string{"deploy/web/common.yaml", "deploy/shared/eu.yaml"}; !reflect.DeepEqual(got, want) {
		t.Errorf("value_files = %v, want %v", got, want)
	}
	if web.Props["replicas"] != 4 {
		t.Errorf("replicas = %v, want the last file's 4", web.Props["replicas"])
	}
}

func TestALocalChartOrKustomizationIsNotAValuesRelease(t *testing.T) {
	ff := extract(t, map[string]string{
		"apps/a.yaml":             releaseApp("a", "eu1", "charts/web", plugin),
		"charts/web/Chart.yaml":   "apiVersion: v2\nname: web\nversion: 1.0.0\n",
		"charts/web/values.yaml":  "replicaCount: 2\n",
		"apps/b.yaml":             releaseApp("b", "eu1", "kust", plugin),
		"kust/kustomization.yaml": "resources: []\n",
		"kust/values.yaml":        "replicaCount: 2\n",
	})
	for _, n := range []string{"apps.Application.a", "apps.Application.b"} {
		if f := find(ff, "symbol", n); f.Props["value_files"] != nil {
			t.Errorf("%s read values %v", n, f.Props["value_files"])
		}
	}
}
