package k8sextractor

import (
	"reflect"
	"sort"
	"testing"
)

// deployed is a plugin-chart Application of `name` on `cluster`, deploying to ns,
// with metadata lines (labels) and spec lines (helm) spliced in.
func deployed(name, cluster, ns, path, meta, spec string) string {
	return `apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: ` + name + `
  namespace: argocd
` + meta + `spec:
  destination: {name: ` + cluster + `, namespace: ` + ns + `}
  source:
    path: ` + path + `
    plugin: {name: chart-resolver}
` + spec
}

func sorted(s []string) []string {
	out := append([]string{}, s...)
	sort.Strings(out)
	return out
}

func TestAnAddressNamesTheApplicationServingIt(t *testing.T) {
	ff := extract(t, map[string]string{
		// cart-api serves by its app label, on eu1 and us1.
		"cart/eu1-application.yaml": deployed("cart-api-eu1", "eu1", "cart", "cart", "  labels: {app: cart-api}\n", ""),
		"cart/us1-application.yaml": deployed("cart-api-us1", "us1", "cart", "cart", "  labels: {app: cart-api}\n", ""),
		// search serves by its name with the cluster appended, on us1 only.
		"search/us1-application.yaml": deployed("search-us1", "us1", "search", "search", "", ""),
		// ledger is on us1 and ap1, neither the caller's cluster: no guess.
		"ledger/us1-application.yaml": deployed("ledger-us1", "us1", "ledger", "ledger", "", ""),
		"ledger/ap1-application.yaml": deployed("ledger-ap1", "ap1", "ledger", "ledger", "", ""),
		// pricing serves by its release name.
		"pricing/eu1-application.yaml": deployed("pricing-main", "eu1", "pricing", "pricing", "", "    helm: {releaseName: pricing-api}\n"),
		// another Application in cart's namespace, serving another name.
		"cart/worker-eu1-application.yaml": deployed("cart-worker-eu1", "eu1", "cart", "cart-worker", "  labels: {app: cart-worker}\n", ""),
		// a Service the repository declares.
		"mail/service.yaml": "apiVersion: v1\nkind: Service\nmetadata: {name: mailer, namespace: mail}\nspec:\n  ports: [{port: 25}]\n",
		// web, in its own namespace, sharing it with a helper it reaches by bare name.
		"web/eu1-application.yaml":    deployed("web-eu1", "eu1", "web", "web", "", ""),
		"helper/eu1-application.yaml": deployed("helper", "eu1", "web", "helper", "", ""),
		"web/eu1-values.yaml": `environment:
  CART_URL: http://cart-api.cart.svc:8080
  SEARCH_URL: http://search.search.svc.cluster.local
  LEDGER_URL: http://ledger.ledger.svc
  PRICING_URL: pricing-api.pricing.svc:80
  UNKNOWN_URL: http://nobody.cart.svc
  HELPER_ADDR: helper:9000
  MAIL_HOST: mailer.mail.svc:25
  MAIL_WRONG_PORT: mailer.mail.svc:2525
  OUTSIDE: https://api.example.com
`,
	})
	web := find(ff, "symbol", "web.Application.web-eu1")
	got := sorted(dependsOn(web))
	want := []string{
		"cart.Application.cart-api-eu1",    // its own cluster's, not us1's
		"helper.Application.helper",        // a bare name, in web's namespace
		"mail.Service.mailer",              // declared, port agreeing
		"pricing.Application.pricing-main", // by release name
		"search.Application.search-us1",    // no eu1 search: the one there is
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("depends_on = %v\nwant         %v", got, want)
	}
}
