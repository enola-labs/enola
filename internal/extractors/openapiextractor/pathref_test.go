package openapiextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func countRoutes(fs []facts.Fact) int {
	n := 0
	for _, f := range fs {
		if f.Kind == facts.KindRoute {
			n++
		}
	}
	return n
}

// A split spec: every path item in service.yml is a `$ref` into a pointer
// inside another file, and those files carry no openapi header.
func TestExtract_PathItemRefWithPointer(t *testing.T) {
	repo := t.TempDir()
	writeSpec(t, repo, "api/openapi/service.yml", `openapi: '3.0.0'
info:
  version: 1.0.0
  title: Products
  x-gateway-config:
    at-gateway-prefix: "/products"
paths:
  /item/{item_id}:
    $ref: './paths/item.yml#/paths/~1item~1{item_id}'
  /more-items/{item_id}:
    $ref: './paths/more_items.yml#/paths/~1more-items~1{item_id}'
`)
	writeSpec(t, repo, "api/openapi/paths/item.yml", `paths:
  /item/{item_id}:
    get:
      operationId: getItem
      x-gateway-capabilities:
        exposed: true
        auth:
          mode: public-token
`)
	writeSpec(t, repo, "api/openapi/paths/more_items.yml", `paths:
  /more-items/{item_id}:
    get:
      operationId: getMoreItems
`)

	got := extract(t, repo)
	item := findRoute(got, "/item/{item_id}", "GET")
	if item == nil {
		t.Fatalf("missing GET /item/{item_id}; got %d routes", countRoutes(got))
	}
	for key, want := range map[string]any{
		"operationId":    "getItem",
		"role":           "server",
		"spec_file":      "api/openapi/service.yml",
		"path_item_file": "api/openapi/paths/item.yml",
		"gateway_path":   "/products/item/{item_id}",
		"exposed":        true,
		"auth_mode":      "public-token",
	} {
		if item.Props[key] != want {
			t.Errorf("%s = %v, want %v", key, item.Props[key], want)
		}
	}
	if findRoute(got, "/more-items/{item_id}", "GET") == nil {
		t.Error("missing GET /more-items/{item_id}")
	}
	if n := countRoutes(got); n != 2 {
		t.Errorf("got %d routes, want 2 (the path files are fragments, not specs)", n)
	}
}

// The reference names a whole file, which IS the path item.
func TestExtract_PathItemRefWholeFile(t *testing.T) {
	repo := t.TempDir()
	writeSpec(t, repo, "packages/exports/api/openapi/exports_api.yaml", `openapi: 3.0.3
info: { title: Exports, version: 1.0.0 }
paths:
  /api/v1/users/{user_id}/exports:
    $ref: './paths/users_{user_id}_exports.yaml'
`)
	writeSpec(t, repo, "packages/exports/api/openapi/paths/users_{user_id}_exports.yaml", `post:
  operationId: createExport
get:
  operationId: showExport
`)

	got := extract(t, repo)
	for _, m := range []string{"POST", "GET"} {
		if findRoute(got, "/api/v1/users/{user_id}/exports", m) == nil {
			t.Errorf("missing %s /api/v1/users/{user_id}/exports", m)
		}
	}
}

// A reference into a file that is itself a full spec is not followed: that
// file extracts its own routes, and following it would emit each one twice.
// A cycle, a missing file and a path outside the repository derive nothing.
func TestExtract_PathItemRefGuards(t *testing.T) {
	repo := t.TempDir()
	writeSpec(t, repo, "api/openapi/service.yml", `openapi: 3.0.0
info: { title: Svc, version: 1.0.0 }
paths:
  /widgets:
    $ref: './other.openapi.yml#/paths/~1widgets'
  /loop:
    $ref: '#/paths/~1loop'
  /missing:
    $ref: './paths/nope.yml'
  /escape:
    $ref: '../../../etc/passwd'
  /remote:
    $ref: 'https://example.com/spec.yml#/paths/~1x'
`)
	writeSpec(t, repo, "api/openapi/other.openapi.yml", `openapi: 3.0.0
info: { title: Other, version: 1.0.0 }
paths:
  /widgets:
    get:
      operationId: listWidgets
`)

	got := extract(t, repo)
	if n := countRoutes(got); n != 1 {
		t.Errorf("got %d routes, want 1 (only other.openapi.yml's own GET /widgets)", n)
	}
	w := findRoute(got, "/widgets", "GET")
	if w == nil || w.Props["spec_file"] != "api/openapi/other.openapi.yml" {
		t.Errorf("GET /widgets should come from its own spec; got %+v", w)
	}
}
