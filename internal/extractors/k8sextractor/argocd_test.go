package k8sextractor

import (
	"reflect"
	"strings"
	"testing"
)

const project = `apiVersion: argoproj.io/v1alpha1
kind: AppProject
metadata:
  name: storefront-prod
  namespace: argocd
`

// pluginApp is an Application whose plugin resolves the chart, handed to it in env.
const pluginApp = `apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: storefront-web-eu1
  namespace: argocd
spec:
  destination:
    name: eu1
    namespace: storefront
  project: storefront-prod
  source:
    repoURL: git@git.example.com:acme/shop-deploy.git
    targetRevision: HEAD
    path: "storefront/web/"
    plugin:
      name: chart-resolver
      env:
        - name: CHART_NAME
          value: base-service
        - name: CHART_VERSION
          value: 1.42.0
`

// chartApp deploys a chart straight from a registry.
const chartApp = `apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: cache
  namespace: argocd
spec:
  destination:
    server: https://kubernetes.default.svc
    namespace: cache
  project: platform
  source:
    repoURL: https://charts.example.com
    chart: keyvalue
    targetRevision: 7.1.2
    helm:
      releaseName: cache-main
`

func TestAnApplicationStatesItsRelease(t *testing.T) {
	ff := extract(t, map[string]string{
		".git/config":                         "[remote \"origin\"]\n\turl = git@git.example.com:acme/shop-deploy.git\n",
		"storefront/eu1-project.yaml":         project,
		"storefront/web/eu1-application.yaml": pluginApp,
		"platform/cache-application.yaml":     chartApp,
	})
	web := find(ff, "symbol", "storefront/web.Application.storefront-web-eu1")
	if web == nil {
		t.Fatal("no Application symbol")
	}
	want := map[string]any{
		"destination_cluster":   "eu1",
		"destination_namespace": "storefront",
		"argocd_project":        "storefront-prod",
		"source_repo":           "git@git.example.com:acme/shop-deploy.git",
		"source_path":           "storefront/web",
		"target_revision":       "HEAD",
		"chart":                 "base-service",
		"chart_version":         "1.42.0",
		"source_plugin":         "chart-resolver",
	}
	for k, v := range want {
		if web.Props[k] != v {
			t.Errorf("%s = %v, want %v", k, web.Props[k], v)
		}
	}
	if got := dependsOn(web); !reflect.DeepEqual(got, []string{"storefront.AppProject.storefront-prod"}) {
		t.Errorf("depends_on = %v, want the AppProject", got)
	}

	cache := find(ff, "symbol", "platform.Application.cache")
	for k, v := range map[string]any{
		"destination_cluster": "https://kubernetes.default.svc",
		"chart":               "keyvalue",
		"chart_version":       "7.1.2",
		"release_name":        "cache-main",
		"source_repo":         "https://charts.example.com",
	} {
		if cache.Props[k] != v {
			t.Errorf("cache %s = %v, want %v", k, cache.Props[k], v)
		}
	}
	if _, ok := cache.Props["source_path"]; ok {
		t.Error("a chart source has no source_path")
	}
	if got := dependsOn(cache); len(got) != 0 {
		t.Errorf("an unresolved project draws nothing, got %v", got)
	}
}

func TestALocalSourcePathIsWhatTheApplicationApplies(t *testing.T) {
	app := func(repo, path string) string {
		return `apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: web
  namespace: argocd
spec:
  destination: {name: eu1, namespace: storefront}
  source:
    repoURL: ` + repo + `
    path: ` + path + `
`
	}
	const cm = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: web-config\n"
	const kust = "resources:\n  - cm.yaml\n"
	const git = "[remote \"origin\"]\n\turl = https://git.example.com/acme/shop-deploy\n"

	t.Run("objects in the path", func(t *testing.T) {
		ff := extract(t, map[string]string{
			".git/config":           git,
			"apps/web.yaml":         app("https://git.example.com/acme/shop-deploy.git", "manifests/web"),
			"manifests/web/cm.yaml": cm,
		})
		got := dependsOn(find(ff, "symbol", "apps.Application.web"))
		if !reflect.DeepEqual(got, []string{"manifests/web.ConfigMap.web-config"}) {
			t.Errorf("depends_on = %v", got)
		}
	})
	t.Run("a kustomization in the path", func(t *testing.T) {
		ff := extract(t, map[string]string{
			".git/config":                      git,
			"apps/web.yaml":                    app("https://git.example.com/acme/shop-deploy.git", "manifests/web"),
			"manifests/web/cm.yaml":            cm,
			"manifests/web/kustomization.yaml": kust,
		})
		got := dependsOn(find(ff, "symbol", "apps.Application.web"))
		if !reflect.DeepEqual(got, []string{"manifests/web.Kustomization"}) {
			t.Errorf("depends_on = %v", got)
		}
	})
	t.Run("a plugin renders the path", func(t *testing.T) {
		plugged := strings.Replace(app("https://git.example.com/acme/shop-deploy.git", "manifests/web"),
			"    path: manifests/web\n", "    path: manifests/web\n    plugin: {name: chart-resolver}\n", 1)
		ff := extract(t, map[string]string{
			".git/config":           git,
			"apps/web.yaml":         plugged,
			"manifests/web/cm.yaml": cm,
		})
		if got := dependsOn(find(ff, "symbol", "apps.Application.web")); len(got) != 0 {
			t.Errorf("depends_on = %v, want nothing", got)
		}
	})
	t.Run("the same path in another repository", func(t *testing.T) {
		ff := extract(t, map[string]string{
			".git/config":           git,
			"apps/web.yaml":         app("https://git.example.com/acme/other-deploy.git", "manifests/web"),
			"manifests/web/cm.yaml": cm,
		})
		if got := dependsOn(find(ff, "symbol", "apps.Application.web")); len(got) != 0 {
			t.Errorf("depends_on = %v, want nothing", got)
		}
	})
}

func TestRepoName(t *testing.T) {
	for in, want := range map[string]string{
		"git@git.example.com:acme/Shop-Deploy.git":  "shop-deploy",
		"https://git.example.com/acme/shop-deploy/": "shop-deploy",
		"https://git.example.com/acme/shop-deploy":  "shop-deploy",
		"shop-deploy": "shop-deploy",
	} {
		if got := repoName(in); got != want {
			t.Errorf("repoName(%q) = %q, want %q", in, got, want)
		}
	}
}
