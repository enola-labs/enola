package k8sextractor

import (
	"reflect"
	"testing"
)

func layeredApp(name, cluster, project, path string) string {
	return `apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: ` + name + `
  namespace: argocd
  labels: {app: ` + name + `}
spec:
  destination: {name: ` + cluster + `, namespace: shop}
  project: ` + project + `
  source:
    path: ` + path + `
    plugin: {name: chart-resolver}
`
}

func TestLayeredValuesApplyByWhatTheirNamesSay(t *testing.T) {
	ff := extract(t, map[string]string{
		"shop/live/web/eu1prod1-application.yaml":          layeredApp("shop-live-web-eu1prod1", "eu1prod1", "shop-production", "shop/live/web"),
		"shop/live/web/eu1prod1-values.yaml":               "replicaCount: 9\n",
		"shop/live/web-admin/eu1prod1-application.yaml":    layeredApp("shop-live-web-admin-eu1prod1", "eu1prod1", "shop-production", "shop/live/web-admin"),
		"shop/test/web/us1sandbox1-application.yaml":       layeredApp("shop-test-web-us1sandbox1", "us1sandbox1", "shop-sandbox", "shop/test/web"),
		"shop/_layered_values/01-global.yaml":              "image: {repository: acme/shop}\nreplicaCount: 1\n",
		"shop/_layered_values/20-production.yaml":          "replicaCount: 2\n",
		"shop/_layered_values/20-sandbox.yaml":             "replicaCount: 3\n",
		"shop/_layered_values/30-eu1.yaml":                 "environment: {REGION: eu1}\n",
		"shop/_layered_values/40-eu1prod1.yaml":            "environment: {CLUSTER: eu1prod1}\n",
		"shop/_layered_values/50-web.yaml":                 "environment: {ROLE: web}\n",
		"shop/_layered_values/50-web-admin.yaml":           "environment: {ROLE: admin}\n",
		"shop/_layered_values/60-web-production.yaml":      "environment: {TIER: live}\n",
		"shop/_layered_values/60-web-sandbox.yaml":         "environment: {TIER: test}\n",
		"shop/_layered_values/60-live-web-production.yaml": "environment: {BY_PATH: yes}\n",
		"shop/_layered_values/70-web-us1.yaml":             "environment: {US: yes}\n",
	})
	web := find(ff, "symbol", "shop/live/web.Application.shop-live-web-eu1prod1")
	if got, want := strs(web, "value_files"), []string{
		"shop/_layered_values/01-global.yaml",
		"shop/_layered_values/20-production.yaml",
		"shop/_layered_values/30-eu1.yaml",
		"shop/_layered_values/40-eu1prod1.yaml",
		"shop/_layered_values/50-web.yaml",
		"shop/_layered_values/60-live-web-production.yaml",
		"shop/_layered_values/60-web-production.yaml",
		"shop/live/web/eu1prod1-values.yaml",
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("web value_files =\n%v\nwant\n%v", got, want)
	}
	if web.Props["replicas"] != 9 {
		t.Errorf("replicas = %v, want its own file's 9 over the layers", web.Props["replicas"])
	}

	admin := find(ff, "symbol", "shop/live/web-admin.Application.shop-live-web-admin-eu1prod1")
	if got, want := strs(admin, "value_files"), []string{
		"shop/_layered_values/01-global.yaml",
		"shop/_layered_values/20-production.yaml",
		"shop/_layered_values/30-eu1.yaml",
		"shop/_layered_values/40-eu1prod1.yaml",
		"shop/_layered_values/50-web-admin.yaml",
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("web-admin value_files =\n%v\nwant\n%v", got, want)
	}

	test := find(ff, "symbol", "shop/test/web.Application.shop-test-web-us1sandbox1")
	if got, want := strs(test, "value_files"), []string{
		"shop/_layered_values/01-global.yaml",
		"shop/_layered_values/20-sandbox.yaml",
		"shop/_layered_values/50-web.yaml",
		"shop/_layered_values/60-web-sandbox.yaml",
		"shop/_layered_values/70-web-us1.yaml",
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("sandbox value_files =\n%v\nwant\n%v", got, want)
	}
	if test.Props["replicas"] != 3 {
		t.Errorf("sandbox replicas = %v, want 3", test.Props["replicas"])
	}
}
