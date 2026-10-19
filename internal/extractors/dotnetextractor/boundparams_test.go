package dotnetextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A minimal-API handler's parameter with an ASP.NET binding attribute is built by
// the framework, so its type is injected into the handler. A plain parameter is
// passed by callers and is not.
func TestExtract_BoundHandlerParametersAreInjected(t *testing.T) {
	src := `namespace Shop.Catalog;

public static class CatalogApi
{
    public static async Task<IResult> GetAllItems(
        [AsParameters] PaginationRequest paginationRequest,
        [Microsoft.AspNetCore.Mvc.FromServicesAttribute] CatalogServices services,
        [FromQuery] Filter filter,
        Sorting sorting,
        string? name)
    {
        return Results.Ok();
    }
}
`
	ff := extractFileAST([]byte(src), "src/Catalog/CatalogApi.cs")
	m := factByName(ff, "src/Catalog.CatalogApi.GetAllItems")
	if m == nil {
		t.Fatalf("handler symbol missing; got %v", names(ff, facts.KindSymbol))
	}
	for _, typ := range []string{"PaginationRequest", "CatalogServices", "Filter"} {
		if !hasRel(m, facts.RelInjects, typ) {
			t.Errorf("missing injects %s; got %v", typ, m.Relations)
		}
	}
	if hasRel(m, facts.RelInjects, "Sorting") {
		t.Errorf("a parameter without a binding attribute must not be injected")
	}
}
