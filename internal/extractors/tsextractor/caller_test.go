package tsextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// Each hand-written call site names the symbol whose body makes it: a class method,
// an arrow function held by a const, the method around a callback that has no
// symbol of its own, and a function held by an object-literal property. A call at
// module scope names nothing rather than a neighbour.
func TestClientCallers_NameTheEnclosingSymbol(t *testing.T) {
	src := `
export class OrdersClient {
  async list() {
    return fetch("/api/orders", { method: "GET" });
  }

  cancel(id: string) {
    return this.ready().then(() => fetch(` + "`/api/orders/${id}/cancel`" + `, { method: "POST" }));
  }
}

export const loadInvoices = async () => {
  const r = await fetch("/api/invoices", { method: "GET" });
  return r.json();
};

export const statusApi = {
  current: async () => fetch("/api/status/current", { method: "GET" }),
};

fetch("/api/boot/config", { method: "GET" });
`
	ff := extractTS(t, src, "src/api/orders.ts")
	symbols := map[string]bool{}
	for _, f := range ff {
		if f.Kind == facts.KindSymbol {
			symbols[f.Name] = true
		}
	}
	got := map[string]string{}
	for _, f := range ff {
		if f.Kind == facts.KindRoute && f.PropAny(facts.PropRole) == facts.RoleClient {
			got[f.Name] = f.PropString(facts.PropCaller)
		}
	}
	for path, want := range map[string]string{
		"/api/orders":           "src/api.OrdersClient.list",
		"/api/orders/{}/cancel": "src/api.OrdersClient.cancel",
		"/api/invoices":         "src/api.loadInvoices",
		"/api/status/current":   "src/api.statusApi.current",
		"/api/boot/config":      "",
	} {
		caller, ok := got[path]
		if !ok {
			t.Errorf("no client route %s; routes: %v", path, got)
			continue
		}
		if caller != want {
			t.Errorf("%s caller = %q, want %q", path, caller, want)
		}
		if caller != "" && !symbols[caller] {
			t.Errorf("%s caller %q is not a symbol this file emitted", path, caller)
		}
	}
}

// A name imported from a package is called by its package-qualified name, not by a
// same-directory name that would claim a symbol of the importing module.
func TestImportedPackageCallsArePackageQualified(t *testing.T) {
	src := `import { createClient } from "@acme/sdk";
import { format } from "./format";

export function run() {
  createClient();
  format();
}

export function createClientLocal() {}
`
	ff := extractTS(t, src, "src/app/run.ts")
	for _, f := range ff {
		if f.Kind != facts.KindSymbol || f.Name != "src/app.run" {
			continue
		}
		if !f.HasRelation(facts.RelCalls, "@acme/sdk.createClient") {
			t.Errorf("package import not qualified: %+v", f.Relations)
		}
		if f.HasRelation(facts.RelCalls, "src/app.createClient") {
			t.Errorf("package import fell back to the importing directory: %+v", f.Relations)
		}
		if !f.HasRelation(facts.RelCalls, "src/app.format") {
			t.Errorf("relative import changed: %+v", f.Relations)
		}
		return
	}
	t.Fatal("symbol src/app.run not emitted")
}
