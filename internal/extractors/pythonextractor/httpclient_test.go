package pythonextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func clientRoutes(ff []facts.Fact) map[string]facts.Fact {
	out := map[string]facts.Fact{}
	for _, f := range ff {
		if f.Kind == facts.KindRoute && f.PropAny(facts.PropRole) == facts.RoleClient &&
			f.PropString(facts.PropSource) == facts.RouteSourcePythonHTTPClient {
			out[f.PropString("method")+" "+f.Name] = f
		}
	}
	return out
}

// Requests through requests, httpx and aiohttp are client routes: module calls
// through the file's imports (an alias included), and calls on a client instance
// bound by assignment or `with … as`. The base URL an f-string or a concatenation
// starts with is dropped, other interpolations become {}, and an absolute URL is
// external. Each names its enclosing function as caller.
func TestPythonHTTPClientCalls(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"app/clients/orders.py": `import requests
import httpx as hx
import aiohttp
from requests import Session

BASE_URL = "http://orders:8080"

class OrdersClient:
    def __init__(self):
        self.session = Session()

    def get(self, order_id):
        return self.session.get(f"{BASE_URL}/api/orders/{order_id}")

    def cancel(self, order_id):
        return requests.request("POST", BASE_URL + "/api/orders/" + str(order_id) + "/cancel")

def search(q):
    return hx.get("/api/search", params={"q": q})

async def stream():
    async with aiohttp.ClientSession() as session:
        async with session.post("https://events.example.com/v1/ingest") as resp:
            return await resp.json()

def create():
    with hx.Client() as client:
        client.put(url="/api/orders/bulk")
`,
		"app/other.py": `requests = {"get": print}

def not_http():
    requests.get("/api/nope")
`,
		"tests/test_orders.py": `import requests

def test_it():
    requests.get("/api/orders/1")
`,
	})
	got := clientRoutes(ff)
	for key, caller := range map[string]string{
		"GET /api/orders/{}":         "app/clients/orders.OrdersClient.get",
		"POST /api/orders/{}/cancel": "app/clients/orders.OrdersClient.cancel",
		"GET /api/search":            "app/clients/orders.search",
		"POST /v1/ingest":            "app/clients/orders.stream",
		"PUT /api/orders/bulk":       "app/clients/orders.create",
	} {
		f, ok := got[key]
		if !ok {
			t.Errorf("missing client route %s; got %v", key, routeKeys(got))
			continue
		}
		if c := f.PropString(facts.PropCaller); c != caller {
			t.Errorf("%s caller = %q, want %q", key, c, caller)
		}
	}
	if f := got["POST /v1/ingest"]; f.PropString("host") != "events.example.com" || !f.PropBool("external") {
		t.Errorf("absolute URL not marked external: %+v", f.Props)
	}
	if _, ok := got["GET /api/nope"]; ok {
		t.Error("a local name shadowing requests was read as the library")
	}
	for k, f := range got {
		if f.File == "tests/test_orders.py" {
			t.Errorf("test file emitted a client route: %s", k)
		}
	}
}

func routeKeys(m map[string]facts.Fact) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
