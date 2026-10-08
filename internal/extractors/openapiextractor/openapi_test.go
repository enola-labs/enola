package openapiextractor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// writeSpec writes content to repo/rel, creating parent dirs.
func writeSpec(t *testing.T, repo, rel, content string) {
	t.Helper()
	p := filepath.Join(repo, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// extract runs the extractor over repo and returns the route facts.
func extract(t *testing.T, repo string) []facts.Fact {
	t.Helper()
	got, err := New().Extract(context.Background(), repo, nil)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	return got
}

// findRoute returns the route fact for (path, method), or nil.
func findRoute(fs []facts.Fact, path, method string) *facts.Fact {
	for i := range fs {
		if fs[i].Kind == facts.KindRoute && fs[i].Name == path && fs[i].Props["method"] == method {
			return &fs[i]
		}
	}
	return nil
}

const widgetsSpec = `openapi: 3.0.0
info:
  title: Widgets
  version: 1.0.0
paths:
  /widgets:
    parameters:
      - name: trace
        in: query
    get:
      operationId: listWidgets
      summary: List widgets
      tags: [widgets, public]
      responses:
        "200": { description: OK }
    post:
      operationId: createWidget
      tags: [widgets]
      responses:
        "201": { description: Created }
  /widgets/{id}:
    get:
      operationId: getWidget
      responses:
        "200": { description: OK }
`

func TestName(t *testing.T) {
	if got := New().Name(); got != "openapi" {
		t.Errorf("Name() = %q, want openapi", got)
	}
}

func TestDetect(t *testing.T) {
	t.Run("positive", func(t *testing.T) {
		repo := t.TempDir()
		writeSpec(t, repo, "api/openapi/widgets.yaml", widgetsSpec)
		ok, err := New().Detect(repo)
		if err != nil {
			t.Fatalf("Detect: %v", err)
		}
		if !ok {
			t.Error("Detect should return true when an OpenAPI spec is present")
		}
	})

	t.Run("negative_plain_yaml", func(t *testing.T) {
		repo := t.TempDir()
		// A YAML file inside an "openapi" dir but without openapi/swagger content
		// must not be detected (Detect reads content, not just the path).
		writeSpec(t, repo, "config.yaml", "name: myapp\nversion: 1\n")
		ok, err := New().Detect(repo)
		if err != nil {
			t.Fatalf("Detect: %v", err)
		}
		if ok {
			t.Error("Detect should return false for a non-spec YAML file")
		}
	})

	t.Run("negative_empty", func(t *testing.T) {
		ok, err := New().Detect(t.TempDir())
		if err != nil {
			t.Fatalf("Detect: %v", err)
		}
		if ok {
			t.Error("Detect should return false for an empty repo")
		}
	})
}

func TestExtract_Routes(t *testing.T) {
	repo := t.TempDir()
	writeSpec(t, repo, "api/openapi/widgets.yaml", widgetsSpec)

	got := extract(t, repo)

	// One route per HTTP method (3), and the non-method "parameters" key skipped.
	if len(got) != 3 {
		t.Fatalf("expected 3 route facts, got %d: %+v", len(got), got)
	}

	r := findRoute(got, "/widgets", "GET")
	if r == nil {
		t.Fatal("missing GET /widgets")
	}
	if r.Props["operationId"] != "listWidgets" {
		t.Errorf("operationId = %v, want listWidgets", r.Props["operationId"])
	}
	if r.Props["summary"] != "List widgets" {
		t.Errorf("summary = %v, want 'List widgets'", r.Props["summary"])
	}
	if r.Props["spec_file"] != "api/openapi/widgets.yaml" {
		t.Errorf("spec_file = %v", r.Props["spec_file"])
	}
	if r.Props["role"] != "server" {
		t.Errorf("role = %v, want server", r.Props["role"])
	}
	tags, ok := r.Props["tags"].([]string)
	if !ok || len(tags) != 2 || tags[0] != "widgets" || tags[1] != "public" {
		t.Errorf("tags = %v, want [widgets public]", r.Props["tags"])
	}

	if findRoute(got, "/widgets", "POST") == nil {
		t.Error("missing POST /widgets")
	}
	if findRoute(got, "/widgets/{id}", "GET") == nil {
		t.Error("missing GET /widgets/{id}")
	}
}

func TestExtract_ClientRole(t *testing.T) {
	repo := t.TempDir()
	writeSpec(t, repo, "api/openapi/widgets.yaml", widgetsSpec)
	writeSpec(t, repo, "api/openapi/client/billing.yml", `openapi: 3.0.0
info: { title: Billing, version: 1.0.0 }
paths:
  /invoices:
    get:
      operationId: listInvoices
      responses:
        "200": { description: OK }
`)

	got := extract(t, repo)

	server := findRoute(got, "/widgets", "GET")
	if server == nil || server.Props["role"] != "server" {
		t.Errorf("top-level spec should be role=server; got %+v", server)
	}
	client := findRoute(got, "/invoices", "GET")
	if client == nil {
		t.Fatal("missing client route /invoices")
	}
	if client.Props["role"] != "client" {
		t.Errorf("spec under openapi/client/ should be role=client; got %v", client.Props["role"])
	}
}

// Vendored specs may live under openapi/clients/ (plural). Read as server
// routes they would make the repository the provider of every API it calls.
func TestExtract_ClientRolePluralDirectory(t *testing.T) {
	repo := t.TempDir()
	writeSpec(t, repo, "api/openapi/clients/geo.yml", `openapi: 3.0.0
info: { title: Geo, version: 1.0.0 }
paths:
  /api/v1/regions:
    get:
      operationId: listRegions
      responses:
        "200": { description: OK }
`)

	got := extract(t, repo)

	client := findRoute(got, "/api/v1/regions", "GET")
	if client == nil {
		t.Fatal("missing client route /api/v1/regions")
	}
	if client.Props["role"] != "client" {
		t.Errorf("spec under openapi/clients/ should be role=client; got %v", client.Props["role"])
	}
}

func TestExtract_GatewayPrefix(t *testing.T) {
	repo := t.TempDir()
	writeSpec(t, repo, "api/openapi/svc.openapi.yaml", `openapi: 3.0.0
info:
  title: Svc
  version: 1.0.0
  x-gateway-config:
    at-gateway-prefix: /svc-example/
paths:
  /ping:
    get:
      operationId: ping
      responses:
        "200": { description: OK }
`)

	r := findRoute(extract(t, repo), "/ping", "GET")
	if r == nil {
		t.Fatal("missing GET /ping")
	}
	if r.Props["gateway_prefix"] != "/svc-example" {
		t.Errorf("gateway_prefix = %v, want /svc-example", r.Props["gateway_prefix"])
	}
	if r.Props["gateway_path"] != "/svc-example/ping" {
		t.Errorf("gateway_path = %v, want /svc-example/ping", r.Props["gateway_path"])
	}
}

func TestExtract_MalformedSpecSkipped(t *testing.T) {
	repo := t.TempDir()
	// Invalid YAML in an openapi-named file: must be skipped, not panic or error.
	writeSpec(t, repo, "openapi/broken.yaml", "openapi: 3.0.0\npaths: [this is not: valid")
	// A valid spec alongside it should still be extracted.
	writeSpec(t, repo, "openapi/good.yaml", `openapi: 3.0.0
info: { title: Good, version: 1.0.0 }
paths:
  /ok:
    get:
      operationId: ok
      responses:
        "200": { description: OK }
`)

	got := extract(t, repo)
	if findRoute(got, "/ok", "GET") == nil {
		t.Errorf("valid spec should still extract despite a malformed sibling; got %+v", got)
	}
}

// This extractor walks the repo itself instead of consuming the engine's
// ignore-glob-filtered file list, so config.Default()'s "**/testdata/**" does
// not reach it — skipDir must exclude fixtures on its own. Without this, the
// client specs in enola's own testdata/repos/** were extracted as facts of the
// enola service, manufacturing outbound HTTP call sites for a repo that makes
// none and reporting it as a cross-repo coverage gap.
func TestExtract_SkipsTestdataFixtures(t *testing.T) {
	repo := t.TempDir()
	writeSpec(t, repo, "api/openapi/widgets.yaml", widgetsSpec)
	writeSpec(t, repo, "internal/engine/testdata/repos/sample/api/openapi/widgets.yaml", widgetsSpec)

	got := extract(t, repo)

	// Only the first-party spec's 3 routes; the fixture copy contributes nothing.
	if len(got) != 3 {
		t.Fatalf("expected 3 route facts from the non-fixture spec, got %d: %+v", len(got), got)
	}
	for _, f := range got {
		if sf, _ := f.Props["spec_file"].(string); strings.Contains(sf, "testdata/") {
			t.Errorf("extracted a fixture spec: %s", sf)
		}
	}
}
