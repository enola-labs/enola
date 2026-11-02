package tsextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A function held by an object literal in a top-level declaration is a symbol named
// by the declaration and its key path. One whose literal was handed to a call (a
// store factory, an RTK Query endpoint builder) is framework_registered; a plain
// object's members are not. `<name>.<member>()` on such a declaration, or on a name
// imported from this repository, resolves to the member; on a package import it
// stays unresolved.
func TestObjectMemberSymbols(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/api/orders.ts": `import { http } from "./http";

export const ordersApi = {
  list: async () => fetch("/api/orders", { method: "GET" }),
  remove(id: string) { return http.del(id); },
};

export const useCart = create((set) => ({
  refresh: async () => { await fetch("/api/cart", { method: "GET" }); },
}));

export const rolesApi = api.injectEndpoints({
  endpoints: (build) => ({
    getRoles: build.query({ query: () => ({ url: "/roles" }) }),
  }),
});
`,
		"src/api/http.ts": `export const http = { del: (id: string) => fetch("/api/x/" + id, { method: "DELETE" }) };
`,
		"src/app/page.ts": `import { ordersApi } from "../api/orders";
import TwoFactorApi from "../api/twofactor";
import dayjs from "dayjs";
import { z } from "zod";

export function load() {
  ordersApi.list();
  TwoFactorApi.enable();
  dayjs.utc();
  z.object({});
}
`,
		"src/api/twofactor.ts": `const TwoFactorApi = { enable: () => fetch("/api/2fa/enable", { method: "POST" }) };
export default TwoFactorApi;
`,
	}, false)

	syms := map[string]facts.Fact{}
	for _, f := range ff {
		if f.Kind == facts.KindSymbol {
			syms[f.Name] = f
		}
	}
	for name, registered := range map[string]bool{
		"src/api.ordersApi.list":                    false,
		"src/api.ordersApi.remove":                  false,
		"src/api.useCart.refresh":                   true,
		"src/api.rolesApi.endpoints":                true,
		"src/api.rolesApi.endpoints.getRoles.query": true,
		"src/api.http.del":                          false,
	} {
		f, ok := syms[name]
		if !ok {
			t.Errorf("no symbol %s", name)
			continue
		}
		if f.PropBool("framework_registered") != registered {
			t.Errorf("%s framework_registered = %v, want %v", name, f.PropBool("framework_registered"), registered)
		}
	}

	load := syms["src/app.load"]
	if !load.HasRelation(facts.RelCalls, "src/api.ordersApi.list") {
		t.Errorf("member call on an imported object not resolved: %+v", load.Relations)
	}
	if !load.HasRelation(facts.RelCalls, "src/api.TwoFactorApi.enable") {
		t.Errorf("member call on a default-imported object not resolved: %+v", load.Relations)
	}
	for _, r := range load.Relations {
		if r.Target == "zod.z.object" || r.Target == "src/app.z.object" || r.Target == "dayjs.dayjs.utc" || r.Target == "src/app.dayjs.utc" {
			t.Errorf("a member call on a package import was resolved: %s", r.Target)
		}
	}
	if !syms["src/api.ordersApi.remove"].HasRelation(facts.RelCalls, "src/api.http.del") {
		t.Errorf("member call inside an object member not resolved: %+v", syms["src/api.ordersApi.remove"].Relations)
	}
}

// A function in the object a factory returns from its body is a symbol that says where a
// line is, with no edges, so the calls in it stay the factory's. What a nested function
// returns is not the factory's object.
func TestReturnedObjectMembersOnlySayWhereALineIs(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/plugin.ts": `import { log } from "./log";

export function plugin() {
  const helper = () => { return { notMine() { log(); } }; };
  if (Math.random() > 2) {
    return { early() { log(); } };
  }
  return {
    name: "p",
    transform() { log(); },
    load: { handler: () => { log(); } },
  };
}

export const arrow = () => {
  return { run() { log(); } };
};

export const inline = () => ({ direct() { log(); } });
`,
		"src/log.ts": "export function log() {}\n",
	}, false)
	byName := map[string]facts.Fact{}
	for _, f := range ff {
		if f.Kind == facts.KindSymbol {
			byName[f.Name] = f
		}
	}
	for _, want := range []string{"src.plugin.early", "src.plugin.transform", "src.plugin.load.handler", "src.arrow.run", "src.inline.direct"} {
		f, ok := byName[want]
		if !ok {
			t.Errorf("%s: no symbol", want)
			continue
		}
		if f.PropString(facts.PropMemberOf) == "" || f.EndLine < f.Line {
			t.Errorf("%s: member_of %q, span %d-%d", want, f.PropString(facts.PropMemberOf), f.Line, f.EndLine)
		}
		if b, _ := f.PropAny("framework_registered").(bool); !b {
			t.Errorf("%s: not framework_registered", want)
		}
		if len(f.Relations) != 1 || f.Relations[0].Kind != facts.RelDeclares {
			t.Errorf("%s: relations %+v, want only its declaration", want, f.Relations)
		}
	}
	if _, ok := byName["src.plugin.notMine"]; ok {
		t.Error("a nested function's returned object was credited to the factory")
	}
	var calls int
	for _, r := range byName["src.plugin"].Relations {
		if r.Kind == facts.RelCalls {
			calls++
		}
	}
	if calls == 0 {
		t.Error("the factory lost the calls inside its returned object")
	}
}
