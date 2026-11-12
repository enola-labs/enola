package k8sextractor

import (
	"reflect"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func TestParseAddress(t *testing.T) {
	cases := map[string]*address{
		"cartservice:7070":                        {name: "cartservice", port: 7070},
		"http://cart:8080/api":                    {name: "cart", port: 8080},
		"redis://user:pw@redis-cart:6379/0":       {name: "redis-cart", port: 6379},
		"dns:///checkout:5050":                    {name: "checkout", port: 5050},
		"cart.shop.svc.cluster.local":             {name: "cart", namespace: "shop"},
		"cart.svc":                                {name: "cart"},
		"http://cart.shop.svc.cluster.local:9000": {name: "cart", namespace: "shop", port: 9000},
		"cart":                    nil, // a bare word is not an address
		"cart.example.com:443":    nil, // a host outside the cluster
		"https://api.example.com": nil,
		"${CART_ADDR}":            nil,
		"true":                    nil,
	}
	for in, want := range cases {
		got, ok := parseAddress(in)
		if want == nil {
			if ok {
				t.Errorf("parseAddress(%q) = %+v, want none", in, got)
			}
			continue
		}
		if !ok || got != *want {
			t.Errorf("parseAddress(%q) = %+v, %v, want %+v", in, got, ok, *want)
		}
	}
	if got := parseAddresses("kafka-0:9092, kafka-1:9092"); len(got) != 2 {
		t.Errorf("a comma-separated list = %+v", got)
	}
}

func TestWorkloadsDependOnTheServicesTheirAddressesName(t *testing.T) {
	ff := extract(t, map[string]string{
		"manifests/checkout.yaml": `apiVersion: apps/v1
kind: Deployment
metadata: {name: checkout}
spec:
  template:
    spec:
      containers:
        - name: server
          image: checkoutservice
          envFrom: [{configMapRef: {name: checkout-env}}]
          env:
            - {name: CART_SERVICE_ADDR, value: "cartservice:7070"}
            - {name: PAYMENT_URL, value: "http://payment:8080/charge"}
            - {name: WRONG_PORT, value: "email:1234"}
            - {name: MODE, value: "cartservice"}
`,
		"manifests/env.yaml": `apiVersion: v1
kind: ConfigMap
metadata: {name: checkout-env}
data:
  REDIS_ADDR: redis-cart:6379
  GREETING: hello
`,
		"manifests/services.yaml": `apiVersion: v1
kind: Service
metadata: {name: cartservice}
spec: {ports: [{port: 7070}]}
---
apiVersion: v1
kind: Service
metadata: {name: payment}
spec: {ports: [{port: 8080}]}
---
apiVersion: v1
kind: Service
metadata: {name: email}
spec: {ports: [{port: 5000}]}
---
apiVersion: v1
kind: Service
metadata: {name: redis-cart}
spec: {ports: [{port: 6379}]}
`,
		"skaffold.yaml": `apiVersion: skaffold/v4beta6
kind: Config
build:
  artifacts:
    - image: checkoutservice
      context: src/checkoutservice
profiles:
  - name: ci
    build:
      artifacts:
        - image: frontend
          context: src/frontend
`,
	})
	dep := find(ff, facts.KindSymbol, "manifests.Deployment.checkout")
	want := []string{"manifests.ConfigMap.checkout-env", "manifests.Service.cartservice", "manifests.Service.payment"}
	if got := dependsOn(dep); !reflect.DeepEqual(got, want) {
		t.Errorf("Deployment depends_on = %v, want %v (email's port is wrong, a bare word is no address)", got, want)
	}
	if got := dependsOn(find(ff, facts.KindSymbol, "manifests.ConfigMap.checkout-env")); !reflect.DeepEqual(got, []string{"manifests.Service.redis-cart"}) {
		t.Errorf("ConfigMap depends_on = %v", got)
	}
	if got := dep.Props[facts.PropBuildContexts]; !reflect.DeepEqual(got, []string{"src/checkoutservice"}) {
		t.Errorf("build_contexts = %v", got)
	}
}

func TestContextsFor(t *testing.T) {
	contexts := map[string]string{"frontend": "src/frontend", "gcr.io/team/api": "api", "a/web": "web1", "b/web": "web2"}
	cases := map[string][]string{
		"frontend": {"src/frontend"},
		"us-docker.pkg.dev/proj/repo/frontend:v1.2": {"src/frontend"}, // skaffold's default repo
		"gcr.io/team/api@sha256:abc":                {"api"},
		"registry.io/x/web:1":                       nil, // only artifacts with a registry carry "web"
		"nginx:1.25":                                nil,
	}
	for img, want := range cases {
		got := contextsFor([]string{img}, contexts)
		if len(got) == 0 && want == nil {
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("contextsFor(%q) = %v, want %v", img, got, want)
		}
	}
}
