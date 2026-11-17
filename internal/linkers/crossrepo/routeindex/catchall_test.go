package routeindex

import "testing"

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
