package routeindex

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func TestIsCatchAll(t *testing.T) {
	for path, want := range map[string]bool{
		"/{filename:path}":           true,
		"/api/{path=**}":             true,
		"/files/{key+}":              true,
		"/proxy/*":                   true,
		"/assets/*rest":              true,
		"/api/[...slug]":             true,
		"/docs/[[...page]]":          true,
		"/orders/{id}":               false,
		"/orders/:id":                false,
		"/orders/[id]":               false,
		"/orders/{id:[0-9]+}/lines/": false,
		"/":                          false,
		"/orders/{id:[0-9]+}":        false,
		"/blobs/{name:.+}":           true,
	} {
		if got := IsCatchAll(path); got != want {
			t.Errorf("IsCatchAll(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestIsStandInServer(t *testing.T) {
	for file, want := range map[string]bool{
		"orders/test/integration/servermock/server.go": true,
		"orders/api/openapi/billing-api-mock.yaml":     true,
		"orders/tools/dummy/dummy_upstreams_server.go": true,
		"orders/internal/stubs/payments.go":            true,
		"orders/internal/http/server.go":               false,
		"orders/api/openapi/service.yml":               false,
		"orders/internal/mockingbird/handler.go":       false,
	} {
		f := facts.Fact{Kind: facts.KindRoute, Repo: "orders", File: file, RepoPrefixed: true}
		if got := IsStandInServer(f); got != want {
			t.Errorf("IsStandInServer(%s) = %v, want %v", file, got, want)
		}
	}
}
