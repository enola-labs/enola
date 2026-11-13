package k8sextractor

import (
	"reflect"
	"testing"
)

// deployed is a plugin-chart Application of `name` on `cluster`, deploying to ns,
// with metadata lines (labels) and spec lines (project) spliced in.
func deployed(name, cluster, ns, path, meta, spec string) string {
	return `apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: ` + name + `
  namespace: argocd
` + meta + `spec:
  destination: {name: ` + cluster + `, namespace: ` + ns + `}
` + spec + `  source:
    path: ` + path + `
    plugin: {name: chart-resolver}
`
}

func TestAnApplicationSaysWhatItServesAndWhere(t *testing.T) {
	ff := extract(t, map[string]string{
		"cart/eu1-application.yaml": deployed("cart-api-eu1", "eu1", "cart", "cart",
			"  labels: {app: cart-api, app.kubernetes.io/instance: cart-main, tier: back}\n",
			"  project: cart-production\n"),
		"search/a.yaml": deployed("search", "us1", "search", "search",
			"  labels: {environment: Prod}\n", "  project: search-sandbox\n"),
		"pricing/a.yaml": deployed("pricing", "eu1", "pricing", "pricing", "",
			"  project: pricing-dev-and-prod\n"),
	})
	cart := find(ff, "symbol", "cart.Application.cart-api-eu1")
	if got, want := strs(cart, "serves"), []string{"cart-api", "cart-api-eu1", "cart-main"}; !reflect.DeepEqual(got, want) {
		t.Errorf("serves = %v, want %v", got, want)
	}
	if cart.Props["environment"] != "production" {
		t.Errorf("environment = %v, want production from the project", cart.Props["environment"])
	}
	// A label wins over the project, and is normalized.
	if got := find(ff, "symbol", "search.Application.search").Props["environment"]; got != "production" {
		t.Errorf("search environment = %v, want production", got)
	}
	// Two environments in a project name say none.
	if got := find(ff, "symbol", "pricing.Application.pricing").Props["environment"]; got != nil {
		t.Errorf("pricing environment = %v, want none", got)
	}
}

func TestAReleaseAddressNamesADeclaredService(t *testing.T) {
	ff := extract(t, map[string]string{
		"mail/service.yaml":   "apiVersion: v1\nkind: Service\nmetadata: {name: mailer, namespace: mail}\nspec:\n  ports: [{port: 25}]\n",
		"web/a.yaml":          deployed("web", "eu1", "web", "web", "", ""),
		"web/eu1-values.yaml": "environment:\n  MAIL_HOST: mailer.mail.svc:25\n  MAIL_WRONG_PORT: mailer.mail.svc:2525\n",
	})
	if got := dependsOn(find(ff, "symbol", "web.Application.web")); !reflect.DeepEqual(got, []string{"mail.Service.mailer"}) {
		t.Errorf("depends_on = %v, want the declared Service, port agreeing", got)
	}
}
