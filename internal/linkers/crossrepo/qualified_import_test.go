package crossrepo

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func qualifiedType(repo, name, fqn string) facts.Fact {
	return facts.Fact{Kind: facts.KindSymbol, Name: name, Repo: repo,
		Props: map[string]any{"symbol_kind": facts.SymbolClass, "fqn": fqn}}
}

// A Java/Scala/.NET import names a type or package by its qualified name, with no
// segment matching a repository label; it links to the one repository declaring it:
// a type, a wildcard package import and a static member import alike. A name two
// repositories declare, and one the importing repository declares itself, link
// nothing.
func TestImports_QualifiedNamesLinkToTheDeclaringRepo(t *testing.T) {
	all := []facts.Fact{
		qualifiedType("inventory", "src/main/java/com/acme/inventory.Client", "com.acme.inventory.Client"),
		qualifiedType("billing", "src/main/java/com/acme/billing.Invoices", "com.acme.billing.Invoices"),
		qualifiedType("shipping", "src/main/java/com/acme/shipping.Rates", "com.acme.shipping.Rates"),
		qualifiedType("fork1", "x.Common", "com.acme.common.Common"),
		qualifiedType("fork2", "y.Common", "com.acme.common.Common"),
		qualifiedType("storefront", "src/main/java/com/shop.Checkout", "com.shop.Checkout"),
		importDep("storefront", "com.acme.inventory.Client"),
		importDep("storefront", "com.acme.billing.*"),
		importDep("storefront", "com.acme.shipping.Rates.forZone"),
		importDep("storefront", "com.acme.common.Common"),
		importDep("storefront", "com.shop.Checkout"),
	}
	edges := importEdges(t, all)
	for _, want := range []string{"storefront -> inventory", "storefront -> billing", "storefront -> shipping"} {
		if !edges[want] {
			t.Errorf("missing edge %s; edges: %v", want, edges)
		}
	}
	for _, unwanted := range []string{"storefront -> fork1", "storefront -> fork2", "storefront -> storefront"} {
		if edges[unwanted] {
			t.Errorf("unexpected edge %s", unwanted)
		}
	}
}
