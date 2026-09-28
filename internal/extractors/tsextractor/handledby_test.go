package tsextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A decorated controller method is the route's handler, and the route names the
// method's own symbol fact: the name must be one this extraction emitted.
func TestDecoratorRoutes_HandledByTheDecoratedMethod(t *testing.T) {
	src := `
@Controller("/v1/catalog")
export class CatalogController {
  @Post("/imports")
  startImport() {}
}
`
	ff := extractTS(t, src, "src/catalog/catalog.controller.ts")
	symbols := map[string]bool{}
	for _, f := range ff {
		if f.Kind == facts.KindSymbol {
			symbols[f.Name] = true
		}
	}
	for _, f := range ff {
		if f.Kind != facts.KindRoute || f.Name != "/v1/catalog/imports" {
			continue
		}
		for _, r := range f.Relations {
			if r.Kind == facts.RelHandledBy {
				if r.Target != "src/catalog.CatalogController.startImport" || !symbols[r.Target] {
					t.Fatalf("handled_by = %q, want the emitted method symbol", r.Target)
				}
				return
			}
		}
		t.Fatalf("route has no handled_by: %+v", f.Relations)
	}
	t.Fatal("route not emitted")
}
