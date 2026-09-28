package tsextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A call on this.<field>.<method>() names the method of the field's stated type:
// a constructor parameter property, an annotated field (generic arguments dropped),
// a `new` initializer and Angular's inject(). A plain constructor parameter declares
// no field, and a field of a union type names no single class.
func TestFieldTypeCalls_ResolveThroughTheStatedType(t *testing.T) {
	src := `import { ResourceConnector } from "@example/sdk";
import { Repo } from "./repo";

export class CatalogService {
  private http: HttpClient<Item>;
  private repo = new Repo();
  private api = inject(ApiClient);
  private either: A | B;

  constructor(private readonly resources: ResourceConnector, plain: Other) {}

  run() {
    this.resources.getCatalogItems();
    this.http.get();
    this.repo.save();
    this.api.fetch();
    this.either.go();
    this.plain.nope();
  }
}
`
	ff := extractTS(t, src, "src/catalog/catalog.service.ts")
	for _, f := range ff {
		if f.Kind != facts.KindSymbol || f.Name != "src/catalog.CatalogService.run" {
			continue
		}
		for _, want := range []string{
			"@example/sdk.ResourceConnector.getCatalogItems",
			"src/catalog.HttpClient.get",
			"src/catalog.Repo.save",
			"src/catalog.ApiClient.fetch",
		} {
			if !f.HasRelation(facts.RelCalls, want) {
				t.Errorf("missing call %s; relations: %+v", want, f.Relations)
			}
		}
		for _, r := range f.Relations {
			if r.Target == "src/catalog.A.go" || r.Target == "src/catalog.B.go" || r.Target == "src/catalog.Other.nope" {
				t.Errorf("a call through a field of no single stated type was resolved: %s", r.Target)
			}
		}
		return
	}
	t.Fatal("method src/catalog.CatalogService.run not emitted")
}
