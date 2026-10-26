package tsextractor

import (
	"context"
	"reflect"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// extractTestRefsOf runs the test-ref pass, the one the engine hands test files to.
func extractTestRefsOf(t *testing.T, files map[string]string) []facts.Fact {
	t.Helper()
	dir := setupTSProject(t, files, false)
	var rel []string
	for f := range files {
		rel = append(rel, f)
	}
	ff, err := New().ExtractTestRefs(context.Background(), dir, rel, nil)
	if err != nil {
		t.Fatal(err)
	}
	return ff
}

func testRefProps(t *testing.T, ff []facts.Fact, file string) facts.Fact {
	t.Helper()
	for _, f := range ff {
		if f.Kind == facts.KindTestRef && f.File == file {
			return f
		}
	}
	t.Fatalf("no test_ref for %s", file)
	return facts.Fact{}
}

func TestTestCalls_CypressRequestsVisitsAndOperations(t *testing.T) {
	ff := extractTestRefsOf(t, map[string]string{
		"src/widgets.ts": "export const x = 1",
		"cypress/tests/api/widgets.spec.ts": `
const apiWidgets = ` + "`${Cypress.env(\"apiUrl\")}/widgets`" + `;
const apiGraphQL = ` + "`${Cypress.env(\"apiUrl\")}/graphql`" + `;
describe("widgets", () => {
  it("lists, reads and deletes", () => {
    cy.request("GET", "/");
    cy.request("GET", apiWidgets);
    cy.request("DELETE", ` + "`${apiWidgets}/${ctx.widgetId}`" + `);
    cy.request({ method: "PATCH", url: ` + "`${apiWidgets}/${id}?force=1`" + `, body: {} });
    cy.request("POST", apiGraphQL, { query: ` + "`query { listWidgets { id } }`" + ` });
    cy.intercept("GET", "/notifications").as("n");
    cy.intercept("POST", apiGraphQL, (req) => {
      if (req.body.operationName === "ListWidgets") { req.alias = "gqlList"; }
    });
    cy.visit(` + "`/widget/${id}`" + `);
  });
});`,
	})
	f := testRefProps(t, ff, "cypress/tests/api/widgets.spec.ts")
	want := map[string][]string{
		propTestHTTPCalls:  {"DELETE /widgets/:param", "GET /", "GET /notifications", "GET /widgets", "PATCH /widgets/:param", "POST /graphql"},
		propTestPageVisits: {"/widget/:param"},
		propTestGraphQL:    {"Query.listWidgets"},
		propTestGraphQLOps: {"ListWidgets"},
	}
	for prop, w := range want {
		if got, _ := f.Props[prop].([]string); !reflect.DeepEqual(got, w) {
			t.Errorf("%s = %v, want %v", prop, got, w)
		}
	}
}

func TestTestCalls_PlaywrightGotoAndRequest(t *testing.T) {
	ff := extractTestRefsOf(t, map[string]string{
		"e2e/orders.spec.ts": `
import { test } from "@playwright/test";
test("orders", async ({ page, request }) => {
  await page.goto("http://localhost:3000/orders/new");
  await request.post("/api/orders", { data: {} });
  await page.request.get(` + "`/api/orders/${id}`" + `);
});`,
	})
	f := testRefProps(t, ff, "e2e/orders.spec.ts")
	if got, _ := f.Props[propTestPageVisits].([]string); !reflect.DeepEqual(got, []string{"/orders/new"}) {
		t.Errorf("visits %v", got)
	}
	if got, _ := f.Props[propTestHTTPCalls].([]string); !reflect.DeepEqual(got, []string{"GET /api/orders/:param", "POST /api/orders"}) {
		t.Errorf("requests %v", got)
	}
}
